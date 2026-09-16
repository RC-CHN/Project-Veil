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
	sl "veil.local/core/internal/streamlink"
	so "veil.local/core/internal/streamopen"
)

type Client struct {
	directSlots   chan struct{}
	directWorkers sync.WaitGroup
	directRun     bool

	pool                  *clientMuxPool
	cfg                   ClientConfig
	seed                  [32]byte
	program               *b.BatchProgram
	flight                *flightProgram
	tls                   *tls.Config
	endpoint, dialAddress string
	observer              Observer
	stats                 counters
	mu                    sync.Mutex
	listen                string
	ready                 bool
	expires               time.Time
	conns                 map[*net.TCPConn]bool
}

func NewClient(cfg ClientConfig, observer Observer) (*Client, error) {
	return newClient(cfg, observer, nil)
}
func newClient(cfg ClientConfig, observer Observer, inputs *MemoryInputs) (*Client, error) {
	if e := cfg.defaults(); e != nil {
		return nil, e
	}
	seed, program, flight, cert, ca, e := clientInputs(cfg, inputs)
	if e != nil {
		return nil, e
	}
	u, e := endpoint(cfg.ServerURL)
	if e != nil {
		return nil, e
	}
	address := cfg.DialAddress
	if address == "" {
		address = u.Host
		if u.Port() == "" {
			address = net.JoinHostPort(u.Hostname(), "443")
		}
	}
	return &Client{cfg: cfg, seed: seed, program: program, flight: flight, endpoint: u.String(), dialAddress: address, tls: &tls.Config{RootCAs: ca, Certificates: []tls.Certificate{cert}, ServerName: u.Hostname(), MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}, observer: observer, conns: make(map[*net.TCPConn]bool), expires: cert.Leaf.NotAfter}, nil
}
func (c *Client) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats.snapshot("client", c.listen, c.modelID(), c.ready && time.Now().Before(c.expires))
	s.ConnectionLimit = c.cfg.MaxConnections
	if c.cfg.Version >= 2 {
		s.InnerProtocol = runtimeInner(c.cfg.Version)
		s.CarrierLimit = c.cfg.MaxCarriers
		s.StreamLimit = c.cfg.MaxConnections
	}
	return s
}
func (c *Client) Run(ctx context.Context) error {
	addr, e := net.ResolveTCPAddr("tcp", c.cfg.Listen)
	if e != nil {
		return e
	}
	l, e := net.ListenTCP("tcp", addr)
	if e != nil {
		return e
	}
	defer l.Close()
	if c.cfg.Version >= 2 {
		c.pool = newClientMuxPool(c, ctx)
	}
	c.mu.Lock()
	c.listen = l.Addr().String()
	c.ready = true
	c.mu.Unlock()
	slots := make(chan struct{}, c.cfg.MaxConnections)
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() {
		l.Close()
		c.mu.Lock()
		for conn := range c.conns {
			conn.SetLinger(0)
			conn.Close()
		}
		c.mu.Unlock()
	})
	defer stop()
	for {
		conn, err := l.AcceptTCP()
		if err != nil {
			e = err
			break
		}
		select {
		case slots <- struct{}{}:
		default:
			c.stats.failed.Add(1)
			c.stats.connectionRejected.Add(1)
			conn.SetLinger(0)
			conn.Close()
			continue
		}
		c.mu.Lock()
		c.conns[conn] = true
		c.stats.openConnection()
		c.mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				conn.Close()
				c.mu.Lock()
				delete(c.conns, conn)
				c.stats.connections.Add(-1)
				c.mu.Unlock()
				// Retire accounting before permitting another accepted connection.
				<-slots
			}()
			if c.cfg.Version >= 2 {
				c.serveMuxSOCKS(ctx, conn)
			} else {
				c.serveSOCKS(ctx, conn)
			}
		}()
	}
	c.mu.Lock()
	c.ready = false
	for conn := range c.conns {
		conn.SetLinger(0)
		conn.Close()
	}
	c.mu.Unlock()
	if c.pool != nil {
		c.pool.close()
	}
	workers.Wait()
	if c.pool != nil {
		c.pool.workers.Wait()
	}
	if ctx.Err() != nil {
		return nil
	}
	return e
}
func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, e := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}
func socksResponse(w io.Writer, code byte) error {
	return writeFull(w, []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
}
func socksCode(code byte) byte {
	switch code {
	case so.OK:
		return 0
	case so.Denied:
		return 2
	case so.ResolutionFailed:
		return 4
	case so.ConnectFailed:
		return 5
	case so.TimedOut:
		return 6
	default:
		return 1
	}
}
func (c *Client) serveSOCKS(parent context.Context, conn *net.TCPConn) {
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	requested, e := readSOCKS(conn, true)
	if e != nil {
		c.stats.failed.Add(1)
		return
	}
	c.stats.sessions.Add(1)
	c.stats.started.Add(1)
	defer c.stats.sessions.Add(-1)
	ctx, cancel := context.WithTimeout(parent, 610*time.Second)
	defer cancel()
	event := SessionEvent{Role: "client", ModelID: c.modelID(), StartedNS: time.Now().UnixNano()}
	var source *openSource
	var session *b.BatchSession
	var transport *http.Transport
	var outer *tls.Conn
	var wire wireCount
	var trace budgetTrace
	var driven adaptiveDriveResult
	replied := false
	var stopIdle func()
	defer func() {
		if stopIdle != nil {
			stopIdle()
		}
		if session != nil {
			event.Model = session.Status()
		}
		if source != nil {
			event.Open, event.Link, event.OpenResultNS = source.snapshot()
			event.Pump = source.finish(e)
			event.UDP = source.udpStatus()
		}
		if !replied {
			_ = socksResponse(conn, 1)
		}
		if e != nil {
			conn.SetLinger(0)
		}
		if transport != nil {
			transport.CloseIdleConnections()
		}
		if outer != nil {
			outer.Close()
		}
		if session != nil {
			session.Close()
		}
		event.TLS = wire.snapshot()
		event.BudgetCount, event.BudgetSHA256, event.Budgets = trace.snapshot()
		event.Transactions = driven.TransactionCount
		event.CarrierTrace = &driven.Trace
		event.MaxActiveWaitUS = driven.MaxClientWaitUS
		event.EndedNS = time.Now().UnixNano()
		event.Error = errorText(e)
		event.Outcome = "transport_error"
		if e == nil && event.Model.State == 2 {
			event.Outcome = "connected_complete"
			c.stats.completed.Add(1)
			if event.Open.Rejected {
				event.Outcome = "open_rejected"
				c.stats.rejected.Add(1)
			}
		} else {
			c.stats.failed.Add(1)
		}
		if c.observer != nil {
			c.observer(event)
		}
	}()
	raw, e := dialCount(ctx, "tcp", c.dialAddress, &wire)
	if e != nil {
		return
	}
	outer = tls.Client(raw, c.tls.Clone())
	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, 5*time.Second)
	e = outer.HandshakeContext(handshakeCtx)
	handshakeCancel()
	if e != nil {
		return
	}
	state := outer.ConnectionState()
	material, e := parallelMaterial(&state, c.modelID())
	if e != nil {
		return
	}
	event.MaterialFingerprint = hashHex(material[:])
	event.Prefix = sessionPrefix(c.cfg.Bucket, c.seed, material)
	session, e = b.NewBatchSession(ctx, c.program, c.seed, material)
	if e != nil {
		return
	}
	h, e := so.NewClient(so.Request{Address: requested.address, Network: requested.network, Limits: so.Limits{Window: c.cfg.Window, MaxBytes: c.cfg.MaxBytes}})
	if e != nil {
		return
	}
	source = newOpenSource(ctx, h)
	source.makeClient = func(l so.Limits) (*socketLink, error) {
		if requested.network == so.NetworkUDP {
			udp, e := newClientDatagrams(ctx, conn, requested.source)
			if e != nil {
				return nil, e
			}
			if e = socksUDPResponse(conn, udp.LocalAddr()); e != nil {
				udp.Close()
				return nil, e
			}
			replied = true
			conn.SetDeadline(time.Time{})
			ready := make(chan struct{})
			close(ready)
			udp.startClient()
			pump, e := newConfiguredSocketLink(ctx, udp, sl.Config{Window: l.Window, MaxBytes: l.MaxBytes}, ready)
			if e != nil {
				udp.Close()
				return nil, e
			}
			return pump, nil
		}

		if e := socksResponse(conn, 0); e != nil {
			return nil, e
		}
		replied = true
		conn.SetDeadline(time.Time{})
		ready := make(chan struct{})
		close(ready)
		return newConfiguredSocketLink(ctx, conn, sl.Config{Window: l.Window, MaxBytes: l.MaxBytes}, ready)
	}
	var dialed atomic.Bool
	transport = &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, MaxConnsPerHost: 1, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: c.tls.Clone(), DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
		if !dialed.CompareAndSwap(false, true) {
			return nil, errors.New("replacement connection prohibited")
		}
		return outer, nil
	}}
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect prohibited") }}
	stopIdle = observeIdle(ctx, cancel, source, time.Duration(c.cfg.IdleTimeoutMS)*time.Millisecond)
	deliver := func(p []byte, eof bool) error {
		if err := source.deliver(p, eof); err != nil {
			return err
		}
		st := source.handshake.Status()
		if st.Rejected && !replied {
			err := socksResponse(conn, socksCode(st.Result.Code))
			replied = true
			return err
		}
		return nil
	}
	driven, e = driveAdaptive(ctx, session, client, c.endpoint, event.Prefix, source, adaptiveHooks{Deliver: deliver, AfterPlan: trace.add})
}
