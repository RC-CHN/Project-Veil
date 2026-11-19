// Package socks provides a bounded SOCKS5 CONNECT-only local entry point.
package socks

import (
	"bufio"
	"bytes"
	"errors"
	"io"

	"veil/internal/wire"
)

// ReadConnect consumes only the negotiation. Pipelined application data stays
// in r for the caller. All peeks fit in the proxy reader (at most 259 bytes).
func ReadConnect(r *bufio.Reader, w io.Writer) (string, error) {
	h, err := r.Peek(2)
	if err != nil {
		return "", err
	}
	if h[0] != 5 || h[1] == 0 {
		return "", wire.ErrProtocol
	}
	greeting, err := r.Peek(2 + int(h[1]))
	if err != nil {
		return "", err
	}
	ok := bytes.IndexByte(greeting[2:], 0) >= 0
	r.Discard(len(greeting))
	if !ok {
		w.Write([]byte{5, 255})
		return "", errors.New("socks: no supported authentication method")
	}
	if _, err := w.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	req, err := r.Peek(4)
	if err != nil {
		return "", err
	}
	if req[0] != 5 || req[2] != 0 {
		return "", wire.ErrProtocol
	}
	if req[1] != 1 {
		Reply(w, 7)
		return "", errors.New("socks: CONNECT only")
	}
	reqType := req[3]
	r.Discard(3)
	var size int
	switch reqType {
	case 1:
		size = 7
	case 4:
		size = 19
	case 3:
		prefix, err := r.Peek(2)
		if err != nil {
			return "", err
		}
		if prefix[1] == 0 {
			return "", wire.ErrProtocol
		}
		size = 4 + int(prefix[1])
	default:
		Reply(w, 8)
		return "", wire.ErrProtocol
	}
	raw, err := r.Peek(size)
	if err != nil {
		return "", err
	}
	address, err := wire.DecodeAddress(raw)
	r.Discard(size)
	return address, err
}
func Reply(c io.Writer, code byte) error {
	_, err := c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}
