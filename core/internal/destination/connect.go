package destination

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	destinationpolicy "veil.local/core/policy"
)

var ErrDenied = destinationpolicy.ErrDenied
var ErrResolve = destinationpolicy.ErrResolve
var ErrConnect = errors.New("destination connection failed")

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type DialFunc func(context.Context, string, string) (net.Conn, error)
type Config struct {
	Allow, Deny []string
	Timeout     time.Duration
}
type Connector struct {
	policy   *destinationpolicy.Policy
	timeout  time.Duration
	resolver Resolver
	dial     DialFunc
	gate     *Gate
}

func New(c Config, g *Gate, resolver Resolver, dial DialFunc) (*Connector, error) {
	if g == nil || c.Timeout <= 0 || c.Timeout > 30*time.Second || len(c.Allow) > 128 || len(c.Deny) > 128 {
		return nil, errors.New("destination configuration")
	}
	policy, err := destinationpolicy.New(c.Allow, c.Deny)
	if err != nil {
		return nil, err
	}
	out := &Connector{policy: policy, timeout: c.Timeout, gate: g, resolver: resolver, dial: dial}
	if out.resolver == nil {
		out.resolver = &net.Resolver{PreferGo: true, StrictErrors: true}
	}
	if out.dial == nil {
		out.dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	return out, nil
}
func (c *Connector) allowed(a netip.Addr) bool {
	return c.policy.Allowed(a)
}

// CheckedConn retains admission until Release, after the owner joins all pumps.
// Close only interrupts socket I/O. Cancellation cannot recycle stream admission
// while the prior owner's workers and buffers are still live.
type CheckedConn struct {
	net.Conn
	Address string
	once    sync.Once
	release func()
	stop    func() bool
}

func (c *CheckedConn) Close() error { return c.Conn.Close() }
func (c *CheckedConn) Release()     { c.once.Do(func() { c.Conn.Close(); c.stop(); c.release() }) }
func (c *CheckedConn) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return errors.New("TCP half-close unsupported")
}
func (c *CheckedConn) SetLinger(n int) error {
	if v, ok := c.Conn.(interface{ SetLinger(int) error }); ok {
		return v.SetLinger(n)
	}
	return errors.New("TCP linger unsupported")
}
func (c *Connector) Open(parent context.Context, principal string, a Address) (*CheckedConn, error) {
	return c.open(parent, principal, a, "tcp")
}

// OpenUDP pins one policy-checked numeric UDP endpoint. Release follows receiver join.
func (c *Connector) OpenUDP(parent context.Context, principal string, a Address) (*CheckedConn, error) {
	return c.open(parent, principal, a, "udp")
}

func (c *Connector) open(parent context.Context, principal string, a Address, network string) (out *CheckedConn, err error) {
	if parent == nil {
		return nil, errors.New("nil open context")
	}
	if e := parent.Err(); e != nil {
		return nil, e
	}
	release, e := c.gate.Acquire(principal)
	if e != nil {
		return nil, e
	}
	defer func() {
		if err != nil {
			release()
		}
	}()
	if !a.Valid() {
		return nil, ErrDenied
	}
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	ips := []netip.Addr{}
	if ip, e := netip.ParseAddr(a.Host); e == nil {
		ips = append(ips, ip)
	} else {
		// Absolute DNS lookup once; no local search suffix and no dial re-resolution.
		ips, e = c.resolver.LookupNetIP(ctx, "ip", a.Host+".")
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrResolve
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if e := c.policy.Check(ips); e != nil {
		return nil, e
	}
	numeric := Address{Host: ips[0].Unmap().String(), Port: a.Port}.String()
	conn, e := c.dial(ctx, network, numeric)
	if e != nil {
		if conn != nil {
			conn.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrConnect
	}
	if conn == nil {
		return nil, ErrConnect
	}
	if ctx.Err() != nil {
		conn.Close()
		return nil, ctx.Err()
	}
	out = &CheckedConn{Conn: conn, Address: numeric, release: release}
	// Install under a separate latch because parent may cancel immediately.
	ready := make(chan struct{})
	out.stop = context.AfterFunc(parent, func() { <-ready; out.Close() })
	close(ready)
	return out, nil
}
