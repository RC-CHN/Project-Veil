package core

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
	"veil/internal/transport"
	"veil/internal/wire"
)

// Server handles authenticated Veil connections. It does not own a listener,
// host routes or service management; the caller bounds concurrent Handle calls.
type Server struct {
	cfg       ServerConfig
	key       []byte
	handshake transport.Handshake
	Stats     Stats
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
	return &Server{cfg: cfg, key: key, handshake: h}, nil
}

// Handle owns raw and closes it before returning. Cancellation closes raw and
// active target connections. The destination dialer must honor its context.
func (s *Server) Handle(ctx context.Context, raw net.Conn) error {
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	hsCtx, cancel := context.WithTimeout(ctx, s.cfg.HandshakeTimeout)
	defer cancel()
	raw.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	c, err := s.handshake(hsCtx, raw)
	if err != nil {
		return err
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
		return err
	}
	if typ != wire.Auth || !wire.VerifyAuth(s.key, exporter, p) {
		return errors.New("veil: authentication failed")
	}
	s.Stats.Authenticated.Add(1)
	cancel()
	c.SetDeadline(time.Time{})
	for {
		c.SetReadDeadline(time.Now().Add(2 * s.cfg.PoolTimeout))
		typ, p, err = r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if typ != wire.Open {
			return wire.ErrProtocol
		}
		address, err := wire.DecodeAddress(p)
		if err != nil {
			return err
		}
		if err := transport.RecordPadding(c, s.cfg.TLS.RecordPadding); err != nil {
			return err
		}
		c.SetReadDeadline(time.Time{})
		target, err := s.cfg.DialContext(ctx, "tcp", address)
		if err != nil {
			c.SetWriteDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
			wire.Write(c, wire.OpenError, []byte{byte(targetFailure(err))})
			return opError("target dial", err)
		}
		c.SetWriteDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
		if err = wire.Write(c, wire.OpenOK, nil); err != nil {
			target.Close()
			return err
		}
		c.SetWriteDeadline(time.Time{})
		finPending, err := relay(ctx, target, c, &r, s.cfg.IdleTimeout, true)
		target.Close()
		if err != nil {
			return err
		}
		c.SetWriteDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
		if err = wire.WriteDone(c, finPending); err != nil {
			return err
		}
		c.SetWriteDeadline(time.Time{})
		s.Stats.Completed.Add(1)
		transport.ReleaseBuffers(c)
	}
}
