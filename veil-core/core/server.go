package core

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
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
	var protocols []string
	if cfg.Fallback != nil {
		if cfg.TLS.Mode != "tls" {
			return nil, errors.New("veil: application fallback requires ordinary TLS")
		}
		fallback := *cfg.Fallback
		if fallback.Handler == nil {
			return nil, errors.New("veil: fallback handler required")
		}
		fallback.Protocols = slices.Clone(fallback.Protocols)
		if len(fallback.Protocols) == 0 {
			fallback.Protocols = []string{"http/1.1"}
		}
		seen := make(map[string]bool)
		for _, protocol := range fallback.Protocols {
			if protocol != "http/1.1" && protocol != "h2" || seen[protocol] {
				return nil, errors.New("veil: invalid fallback protocols")
			}
			seen[protocol] = true
		}
		cfg.Fallback = &fallback
		protocols = fallback.Protocols
	}
	h, err := transport.Server(cfg.TLS, cfg.HandshakeTimeout, cfg.IdleTimeout, protocols...)
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
	// T clients use HTTP/1.1 ALPN. HTTP/2 belongs to the website, which
	// may send its SETTINGS before the client sends any application bytes.
	if s.cfg.Fallback != nil && transport.Protocol(c) == "h2" {
		if err := hsCtx.Err(); err != nil {
			return opError("TLS fallback", err)
		}
		cancel()
		if err := c.SetDeadline(time.Time{}); err != nil {
			return err
		}
		return opError("TLS fallback", s.cfg.Fallback.Handler(ctx, c, "h2"))
	}
	exporter, err := transport.Export(c)
	if err != nil {
		return err
	}
	r := wire.Reader{R: c}
	var prefix *authPrefix
	if s.cfg.Fallback != nil {
		prefix = &authPrefix{Conn: c}
		r.R = prefix
	}
	defer r.Release()
	typ, p, err := r.Read()
	if err != nil || typ != wire.Auth || !wire.VerifyAuth(s.key, exporter, p) {
		failedAuth := err == nil || errors.Is(err, wire.ErrProtocol)
		if prefix != nil && prefix.size > 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			failedAuth = true
		}
		if err == nil {
			err = errors.New("veil: authentication failed")
		}
		// A timed-out or canceled connection must not initiate new fallback work.
		var timeout net.Error
		if prefix == nil || !failedAuth || hsCtx.Err() != nil || errors.As(err, &timeout) && timeout.Timeout() {
			return opError("authentication", err)
		}
		cancel()
		r.Release()
		if err := c.SetDeadline(time.Time{}); err != nil {
			return err
		}
		prefix.replay = true
		return opError("TLS fallback", s.cfg.Fallback.Handler(ctx, prefix, transport.Protocol(c)))
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
