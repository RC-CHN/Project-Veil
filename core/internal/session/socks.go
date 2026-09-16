package session

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/netip"
	d "veil.local/core/internal/destination"
	so "veil.local/core/internal/streamopen"
)

type socksRequest struct {
	address d.Address
	network byte
	source  netip.AddrPort
}

func readSOCKS(conn net.Conn, udp bool) (socksRequest, error) {
	var h [4]byte
	if _, e := io.ReadFull(conn, h[:2]); e != nil {
		return socksRequest{}, e
	}
	if h[0] != 5 || h[1] == 0 {
		return socksRequest{}, errors.New("SOCKS greeting")
	}
	methods := make([]byte, int(h[1]))
	if _, e := io.ReadFull(conn, methods); e != nil {
		return socksRequest{}, e
	}
	if !bytes.Contains(methods, []byte{0}) {
		writeFull(conn, []byte{5, 255})
		return socksRequest{}, errors.New("SOCKS method")
	}
	if e := writeFull(conn, []byte{5, 0}); e != nil {
		return socksRequest{}, e
	}
	if _, e := io.ReadFull(conn, h[:]); e != nil {
		return socksRequest{}, e
	}
	if h[0] != 5 || h[2] != 0 {
		return socksRequest{}, errors.New("SOCKS request")
	}
	if h[1] != 1 && !(udp && h[1] == 3) {
		socksResponse(conn, 7)
		return socksRequest{}, errors.New("SOCKS command unsupported")
	}
	command := h[1]
	var host string
	switch h[3] {
	case 1:
		var p [4]byte
		if _, e := io.ReadFull(conn, p[:]); e != nil {
			return socksRequest{}, e
		}
		host = netip.AddrFrom4(p).String()
	case 4:
		var p [16]byte
		if _, e := io.ReadFull(conn, p[:]); e != nil {
			return socksRequest{}, e
		}
		host = netip.AddrFrom16(p).Unmap().String()
	case 3:
		var n [1]byte
		if _, e := io.ReadFull(conn, n[:]); e != nil {
			return socksRequest{}, e
		}
		p := make([]byte, int(n[0]))
		if _, e := io.ReadFull(conn, p); e != nil {
			return socksRequest{}, e
		}
		host = string(p)
	default:
		socksResponse(conn, 8)
		return socksRequest{}, errors.New("SOCKS address type")
	}
	var p [2]byte
	if _, e := io.ReadFull(conn, p[:]); e != nil {
		return socksRequest{}, e
	}
	port := uint16(p[0])<<8 | uint16(p[1])
	if command == 3 {
		ip, e := netip.ParseAddr(host)
		peer, e2 := netip.ParseAddrPort(conn.RemoteAddr().String())
		if e != nil || e2 != nil || ip.Zone() != "" || ip.Is4() != peer.Addr().Unmap().Is4() || !ip.IsUnspecified() && ip.Unmap() != peer.Addr().Unmap() {
			socksResponse(conn, 8)
			return socksRequest{}, errors.New("UDP source must match control peer")
		}
		return socksRequest{network: so.NetworkUDP, source: netip.AddrPortFrom(peer.Addr().Unmap(), port)}, nil
	}
	a, e := d.Parse(host, port)
	if e != nil {
		socksResponse(conn, 8)
	}
	return socksRequest{address: a}, e
}

// Preserve a TCP-only parser helper for existing bounded parsing tests.
func socksDestination(conn net.Conn) (d.Address, error) {
	r, e := readSOCKS(conn, false)
	return r.address, e
}
func socksUDPResponse(conn net.Conn, local net.Addr) error {
	a, e := netip.ParseAddrPort(local.String())
	if e != nil {
		return e
	}
	ip := a.Addr().Unmap()
	out := []byte{5, 0, 0, 1}
	if ip.Is4() {
		v := ip.As4()
		out = append(out, v[:]...)
	} else {
		out[3] = 4
		v := ip.As16()
		out = append(out, v[:]...)
	}
	out = append(out, byte(a.Port()>>8), byte(a.Port()))
	return writeFull(conn, out)
}
