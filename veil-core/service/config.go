package service

import (
	"errors"
	"fmt"
	"time"
	"veil/core"
	"veil/internal/transport"
	"veil/internal/wire"
)

type Config struct {
	Role             string               `json:"role"`
	Listen           string               `json:"listen"`
	Server           string               `json:"server"`
	Target           string               `json:"target,omitempty"`
	Inbound          string               `json:"inbound,omitempty"`
	Secret           string               `json:"secret"`
	TLS              transport.Settings   `json:"tls"`
	HTTPFallback     *HTTPFallbackConfig  `json:"http_fallback,omitempty"`
	Traffic          *core.TrafficProfile `json:"traffic,omitempty"`
	MaxConnections   int                  `json:"max_connections"`
	MaxIdle          int                  `json:"max_idle"`
	HandshakeSeconds int                  `json:"handshake_seconds"`
	DialSeconds      int                  `json:"dial_seconds"`
	IdleSeconds      int                  `json:"idle_seconds"`
	PoolSeconds      int                  `json:"pool_seconds"`
}

// HTTPFallbackConfig routes unauthenticated ordinary TLS bytes to fixed HTTP
// backends. Choose plaintext HTTP/1 (optionally h2c), or a verified HTTPS site.
type HTTPFallbackConfig struct {
	HTTP1 string               `json:"http1,omitempty"`
	H2C   string               `json:"h2c,omitempty"`
	HTTPS *HTTPSFallbackConfig `json:"https,omitempty"`
}

type HTTPSFallbackConfig struct {
	Address    string `json:"address"`
	ServerName string `json:"server_name"`
	CAFile     string `json:"ca_file,omitempty"`
	HTTP2      bool   `json:"http2,omitempty"`
}

func (c *Config) Defaults() error {
	if c.Traffic != nil {
		p := *c.Traffic
		if err := p.Validate(); err != nil {
			return err
		}
		c.Traffic = &p
	}
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
	if c.Inbound != "" {
		if c.Role != "client" || c.Target != "" {
			return errors.New("inbound is only valid for a proxy client without target")
		}
		if c.Inbound != "socks" && c.Inbound != "http" && c.Inbound != "mixed" {
			return errors.New("inbound must be socks, http or mixed")
		}
	}
	if c.HTTPFallback != nil {
		if c.Role != "server" || c.TLS.Mode != "tls" {
			return errors.New("http_fallback is only valid for an ordinary TLS server")
		}
		fallback := *c.HTTPFallback
		addresses := map[string]string{"http1": fallback.HTTP1, "h2c": fallback.H2C}
		if fallback.HTTPS != nil {
			if fallback.HTTP1 != "" || fallback.H2C != "" {
				return errors.New("http_fallback.https cannot be combined with http1 or h2c")
			}
			https := *fallback.HTTPS
			if https.Address == "" || https.ServerName == "" {
				return errors.New("http_fallback.https requires address and server_name")
			}
			fallback.HTTPS = &https
			addresses["https.address"] = https.Address
		} else if fallback.HTTP1 == "" {
			return errors.New("http_fallback.http1 is required")
		}
		for name, address := range addresses {
			if address == "" {
				continue
			}
			encoded, err := wire.EncodeAddress(address)
			if err == nil {
				_, err = wire.DecodeAddress(encoded)
			}
			if err != nil {
				return fmt.Errorf("http_fallback.%s: %w", name, err)
			}
		}
		c.HTTPFallback = &fallback
	}
	_, err := transport.DecodeKey(c.Secret)
	return err
}
func sec(n int) time.Duration { return time.Duration(n) * time.Second }
