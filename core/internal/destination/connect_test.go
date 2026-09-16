package destination

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lookupFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f lookupFunc) LookupNetIP(c context.Context, n, h string) ([]netip.Addr, error) {
	return f(c, n, h)
}
func TestCanonicalAddress(t *testing.T) {
	for _, host := range []string{"", "a..b", "-a.test", "a-.test", "a_b.test", "127.1", "123", "fe80::1%eth0", "a/b", "ümlaut.test"} {
		if _, e := Parse(host, 443); e == nil {
			t.Fatal(host)
		}
	}
	for raw, want := range map[string]string{"ExAmPle.Test.": "example.test", "::ffff:127.0.0.2": "127.0.0.2", "2001:db8::1": "2001:db8::1"} {
		a, e := Parse(raw, 443)
		if e != nil || a.Host != want || !a.Valid() {
			t.Fatal(a, e)
		}
	}
	if _, e := Parse("example.test", 0); e == nil {
		t.Fatal("zero port")
	}
}
func TestPinAndDenyBeforeDial(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ips     []netip.Addr
		deny    []string
		allowed bool
	}{
		{"pinned", []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil, true},
		{"mixed", []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("10.0.0.1")}, nil, false},
		{"deny_precedence", []netip.Addr{netip.MustParseAddr("127.0.0.2")}, []string{"127.0.0.2/32"}, false},
		{"zone", []netip.Addr{netip.MustParseAddr("fe80::1%eth0")}, nil, false},
		{"too_many", make([]netip.Addr, 17), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := NewGate(2, 1)
			lookups, dials := 0, 0
			resolver := lookupFunc(func(ctx context.Context, n, h string) ([]netip.Addr, error) {
				lookups++
				if n != "ip" || h != "owned.test." || g.Status().Active != 1 {
					t.Fatal(n, h, g.Status())
				}
				return tc.ips, nil
			})
			dial := func(ctx context.Context, n, a string) (net.Conn, error) {
				dials++
				if n != "tcp" || a != "127.0.0.2:443" {
					t.Fatal(n, a)
				}
				aConn, bConn := net.Pipe()
				bConn.Close()
				return aConn, nil
			}
			c, e := New(Config{Allow: []string{"127.0.0.2/32"}, Deny: tc.deny, Timeout: time.Second}, g, resolver, dial)
			if e != nil {
				t.Fatal(e)
			}
			conn, e := c.Open(context.Background(), "verified-cert", Address{"owned.test", 443})
			if tc.allowed {
				if e != nil || dials != 1 || g.Status().Active != 1 {
					t.Fatal(e, dials, g.Status())
				}
				conn.Close()
				if g.Status().Active != 1 {
					t.Fatal("close recycled admission before join")
				}
				conn.Release()
				conn.Release()
			} else if e == nil || dials != 0 {
				t.Fatal(e, dials)
			}
			if lookups != 1 || g.Status().Active != 0 || g.Status().Principals != 0 {
				t.Fatal(lookups, g.Status())
			}
		})
	}
}
func TestGateConcurrencyAndTimeoutRelease(t *testing.T) {
	g, _ := NewGate(4, 2)
	release1, _ := g.Acquire("a")
	release2, _ := g.Acquire("a")
	if _, e := g.Acquire("a"); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	release3, _ := g.Acquire("b")
	release4, _ := g.Acquire("b")
	if _, e := g.Acquire("c"); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		for _, release := range []func(){release1, release2, release3, release4} {
			wg.Add(1)
			go func() { defer wg.Done(); release() }()
		}
	}
	wg.Wait()
	if s := g.Status(); s.Active != 0 || s.Principals != 0 || s.Maximum != 4 {
		t.Fatal(s)
	}
	resolver := lookupFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) { <-ctx.Done(); return nil, ctx.Err() })
	var dialed atomic.Int32
	c, _ := New(Config{Timeout: 10 * time.Millisecond}, g, resolver, func(context.Context, string, string) (net.Conn, error) {
		dialed.Add(1)
		return nil, errors.New("unexpected")
	})
	if _, e := c.Open(context.Background(), "a", Address{"owned.test", 443}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if dialed.Load() != 0 || g.Status().Active != 0 {
		t.Fatal("timeout leaked")
	}
}
func TestParentCancellationClosesRetainedConnection(t *testing.T) {
	g, _ := NewGate(1, 1)
	a, b := net.Pipe()
	defer b.Close()
	c, _ := New(Config{Allow: []string{"127.0.0.2/32"}, Timeout: time.Second}, g, nil, func(context.Context, string, string) (net.Conn, error) { return a, nil })
	ctx, cancel := context.WithCancel(context.Background())
	conn, e := c.Open(ctx, "a", Address{"127.0.0.2", 443})
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	done := make(chan struct{})
	go func() { var buf [1]byte; b.Read(buf[:]); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("socket stranded")
	}
	conn.Close()
	if g.Status().Active != 1 {
		t.Fatal("cancellation recycled admission before owner release")
	}
	conn.Release()
	if g.Status().Active != 0 {
		t.Fatal(g.Status())
	}
}
func TestDefaultPrivateAndMappedDenial(t *testing.T) {
	g, _ := NewGate(1, 1)
	c, _ := New(Config{Timeout: time.Second}, g, nil, nil)
	for _, ip := range []string{"127.0.0.1", "::ffff:127.0.0.1", "10.0.0.1", "::", "0.0.0.0", "169.254.169.254", "fc00::1", "224.0.0.1", "100.64.0.1", "192.0.2.1", "2001:db8::1"} {
		if c.allowed(netip.MustParseAddr(ip)) {
			t.Fatal(ip)
		}
	}
}

func TestAdmissionBeforeDNSAndCancelledDialCleanup(t *testing.T) {
	g, _ := NewGate(1, 1)
	release, _ := g.Acquire("occupied")
	lookups := 0
	c, _ := New(Config{Allow: []string{"127.0.0.2/32"}, Timeout: time.Second}, g, lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
	}), nil)
	if _, e := c.Open(context.Background(), "another", Address{"owned.test", 443}); !errors.Is(e, ErrBusy) || lookups != 0 {
		t.Fatal(e, lookups)
	}
	release()
	a, b := net.Pipe()
	defer b.Close()
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	c, _ = New(Config{Allow: []string{"127.0.0.2/32"}, Timeout: time.Second}, g, nil, func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		return a, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { _, e := c.Open(ctx, "a", Address{"127.0.0.2", 443}); done <- e }()
	<-entered
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if g.Status().Active != 0 {
		t.Fatal(g.Status())
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, e := b.Read(one[:]); e == nil {
		t.Fatal("returned failed socket left open")
	}
}
