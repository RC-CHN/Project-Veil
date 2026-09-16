// Package streamopen negotiates one bounded TCP stream or UDP association before streamlink data.
package streamopen

import (
	"encoding/binary"
	"errors"
	"net/netip"
	d "veil.local/core/internal/destination"
	sl "veil.local/core/internal/streamlink"
)

const Header = 8
const MaxBody = 16 + 253
const (
	OK byte = iota
	Denied
	Busy
	ResolutionFailed
	ConnectFailed
	TimedOut
)

type Limits struct {
	Window   int
	MaxBytes uint64
}

func (l Limits) Valid() bool {
	return l.Window > 0 && l.Window <= sl.MaxWindow && l.MaxBytes > 0 && l.MaxBytes <= 1<<40
}
func (l Limits) within(cap Limits) bool {
	return l.Valid() && l.Window <= cap.Window && l.MaxBytes <= cap.MaxBytes
}

const NetworkUDP byte = 2

// Network zero preserves the original TCP request and JSON representation.
// UDP associations have NetworkUDP and an empty Address; datagrams carry targets.
type Request struct {
	Address d.Address
	Limits  Limits
	Network byte `json:",omitempty"`
}
type Result struct {
	Code   byte
	Limits Limits
}

func prefix(kind byte, body []byte) []byte {
	p := make([]byte, Header+len(body))
	p[0] = 1
	p[1] = kind
	binary.BigEndian.PutUint32(p[4:8], uint32(len(body)))
	copy(p[Header:], body)
	return p
}
func EncodeRequest(r Request) ([]byte, error) {
	if !r.Limits.Valid() || r.Network != 0 && r.Network != NetworkUDP || r.Network == 0 && !r.Address.Valid() || r.Network == NetworkUDP && r.Address != (d.Address{}) {
		return nil, errors.New("OPEN request bound")
	}
	body := make([]byte, 16)
	body[0] = 1
	if r.Network == NetworkUDP {
		body[0] = NetworkUDP
		binary.BigEndian.PutUint32(body[4:8], uint32(r.Limits.Window))
		binary.BigEndian.PutUint64(body[8:16], r.Limits.MaxBytes)
		return prefix(1, body), nil
	}
	if ip, e := netip.ParseAddr(r.Address.Host); e == nil {
		if ip.Is4() {
			body[1] = 1
			p := ip.As4()
			body = append(body, p[:]...)
		} else {
			body[1] = 2
			p := ip.As16()
			body = append(body, p[:]...)
		}
	} else {
		body[1] = 3
		body = append(body, []byte(r.Address.Host)...)
	}
	binary.BigEndian.PutUint16(body[2:4], r.Address.Port)
	binary.BigEndian.PutUint32(body[4:8], uint32(r.Limits.Window))
	binary.BigEndian.PutUint64(body[8:16], r.Limits.MaxBytes)
	return prefix(1, body), nil
}
func decodeRequest(p []byte) (Request, error) {
	if len(p) == 16 && p[0] == NetworkUDP && p[1] == 0 && p[2] == 0 && p[3] == 0 {
		r := Request{Network: NetworkUDP, Limits: Limits{int(binary.BigEndian.Uint32(p[4:8])), binary.BigEndian.Uint64(p[8:16])}}
		if !r.Limits.Valid() {
			return Request{}, errors.New("association limits")
		}
		return r, nil
	}
	if len(p) < 17 || len(p) > MaxBody || p[0] != 1 {
		return Request{}, errors.New("OPEN body")
	}
	var host string
	switch p[1] {
	case 1:
		if len(p) != 20 {
			return Request{}, errors.New("IPv4 length")
		}
		host = netip.AddrFrom4([4]byte(p[16:])).String()
	case 2:
		if len(p) != 32 {
			return Request{}, errors.New("IPv6 length")
		}
		ip := netip.AddrFrom16([16]byte(p[16:]))
		if ip.Is4In6() {
			return Request{}, errors.New("mapped IPv6")
		}
		host = ip.String()
	case 3:
		host = string(p[16:])
		if _, e := netip.ParseAddr(host); e == nil {
			return Request{}, errors.New("numeric DNS address")
		}
	default:
		return Request{}, errors.New("address type")
	}
	a, e := d.Parse(host, binary.BigEndian.Uint16(p[2:4]))
	if e != nil || a.Host != host {
		return Request{}, errors.New("noncanonical destination")
	}
	r := Request{Address: a, Limits: Limits{int(binary.BigEndian.Uint32(p[4:8])), binary.BigEndian.Uint64(p[8:16])}}
	if !r.Limits.Valid() {
		return Request{}, errors.New("OPEN limits")
	}
	return r, nil
}
func EncodeResult(r Result) ([]byte, error) {
	if r.Code > TimedOut || r.Code == OK && !r.Limits.Valid() || r.Code != OK && r.Limits != (Limits{}) {
		return nil, errors.New("RESULT code or limits")
	}
	p := make([]byte, 16)
	p[0] = r.Code
	binary.BigEndian.PutUint32(p[4:8], uint32(r.Limits.Window))
	binary.BigEndian.PutUint64(p[8:], r.Limits.MaxBytes)
	return prefix(2, p), nil
}
func decodeResult(p []byte) (Result, error) {
	if len(p) != 16 || p[1] != 0 || p[2] != 0 || p[3] != 0 {
		return Result{}, errors.New("RESULT body")
	}
	r := Result{p[0], Limits{int(binary.BigEndian.Uint32(p[4:8])), binary.BigEndian.Uint64(p[8:])}}
	if _, e := EncodeResult(r); e != nil {
		return Result{}, e
	}
	return r, nil
}
