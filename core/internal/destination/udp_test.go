package destination

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestUDPPolicyPinnedDialAndRelease(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		gate, _ := NewGate(1, 1)
		lookups, dials := 0, 0
		resolver := lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			lookups++
			ips := []netip.Addr{netip.MustParseAddr("127.0.0.2")}
			if mixed {
				ips = append(ips, netip.MustParseAddr("10.0.0.1"))
			}
			return ips, nil
		})
		connector, e := New(Config{Allow: []string{"127.0.0.2/32"}, Timeout: time.Second}, gate, resolver, func(_ context.Context, network, address string) (net.Conn, error) {
			dials++
			if network != "udp" || address != "127.0.0.2:53" {
				t.Fatal(network, address)
			}
			a, b := net.Pipe()
			b.Close()
			return a, nil
		})
		if e != nil {
			t.Fatal(e)
		}
		conn, e := connector.OpenUDP(context.Background(), "peer", Address{"owned.test", 53})
		if mixed {
			if !errors.Is(e, ErrDenied) || dials != 0 || gate.Status().Active != 0 {
				t.Fatal(e, dials, gate.Status())
			}
			continue
		}
		if e != nil || dials != 1 || lookups != 1 {
			t.Fatal(e, dials, lookups)
		}
		if _, e = connector.OpenUDP(context.Background(), "peer", Address{"another.test", 53}); !errors.Is(e, ErrBusy) || lookups != 1 {
			t.Fatal("UDP gate did not precede DNS", e)
		}
		conn.Close()
		if gate.Status().Active != 1 {
			t.Fatal("early UDP admission release")
		}
		conn.Release()
		conn.Release()
		if gate.Status().Active != 0 {
			t.Fatal("UDP admission leaked")
		}
	}
}
