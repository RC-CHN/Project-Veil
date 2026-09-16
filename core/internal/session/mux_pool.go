package session

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	b "veil.local/core/internal/behavior"
	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

type clientMuxPool struct {
	mu      sync.Mutex
	client  *Client
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
	entries []*clientMuxCarrier
	workers sync.WaitGroup
}
type clientMuxCarrier struct {
	lifetime                       *carrierLifetime
	pool                           *clientMuxPool
	ctx                            context.Context
	cancel                         context.CancelFunc
	source                         *muxSource
	flightSource                   *flightMuxSource // source.mux is a shared handle; only flightSource owns I/O and cleanup.
	readyDone, done                chan struct{}
	ready, readySignaled, retiring bool
	err                            error
	prefix                         string
	material                       [32]byte
	workers                        sync.WaitGroup
}

func newClientMuxPool(client *Client, parent context.Context) *clientMuxPool {
	ctx, cancel := context.WithCancel(parent)
	return &clientMuxPool{client: client, ctx: ctx, cancel: cancel}
}
func (p *clientMuxPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		p.cancel()
	}
}
func (p *clientMuxPool) acquire(ctx context.Context, r so.Request) (*clientMuxCarrier, *sm.Stream, error) {
	for {
		if e := ctx.Err(); e != nil {
			return nil, nil, e
		}
		p.mu.Lock()
		if p.closed || p.ctx.Err() != nil {
			p.mu.Unlock()
			return nil, nil, io.ErrClosedPipe
		}
		var starting, retiring *clientMuxCarrier
		var first *sm.Stream
		for _, entry := range p.entries {
			if entry.retiring {
				retiring = entry
				continue
			}
			if !entry.ready {
				starting = entry
				continue
			}
			stream, e := entry.source.mux.Open(r)
			if e == nil {
				entry.workers.Add(1)
				p.mu.Unlock()
				return entry, stream, nil
			}
			if errors.Is(e, sm.ErrDraining) {
				// This carrier still owns its streams and backend cleanup slot.
				// A not-yet-issued OPEN may wait for its replacement. Do not
				// mark entry.retiring here: only run's cleanup owns that state.
				retiring = entry
				continue
			}
			if !errors.Is(e, sm.ErrBusy) && !errors.Is(e, sm.ErrDraining) {
				p.mu.Unlock()
				return nil, nil, e
			}
		}
		if starting == nil && len(p.entries) < p.client.cfg.MaxCarriers {
			lifetime := newCarrierLifetime(p.ctx, p.client.flight)
			child, cancel := lifetime.ctx, lifetime.cancel
			cfg := sm.Config{Client: true, Limits: p.client.cfg.Mux.settings(p.client.cfg.Window, p.client.cfg.MaxBytes)}
			var m *sm.Session
			var flightSource *flightMuxSource
			var e error
			if p.client.flight != nil {
				cfg.MaxPending = p.client.flight.instances
				cfg.EarlyOpen = p.client.flight.earlyOpen()
				flightSource, e = newFlightMuxSource(child, cfg)
				if e == nil {
					m = flightSource.mux
				}
			} else {
				m, e = sm.New(child, cfg)
			}
			if e != nil {
				cancel()
				p.mu.Unlock()
				return nil, nil, e
			}
			starting = &clientMuxCarrier{lifetime: lifetime, pool: p, ctx: child, cancel: cancel, source: &muxSource{mux: m}, flightSource: flightSource, readyDone: make(chan struct{}), done: make(chan struct{})}
			if cfg.EarlyOpen {
				first, e = m.OpenFirst(r)
				if e != nil {
					flightSource.finish(e)
					cancel()
					p.mu.Unlock()
					return nil, nil, e
				}
				// Own the provisional stream before run can fail and join.
				starting.workers.Add(1)
			}
			p.entries = append(p.entries, starting)
			p.workers.Add(1)
			p.client.stats.sessions.Add(1)
			p.client.stats.started.Add(1)
			go starting.run()
		}
		p.mu.Unlock()
		if first != nil {
			return starting.awaitFirst(ctx, first)
		}
		if starting != nil {
			select {
			case <-starting.readyDone:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-p.ctx.Done():
				return nil, nil, p.ctx.Err()
			}
			p.mu.Lock()
			e := starting.err
			p.mu.Unlock()
			if e != nil {
				return nil, nil, e
			}
			continue
		}
		if retiring != nil {
			select {
			case <-retiring.done:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-p.ctx.Done():
				return nil, nil, p.ctx.Err()
			}
		}
		return nil, nil, sm.ErrBusy
	}
}

// awaitFirst never retries the provisional OPEN: the authenticated peer may
// already have started the target connection. Publication waits for readyDone
// so prefix/material are immutable before application event fields read them.
func (c *clientMuxCarrier) awaitFirst(ctx context.Context, first *sm.Stream) (*clientMuxCarrier, *sm.Stream, error) {
	handedOff := false
	defer func() {
		if !handedOff {
			_ = first.Reset(sm.EndCancelled)
			first.Release()
			c.workers.Done()
		}
	}()
	select {
	case <-c.readyDone:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-c.pool.ctx.Done():
		return nil, nil, c.pool.ctx.Err()
	}
	c.pool.mu.Lock()
	e := c.err
	if e == nil && (c.pool.closed || c.retiring || !c.ready) {
		e = io.ErrClosedPipe
	}
	c.pool.mu.Unlock()
	if e == nil {
		e = ctx.Err()
	}
	if e != nil {
		return nil, nil, e
	}
	handedOff = true
	return c, first, nil
}
func (c *clientMuxCarrier) markReady(e error) {
	if e == nil && !c.lifetime.establish() {
		c.cancel()
		e = context.DeadlineExceeded
	}
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	if !c.readySignaled {
		c.readySignaled = true
		c.ready = e == nil
		c.err = e
		close(c.readyDone)
	}
}
func (c *clientMuxCarrier) run() {
	p := c.pool
	client := p.client
	event := SessionEvent{EventType: "mux_carrier", Role: "client", ModelID: client.modelID(), StartedNS: time.Now().UnixNano()}
	var cause error
	var session *b.BatchSession
	var transport *http.Transport
	var outer *tls.Conn
	var wire wireCount
	var trace budgetTrace
	var driven adaptiveDriveResult
	var flightDriven flightDriveResult
	var stopIdle func()
	defer func() {
		if session != nil {
			event.Model = session.Status()
		}
		modelComplete := event.Model.State == 2 && !event.Model.Closed
		if client.flight != nil {
			modelComplete = flightComplete(flightDriven.Event, client.flight.instances)
		}
		before := c.source.mux.Status()
		if cause == nil && (!modelComplete || !before.SourceEOF || !before.PeerEOF || before.Active != 0 || before.Pending) {
			cause = errors.New("carrier ended without completed model and mux")
		}
		p.mu.Lock()
		c.retiring = true
		if cause != nil {
			c.err = cause
		}
		p.mu.Unlock()
		if c.flightSource != nil {
			c.flightSource.finish(cause)
			if st := c.flightSource.status(); cause == nil && st.Error != "" {
				cause = errors.New(st.Error)
			}
		} else {
			c.source.finish(cause)
		}
		c.cancel()
		c.markReady(func() error {
			if cause != nil {
				return cause
			}
			return io.ErrClosedPipe
		}())
		if transport != nil {
			transport.CloseIdleConnections()
		}
		if outer != nil {
			outer.Close()
		}
		if stopIdle != nil {
			stopIdle()
		}
		// acquire registers ownership under the same pool mutex before this
		// retiring transition, so no stream worker can be added during Wait.
		c.workers.Wait()
		if session != nil {
			session.Close()
		}
		v := c.source.mux.Status()
		event.Mux = &v
		if c.flightSource != nil {
			event.MuxOutputSHA256, event.MuxInputSHA256 = c.flightSource.hashes()
		} else {
			event.MuxOutputSHA256, event.MuxInputSHA256 = c.source.hashes()
		}
		event.Prefix = c.prefix
		event.MaterialFingerprint = hashHex(c.material[:])
		event.ModelMaterialFingerprint = hashHex(c.material[:])
		event.TLS = wire.snapshot()
		event.Transactions = driven.TransactionCount
		event.CarrierTrace = &driven.Trace
		event.MaxActiveWaitUS = driven.MaxClientWaitUS
		event.BudgetCount, event.BudgetSHA256, event.Budgets = trace.snapshot()
		if flightDriven.Event != nil {
			event.CarrierTrace = nil
			flightDriven.Event.Source = c.flightSource.status()
			flightDriven.Event.apply(&event)
		}
		event.EndedNS = time.Now().UnixNano()
		event.Error = errorText(cause)
		event.Outcome = "transport_error"
		if cause == nil && modelComplete {
			event.Outcome = "connected_complete"
			client.stats.completed.Add(1)
		} else {
			client.stats.failed.Add(1)
		}
		if v.Active != 0 {
			client.stats.cleanupFailures.Add(1)
		}
		if client.observer != nil {
			client.observer(event)
		}
		p.mu.Lock()
		for i, v := range p.entries {
			if v == c {
				copy(p.entries[i:], p.entries[i+1:])
				p.entries[len(p.entries)-1] = nil
				p.entries = p.entries[:len(p.entries)-1]
				break
			}
		}
		client.stats.sessions.Add(-1)
		close(c.done)
		p.mu.Unlock()
		p.workers.Done()
	}()
	raw, e := dialCount(c.ctx, "tcp", client.dialAddress, &wire)
	if e != nil {
		cause = e
		return
	}
	outer = tls.Client(raw, client.tls.Clone())
	event.LocalAddress, event.RemoteAddress = raw.LocalAddr().String(), raw.RemoteAddr().String()
	handshakeCtx, handshakeCancel := context.WithTimeout(c.ctx, 5*time.Second)
	cause = outer.HandshakeContext(handshakeCtx)
	handshakeCancel()
	if cause != nil {
		return
	}
	state := outer.ConnectionState()
	c.material, cause = modelMaterial(&state, client.program, client.flight, client.cfg.Version)
	if cause != nil {
		return
	}
	c.prefix = sessionPrefix(client.cfg.Bucket, client.seed, c.material)
	if client.flight == nil {
		session, cause = b.NewBatchSession(c.ctx, client.program, client.seed, c.material)
		if cause != nil {
			return
		}
	}
	var dialed atomic.Bool
	transport = &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, MaxConnsPerHost: 1, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: client.tls.Clone(), DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
		if !dialed.CompareAndSwap(false, true) {
			return nil, errors.New("replacement mux connection prohibited")
		}
		return outer, nil
	}}
	httpClient := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect prohibited") }}
	stopIdle = watchMuxIdle(c.ctx, c.source.mux, time.Duration(client.cfg.Mux.CarrierIdleMS)*time.Millisecond)
	if client.flight != nil {
		flightDriven, cause = driveFlight(c.ctx, client.flight, client.seed, &state, httpClient, client.endpoint, client.cfg.Bucket, c.flightSource, func() { c.markReady(nil) })
		return
	}
	deliver := func(data []byte, eof bool) error {
		if e := c.source.deliver(data, eof); e != nil {
			return e
		}
		if c.source.mux.Status().Ready {
			c.markReady(nil)
		}
		return nil
	}
	driven, cause = driveAdaptive(c.ctx, session, httpClient, client.endpoint, c.prefix, c.source, adaptiveHooks{Deliver: deliver, AfterPlan: trace.add})
}
