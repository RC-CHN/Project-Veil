package core

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"veil/internal/mux"
	"veil/internal/transport"
	"veil/internal/wire"
)

type session struct {
	mux      *mux.Session
	active   int
	at       time.Time
	draining bool // No new streams; existing streams retain their lifetime.
}
type pool struct {
	mu              sync.Mutex
	all             map[*session]bool
	total           int
	closed, dialing bool
	changed         chan struct{}
	dialError       error
	retryAt         time.Time
	retryDelay      time.Duration
	cfg             ClientConfig
	key             []byte
	handshake       transport.Handshake
	traffic         atomic.Pointer[TrafficProfile]
}

func newPool(cfg ClientConfig, key []byte, h transport.Handshake) *pool {
	p := &pool{cfg: cfg, key: key, handshake: h, all: make(map[*session]bool)}
	p.traffic.Store(cfg.Traffic)
	return p
}
func (p *pool) notifyLocked() {
	if p.changed != nil {
		close(p.changed)
		p.changed = nil
	}
}
func (p *pool) idleLocked() int {
	n := 0
	for s := range p.all {
		if s.active == 0 {
			n++
		}
	}
	return n
}
func (p *pool) removeLocked(s *session) bool {
	if !p.all[s] {
		return false
	}
	delete(p.all, s)
	p.total--
	p.notifyLocked()
	return true
}
func stopSession(s *session) { s.mux.Close(); s.mux.Wait() }
func (p *pool) get(ctx context.Context) (*session, error) {
	var cancel context.CancelFunc
	var dialErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, net.ErrClosed
		}
		var best *session
		var dead []*session
		bestBulk := false
		for s := range p.all {
			_, bulk, closed := s.mux.Snapshot()
			if closed || s.active == 0 && (s.draining || time.Since(s.at) >= p.cfg.PoolTimeout) {
				p.removeLocked(s)
				dead = append(dead, s)
				continue
			}
			if s.draining {
				continue
			}
			if s.active < mux.MaxStreams && (best == nil || bestBulk && !bulk || bestBulk == bulk && s.active < best.active) {
				best = s
				bestBulk = bulk
			}
		}
		if len(dead) > 0 {
			p.mu.Unlock()
			for _, s := range dead {
				stopSession(s)
			}
			continue
		}
		if best == nil && dialErr != nil {
			p.mu.Unlock()
			return nil, dialErr
		}
		// Mix real concurrency first. A second TCP lane separates sustained bulk
		// transfer or a busier group from new interactive work.
		wantNew := best == nil || (dialErr == nil && p.total < min(2, p.cfg.MaxConnections) && (bestBulk || best.active >= 4))
		if wantNew && p.dialError != nil && time.Now().Before(p.retryAt) {
			if best == nil {
				err := p.dialError
				p.mu.Unlock()
				return nil, err
			}
			wantNew = false
		}
		if wantNew && p.total < p.cfg.MaxConnections && !p.dialing {
			p.total++
			p.dialing = true
			p.mu.Unlock()
			s, err := p.dial(ctx)
			p.mu.Lock()
			p.dialing = false
			p.notifyLocked()
			if err != nil {
				p.total--
				if ctx.Err() == nil && !p.closed {
					// One short, shared cooldown prevents a request burst from
					// repeating the same failing TCP/TLS handshake. No timer or
					// background dial is needed: a later request retries.
					p.retryDelay = min(max(2*p.retryDelay, 100*time.Millisecond), time.Second)
					p.retryAt = time.Now().Add(p.retryDelay + rand.N(p.retryDelay))
					p.dialError = err
				}
				p.mu.Unlock()
				if best != nil && ctx.Err() == nil {
					// Expansion is optional. Recheck the live lanes instead of
					// failing work that the original connection can still carry.
					dialErr = err
					continue
				}
				return nil, err
			}
			if p.closed {
				p.total--
				p.mu.Unlock()
				stopSession(s)
				return nil, net.ErrClosed
			}
			p.dialError, p.retryAt, p.retryDelay = nil, time.Time{}, 0
			s.active = 1
			p.all[s] = true
			p.mu.Unlock()
			return s, nil
		}
		if wantNew && p.dialing && best == nil {
			if p.changed == nil {
				p.changed = make(chan struct{})
			}
			changed := p.changed
			p.mu.Unlock()
			if cancel == nil {
				// Bound queueing and any subsequent dial together. Allocate a
				// deadline only for queued opens, not the normal reuse path.
				ctx, cancel = context.WithTimeout(ctx, p.cfg.HandshakeTimeout)
				defer cancel()
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		if best != nil {
			best.active++
			p.mu.Unlock()
			return best, nil
		}
		p.mu.Unlock()
		return nil, errors.New("veil: connection pool limit")
	}
}

func (p *pool) drain(s *session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s.draining = true
	p.notifyLocked()
}

func muxOptions(c net.Conn, cfg Config, server bool) mux.Options {
	opts := mux.Options{Server: server, Profile: *cfg.Traffic, IdleTimeout: 2 * cfg.PoolTimeout, WriteTimeout: cfg.IdleTimeout}
	if cfg.TLS.RecordPadding {
		opts.Padding = func(limit, records, budget int) error { return transport.RecordBudget(c, limit, records, budget) }
	}
	return opts
}
func (p *pool) dial(ctx context.Context) (*session, error) {
	cfg := p.cfg.Config
	cfg.Traffic = p.traffic.Load()
	ctx, cancel := context.WithTimeout(ctx, p.cfg.HandshakeTimeout)
	defer cancel()
	raw, err := p.cfg.DialContext(ctx, "tcp", p.cfg.Server)
	if err != nil {
		return nil, opError("tunnel dial", err)
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	raw.SetDeadline(time.Now().Add(p.cfg.HandshakeTimeout))
	c, err := p.handshake(ctx, raw)
	if err != nil {
		raw.Close()
		return nil, opError("TLS handshake", err)
	}
	exporter, err := transport.Export(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	auth, err := wire.AuthPayload(p.key, exporter)
	if err != nil {
		c.Close()
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	var prefix bytes.Buffer
	if err = wire.Write(&prefix, wire.Auth, auth); err != nil {
		c.Close()
		return nil, err
	}
	opts := muxOptions(c, cfg, false)
	opts.OpenTimeout = p.cfg.DialTimeout + p.cfg.HandshakeTimeout
	opts.Prefix = prefix.Bytes()
	m, err := mux.New(c, opts)
	if err != nil {
		c.Close()
		return nil, err
	}
	return &session{mux: m}, nil
}
func (p *pool) put(s *session) {
	p.mu.Lock()
	if !p.all[s] {
		p.mu.Unlock()
		return
	}
	s.active--
	s.at = time.Now()
	_, _, closed := s.mux.Snapshot()
	drop := closed || p.closed || (s.active == 0 && (s.draining || p.idleLocked() > p.cfg.MaxIdle))
	if drop {
		p.removeLocked(s)
	} else {
		p.notifyLocked()
	}
	p.mu.Unlock()
	if drop {
		stopSession(s)
	}
}
func (p *pool) expire() {
	p.mu.Lock()
	var expired []*session
	for s := range p.all {
		_, _, closed := s.mux.Snapshot()
		if closed || s.active == 0 && (s.draining || time.Since(s.at) >= p.cfg.PoolTimeout) {
			p.removeLocked(s)
			expired = append(expired, s)
		}
	}
	p.mu.Unlock()
	for _, s := range expired {
		stopSession(s)
	}
}
func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	p.notifyLocked()
	var all []*session
	for s := range p.all {
		p.removeLocked(s)
		all = append(all, s)
	}
	p.mu.Unlock()
	for _, s := range all {
		stopSession(s)
	}
}
