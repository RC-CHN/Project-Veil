package socks5

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"veil.local/core/endpoint"
)

func handshake(c net.Conn) (byte, string, uint16, error) {
	var h [2]byte
	if _, e := io.ReadFull(c, h[:]); e != nil {
		return 0, "", 0, e
	}
	if h[0] != 5 || h[1] == 0 {
		return 0, "", 0, errors.New("SOCKS greeting")
	}
	methods := make([]byte, int(h[1]))
	if _, e := io.ReadFull(c, methods); e != nil {
		return 0, "", 0, e
	}
	allowed := false
	for _, m := range methods {
		if m == 0 {
			allowed = true
		}
	}
	if !allowed {
		c.Write([]byte{5, 255})
		return 0, "", 0, errors.New("SOCKS authentication")
	}
	if _, e := c.Write([]byte{5, 0}); e != nil {
		return 0, "", 0, e
	}
	var req [4]byte
	if _, e := io.ReadFull(c, req[:]); e != nil {
		return 0, "", 0, e
	}
	if req[0] != 5 || req[2] != 0 {
		return 0, "", 0, errors.New("SOCKS request")
	}
	host, port, e := readAddress(c, req[3])
	return req[1], host, port, e
}
func readAddress(r io.Reader, kind byte) (string, uint16, error) {
	n := 0
	switch kind {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var b [1]byte
		if _, e := io.ReadFull(r, b[:]); e != nil {
			return "", 0, e
		}
		n = int(b[0])
		if n < 1 || n > 253 {
			return "", 0, errors.New("SOCKS domain length")
		}
	default:
		return "", 0, errors.New("SOCKS address type")
	}
	raw := make([]byte, n+2)
	if _, e := io.ReadFull(r, raw); e != nil {
		return "", 0, e
	}
	host := string(raw[:n])
	if kind != 3 {
		ip, ok := netip.AddrFromSlice(raw[:n])
		if !ok {
			return "", 0, errors.New("SOCKS address")
		}
		host = ip.Unmap().String()
	}
	return host, binary.BigEndian.Uint16(raw[n:]), nil
}
func address(e endpoint.Endpoint) []byte {
	out := []byte{}
	if ip, ok := e.IP(); ok {
		if ip.Is4() {
			out = append(out, 1)
		} else {
			out = append(out, 4)
		}
		out = append(out, ip.AsSlice()...)
	} else {
		out = append(out, 3, byte(len(e.Host())))
		out = append(out, e.Host()...)
	}
	return binary.BigEndian.AppendUint16(out, e.Port())
}
func reply(w io.Writer, code byte, a net.Addr) error {
	out := []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}
	if a != nil {
		e, err := endpoint.ParseAddress(a.String())
		if err != nil {
			return err
		}
		out = append([]byte{5, code, 0}, address(e)...)
	}
	n, e := w.Write(out)
	if e == nil && n != len(out) {
		return io.ErrShortWrite
	}
	return e
}

type sliceReader struct {
	p  []byte
	at int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.at == len(r.p) {
		return 0, io.EOF
	}
	n := copy(p, r.p[r.at:])
	r.at += n
	return n, nil
}
func decodeUDP(raw []byte) (endpoint.Endpoint, []byte, error) {
	if len(raw) < 4 || len(raw) > 65507 || raw[0] != 0 || raw[1] != 0 || raw[2] != 0 {
		return endpoint.Endpoint{}, nil, errors.New("SOCKS datagram framing")
	}
	r := &sliceReader{p: raw[4:]}
	host, port, e := readAddress(r, raw[3])
	if e != nil {
		return endpoint.Endpoint{}, nil, e
	}
	target, e := endpoint.Parse(host, port)
	return target, raw[4+r.at:], e
}
func encodeUDP(e endpoint.Endpoint, p []byte) ([]byte, error) {
	if !e.Valid() {
		return nil, endpoint.ErrInvalid
	}
	out := append([]byte{0, 0, 0}, address(e)...)
	if len(out)+len(p) > 65507 {
		return nil, errors.New("SOCKS datagram size")
	}
	return append(out, p...), nil
}
