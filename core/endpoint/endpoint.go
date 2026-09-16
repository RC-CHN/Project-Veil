// Package endpoint represents canonical destinations without resolving them.
package endpoint

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

type Endpoint struct {
	host string
	port uint16
}

var ErrInvalid = errors.New("invalid endpoint")

func Parse(host string, port uint16) (Endpoint, error) {
	if port == 0 || len(host) == 0 || len(host) > 253 {
		return Endpoint{}, ErrInvalid
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return Endpoint{}, ErrInvalid
		}
		return Endpoint{ip.Unmap().String(), port}, nil
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return Endpoint{}, ErrInvalid
	}
	numeric := true
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return Endpoint{}, ErrInvalid
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return Endpoint{}, ErrInvalid
			}
			if c < '0' || c > '9' {
				numeric = false
			}
		}
	}
	if numeric {
		return Endpoint{}, ErrInvalid
	}
	return Endpoint{host, port}, nil
}
func ParseAddress(address string) (Endpoint, error) {
	h, p, err := net.SplitHostPort(address)
	if err != nil {
		return Endpoint{}, ErrInvalid
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return Endpoint{}, ErrInvalid
	}
	return Parse(h, uint16(n))
}
func (e Endpoint) Host() string           { return e.host }
func (e Endpoint) Port() uint16           { return e.port }
func (e Endpoint) Valid() bool            { v, err := Parse(e.host, e.port); return err == nil && v == e }
func (e Endpoint) IP() (netip.Addr, bool) { v, err := netip.ParseAddr(e.host); return v, err == nil }
func (e Endpoint) String() string         { return net.JoinHostPort(e.host, strconv.Itoa(int(e.port))) }
func (e Endpoint) Network() string        { return "veil" }
