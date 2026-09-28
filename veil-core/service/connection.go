package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
	"veil/core"
	"veil/inbound"
)

// Inlet is a local proxy listener. All inlets of a Connection share one pool.
type Inlet struct {
	Protocol string `json:"protocol"`
	Listen   string `json:"listen"`
}

// Connection owns a client, its listeners and an optional local relay adapter.
// The owner serializes SetInlets and Close; snapshots and probes may run concurrently.
// Relay uses the same TCP forwarding path as a separately deployed Veil chain.
type proxyListener struct {
	inlet   Inlet
	handler atomic.Pointer[inbound.Handler]
	cancel  context.CancelFunc
	done    chan struct{}
}

type Connection struct {
	ctx       context.Context
	cfg       Config
	listeners map[string]*proxyListener
	mu        sync.Mutex
	client    *core.Client
	relay     Runtime
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	inlets    []Inlet
	lastError string
}

func clientSettings(c Config) core.ClientConfig {
	return core.ClientConfig{Server: c.Server, Config: core.Config{
		Secret: c.Secret, TLS: c.TLS, Traffic: c.Traffic, MaxConnections: c.MaxConnections, MaxIdle: c.MaxIdle,
		HandshakeTimeout: sec(c.HandshakeSeconds), DialTimeout: sec(c.DialSeconds), IdleTimeout: sec(c.IdleSeconds), PoolTimeout: sec(c.PoolSeconds),
	}}
}

func OpenConnection(cfg Config, inlets []Inlet, relay *Config) (*Connection, error) {
	if cfg.Role != "client" || cfg.Target != "" {
		return nil, errors.New("connection requires a proxy client")
	}
	if err := cfg.Defaults(); err != nil {
		return nil, err
	}
	c := &Connection{}
	if relay != nil {
		r := *relay
		r.Role = "client"
		r.Listen = "127.0.0.1:0"
		r.Target = cfg.Server
		r.Inbound = ""
		if err := c.relay.Start(r); err != nil {
			return nil, fmt.Errorf("relay: %w", err)
		}
		cfg.Server = c.relay.Snapshot().Listen
	}
	client, err := core.NewClient(clientSettings(cfg))
	if err != nil {
		c.relay.Close()
		return nil, err
	}
	c.client = client
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.cfg = cfg
	c.listeners = map[string]*proxyListener{}
	if err := c.SetInlets(inlets); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// SetInlets changes listeners without closing the shared client pool. New binds
// are prepared first; stopping one listener joins only its own handlers.
func (c *Connection) SetInlets(inlets []Inlet) error {
	staged := map[string]net.Listener{}
	handlers := map[string]inbound.Handler{}
	rollback := func() {
		for _, ln := range staged {
			ln.Close()
		}
	}
	for _, entry := range inlets {
		if _, duplicate := handlers[entry.Listen]; duplicate {
			rollback()
			return errors.New("duplicate listener address")
		}
		var h inbound.Handler
		switch entry.Protocol {
		case "socks":
			h = inbound.SOCKS5(c.client, sec(c.cfg.HandshakeSeconds))
		case "http":
			h = inbound.HTTP(c.client, sec(c.cfg.HandshakeSeconds))
		case "mixed":
			h = inbound.Mixed(c.client, sec(c.cfg.HandshakeSeconds))
		default:
			rollback()
			return errors.New("invalid inlet protocol")
		}
		handlers[entry.Listen] = h
		if c.listeners[entry.Listen] == nil {
			ln, err := net.Listen("tcp", entry.Listen)
			if err != nil {
				rollback()
				return err
			}
			staged[entry.Listen] = ln
		}
	}
	for addr, entry := range c.listeners {
		if _, keep := handlers[addr]; !keep {
			entry.cancel()
			<-entry.done
			delete(c.listeners, addr)
		}
	}
	var actual []Inlet
	for _, entry := range inlets {
		h := handlers[entry.Listen]
		if old := c.listeners[entry.Listen]; old != nil {
			old.handler.Store(&h)
			old.inlet.Protocol = entry.Protocol
			actual = append(actual, old.inlet)
			continue
		}
		ln := staged[entry.Listen]
		ctx, cancel := context.WithCancel(c.ctx)
		listener := &proxyListener{inlet: Inlet{entry.Protocol, ln.Addr().String()}, cancel: cancel, done: make(chan struct{})}
		listener.handler.Store(&h)
		c.listeners[entry.Listen] = listener
		actual = append(actual, listener.inlet)
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer close(listener.done)
			err := inbound.Serve(ctx, ln, func(ctx context.Context, n net.Conn) error { return (*listener.handler.Load())(ctx, n) }, inbound.ServeOptions{MaxConnections: c.cfg.MaxConnections, Stats: &c.client.Stats, OnError: c.failure})
			if err != nil {
				c.failure(err)
			}
		}()
	}
	c.mu.Lock()
	c.inlets = actual
	c.mu.Unlock()
	return nil
}

func (c *Connection) failure(err error) { c.mu.Lock(); defer c.mu.Unlock(); c.lastError = err.Error() }
func (c *Connection) Close() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.client != nil {
		c.client.Close()
	}
	c.wg.Wait()
	c.relay.Close()
}
func (c *Connection) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Snapshot{State: "running", Role: "client", LastConnectionError: c.lastError}
	if len(c.inlets) > 0 {
		s.Listen = c.inlets[0].Listen
	}
	a := &c.client.Stats
	s.Stats = Counters{a.Accepted.Load(), a.Rejected.Load(), a.Completed.Load(), a.Failed.Load(), a.Authenticated.Load()}
	return s
}
func (c *Connection) Inlets() []Inlet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Inlet(nil), c.inlets...)
}

// ProbeHTTP measures an HTTPS request through this client's actual tunnel pool.
// The temporary proxy is loopback-only and always joined before returning.
func (c *Connection) ProbeHTTP(ctx context.Context, target string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	proxy, _ := url.Parse("http://" + ln.Addr().String())
	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		inbound.Serve(serveCtx, ln, inbound.HTTP(c.client, 8*time.Second), inbound.ServeOptions{})
	}()
	defer func() { cancel(); ln.Close(); <-done }()
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("expected HTTP 204, received %d", response.StatusCode)
	}
	return nil
}
