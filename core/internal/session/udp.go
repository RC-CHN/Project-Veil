package session

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
	dg "veil.local/core/internal/datagram"
	d "veil.local/core/internal/destination"
)

type UDPStatus struct {
	ToNetwork, FromNetwork, Invalid, SourceDenied, TargetDenied, ResolveFailed, CapacityDenied, IOErrors uint64
	ActiveTargets, MaxTargets                                                                            int
	Queue                                                                                                dg.QueueStatus
}
type udpRemote struct {
	conn    *d.CheckedConn
	address d.Address
	done    chan struct{}
}
type datagramSocket struct {
	ctx           context.Context
	cancel        context.CancelFunc
	once          sync.Once
	queue         *dg.Queue
	readMu        sync.Mutex
	current       []byte
	writeMu       sync.Mutex
	decoder       dg.Decoder
	mu            sync.Mutex
	stats         UDPStatus
	peer          netip.AddrPort
	sourceIP      netip.Addr
	local         *net.UDPConn
	control       *net.TCPConn
	connector     *d.Connector
	principal     string
	maxTargets    int
	remoteIdle    time.Duration
	remoteMu      sync.Mutex
	remotes       map[string]*udpRemote
	remotesClosed bool
	workers       sync.WaitGroup
	readers       sync.WaitGroup
}

func baseDatagramSocket(parent context.Context) *datagramSocket {
	ctx, cancel := context.WithCancel(parent)
	return &datagramSocket{ctx: ctx, cancel: cancel, queue: dg.NewQueue(), remotes: make(map[string]*udpRemote)}
}
func newClientDatagrams(parent context.Context, control *net.TCPConn, source netip.AddrPort) (*datagramSocket, error) {
	local := control.LocalAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
	network := "udp4"
	if local.Is6() {
		network = "udp6"
	}
	conn, e := net.ListenUDP(network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)))
	if e != nil {
		return nil, e
	}
	u := baseDatagramSocket(parent)
	u.local = conn
	u.control = control
	u.sourceIP = source.Addr()
	u.peer = source
	return u, nil
}
func newServerDatagrams(parent context.Context, connector *d.Connector, principal string, maxTargets int, idle time.Duration) *datagramSocket {
	u := baseDatagramSocket(parent)
	u.connector = connector
	u.principal = principal
	u.maxTargets = maxTargets
	u.remoteIdle = idle
	return u
}
func (u *datagramSocket) bump(fn func(*UDPStatus)) { u.mu.Lock(); fn(&u.stats); u.mu.Unlock() }
func (u *datagramSocket) status() UDPStatus {
	u.mu.Lock()
	s := u.stats
	u.mu.Unlock()
	s.Queue = u.queue.Status()
	return s
}

// Start only after the SOCKS reply, so no UDP packet can precede association success.
func (u *datagramSocket) startClient() {
	u.workers.Add(2)
	go func() {
		defer u.workers.Done()
		defer u.queue.Close(false)
		p := make([]byte, 65508)
		for {
			n, peer, e := u.local.ReadFromUDPAddrPort(p)
			if e != nil {
				return
			}
			packet, e := dg.ParseSOCKS(p[:n])
			if e != nil {
				u.bump(func(s *UDPStatus) { s.Invalid++ })
				continue
			}
			peer = netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
			u.mu.Lock()
			valid := peer.Addr() == u.sourceIP && (u.peer.Port() == 0 || peer.Port() == u.peer.Port())
			if valid && u.peer.Port() == 0 {
				u.peer = peer
			}
			if !valid {
				u.stats.SourceDenied++
			}
			u.mu.Unlock()
			if !valid {
				continue
			}
			frame, e := dg.Encode(packet)
			if e != nil {
				u.bump(func(s *UDPStatus) { s.Invalid++ })
				continue
			}
			u.bump(func(s *UDPStatus) { s.FromNetwork++ })
			u.queue.Push(frame)
		}
	}()
	go func() {
		defer u.workers.Done()
		var p [1]byte
		n, e := u.control.Read(p[:])
		if n == 0 && e == io.EOF {
			u.local.Close()
			return
		}
		u.cancel()
		u.local.Close()
	}()
}
func (u *datagramSocket) Read(p []byte) (int, error) {
	u.readMu.Lock()
	defer u.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if e := u.ctx.Err(); e != nil {
		return 0, e
	}
	if len(u.current) == 0 {
		frame, e := u.queue.Pop(u.ctx)
		if e != nil {
			return 0, e
		}
		if e := u.ctx.Err(); e != nil {
			return 0, e
		}
		u.current = frame
	}
	n := copy(p, u.current)
	u.current = u.current[n:]
	return n, nil
}
func (u *datagramSocket) Write(p []byte) (int, error) {
	u.writeMu.Lock()
	defer u.writeMu.Unlock()
	return u.decoder.Write(p, u.deliver)
}
func (u *datagramSocket) deliver(packet dg.Packet) error {
	if e := u.ctx.Err(); e != nil {
		return e
	}
	if u.local != nil {
		wire, e := dg.SOCKS(packet)
		if e != nil {
			u.bump(func(s *UDPStatus) { s.Invalid++ })
			return nil
		}
		u.mu.Lock()
		peer := u.peer
		u.mu.Unlock()
		if peer.Port() == 0 {
			u.bump(func(s *UDPStatus) { s.SourceDenied++ })
			return nil
		}
		u.local.SetWriteDeadline(time.Now().Add(time.Second))
		n, e := u.local.WriteToUDPAddrPort(wire, peer)
		if e != nil || n != len(wire) {
			u.bump(func(s *UDPStatus) { s.IOErrors++ })
		} else {
			u.bump(func(s *UDPStatus) { s.ToNetwork++ })
		}
		return nil
	}
	u.remoteMu.Lock()
	defer u.remoteMu.Unlock()
	if u.remotesClosed {
		return errors.New("UDP association input closed")
	}
	for key, r := range u.remotes {
		select {
		case <-r.done:
			delete(u.remotes, key)
		default:
		}
	}
	key := packet.Address.String()
	remote := u.remotes[key]
	if remote == nil {
		if len(u.remotes) >= u.maxTargets {
			u.bump(func(s *UDPStatus) { s.CapacityDenied++ })
			return nil
		}
		conn, e := u.connector.OpenUDP(u.ctx, u.principal, packet.Address)
		if e != nil {
			u.bump(func(s *UDPStatus) {
				switch {
				case errors.Is(e, d.ErrDenied):
					s.TargetDenied++
				case errors.Is(e, d.ErrResolve), errors.Is(e, context.DeadlineExceeded):
					s.ResolveFailed++
				case errors.Is(e, d.ErrBusy):
					s.CapacityDenied++
				default:
					s.IOErrors++
				}
			})
			if u.ctx.Err() != nil {
				return u.ctx.Err()
			}
			return nil
		}
		numeric, e := netip.ParseAddrPort(conn.Address)
		if e != nil {
			conn.Release()
			return e
		}
		remote = &udpRemote{conn: conn, address: d.Address{Host: numeric.Addr().String(), Port: numeric.Port()}, done: make(chan struct{})}
		u.remotes[key] = remote
		u.bump(func(s *UDPStatus) { s.ActiveTargets++; s.MaxTargets = max(s.MaxTargets, s.ActiveTargets) })
		u.readers.Add(1)
		go u.receiveRemote(remote)
	}
	remote.conn.SetWriteDeadline(time.Now().Add(time.Second))
	remote.conn.SetReadDeadline(time.Now().Add(u.remoteIdle))
	n, e := remote.conn.Write(packet.Payload)
	if e != nil || n != len(packet.Payload) {
		u.bump(func(s *UDPStatus) { s.IOErrors++ })
		remote.conn.Close()
	} else {
		u.bump(func(s *UDPStatus) { s.ToNetwork++ })
	}
	return nil
}
func (u *datagramSocket) receiveRemote(r *udpRemote) {
	defer u.readers.Done()
	defer func() { r.conn.Release(); u.bump(func(s *UDPStatus) { s.ActiveTargets-- }); close(r.done) }()
	p := make([]byte, dg.MaxPayload+1)
	for {
		r.conn.SetReadDeadline(time.Now().Add(u.remoteIdle))
		n, e := r.conn.Read(p)
		if e != nil {
			return
		}
		if n > dg.MaxPayload {
			u.bump(func(s *UDPStatus) { s.Invalid++ })
			continue
		}
		frame, e := dg.Encode(dg.Packet{Address: r.address, Payload: p[:n]})
		if e != nil {
			u.bump(func(s *UDPStatus) { s.Invalid++ })
			continue
		}
		u.bump(func(s *UDPStatus) { s.FromNetwork++ })
		u.queue.Push(frame)
	}
}
func (u *datagramSocket) closeRemotes() {
	u.remoteMu.Lock()
	u.remotesClosed = true
	for _, r := range u.remotes {
		r.conn.Close()
	}
	u.remoteMu.Unlock()
	u.readers.Wait()
}
func (u *datagramSocket) CloseWrite() error {
	u.writeMu.Lock()
	e := u.decoder.Finish()
	u.writeMu.Unlock()
	if e != nil {
		return e
	}
	if u.local != nil {
		u.local.Close()
	} else {
		u.closeRemotes()
	}
	u.queue.Close(false)
	return nil
}
func (u *datagramSocket) Close() error {
	u.once.Do(func() {
		u.cancel()
		if u.local != nil {
			u.local.Close()
			u.control.Close()
		}
		u.closeRemotes()
		u.queue.Close(true)
		u.workers.Wait()
		u.readMu.Lock()
		clear(u.current)
		u.current = nil
		u.readMu.Unlock()
		u.writeMu.Lock()
		u.decoder.Clear()
		u.writeMu.Unlock()
	})
	return nil
}
func (u *datagramSocket) LocalAddr() net.Addr {
	if u.local != nil {
		return u.local.LocalAddr()
	}
	return &net.UDPAddr{}
}
func (u *datagramSocket) RemoteAddr() net.Addr {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.peer.IsValid() {
		return net.UDPAddrFromAddrPort(u.peer)
	}
	return &net.UDPAddr{}
}
func (*datagramSocket) SetDeadline(time.Time) error      { return nil }
func (*datagramSocket) SetReadDeadline(time.Time) error  { return nil }
func (*datagramSocket) SetWriteDeadline(time.Time) error { return nil }
func (*datagramSocket) SetLinger(int) error              { return nil }
