package core

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"veil/internal/mux"
	"veil/internal/transport"
	"veil/internal/wire"
)

// Server authenticates physical connections and handles independent streams.
// The caller owns listeners, host routes and service management.
type Server struct {
	cfg           ServerConfig
	key           []byte
	handshake     transport.Handshake
	Stats         Stats
	OnStreamError func(error)
	traffic       atomic.Pointer[TrafficProfile]
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if err := cfg.Config.defaults(); err != nil {
		return nil, err
	}
	cfg.DialContext = withDialTimeout(cfg.DialContext, cfg.DialTimeout)
	key, err := transport.DecodeKey(cfg.Secret)
	if err != nil {
		return nil, err
	}
	h, err := transport.Server(cfg.TLS, cfg.HandshakeTimeout, cfg.IdleTimeout)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, key: key, handshake: h}
	s.traffic.Store(cfg.Traffic)
	return s, nil
}

// SetTrafficProfile affects subsequent physical connections only.
func (s *Server) SetTrafficProfile(p TrafficProfile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	s.traffic.Store(&p)
	return nil
}

// Handle owns raw. Cancellation joins all stream handlers before returning.
func (s *Server) Handle(ctx context.Context, raw net.Conn) error {
	cfg := s.cfg.Config
	cfg.Traffic = s.traffic.Load()
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	hsCtx, cancel := context.WithTimeout(ctx, s.cfg.HandshakeTimeout)
	defer cancel()
	raw.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	c, err := s.handshake(hsCtx, raw)
	if err != nil {
		return opError("TLS handshake", err)
	}
	defer c.Close()
	exporter, err := transport.Export(c)
	if err != nil {
		return err
	}
	r := wire.Reader{R: c}
	defer r.Release()
	typ, p, err := r.Read()
	if err != nil {
		return opError("authentication", err)
	}
	if typ != wire.Auth || !wire.VerifyAuth(s.key, exporter, p) {
		return opError("authentication", errors.New("veil: authentication failed"))
	}
	s.Stats.Authenticated.Add(1)
	cancel()
	r.Release()
	c.SetDeadline(time.Time{})
	opts := muxOptions(c, cfg, true)
	opts.Context = ctx
	m, err := mux.New(c, opts)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	slots := make(chan struct{}, mux.MaxStreams)
	defer func() { m.Close(); workers.Wait(); m.Wait() }()
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		stream, err := m.Accept(ctx)
		if err != nil {
			<-slots
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.Stats.ActiveStreams.Add(1)
		workers.Go(func() {
			defer s.Stats.ActiveStreams.Add(-1)
			defer func() { <-slots }()
			defer stream.Close()
			if err := s.serve(stream.Context(), stream); err != nil && ctx.Err() == nil {
				s.Stats.Failed.Add(1)
				if s.OnStreamError != nil {
					s.OnStreamError(err)
				}
			}
		})
	}
}
func (s *Server) serve(ctx context.Context, stream *mux.Stream) error {
	address, err := wire.DecodeAddress(stream.Metadata())
	if err != nil {
		stream.Respond(byte(wire.OpenFailed))
		return err
	}
	target, err := s.cfg.DialContext(ctx, "tcp", address)
	if err != nil {
		stream.Respond(byte(targetFailure(err)))
		return opError("target dial", err)
	}
	defer target.Close()
	if err = stream.Respond(0); err != nil {
		return err
	}
	if err = relay(ctx, target, stream, s.cfg.IdleTimeout); err != nil {
		return err
	}
	if err = stream.Finish(); err != nil {
		return err
	}
	s.Stats.Completed.Add(1)
	return nil
}
