// Package service assembles Veil configuration and owns one application instance.
package service

import (
	"context"
	"net"
	"veil/core"
	"veil/inbound"
)

type Stats = core.Stats
type Service struct {
	cfg     Config
	client  *core.Client
	handle  inbound.Handler
	Stats   *Stats
	OnError func(error)
}

func New(cfg Config) (*Service, error) {
	if err := cfg.Defaults(); err != nil {
		return nil, err
	}
	opts := core.Config{
		Secret: cfg.Secret, TLS: cfg.TLS, Traffic: cfg.Traffic,
		MaxConnections: cfg.MaxConnections, MaxIdle: cfg.MaxIdle,
		HandshakeTimeout: sec(cfg.HandshakeSeconds), DialTimeout: sec(cfg.DialSeconds),
		IdleTimeout: sec(cfg.IdleSeconds), PoolTimeout: sec(cfg.PoolSeconds),
	}
	s := &Service{cfg: cfg}
	if cfg.Role == "server" {
		fallback, err := httpFallback(cfg)
		if err != nil {
			return nil, err
		}
		server, err := core.NewServer(core.ServerConfig{Config: opts, Fallback: fallback})
		if err != nil {
			return nil, err
		}
		s.handle, s.Stats = server.Handle, &server.Stats
		server.OnStreamError = func(err error) {
			if s.OnError != nil {
				s.OnError(err)
			}
		}
	} else {
		client, err := core.NewClient(core.ClientConfig{Config: opts, Server: cfg.Server})
		if err != nil {
			return nil, err
		}
		s.client, s.Stats = client, &client.Stats
		s.handle = inbound.SOCKS5(client, opts.HandshakeTimeout)
		switch cfg.Inbound {
		case "http":
			s.handle = inbound.HTTP(client, opts.HandshakeTimeout)
		case "mixed":
			s.handle = inbound.Mixed(client, opts.HandshakeTimeout)
		}
		if cfg.Target != "" {
			s.handle, err = inbound.Forward(client, cfg.Target)
			if err != nil {
				client.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Service) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	defer s.Close()
	return inbound.Serve(ctx, ln, s.handle, inbound.ServeOptions{
		MaxConnections: s.cfg.MaxConnections, Stats: s.Stats, OnError: s.OnError,
	})
}
