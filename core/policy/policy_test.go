package policy

import (
	"errors"
	"net/netip"
	"testing"
)

func TestPolicyCompleteAnswerSet(t *testing.T) {
	p, err := New([]string{"10.1.2.0/24", "fd12::/64", "0.0.0.0/32", "224.0.0.0/4"}, []string{"10.1.2.3/32"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		addresses []string
		want      error
	}{
		{"public_not_in_allow", []string{"8.8.8.8", "2606:4700:4700::1111"}, nil},
		{"exceptions", []string{"10.1.2.4", "fd12::1"}, nil},
		{"deny_precedence", []string{"10.1.2.3"}, ErrDenied},
		{"same_family_mixed", []string{"10.1.2.4", "10.2.3.4"}, ErrDenied},
		{"cross_family_mixed", []string{"10.1.2.4", "fd13::1"}, ErrDenied},
		{"mapped_denied", []string{"8.8.8.8", "::ffff:10.1.2.3"}, ErrDenied},
		{"multicast_not_overridden", []string{"224.0.0.1"}, ErrDenied},
		{"unspecified_not_overridden", []string{"0.0.0.0"}, ErrDenied},
		{"zone", []string{"fe80::1%eth0"}, ErrDenied},
		{"empty", nil, ErrResolve},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var addresses []netip.Addr
			for _, raw := range tc.addresses {
				addresses = append(addresses, netip.MustParseAddr(raw))
			}
			if err := p.Check(addresses); !errors.Is(err, tc.want) {
				t.Fatal(err, tc.want)
			}
		})
	}
	addresses := make([]netip.Addr, 17)
	for i := range addresses {
		addresses[i] = netip.MustParseAddr("8.8.8.8")
	}
	if p.Check(addresses[:16]) != nil || !errors.Is(p.Check(addresses), ErrResolve) {
		t.Fatal("answer bound")
	}
	for _, raw := range []string{"127.0.0.1", "::1", "169.254.169.254", "100.64.0.1", "192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1", "2001:db8::1"} {
		if p.Allowed(netip.MustParseAddr(raw)) {
			t.Fatal(raw)
		}
	}
	var missing *Policy
	if missing.Allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("nil policy")
	}
}

func TestPolicyConfiguration(t *testing.T) {
	for _, list := range [][]string{{"garbage"}, {"::ffff:10.0.0.0/120"}, make([]string, 129)} {
		if _, err := New(list, nil); err == nil {
			t.Fatal(list)
		}
		if _, err := New(nil, list); err == nil {
			t.Fatal(list)
		}
	}
	p, err := New([]string{"10.1.2.77/24"}, nil)
	if err != nil || !p.Allowed(netip.MustParseAddr("10.1.2.4")) {
		t.Fatal("prefix normalization", err)
	}
}
