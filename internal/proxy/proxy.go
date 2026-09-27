package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"veil/internal/socks"
	"veil/internal/transport"
	"veil/internal/wire"
)

type Stats struct{ Accepted, Rejected, Completed, Failed, Authenticated atomic.Uint64 }
type Service struct {
	cfg       Config
	key       []byte
	handshake transport.Handshake
	pool      *pool
	Stats     Stats
	OnError   func(error)
}

func New(cfg Config) (*Service, error) {
	if err := cfg.Defaults(); err != nil {
		return nil, err
	}
	key, _ := transport.DecodeKey(cfg.Secret)
	s := &Service{cfg: cfg, key: key}
	var err error
	if cfg.Role == "server" {
		s.handshake, err = transport.Server(cfg.TLS, sec(cfg.HandshakeSeconds), sec(cfg.IdleSeconds))
	} else {
		s.handshake, err = transport.Client(cfg.TLS)
		s.pool = newPool(cfg, key, s.handshake)
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	slots := make(chan struct{}, s.cfg.MaxConnections)
	var wg sync.WaitGroup
	if s.pool != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(min(sec(s.cfg.PoolSeconds)/2, 5*time.Second))
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					s.pool.close()
					return
				case <-tick.C:
					s.pool.expire()
				}
			}
		}()
	}
	var acceptErr error
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				acceptErr = err
			}
			break
		}
		select {
		case slots <- struct{}{}:
		default:
			s.Stats.Rejected.Add(1)
			c.Close()
			continue
		}
		s.Stats.Accepted.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			var err error
			if s.cfg.Role == "server" {
				err = s.server(ctx, c)
			} else {
				err = s.client(ctx, c)
			}
			if err != nil {
				s.Stats.Failed.Add(1)
				if s.OnError != nil {
					s.OnError(err)
				}
			}
		}()
	}
	cancel()
	ln.Close()
	wg.Wait()
	return acceptErr
}
func (s *Service) server(ctx context.Context, raw net.Conn) error {
	hsCtx, cancel := context.WithTimeout(ctx, sec(s.cfg.HandshakeSeconds))
	defer cancel()
	raw.SetDeadline(time.Now().Add(sec(s.cfg.HandshakeSeconds)))
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
		c.SetReadDeadline(time.Now().Add(2 * sec(s.cfg.PoolSeconds)))
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
		c.SetReadDeadline(time.Time{})
		target, err := (&net.Dialer{Timeout: sec(s.cfg.DialSeconds)}).DialContext(ctx, "tcp", address)
		if err != nil {
			c.SetWriteDeadline(time.Now().Add(sec(s.cfg.HandshakeSeconds)))
			wire.Write(c, wire.OpenError, []byte{1})
			return err
		}
		c.SetWriteDeadline(time.Now().Add(sec(s.cfg.HandshakeSeconds)))
		if err = wire.Write(c, wire.OpenOK, nil); err != nil {
			target.Close()
			return err
		}
		c.SetWriteDeadline(time.Time{})
		finPending, err := relay(ctx, target, c, &r, sec(s.cfg.IdleSeconds), true)
		target.Close()
		if err != nil {
			return err
		}
		c.SetWriteDeadline(time.Now().Add(sec(s.cfg.HandshakeSeconds)))
		if err = wire.WriteDone(c, finPending); err != nil {
			return err
		}
		c.SetWriteDeadline(time.Time{})
		s.Stats.Completed.Add(1)
		transport.ReleaseBuffers(c)
	}
}
func (s *Service) client(ctx context.Context, local net.Conn) error {
	local.SetDeadline(time.Now().Add(sec(s.cfg.HandshakeSeconds)))
	address, err := socks.ReadConnect(local)
	if err != nil {
		return err
	}
	channel, err := s.pool.get(ctx)
	if err != nil {
		socks.Reply(local, 1)
		return err
	}
	reusable := false
	defer func() {
		if reusable {
			s.pool.put(channel)
		} else {
			s.pool.drop(channel)
		}
	}()
	channel.SetDeadline(time.Now().Add(sec(s.cfg.DialSeconds + s.cfg.HandshakeSeconds)))
	p, err := wire.EncodeAddress(address)
	if err == nil {
		err = wire.WriteOpen(channel, channel.auth, p)
	}
	if err == nil {
		err = wire.Expect(&channel.r, wire.OpenOK)
	}
	if err != nil {
		socks.Reply(local, 1)
		return err
	}
	channel.auth = nil
	if err = socks.Reply(local, 0); err != nil {
		return err
	}
	local.SetDeadline(time.Time{})
	channel.SetDeadline(time.Time{})
	if _, err = relay(ctx, local, channel, &channel.r, sec(s.cfg.IdleSeconds), false); err != nil {
		return err
	}
	channel.SetReadDeadline(time.Now().Add(sec(s.cfg.HandshakeSeconds)))
	if err = wire.Expect(&channel.r, wire.Done); err != nil {
		return err
	}
	channel.SetReadDeadline(time.Time{})
	reusable = true
	s.Stats.Completed.Add(1)
	return nil
}
