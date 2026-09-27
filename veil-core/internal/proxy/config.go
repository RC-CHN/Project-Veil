package proxy

import (
	"errors"
	"time"
	"veil/internal/transport"
	"veil/internal/wire"
)

type Config struct {
	Role             string             `json:"role"`
	Listen           string             `json:"listen"`
	Server           string             `json:"server"`
	Target           string             `json:"target,omitempty"`
	Secret           string             `json:"secret"`
	TLS              transport.Settings `json:"tls"`
	MaxConnections   int                `json:"max_connections"`
	MaxIdle          int                `json:"max_idle"`
	HandshakeSeconds int                `json:"handshake_seconds"`
	DialSeconds      int                `json:"dial_seconds"`
	IdleSeconds      int                `json:"idle_seconds"`
	PoolSeconds      int                `json:"pool_seconds"`
}

func (c *Config) Defaults() error {
	if c.MaxConnections == 0 {
		c.MaxConnections = 64
	}
	if c.MaxIdle == 0 {
		c.MaxIdle = 8
	}
	if c.HandshakeSeconds == 0 {
		c.HandshakeSeconds = 10
	}
	if c.DialSeconds == 0 {
		c.DialSeconds = 10
	}
	if c.IdleSeconds == 0 {
		c.IdleSeconds = 120
	}
	if c.PoolSeconds == 0 {
		c.PoolSeconds = 20
	}
	if c.Listen == "" {
		if c.Role == "client" {
			c.Listen = "127.0.0.1:1080"
		} else {
			c.Listen = "127.0.0.1:8443"
		}
	}
	if c.MaxConnections < 1 || c.MaxConnections > 4096 || c.MaxIdle < 0 || c.MaxIdle > c.MaxConnections {
		return errors.New("invalid connection or pool limits")
	}
	for _, v := range []int{c.HandshakeSeconds, c.DialSeconds, c.IdleSeconds, c.PoolSeconds} {
		if v < 1 || v > 86400 {
			return errors.New("timeouts must be 1..86400 seconds")
		}
	}
	if c.Role != "client" && c.Role != "server" {
		return errors.New("role must be client or server")
	}
	if c.Role == "client" && c.Server == "" {
		return errors.New("server address required")
	}
	if c.Target != "" {
		if c.Role != "client" {
			return errors.New("target is only valid for client forwarding")
		}
		if _, err := wire.EncodeAddress(c.Target); err != nil {
			return err
		}
	}
	_, err := transport.DecodeKey(c.Secret)
	return err
}
func sec(n int) time.Duration { return time.Duration(n) * time.Second }
