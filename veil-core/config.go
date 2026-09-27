// Package core implements Veil TCP streams without listener, UI or host policy.
package core

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"time"
	"veil/internal/transport"
)

// TLSConfig selects the existing TLS 1.3 or REALITY transport.
type TLSConfig = transport.Settings

// DialFunc must honor ctx and return a connection supporting TCP half-close.
// Client hooks can bind/protect sockets; server hooks can apply destination policy.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// The default net.Dialer already enforces its timeout. Only injected dialers
// need an outer deadline; wrapping both would allocate duplicate timers.
func withDialTimeout(dial DialFunc, timeout time.Duration) DialFunc {
	if dial == nil {
		return (&net.Dialer{Timeout: timeout}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return dial(ctx, network, address)
	}
}

// Config is copied at construction. Zero values select the CLI's defaults.
// MaxConnections/MaxIdle bound the client pool; listener limits are separate.
type Config struct {
	Secret                                                  string
	TLS                                                     TLSConfig
	MaxConnections, MaxIdle                                 int
	HandshakeTimeout, DialTimeout, IdleTimeout, PoolTimeout time.Duration
}

type ClientConfig struct {
	Config
	Server      string
	DialContext DialFunc
}

type ServerConfig struct {
	Config
	DialContext DialFunc
}

// Stats contains monotonic counters. Core updates Completed and Authenticated;
// an optional listener runner updates Accepted, Rejected and Failed.
// Do not copy Stats after use.
type Stats struct{ Accepted, Rejected, Completed, Failed, Authenticated atomic.Uint64 }

func (c *Config) defaults() error {
	if c.MaxConnections == 0 {
		c.MaxConnections = 64
	}
	if c.MaxIdle == 0 {
		c.MaxIdle = min(8, c.MaxConnections)
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 10 * time.Second
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 120 * time.Second
	}
	if c.PoolTimeout == 0 {
		c.PoolTimeout = 20 * time.Second
	}
	if c.MaxConnections < 1 || c.MaxConnections > 4096 || c.MaxIdle < 0 || c.MaxIdle > c.MaxConnections {
		return errors.New("invalid connection or pool limits")
	}
	for _, v := range []time.Duration{c.HandshakeTimeout, c.DialTimeout, c.IdleTimeout, c.PoolTimeout} {
		if v < time.Millisecond || v > 24*time.Hour {
			return errors.New("timeouts must be 1ms..24h")
		}
	}
	c.TLS.Fingerprints = slices.Clone(c.TLS.Fingerprints)
	return nil
}
