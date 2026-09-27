package core

import (
	"context"
	"net"
	"time"
	"veil/internal/transport"
	"veil/internal/wire"
)

// Client owns one bounded connection pool, shared by any number of inbounds.
// A physical TLS connection carries one active stream at a time.
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

// PoolStats is a consistent snapshot; Total includes in-progress dials.
type PoolStats struct{ Total, Idle int }

func (c *Client) PoolStats() PoolStats {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	return PoolStats{Total: c.pool.total, Idle: len(c.pool.idle)}
}

// Open waits for remote OPEN_OK. ctx governs the entire stream lifetime, not
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
	s := &Stream{client: c, channel: channel, ctx: ctx, cancel: cancel, stopClient: stopClient, busy: true}
	// Install under the lock so an already-cancelled context cannot race setup.
	s.mu.Lock()
	s.stop = context.AfterFunc(ctx, func() { s.Close() })
	s.mu.Unlock()
	channel.SetDeadline(time.Now().Add(c.cfg.DialTimeout + c.cfg.HandshakeTimeout))
	err = transport.RecordPadding(channel.Conn, c.cfg.TLS.RecordPadding)
	if err == nil {
		err = wire.WriteOpen(channel, channel.auth, payload)
	}
	if err == nil {
		err = wire.ReadOpenResult(&channel.r)
	}
	if err == nil {
		channel.auth = nil
		channel.SetDeadline(time.Time{})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		s.finishLocked(false)
		return nil, opError("open target", err)
	}
	return s, nil
}
