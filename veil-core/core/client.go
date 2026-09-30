package core

import (
	"context"
	"errors"
	"net"
	"time"
	"veil/internal/mux"
	"veil/internal/transport"
	"veil/internal/wire"
)

// Client owns one bounded connection pool, shared by any number of inbounds.
// Each physical TLS connection carries independently flow-controlled streams.
type Client struct {
	cfg    ClientConfig
	pool   *pool
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	Stats  Stats
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if err := cfg.Config.defaults(); err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(cfg.Server); err != nil {
		return nil, err
	}
	cfg.DialContext = withDialTimeout(cfg.DialContext, cfg.DialTimeout)
	key, err := transport.DecodeKey(cfg.Secret)
	if err != nil {
		return nil, err
	}
	h, err := transport.Client(cfg.TLS)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{cfg: cfg, pool: newPool(cfg, key, h), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go c.expire()
	return c, nil
}

func (c *Client) expire() {
	defer close(c.done)
	tick := time.NewTicker(min(c.cfg.PoolTimeout/2, 5*time.Second))
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			c.pool.close()
			return
		case <-tick.C:
			c.pool.expire()
		}
	}
}

// Close is idempotent. It aborts pending opens and active streams, closes idle
// connections and joins pool maintenance. Callers join their own Relay/Handle calls.
func (c *Client) Close() error { c.cancel(); <-c.done; return nil }

// SetTrafficProfile affects subsequent physical connections only.
func (c *Client) SetTrafficProfile(p TrafficProfile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	c.pool.traffic.Store(&p)
	return nil
}

// PoolStats is a consistent snapshot; Total includes in-progress dials.
type PoolStats struct {
	Total    int  `json:"total"`
	Idle     int  `json:"idle"`
	Streams  int  `json:"streams"`
	Dialing  bool `json:"dialing"`
	Limit    int  `json:"limit"`
	Draining int  `json:"draining,omitempty"`
}

func (c *Client) PoolStats() PoolStats {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	p := PoolStats{Total: c.pool.total, Idle: c.pool.idleLocked(), Dialing: c.pool.dialing, Limit: c.cfg.MaxConnections}
	for s := range c.pool.all {
		p.Streams += s.active
		if s.draining {
			p.Draining++
		}
	}
	return p
}

// Open waits for remote OPENED. ctx governs the entire stream lifetime, not
// just dialing. The caller must Close the stream even if it never calls Relay.
func (c *Client) Open(ctx context.Context, address string) (*Stream, error) {
	payload, err := wire.EncodeAddress(address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stopClient := context.AfterFunc(c.ctx, cancel)
	if c.ctx.Err() != nil {
		cancel()
	}
	channel, err := c.pool.get(ctx)
	if err != nil {
		stopClient()
		cancel()
		return nil, opError("acquire tunnel", err)
	}
	stream, err := channel.mux.Open(ctx, payload)
	if err != nil {
		if errors.Is(err, mux.ErrOpenTimeout) && ctx.Err() == nil {
			c.pool.drain(channel)
		}
		c.pool.put(channel)
		stopClient()
		cancel()
		var failure mux.OpenError
		if errors.As(err, &failure) {
			err = wire.OpenFailure(failure)
		}
		return nil, opError("open target", err)
	}
	c.Stats.ActiveStreams.Add(1)
	s := &Stream{client: c, channel: stream, session: channel, ctx: ctx, cancel: cancel, stopClient: stopClient}
	s.mu.Lock()
	s.stop = context.AfterFunc(ctx, func() { s.Close() })
	s.mu.Unlock()
	if err = ctx.Err(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
