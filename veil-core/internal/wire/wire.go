// Package wire implements the bounded, sequential Veil framing layer.
package wire

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const HeaderSize = 4

// Include the frame header in the eight-record TLS plaintext budget.
const MaxData = 128*1024 - HeaderSize
const authVersion = 1
const (
	Auth byte = iota + 1
	_         // v0 AUTH_OK is no longer accepted.
	Open
	OpenOK
	OpenError
	Data
	Fin
	Done
	Cancel
)

var ErrProtocol = errors.New("veil: invalid frame or state")

func valid(t byte, n int) bool {
	switch t {
	case Auth:
		return n == 49
	case OpenOK, Fin, Done, Cancel:
		return n == 0
	case Open:
		return n >= 4 && n <= 259
	case OpenError:
		return n == 1
	case Data:
		return n > 0 && n <= MaxData
	default:
		return false
	}
}

// Reader owns its returned slice until the next Read. Limits are checked before
// allocation. Idle connections retain only a small control buffer.
type Reader struct {
	R   io.Reader
	buf []byte
}

func (r *Reader) Read() (byte, []byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r.R, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	if !valid(h[0], n) {
		return 0, nil, ErrProtocol
	}
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	} else {
		r.buf = r.buf[:n]
	}
	_, err := io.ReadFull(r.R, r.buf)
	return h[0], r.buf, err
}
func (r *Reader) Release() { r.buf = nil }

// WriteBuffer writes one header and payload in one TLS Write. The caller owns
// storage and must reserve HeaderSize leading bytes. No concurrent writers.
func WriteBuffer(w io.Writer, t byte, storage []byte, n int) error {
	if n < 0 || len(storage) < n+4 || !valid(t, n) {
		return ErrProtocol
	}
	storage[0], storage[1], storage[2], storage[3] = t, byte(n>>16), byte(n>>8), byte(n)
	written, err := w.Write(storage[:n+4])
	if err == nil && written != n+4 {
		err = io.ErrShortWrite
	}
	return err
}
func Write(w io.Writer, t byte, p []byte) error {
	b := make([]byte, 4+len(p))
	copy(b[4:], p)
	return WriteBuffer(w, t, b, len(p))
}

// WriteDone preserves the two frame types while sharing one TLS record when
// FIN can be deferred until the stream's completion barrier. No timer or queue.
func WriteDone(w io.Writer, finPending bool) error {
	if !finPending {
		return Write(w, Done, nil)
	}
	b := [2 * HeaderSize]byte{Fin, 0, 0, 0, Done, 0, 0, 0}
	n, err := w.Write(b[:])
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

// WriteOpen sends the first AUTH and OPEN in one TLS Write, with no AUTH_OK
// round trip. Reused connections pass nil for auth and send only OPEN.
func WriteOpen(w io.Writer, auth, address []byte) error {
	if !valid(Open, len(address)) || (auth != nil && !valid(Auth, len(auth))) {
		return ErrProtocol
	}
	if auth == nil {
		return Write(w, Open, address)
	}
	var b bytes.Buffer
	if err := Write(&b, Auth, auth); err != nil {
		return err
	}
	if err := Write(&b, Open, address); err != nil {
		return err
	}
	n, err := w.Write(b.Bytes())
	if err == nil && n != b.Len() {
		return io.ErrShortWrite
	}
	return err
}
func Expect(r *Reader, t byte) error {
	got, _, err := r.Read()
	if err != nil {
		return err
	}
	if got != t {
		return ErrProtocol
	}
	return nil
}

// Authentication is tied to this TLS connection's exporter; replaying a proof
// on another connection fails, including across TLS session resumption.
func proof(key, exporter, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("Veil-v0.1 client authentication\x00"))
	m.Write(exporter)
	m.Write(body)
	return m.Sum(nil)
}
func AuthPayload(key, exporter []byte) ([]byte, error) {
	if len(key) != 32 || len(exporter) != 32 {
		return nil, ErrProtocol
	}
	b := make([]byte, 49)
	b[0] = authVersion
	if _, err := rand.Read(b[1:17]); err != nil {
		return nil, err
	}
	copy(b[17:], proof(key, exporter, b[:17]))
	return b, nil
}
func VerifyAuth(key, exporter, p []byte) bool {
	return len(key) == 32 && len(exporter) == 32 && len(p) == 49 && p[0] == authVersion && hmac.Equal(p[17:], proof(key, exporter, p[:17]))
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
