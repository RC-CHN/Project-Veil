// Package streammux multiplexes bounded inner streams inside an ordered carrier.
// These records are inner protocol content; they do not replace the outer model.
package streammux

import (
	"encoding/binary"
	"errors"

	sl "veil.local/core/internal/streamlink"
	so "veil.local/core/internal/streamopen"
)

const (
	Header            = 12
	Quantum           = sl.MaxData + 2*sl.Header
	MaxBody           = Quantum
	MaxStreams        = 32
	MaxReservedWindow = 1 << 20
	MaxOpened         = 4096
	SettingsSize      = 28
)
const (
	SettingsKind byte = 1 + iota
	OpenKind
	ResultKind
	DataKind
	EndKind
	DrainKind
	GrantKind
	// EarlyOpenKind is an opt-in extension. The surrounding authenticated
	// protocol must bind support; the legacy Session rejects this record.
	EarlyOpenKind
)
const (
	EndNormal byte = iota
	EndCancelled
	EndBudget
	EndInternal
)

// Settings bound concurrent work, reserved consumer credit, stream lifetime and
// total encoded inner bytes. Each side advertises caps; negotiation takes minima.
type Settings struct {
	Streams         int
	Window          int
	StreamBytes     uint64
	ConnectionBytes uint64
	Opened          uint32
}

func (s Settings) Valid() bool {
	return s.Streams >= 1 && s.Streams <= MaxStreams && s.Window >= sl.MaxData && s.Window <= sl.MaxWindow && s.Streams*s.Window <= MaxReservedWindow && s.StreamBytes >= 1 && s.StreamBytes <= 1<<40 && s.ConnectionBytes >= 4096 && s.ConnectionBytes <= 1<<40 && s.Opened >= 1 && s.Opened <= MaxOpened
}
func (s Settings) Intersect(p Settings) (Settings, error) {
	if !s.Valid() || !p.Valid() {
		return Settings{}, errors.New("mux settings bounds")
	}
	return Settings{min(s.Streams, p.Streams), min(s.Window, p.Window), min(s.StreamBytes, p.StreamBytes), min(s.ConnectionBytes, p.ConnectionBytes), min(s.Opened, p.Opened)}, nil
}
func EncodeSettings(s Settings) ([]byte, error) {
	if !s.Valid() {
		return nil, errors.New("mux settings bounds")
	}
	p := make([]byte, SettingsSize)
	binary.BigEndian.PutUint16(p[:2], uint16(s.Streams))
	binary.BigEndian.PutUint32(p[4:8], uint32(s.Window))
	binary.BigEndian.PutUint64(p[8:16], s.StreamBytes)
	binary.BigEndian.PutUint64(p[16:24], s.ConnectionBytes)
	binary.BigEndian.PutUint32(p[24:28], s.Opened)
	return p, nil
}
func DecodeSettings(p []byte) (Settings, error) {
	if len(p) != SettingsSize || p[2] != 0 || p[3] != 0 {
		return Settings{}, errors.New("mux settings encoding")
	}
	s := Settings{int(binary.BigEndian.Uint16(p[:2])), int(binary.BigEndian.Uint32(p[4:8])), binary.BigEndian.Uint64(p[8:16]), binary.BigEndian.Uint64(p[16:24]), binary.BigEndian.Uint32(p[24:28])}
	if !s.Valid() {
		return Settings{}, errors.New("mux settings bounds")
	}
	return s, nil
}

type Frame struct {
	Kind byte
	ID   uint32
	Body []byte
}

func validShape(kind byte, id uint32, n int) bool {
	if n < 0 || n > MaxBody {
		return false
	}
	switch kind {
	case SettingsKind:
		return id == 0 && n == SettingsSize
	case OpenKind, EarlyOpenKind:
		return id > 0 && n >= so.Header+16 && n <= so.Header+so.MaxBody
	case ResultKind:
		return id > 0 && n == so.Header+16
	case DataKind:
		return id > 0 && n > 0
	case EndKind:
		return id > 0 && n == 1
	case DrainKind:
		return id == 0 && n == 0
	case GrantKind:
		return id == 0 && n == 4
	default:
		return false
	}
}
func validBody(f Frame) error {
	switch f.Kind {
	case SettingsKind:
		_, e := DecodeSettings(f.Body)
		return e
	case OpenKind, EarlyOpenKind:
		_, e := so.DecodeRequest(f.Body)
		return e
	case ResultKind:
		_, e := so.DecodeResult(f.Body)
		return e
	case EndKind:
		if f.Body[0] > EndInternal {
			return errors.New("mux END code")
		}
	case GrantKind:
		if n := binary.BigEndian.Uint32(f.Body); n == 0 || n > MaxOpened {
			return errors.New("mux GRANT count")
		}
	}
	return nil
}
func Encode(f Frame) ([]byte, error) {
	if !validShape(f.Kind, f.ID, len(f.Body)) {
		return nil, errors.New("mux frame shape")
	}
	if e := validBody(f); e != nil {
		return nil, e
	}
	p := make([]byte, Header+len(f.Body))
	p[0] = 1
	p[1] = f.Kind
	binary.BigEndian.PutUint32(p[4:8], f.ID)
	binary.BigEndian.PutUint32(p[8:12], uint32(len(f.Body)))
	copy(p[Header:], f.Body)
	return p, nil
}

// Decoder owns one fixed-size scratch record. Callback bodies are borrowed only
// for the call. The caller serializes Feed/Finish/Close and does not reenter Feed.
type Decoder struct {
	buffer       [Header + MaxBody]byte
	parsed, need int
	err          error
	closed       bool
}

func (d *Decoder) fail(e error) error {
	clear(d.buffer[:])
	d.parsed = 0
	d.need = 0
	d.err = e
	d.closed = true
	return e
}
func (d *Decoder) Feed(p []byte, accept func(Frame) error) error {
	if d.closed {
		if d.err != nil {
			return d.err
		}
		return errors.New("mux decoder closed")
	}
	if accept == nil {
		return d.fail(errors.New("nil mux receiver"))
	}
	if d.need == 0 {
		d.need = Header
	}
	for len(p) > 0 {
		n := copy(d.buffer[d.parsed:d.need], p)
		d.parsed += n
		p = p[n:]
		if d.parsed < d.need {
			continue
		}
		if d.need == Header {
			size := binary.BigEndian.Uint32(d.buffer[8:12])
			id := binary.BigEndian.Uint32(d.buffer[4:8])
			kind := d.buffer[1]
			if d.buffer[0] != 1 || d.buffer[2] != 0 || d.buffer[3] != 0 || size > MaxBody || !validShape(kind, id, int(size)) {
				return d.fail(errors.New("mux frame header"))
			}
			d.need = Header + int(size)
			if d.parsed < d.need {
				continue
			}
		}
		f := Frame{Kind: d.buffer[1], ID: binary.BigEndian.Uint32(d.buffer[4:8]), Body: d.buffer[Header:d.need]}
		if e := validBody(f); e != nil {
			return d.fail(e)
		}
		if e := accept(f); e != nil {
			return d.fail(e)
		}
		clear(d.buffer[:d.need])
		d.parsed = 0
		d.need = Header
	}
	return nil
}
func (d *Decoder) Finish() error {
	if d.closed {
		if d.err != nil {
			return d.err
		}
		return errors.New("mux decoder closed")
	}
	if d.parsed != 0 {
		return d.fail(errors.New("truncated mux frame"))
	}
	d.closed = true
	clear(d.buffer[:])
	return nil
}
func (d *Decoder) Close() { d.fail(errors.New("mux decoder closed")) }
