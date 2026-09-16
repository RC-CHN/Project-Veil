package session

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
	b "veil.local/core/internal/behavior"
	d "veil.local/core/internal/destination"
	sl "veil.local/core/internal/streamlink"
	so "veil.local/core/internal/streamopen"
)

type Server struct {
	OnReady func()

	streamAdmission *d.Gate
	udpConnector    *d.Connector
	stopping        bool
	cfg             ServerConfig
	seed            [32]byte
	program         *b.BatchProgram
	flight          *flightProgram
	tls             *tls.Config
	store           *backend
	admission       *d.Gate
	connector       *d.Connector
	stats           counters
	observer        Observer
	mu              sync.Mutex
	listen          string
	ready           bool
	expires         time.Time
	entries         map[net.Conn]*serverConnection
	workers         sync.WaitGroup
}
type connectionKey struct{}
type serverConnection struct {
	lifetime          *carrierLifetime
	muxSource         *muxSource
	flightFront       *flightFront
	muxAcceptDone     chan struct{}
	muxWorkers        sync.WaitGroup
	httpDone          chan struct{}
	httpOnce          sync.Once
	server            *Server
	conn              net.Conn
	ctx               context.Context
	cancel            context.CancelFunc
	once, stopOnce    sync.Once
	mu                sync.Mutex
	closing           bool
	handlers          sync.WaitGroup
	initErr           error
	front             *adaptiveFront
	source            *openSource
	opener            chan error
	stopIdle          func()
	release           func()
	prefix, principal string
	material          [32]byte
	created           [2]bool
	started           int64
	trace             budgetTrace
}

func NewServer(cfg ServerConfig, observer Observer) (*Server, error) {
	return newServer(cfg, observer, nil)
}
func newServer(cfg ServerConfig, observer Observer, inputs *MemoryInputs) (*Server, error) {
	if e := cfg.defaults(); e != nil {
		return nil, e
	}
	seed, program, flight, cert, ca, e := serverInputs(cfg, inputs)
	if e != nil {
		return nil, e
	}
	if flight != nil && cfg.MaxSessions*flight.instances*2 > 64 {
		return nil, errors.New("flight object capacity exceeds 64")
	}
	var store *backend
	if cfg.BackendMode == "local-object-v1" {
		objects := cfg.MaxSessions * 2
		if flight != nil {
			objects *= flight.instances
		}
		store, e = newLocalBackend(cfg.Bucket, objects)
		if e != nil {
			return nil, e
		}
	} else {
		backendCA, err := roots(cfg.BackendCA)
		if err != nil {
			return nil, err
		}
		u, err := endpoint(cfg.BackendURL)
		if err != nil {
			return nil, err
		}
		store = newBackend(u.String(), cfg.BackendAccess, cfg.BackendSecret, backendCA)
	}
	allowed := make(map[string]bool)
	for _, f := range cfg.ClientFingerprints {
		p, e := hex.DecodeString(f)
		if e != nil || len(p) != 32 || hex.EncodeToString(p) != f {
			return nil, errors.New("client certificate fingerprint")
		}
		allowed[f] = true
	}
	admission, e := d.NewGate(cfg.MaxSessions, cfg.MaxSessionsPerIdentity)
	if e != nil {
		return nil, e
	}
	targetTotal, targetPer := cfg.MaxSessions, cfg.MaxSessionsPerIdentity
	if cfg.Version >= 2 {
		targetTotal, targetPer = cfg.MaxActiveStreams, cfg.MaxActiveStreamsPerIdentity
	}
	targetGate, _ := d.NewGate(targetTotal, targetPer)
	var resolver d.Resolver
	if cfg.DNSAddress != "" {
		a, e := netip.ParseAddrPort(cfg.DNSAddress)
		if e != nil || a.Port() == 0 || a.Addr().Zone() != "" {
			return nil, errors.New("DNS address must be numeric with a port")
		}
		resolver = &net.Resolver{PreferGo: true, StrictErrors: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, cfg.DNSAddress)
		}}
	}
	connector, e := d.New(d.Config{Allow: cfg.AllowCIDRs, Deny: cfg.DenyCIDRs, Timeout: time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond}, targetGate, resolver, nil)
	if e != nil {
		return nil, e
	}
	s := &Server{cfg: cfg, seed: seed, program: program, flight: flight, admission: admission, connector: connector, observer: observer, entries: make(map[net.Conn]*serverConnection), expires: cert.Leaf.NotAfter}
	if cfg.Version >= 2 {
		s.streamAdmission, _ = d.NewGate(targetTotal, targetPer)
	}
	udpGate, e := d.NewGate(targetTotal*cfg.UDPMaxTargets, targetPer*cfg.UDPMaxTargets)
	if e != nil {
		return nil, e
	}
	s.udpConnector, e = d.New(d.Config{Allow: cfg.AllowCIDRs, Deny: cfg.DenyCIDRs, Timeout: time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond}, udpGate, resolver, nil)
	if e != nil {
		return nil, e
	}
	s.store = store
	s.tls = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, ClientCAs: ca, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"h2"}, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 || !allowed[hashHex(cs.PeerCertificates[0].Raw)] {
			s.stats.authDenied.Add(1)
			return errors.New("client certificate is not admitted")
		}
		return nil
	}}
	return s, nil
}
func (s *Server) Snapshot() Snapshot {
	s.mu.Lock()
	v := s.stats.snapshot("server", s.listen, s.modelID(), s.ready && time.Now().Before(s.expires))
	s.mu.Unlock()
	v.ConnectionLimit = s.cfg.MaxConnections
	if s.cfg.Version >= 2 {
		v.InnerProtocol = runtimeInner(s.cfg.Version)
		v.CarrierLimit = s.cfg.MaxSessions
		v.StreamLimit = s.cfg.MaxSessions * s.cfg.Mux.Streams
		v.StreamAdmissionLimit = s.cfg.MaxActiveStreams
		g := s.streamAdmission.Status()
		v.AdmittedStreams = g.Active
		v.MaximumAdmittedStreams = g.Maximum
	}
	s.store.mu.Lock()
	v.BackendRequests = s.store.requests
	v.BackendFailures = s.store.failures
	s.store.mu.Unlock()
	backendWire := s.store.wire.snapshot()
	v.BackendTLS = &backendWire
	v.BackendMode = s.cfg.BackendMode
	if s.store.local != nil {
		local := s.store.local.Status()
		v.LocalObjects = &local
	}
	return v
}
func (s *Server) Run(ctx context.Context) error {
	l, e := net.Listen("tcp", s.cfg.Listen)
	if e != nil {
		s.store.close()
		return e
	}
	return s.Serve(ctx, l)
}

// Serve owns the supplied listener from entry through all return paths.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	defer l.Close()
	defer s.store.close()
	setup, cancel := context.WithTimeout(ctx, 8*time.Second)
	e := ensureBucket(setup, s.store, s.cfg.Bucket)
	cancel()
	if e != nil {
		return e
	}
	limited := &cappedListener{Listener: l, slots: make(chan struct{}, s.cfg.MaxConnections), reject: func() { s.stats.connectionRejected.Add(1) }}
	hs := &http.Server{TLSConfig: s.tls, MaxHeaderBytes: 32 << 10, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 8 * time.Second, IdleTimeout: 5 * time.Second}
	hs.ConnContext = func(base context.Context, c net.Conn) context.Context {
		lifetime := newCarrierLifetime(ctx, s.flight)
		cc, stop := lifetime.ctx, lifetime.cancel
		entry := &serverConnection{lifetime: lifetime, httpDone: make(chan struct{}), server: s, conn: c, ctx: cc, cancel: stop, started: time.Now().UnixNano()}
		s.mu.Lock()
		s.entries[c] = entry
		s.workers.Add(1)
		s.stats.openConnection()
		s.mu.Unlock()
		context.AfterFunc(cc, entry.stop)
		return context.WithValue(base, connectionKey{}, entry)
	}
	hs.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateClosed || state == http.StateHijacked {
			s.mu.Lock()
			entry := s.entries[c]
			s.mu.Unlock()
			if entry != nil {
				entry.httpOnce.Do(func() { close(entry.httpDone) })
				entry.stop()
			}
		}
	}
	hs.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry, ok := r.Context().Value(connectionKey{}).(*serverConnection)
		if !ok {
			http.Error(w, "connection context", 400)
			return
		}
		entry.serve(w, r)
	})
	s.mu.Lock()
	s.listen = l.Addr().String()
	s.ready = true
	s.mu.Unlock()
	if s.OnReady != nil {
		s.OnReady()
	}
	done := make(chan error, 1)
	go func() { done <- hs.ServeTLS(limited, "", "") }()
	serveStopped := false
	select {
	case e = <-done:
		serveStopped = true
	case <-ctx.Done():
		e = ctx.Err()
	}
	s.mu.Lock()
	s.ready = false
	s.stopping = true
	s.mu.Unlock()
	hs.Close()
	limited.Close()
	// Join the accept loop before waiting for workers, so ConnContext cannot
	// register another worker concurrently with Wait, even during shutdown.
	if !serveStopped {
		<-done
	}
	s.mu.Lock()
	entries := make([]*serverConnection, 0, len(s.entries))
	for _, c := range s.entries {
		entries = append(entries, c)
	}
	s.mu.Unlock()
	for _, c := range entries {
		c.stop()
	}

	s.workers.Wait()
	if errors.Is(e, context.Canceled) || errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
func (c *serverConnection) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		http.Error(w, "closed", 400)
		return
	}
	c.handlers.Add(1)
	c.mu.Unlock()
	defer c.handlers.Done()
	c.once.Do(func() { c.initErr = c.initialize(r) })
	if c.initErr != nil {
		http.Error(w, "session setup rejected", 400)
		go c.stop()
		return
	}
	if c.flightFront != nil {
		c.flightFront.ServeHTTP(w, r)
		if c.flightFront.source.mux.Status().Ready && !c.lifetime.establish() {
			c.cancel()
		}
	} else {
		c.front.ServeHTTP(w, r)
	}
}
func (c *serverConnection) initialize(r *http.Request) error {
	s := c.server
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return errors.New("verified peer required")
	}
	material, e := modelMaterial(r.TLS, s.program, s.flight, s.cfg.Version)
	if e != nil {
		return e
	}
	c.material = material
	c.principal = hashHex(r.TLS.PeerCertificates[0].Raw)
	c.prefix = sessionPrefix(s.cfg.Bucket, s.seed, material)
	if s.flight != nil {
		if _, e = flightPrefixes(r, s.flight, s.seed, material, s.cfg.Bucket); e != nil {
			return e
		}
	} else if r.URL.RawQuery != "" || r.URL.RawPath != "" || !(r.Method == "HEAD" && r.URL.Path == "/"+c.prefix+"/upload" || r.Method == "GET" && r.URL.Path == "/"+c.prefix+"/download") {
		return errors.New("bootstrap path or operation")
	}
	release, e := s.admission.Acquire(c.principal)
	if e != nil {
		return e
	}
	c.release = release
	s.stats.sessions.Add(1)
	s.stats.started.Add(1)
	if s.flight != nil {
		return c.initializeFlight(r)
	}
	if e = prepareObjects(c.ctx, s.store, c.prefix, &c.created); e != nil {
		return e
	}
	if s.cfg.Version >= 2 {
		return c.initializeMux()
	}
	h, e := so.NewServer(so.Limits{Window: s.cfg.Window, MaxBytes: s.cfg.MaxBytes})
	if e != nil {
		return e
	}
	source := newOpenSource(c.ctx, h)
	c.source = source
	c.front = &adaptiveFront{parallelFront: &parallelFront{ctx: c.ctx, cancel: c.cancel, program: s.program, seed: s.seed, store: s.store, bucket: c.prefix, active: make(chan struct{}, 2), onPlan: c.trace.add}, output: source, inputDelivery: source.deliver, inputHash: sha256.New()}
	c.opener = make(chan error, 1)
	go func() {
		request, e := h.WaitRequest(c.ctx)
		if e != nil {
			c.opener <- e
			return
		}
		if request.Network == so.NetworkUDP {
			limits := so.Limits{Window: min(request.Limits.Window, s.cfg.Window), MaxBytes: min(request.Limits.MaxBytes, s.cfg.MaxBytes)}
			udp := newServerDatagrams(c.ctx, s.udpConnector, c.principal, s.cfg.UDPMaxTargets, time.Duration(s.cfg.UDPIdleMS)*time.Millisecond)
			ready := make(chan struct{})
			close(ready)
			pump, e := newConfiguredSocketLink(c.ctx, udp, sl.Config{Window: limits.Window, MaxBytes: limits.MaxBytes}, ready)
			if e != nil {
				udp.Close()
				c.opener <- e
				return
			}
			e = source.attach(pump, so.Result{Code: so.OK, Limits: limits})
			if e != nil {
				pump.close(e)
			}
			c.opener <- e
			return
		}
		conn, e := s.connector.Open(c.ctx, c.principal, request.Address)
		if e != nil {
			if c.ctx.Err() != nil {
				c.opener <- c.ctx.Err()
				return
			}
			code := byte(so.ConnectFailed)
			switch {
			case errors.Is(e, d.ErrDenied):
				code = so.Denied
			case errors.Is(e, d.ErrBusy):
				code = so.Busy
			case errors.Is(e, d.ErrResolve):
				code = so.ResolutionFailed
			case errors.Is(e, context.DeadlineExceeded):
				code = so.TimedOut
			}
			c.opener <- source.attach(nil, so.Result{Code: code})
			return
		}
		limits := so.Limits{Window: min(request.Limits.Window, s.cfg.Window), MaxBytes: min(request.Limits.MaxBytes, s.cfg.MaxBytes)}
		ready := make(chan struct{})
		close(ready)
		pump, e := newConfiguredSocketLink(c.ctx, conn, sl.Config{Window: limits.Window, MaxBytes: limits.MaxBytes}, ready)
		if e != nil {
			conn.Release()
			c.opener <- e
			return
		}
		e = source.attach(pump, so.Result{Code: so.OK, Limits: limits})
		if e != nil {
			pump.close(e)
		}
		c.opener <- e
	}()
	c.stopIdle = observeIdle(c.ctx, c.cancel, source, time.Duration(s.cfg.IdleTimeoutMS)*time.Millisecond)
	return nil
}
func (c *serverConnection) stop() {
	c.stopOnce.Do(func() { c.mu.Lock(); c.closing = true; c.mu.Unlock(); go c.cleanup() })
}
func (c *serverConnection) cleanup() {
	s := c.server
	// Closing the frontend interrupts handlers/setup. Its model may already be
	// complete; preserve clean inner FIN state before cancelling that context.
	c.conn.Close()
	<-c.httpDone
	c.handlers.Wait()
	event := SessionEvent{Role: "server", Prefix: c.prefix, Principal: c.principal, ModelID: s.modelID(), MaterialFingerprint: hashHex(c.material[:]), StartedNS: c.started}
	normal := false
	if c.front != nil {
		c.front.mu.Lock()
		if c.front.session != nil {
			event.Model = c.front.session.Status()
			if s.cfg.Version >= 2 {
				event.ModelMaterialFingerprint = hashHex(c.front.material[:])
			}
		}
		event.Transactions = c.front.eventCount
		trace := c.front.trace
		trace.Points = append([]CarrierPoint(nil), trace.Points...)
		event.CarrierTrace = &trace
		event.MaxActiveWaitUS = c.front.maxWaitUS
		c.front.mu.Unlock()
		normal = event.Model.State == 2 && !event.Model.Closed
	}
	if c.flightFront != nil {
		event.ModelMaterialFingerprint = hashHex(c.material[:])
		c.flightFront.snapshot().apply(&event)
		normal = flightComplete(event.Flight, s.flight.instances)
	}
	if c.source != nil {
		event.Open, event.Link, event.OpenResultNS = c.source.snapshot()
	}
	cause := errors.New("frontend session ended before model completion")
	if c.initErr != nil {
		cause = c.initErr
	}
	if normal {
		cause = nil
	}
	if c.muxSource != nil {
		event.EventType = "mux_carrier"
		event.LocalAddress, event.RemoteAddress = c.conn.LocalAddr().String(), c.conn.RemoteAddr().String()
		before := c.muxSource.mux.Status()
		if normal && (before.Active != 0 || !before.SourceEOF || !before.PeerEOF || before.Pending) {
			normal = false
			cause = errors.New("model ended without mux terminal")
		}
		if c.flightFront != nil {
			c.flightFront.source.finish(cause)
			if st := c.flightFront.source.status(); cause == nil && st.Error != "" {
				cause = errors.New(st.Error)
				normal = false
			}
		} else {
			c.muxSource.finish(cause)
		}
	}
	if normal && c.source != nil {
		event.Pump = c.source.finish(nil)
	}
	c.cancel()
	if c.stopIdle != nil {
		c.stopIdle()
	}
	if c.opener != nil {
		if e := <-c.opener; e != nil && cause == nil {
			cause = e
		}
	}
	if c.muxAcceptDone != nil {
		<-c.muxAcceptDone
		c.muxWorkers.Wait()
		v := c.muxSource.mux.Status()
		event.Mux = &v
		if c.flightFront != nil {
			event.MuxOutputSHA256, event.MuxInputSHA256 = c.flightFront.source.hashes()
		} else {
			event.MuxOutputSHA256, event.MuxInputSHA256 = c.muxSource.hashes()
		}
		if v.Active != 0 {
			s.stats.cleanupFailures.Add(1)
		}
	}
	if !normal && c.source != nil {
		event.Pump = c.source.finish(cause)
	}
	if c.source != nil {
		event.UDP = c.source.udpStatus()
	}
	if c.flightFront != nil {
		event.ObjectsDeleted = c.flightFront.finish(cause)
		event.Flight.Source = c.flightFront.source.status()
	} else {
		event.ObjectsDeleted = deleteObjects(s.store, c.prefix, c.created)
	}
	if !event.ObjectsDeleted {
		s.stats.cleanupFailures.Add(1)
	}
	if c.release != nil {
		c.release()
		s.stats.sessions.Add(-1)
	}
	if c.flightFront == nil {
		event.BudgetCount, event.BudgetSHA256, event.Budgets = c.trace.snapshot()
	}
	event.EndedNS = time.Now().UnixNano()
	event.Error = errorText(cause)
	event.Outcome = "transport_error"
	if normal {
		event.Outcome = "connected_complete"
		s.stats.completed.Add(1)
		if event.Open.Rejected {
			event.Outcome = "open_rejected"
			s.stats.rejected.Add(1)
		}
	} else {
		s.stats.failed.Add(1)
	}
	if s.observer != nil {
		s.observer(event)
	}
	s.mu.Lock()
	delete(s.entries, c.conn)
	s.stats.connections.Add(-1)
	s.mu.Unlock()
	releaseAccepted(c.conn)
	s.workers.Done()
}
