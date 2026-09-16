// Package datagram preserves bounded UDP packet boundaries over an ordered byte stream.
package datagram

import (
	"encoding/binary"
	"errors"
	"net/netip"
	d "veil.local/core/internal/destination"
)

const MaxPayload = 65507
const Header = 8
const MaxRecord = Header + 4 + 253 + MaxPayload

var ErrWire = errors.New("invalid datagram record")

type Packet struct {
	Address d.Address
	Payload []byte
}

func address(a d.Address) (byte, []byte, error) {
	if !a.Valid() {
		return 0, nil, ErrWire
	}
	if ip, e := netip.ParseAddr(a.Host); e == nil {
		if ip.Is4() {
			v := ip.As4()
			return 1, v[:], nil
		}
		v := ip.As16()
		return 2, v[:], nil
	}
	return 3, []byte(a.Host), nil
}
func Encode(p Packet) ([]byte, error) {
	if len(p.Payload) > MaxPayload {
		return nil, ErrWire
	}
	kind, raw, e := address(p.Address)
	if e != nil {
		return nil, e
	}
	out := make([]byte, Header+4+len(raw)+len(p.Payload))
	out[0] = 1
	out[1] = 1
	binary.BigEndian.PutUint32(out[4:8], uint32(len(out)-Header))
	out[8] = kind
	out[9] = byte(len(raw))
	binary.BigEndian.PutUint16(out[10:12], p.Address.Port)
	copy(out[12:], raw)
	copy(out[12+len(raw):], p.Payload)
	return out, nil
}
func decodeAddress(kind byte, raw []byte, port uint16) (d.Address, error) {
	var host string
	switch kind {
	case 1:
		if len(raw) != 4 {
			return d.Address{}, ErrWire
		}
		host = netip.AddrFrom4([4]byte(raw)).String()
	case 2:
		if len(raw) != 16 {
			return d.Address{}, ErrWire
		}
		ip := netip.AddrFrom16([16]byte(raw))
		if ip.Is4In6() {
			return d.Address{}, ErrWire
		}
		host = ip.String()
	case 3:
		if len(raw) < 1 || len(raw) > 253 {
			return d.Address{}, ErrWire
		}
		host = string(raw)
		if _, e := netip.ParseAddr(host); e == nil {
			return d.Address{}, ErrWire
		}
	default:
		return d.Address{}, ErrWire
	}
	a, e := d.Parse(host, port)
	if e != nil || a.Host != host {
		return d.Address{}, ErrWire
	}
	return a, nil
}
func Decode(raw []byte) (Packet, error) {
	if len(raw) < Header+5 || len(raw) > MaxRecord || raw[0] != 1 || raw[1] != 1 || raw[2] != 0 || raw[3] != 0 || uint64(binary.BigEndian.Uint32(raw[4:8])) != uint64(len(raw)-Header) {
		return Packet{}, ErrWire
	}
	n := int(raw[9])
	if len(raw) < 12+n || len(raw)-12-n > MaxPayload {
		return Packet{}, ErrWire
	}
	a, e := decodeAddress(raw[8], raw[12:12+n], binary.BigEndian.Uint16(raw[10:12]))
	if e != nil {
		return Packet{}, e
	}
	return Packet{a, raw[12+n:]}, nil
}

// Decoder owns at most one fixed-size record; callback borrows payload until return.
// Write consumes complete frames in order and stops permanently after malformed input.
type Decoder struct {
	buffer     [MaxRecord]byte
	used, need int
	failed     bool
}

func (d0 *Decoder) Write(p []byte, deliver func(Packet) error) (int, error) {
	if d0.failed {
		return 0, ErrWire
	}
	total := 0
	for len(p) > 0 {
		if d0.need == 0 {
			d0.need = Header
		}
		n := copy(d0.buffer[d0.used:d0.need], p)
		d0.used += n
		total += n
		p = p[n:]
		if d0.used < d0.need {
			continue
		}
		if d0.need == Header {
			size := binary.BigEndian.Uint32(d0.buffer[4:8])
			if d0.buffer[0] != 1 || d0.buffer[1] != 1 || d0.buffer[2] != 0 || d0.buffer[3] != 0 || size < 5 || size > MaxRecord-Header {
				d0.failed = true
				return total, ErrWire
			}
			d0.need = Header + int(size)
			continue
		}
		packet, e := Decode(d0.buffer[:d0.need])
		if e == nil {
			e = deliver(packet)
		}
		if e != nil {
			d0.failed = true
			return total, e
		}
		clear(d0.buffer[:d0.need])
		d0.used = 0
		d0.need = Header
	}
	return total, nil
}
func (d0 *Decoder) Finish() error {
	if d0.failed || d0.used != 0 {
		return ErrWire
	}
	return nil
}
func (d0 *Decoder) Buffered() int { return d0.used }
func (d0 *Decoder) Clear()        { clear(d0.buffer[:]); d0.used = 0; d0.need = 0; d0.failed = true }

// ParseSOCKS rejects fragments instead of silently truncating or reassembling.
func ParseSOCKS(raw []byte) (Packet, error) {
	if len(raw) > 65507 || len(raw) < 4 || raw[0] != 0 || raw[1] != 0 || raw[2] != 0 {
		return Packet{}, ErrWire
	}
	kind, n, at := raw[3], 0, 4
	switch kind {
	case 1:
		n = 4
	case 4:
		kind = 2
		n = 16
	case 3:
		if len(raw) < 5 {
			return Packet{}, ErrWire
		}
		n = int(raw[4])
		at = 5
	default:
		return Packet{}, ErrWire
	}
	if len(raw) < at+n+2 || len(raw)-at-n-2 > MaxPayload {
		return Packet{}, ErrWire
	}
	port := binary.BigEndian.Uint16(raw[at+n : at+n+2])
	var a d.Address
	var e error
	if kind == 3 {
		a, e = d.Parse(string(raw[at:at+n]), port)
	} else {
		a, e = decodeAddress(kind, raw[at:at+n], port)
	}
	if e != nil {
		return Packet{}, ErrWire
	}
	return Packet{a, raw[at+n+2:]}, nil
}
func SOCKS(p Packet) ([]byte, error) {
	if len(p.Payload) > MaxPayload {
		return nil, ErrWire
	}
	kind, raw, e := address(p.Address)
	if e != nil {
		return nil, e
	}
	if kind == 2 {
		kind = 4
	}
	out := []byte{0, 0, 0, kind}
	if kind == 3 {
		out = append(out, byte(len(raw)))
	}
	out = append(out, raw...)
	out = binary.BigEndian.AppendUint16(out, p.Address.Port)
	out = append(out, p.Payload...)
	if len(out) > 65507 {
		return nil, ErrWire
	}
	return out, nil
}
