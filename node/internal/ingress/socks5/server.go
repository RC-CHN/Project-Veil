// Package socks5 adapts local SOCKS traffic to the public core transport API.
package socks5

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
	"veil.local/core/endpoint"
	"veil.local/core/transport"
	"veil.local/node/internal/lifecycle"
)

type Server struct {
	listen  string
	limit   int
	dialer  transport.Dialer
	before  func(context.Context) error
	ready   *lifecycle.Gate
	mu      sync.Mutex
	address string
	started bool
}

func New(listen string, limit int, dialer transport.Dialer, before func(context.Context) error) (*Server, error) {
	a, e := netip.ParseAddrPort(listen)
	if e != nil || !a.Addr().IsLoopback() || a.Addr().Zone() != "" || limit < 1 || limit > 32 || dialer == nil {
		return nil, errors.New("SOCKS requires a numeric loopback listener and bounded connections")
	}
	return &Server{listen: listen, limit: limit, dialer: dialer, before: before, ready: lifecycle.NewGate()}, nil
}
func (s *Server) Run(ctx context.Context) (err error) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("SOCKS already run")
	}
	s.started = true
	s.mu.Unlock()
	defer func() {
		if err != nil {
			s.ready.Finish(err)
		}
	}()
	if s.before != nil {
		if err = s.before(ctx); err != nil {
			return err
		}
	}
	l, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}
	defer l.Close()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(run, func() { l.Close() })
	defer stop()
	s.mu.Lock()
	s.address = l.Addr().String()
	s.mu.Unlock()
	s.ready.Finish(nil)
	var workers sync.WaitGroup
	slots := make(chan struct{}, s.limit)
	for {
		c, e := l.Accept()
		if e != nil {
			err = e
			break
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		workers.Add(1)
		go func(c net.Conn) { defer workers.Done(); defer func() { <-slots }(); defer c.Close(); s.serve(run, c) }(c)
	}
	cancel()
	workers.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return err
}
func (s *Server) WaitReady(ctx context.Context) error { return s.ready.WaitReady(ctx) }
func (s *Server) Address() string                     { s.mu.Lock(); defer s.mu.Unlock(); return s.address }
func (s *Server) serve(parent context.Context, c net.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.SetDeadline(time.Now().Add(8 * time.Second))
	command, host, port, e := handshake(c)
	if e != nil {
		return
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, 8*time.Second)
	defer cancelOpen()
	switch command {
	case 1:
		target, e := endpoint.Parse(host, port)
		if e != nil {
			reply(c, 8, nil)
			return
		}
		remote, e := s.dialer.DialStream(openCtx, target)
		if e != nil {
			reply(c, 1, nil)
			return
		}
		defer remote.Close()
		if e = reply(c, 0, nil); e != nil {
			return
		}
		c.SetDeadline(time.Time{})
		cancelOpen()
		interrupt := context.AfterFunc(ctx, func() { remote.Close() })
		defer interrupt()
		joined := make(chan error, 2)
		go func() {
			_, e := io.Copy(remote, c)
			if e == nil {
				e = remote.CloseWrite()
			}
			joined <- e
		}()
		go func() {
			_, e := io.Copy(c, remote)
			if e == nil {
				if h, ok := c.(interface{ CloseWrite() error }); ok {
					e = h.CloseWrite()
				} else {
					e = transport.ErrUnsupported
				}
			}
			joined <- e
		}()
		for range 2 {
			if e := <-joined; e != nil {
				cancel()
				remote.Close()
				c.Close()
			}
		}
	case 3:
		a, e := s.dialer.OpenAssociation(openCtx)
		if e != nil {
			reply(c, 1, nil)
			return
		}
		defer a.Close()
		s.serveUDP(ctx, c, a, host, port, cancelOpen)
	default:
		reply(c, 7, nil)
	}
}
func (s *Server) serveUDP(parent context.Context, c net.Conn, a transport.Association, host string, port uint16, cancelOpen context.CancelFunc) {
	peer, e := netip.ParseAddrPort(c.RemoteAddr().String())
	if e != nil {
		return
	}
	peer = netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
	requested, e := netip.ParseAddr(host)
	if e != nil || requested.Zone() != "" || (!requested.IsUnspecified() && requested.Unmap() != peer.Addr()) {
		reply(c, 2, nil)
		return
	}
	local, e := netip.ParseAddrPort(c.LocalAddr().String())
	if e != nil {
		return
	}
	u, e := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local.Addr(), 0)))
	if e != nil {
		reply(c, 1, nil)
		return
	}
	defer u.Close()
	if e = reply(c, 0, u.LocalAddr()); e != nil {
		return
	}
	c.SetDeadline(time.Time{})
	cancelOpen()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { u.Close(); a.Close(); c.Close() })
	defer stop()
	var mu sync.Mutex
	source := netip.AddrPortFrom(peer.Addr(), port)
	var workers sync.WaitGroup
	workers.Add(3)
	go func() { defer workers.Done(); defer cancel(); var p [1]byte; c.Read(p[:]) }()
	go func() {
		defer workers.Done()
		defer cancel()
		p := make([]byte, 65508)
		for {
			n, from, e := u.ReadFromUDPAddrPort(p)
			if e != nil {
				return
			}
			target, payload, e := decodeUDP(p[:n])
			if e != nil {
				continue
			}
			from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
			mu.Lock()
			valid := from.Addr() == source.Addr() && (source.Port() == 0 || from.Port() == source.Port())
			if valid && source.Port() == 0 {
				source = from
			}
			mu.Unlock()
			if valid {
				if e = a.Send(ctx, target, payload); e != nil && ctx.Err() != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer workers.Done()
		defer cancel()
		p := make([]byte, transport.MaxDatagram)
		for {
			n, from, e := a.Receive(ctx, p)
			if e != nil {
				if errors.Is(e, transport.ErrShortBuffer) {
					continue
				}
				return
			}
			wire, e := encodeUDP(from, p[:n])
			if e != nil {
				continue
			}
			mu.Lock()
			to := source
			mu.Unlock()
			if to.Port() == 0 {
				continue
			}
			u.SetWriteDeadline(time.Now().Add(time.Second))
			if _, e = u.WriteToUDPAddrPort(wire, to); e != nil && ctx.Err() != nil {
				return
			}
		}
	}()
	workers.Wait()
}
