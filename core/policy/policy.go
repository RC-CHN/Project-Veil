// Package destinationpolicy is the common numeric-address policy used by the
// server connector and by DNS validation before issuing a TUN synthetic address.
package policy

import (
	"errors"
	"net/netip"
)

var ErrDenied = errors.New("destination denied")
var ErrResolve = errors.New("destination resolution failed")

type Policy struct {
	allow, deny []netip.Prefix
}

var reserved = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"),
}

func New(allow, deny []string) (*Policy, error) {
	if len(allow) > 128 || len(deny) > 128 {
		return nil, errors.New("destination configuration")
	}
	out := &Policy{}
	for i, list := range [][]string{allow, deny} {
		for _, raw := range list {
			p, err := netip.ParsePrefix(raw)
			if err != nil || p.Addr().Is4In6() {
				return nil, errors.New("policy prefix")
			}
			if i == 0 {
				out.allow = append(out.allow, p.Masked())
			} else {
				out.deny = append(out.deny, p.Masked())
			}
		}
	}
	return out, nil
}

// Allowed retains the server's original precedence: invalid/unspecified/
// multicast, explicit deny, explicit allow, then the default public policy.
// Allow prefixes are exceptions, not an exclusive allowlist.
func (p *Policy) Allowed(a netip.Addr) bool {
	if p == nil || !a.IsValid() || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
		return false
	}
	a = a.Unmap()
	for _, prefix := range p.deny {
		if prefix.Contains(a) {
			return false
		}
	}
	for _, prefix := range p.allow {
		if prefix.Contains(a) {
			return true
		}
	}
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range reserved {
		if prefix.Contains(a) {
			return false
		}
	}
	return true
}

// Check requires the complete A/AAAA result to fit the shared bound and every
// address to pass. Callers must never use a permitted subset of a mixed result.
func (p *Policy) Check(addresses []netip.Addr) error {
	if len(addresses) == 0 || len(addresses) > 16 {
		return ErrResolve
	}
	for _, address := range addresses {
		if !p.Allowed(address) {
			return ErrDenied
		}
	}
	return nil
}
