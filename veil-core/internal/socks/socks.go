// Package socks provides a bounded SOCKS5 CONNECT-only local entry point.
package socks

import (
	"errors"
	"io"
	"net"
	"veil/internal/wire"
)

func ReadConnect(c net.Conn) (string, error) {
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return "", err
	}
	if h[0] != 5 || h[1] == 0 {
		return "", wire.ErrProtocol
	}
	methods := make([]byte, int(h[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	ok := false
	for _, m := range methods {
		if m == 0 {
			ok = true
		}
	}
	if !ok {
		c.Write([]byte{5, 255})
		return "", errors.New("socks: no supported authentication method")
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return "", err
	}
	if req[0] != 5 || req[2] != 0 {
		return "", wire.ErrProtocol
	}
	if req[1] != 1 {
		Reply(c, 7)
		return "", errors.New("socks: CONNECT only")
	}
	b := []byte{req[3]}
	n := 0
	switch req[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return "", err
		}
		b = append(b, size[0])
		n = int(size[0])
		if n == 0 {
			return "", wire.ErrProtocol
		}
	default:
		Reply(c, 8)
		return "", wire.ErrProtocol
	}
	rest := make([]byte, n+2)
	if _, err := io.ReadFull(c, rest); err != nil {
		return "", err
	}
	return wire.DecodeAddress(append(b, rest...))
}
func Reply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}
