// Package wire implements TLS-bound authentication and destination encoding.
package wire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const (
	HeaderSize       = 4
	AuthSize         = 49
	authVersion      = 2
	Auth        byte = 1
)

var ErrProtocol = errors.New("veil: invalid frame or state")

// OpenFailure reports a failed destination dial to the caller.
type OpenFailure byte

const (
	OpenFailed OpenFailure = iota + 1
	OpenDNS
	OpenRefused
	OpenTimeout
)

func (e OpenFailure) Error() string {
	switch e {
	case OpenDNS:
		return "veil: target DNS lookup failed"
	case OpenRefused:
		return "veil: target connection refused"
	case OpenTimeout:
		return "veil: target dial timed out"
	default:
		return "veil: target dial failed"
	}
}

// Reader owns its returned slice until the next Read. Limits are checked before
// allocation. Idle connections retain only a small control buffer.
type Reader struct {
	R      io.Reader
	buf    []byte
	header [HeaderSize]byte
}

func (r *Reader) Read() (byte, []byte, error) {
	t, n, err := r.ReadHeader()
	if err != nil {
		return 0, nil, err
	}
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	} else {
		r.buf = r.buf[:n]
	}
	_, err = io.ReadFull(r.R, r.buf)
	return t, r.buf, err
}

// ReadHeader validates the length before a caller allocates or streams payload.
// The caller must consume exactly n bytes before reading another header.
func (r *Reader) ReadHeader() (t byte, n int, err error) {
	h := r.header[:]
	if _, err := io.ReadFull(r.R, h[:]); err != nil {
		return 0, 0, err
	}
	n = int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	if h[0] != Auth || n != AuthSize {
		return 0, 0, ErrProtocol
	}
	return h[0], n, nil
}
func (r *Reader) Release() { r.buf = nil }

// Write emits the authentication frame in one write. Stream frames belong to mux.
func Write(w io.Writer, t byte, p []byte) error {
	if t != Auth || len(p) != AuthSize {
		return ErrProtocol
	}
	var b [HeaderSize + AuthSize]byte
	b[0], b[3] = Auth, AuthSize
	copy(b[HeaderSize:], p)
	n, err := w.Write(b[:])
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

// Authentication is tied to this TLS connection's exporter; replaying a proof
// on another connection fails, including across TLS session resumption.
func proof(key, exporter, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("Veil-v0.2 client authentication\x00"))
	m.Write(exporter)
	m.Write(body)
	return m.Sum(nil)
}
func AuthPayload(key, exporter []byte) ([]byte, error) {
	if len(key) != 32 || len(exporter) != 32 {
		return nil, ErrProtocol
	}
	b := make([]byte, AuthSize)
	b[0] = authVersion
	if _, err := rand.Read(b[1:17]); err != nil {
		return nil, err
	}
	copy(b[17:], proof(key, exporter, b[:17]))
	return b, nil
}
func VerifyAuth(key, exporter, p []byte) bool {
	return len(key) == 32 && len(exporter) == 32 && len(p) == AuthSize && p[0] == authVersion && hmac.Equal(p[17:], proof(key, exporter, p[:17]))
}

func EncodeAddress(address string) ([]byte, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	pn, err := strconv.Atoi(port)
	if err != nil || pn < 1 || pn > 65535 {
		return nil, ErrProtocol
	}
	var b []byte
	ip := net.ParseIP(host)
	if v := ip.To4(); v != nil {
		b = append([]byte{1}, v...)
	} else if ip != nil {
		b = append([]byte{4}, ip.To16()...)
	} else {
		if len(host) == 0 || len(host) > 255 {
			return nil, ErrProtocol
		}
		b = append([]byte{3, byte(len(host))}, []byte(host)...)
	}
	return append(b, byte(pn>>8), byte(pn)), nil
}
func DecodeAddress(p []byte) (string, error) {
	if len(p) < 4 {
		return "", ErrProtocol
	}
	var host string
	var n int
	switch p[0] {
	case 1:
		n = 5
		if len(p) >= n {
			host = net.IP(p[1:n]).String()
		}
	case 4:
		n = 17
		if len(p) >= n {
			host = net.IP(p[1:n]).String()
		}
	case 3:
		n = 2 + int(p[1])
		if p[1] == 0 {
			return "", ErrProtocol
		}
		if len(p) >= n {
			host = string(p[2:n])
			for _, c := range []byte(host) {
				if c <= 32 || c == 127 || c == ':' || c == '/' || c == '\\' {
					return "", ErrProtocol
				}
			}
		}
	default:
		return "", ErrProtocol
	}
	if len(p) != n+2 {
		return "", ErrProtocol
	}
	port := int(p[n])<<8 | int(p[n+1])
	if port == 0 {
		return "", ErrProtocol
	}
	return net.JoinHostPort(host, fmt.Sprint(port)), nil
}
