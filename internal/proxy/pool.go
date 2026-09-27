package proxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
	"veil/internal/transport"
	"veil/internal/wire"
)

type session struct {
	net.Conn
	r    wire.Reader
	at   time.Time
	auth []byte // pending first-OPEN proof; never retained by an idle session
}
type pool struct {
	mu        sync.Mutex
	idle      []*session
	all       map[*session]bool
	total     int
	closed    bool
	cfg       Config
	key       []byte
	handshake transport.Handshake
}

func newPool(cfg Config, key []byte, h transport.Handshake) *pool {
	return &pool{cfg: cfg, key: key, handshake: h, all: make(map[*session]bool)}
}
func (p *pool) drop(s *session) {
	s.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.all[s] {
		delete(p.all, s)
		p.total--
	}
}
func (p *pool) get(ctx context.Context) (*session, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	for len(p.idle) > 0 {
		last := len(p.idle) - 1
		s := p.idle[last]
		p.idle[last] = nil
		p.idle = p.idle[:last]
		if time.Since(s.at) < sec(p.cfg.PoolSeconds) {
			p.mu.Unlock()
			return s, nil
		}
		delete(p.all, s)
		p.total--
		s.Close()
	}
	if p.total >= p.cfg.MaxConnections {
		p.mu.Unlock()
		return nil, errors.New("veil: connection pool limit")
	}
	p.total++
	p.mu.Unlock()
	s, err := p.dial(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.total--
		return nil, err
	}
	if p.closed {
		p.total--
		s.Close()
		return nil, net.ErrClosed
	}
	p.all[s] = true
	return s, nil
}
func (p *pool) dial(ctx context.Context) (*session, error) {
	ctx, cancel := context.WithTimeout(ctx, sec(p.cfg.HandshakeSeconds))
	defer cancel()
	raw, err := (&net.Dialer{Timeout: sec(p.cfg.DialSeconds)}).DialContext(ctx, "tcp", p.cfg.Server)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	raw.SetDeadline(time.Now().Add(sec(p.cfg.HandshakeSeconds)))
	c, err := p.handshake(ctx, raw)
	if err != nil {
		raw.Close()
		return nil, err
	}
	s := &session{Conn: c, r: wire.Reader{R: c}}
	exporter, err := transport.Export(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	s.auth, err = wire.AuthPayload(p.key, exporter)
	if err != nil {
		raw.Close()
		c.Close()
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return s, nil
}
func (p *pool) put(s *session) {
	s.r.Release()
	transport.ReleaseBuffers(s.Conn)
	s.at = time.Now()
	p.mu.Lock()
	if p.closed || len(p.idle) >= p.cfg.MaxIdle {
		p.mu.Unlock()
		p.drop(s)
		return
	}
	p.idle = append(p.idle, s)
	p.mu.Unlock()
}
func (p *pool) expire() {
	p.mu.Lock()
	var expired []*session
	keep := p.idle[:0]
	for _, s := range p.idle {
		if time.Since(s.at) >= sec(p.cfg.PoolSeconds) {
			expired = append(expired, s)
		} else {
			keep = append(keep, s)
		}
	}
	for i := len(keep); i < len(p.idle); i++ {
		p.idle[i] = nil
	}
	p.idle = keep
	p.mu.Unlock()
	for _, s := range expired {
		p.drop(s)
	}
}
func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	all := make([]*session, 0, len(p.all))
	for s := range p.all {
		all = append(all, s)
	}
	p.idle = nil
	p.mu.Unlock()
	for _, s := range all {
		p.drop(s)
	}
}
