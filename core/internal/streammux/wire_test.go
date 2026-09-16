package streammux

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func settings() Settings {
	return Settings{Streams: 8, Window: 65536, StreamBytes: 8 << 20, ConnectionBytes: 64 << 20, Opened: 4096}
}
func TestSettingsCreditReservation(t *testing.T) {
	s := settings()
	if !s.Valid() {
		t.Fatal("settings")
	}
	for _, v := range []Settings{{0, 16384, 1, 4096, 1}, {33, 16384, 1, 4096, 1}, {32, 65536, 1, 4096, 1}, {1, 16383, 1, 4096, 1}, {1, 16384, 0, 4096, 1}, {1, 16384, 1, 4095, 1}, {1, 16384, 1, 4096, 4097}} {
		if v.Valid() {
			t.Fatalf("accepted %+v", v)
		}
	}
	p := Settings{Streams: 2, Window: 16384, StreamBytes: 4096, ConnectionBytes: 8192, Opened: 32}
	n, e := s.Intersect(p)
	if e != nil || n != p || n.Streams*n.Window > MaxReservedWindow {
		t.Fatal(n, e)
	}
}
func TestIndependentMuxVectors(t *testing.T) {
	p, e := os.ReadFile("../../testdata/streammux-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var vectors []struct {
		Name string
		Kind byte
		ID   uint32
		Hex  string
	}
	if e = json.Unmarshal(p, &vectors); e != nil {
		t.Fatal(e)
	}
	if len(vectors) != 10 {
		t.Fatal("missing independent vectors")
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			wire, e := hex.DecodeString(v.Hex)
			if e != nil {
				t.Fatal(e)
			}
			for split := 0; split <= len(wire); split++ {
				var d Decoder
				calls := 0
				accept := func(f Frame) error {
					calls++
					if f.Kind != v.Kind || f.ID != v.ID {
						t.Fatal("identity")
					}
					encoded, e := Encode(f)
					if e != nil || !bytes.Equal(encoded, wire) {
						t.Fatal("independent bytes mismatch", e)
					}
					return nil
				}
				if e = d.Feed(wire[:split], accept); e != nil {
					t.Fatal(e)
				}
				if e = d.Feed(wire[split:], accept); e != nil {
					t.Fatal(e)
				}
				if e = d.Finish(); e != nil || calls != 1 {
					t.Fatal("record completion", e, calls)
				}
			}
		})
	}
}
func TestHeaderFailsBeforeBodyAndStaysClosed(t *testing.T) {
	p, _ := Encode(Frame{Kind: DataKind, ID: 1, Body: []byte{0}})
	mutations := [][]byte{}
	for _, i := range []int{0, 1, 2, 3} {
		q := bytes.Clone(p[:Header])
		q[i] = 255
		mutations = append(mutations, q)
	}
	q := bytes.Clone(p[:Header])
	clear(q[4:8])
	mutations = append(mutations, q)
	for _, n := range []uint32{0, MaxBody + 1, 0xffffffff} {
		q = bytes.Clone(p[:Header])
		binary.BigEndian.PutUint32(q[8:12], n)
		mutations = append(mutations, q)
	}
	for _, q := range mutations {
		var d Decoder
		called := false
		accept := func(Frame) error { called = true; return nil }
		if d.Feed(q, accept) == nil || called {
			t.Fatal("header not rejected immediately")
		}
		if d.parsed != 0 || d.buffer != ([Header + MaxBody]byte{}) {
			t.Fatal("retained rejected input")
		}
		if d.Feed(p, accept) == nil || d.Finish() == nil || called {
			t.Fatal("failure reopened")
		}
	}
}
func TestRecordBoundsTruncationAndCallbackFailure(t *testing.T) {
	body := bytes.Repeat([]byte{0x5a}, MaxBody)
	p, e := Encode(Frame{Kind: DataKind, ID: 4096, Body: body})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Encode(Frame{Kind: DataKind, ID: 1, Body: append(body, 0)}); e == nil {
		t.Fatal("oversize")
	}
	var d Decoder
	n := 0
	for _, b := range p {
		if e = d.Feed([]byte{b}, func(f Frame) error {
			n++
			if !bytes.Equal(f.Body, body) {
				t.Fatal("fragmented data")
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
	}
	if e = d.Finish(); e != nil || n != 1 {
		t.Fatal(e, n)
	}
	for _, cut := range []int{1, Header - 1, Header, Header + 1, len(p) - 1} {
		var d Decoder
		if e = d.Feed(p[:cut], func(Frame) error { t.Fatal("partial callback"); return nil }); e != nil {
			t.Fatal(e)
		}
		if d.Finish() == nil {
			t.Fatal("accepted truncation")
		}
	}
	var failed Decoder
	cause := errors.New("receiver failure")
	if e = failed.Feed(p, func(Frame) error { return cause }); !errors.Is(e, cause) {
		t.Fatal(e)
	}
	if failed.buffer != ([Header + MaxBody]byte{}) {
		t.Fatal("callback data retained")
	}
	if e = failed.Feed(p, func(Frame) error { t.Fatal("called after failure"); return nil }); !errors.Is(e, cause) {
		t.Fatal(e)
	}
}
func FuzzMuxRecords(f *testing.F) {
	s, _ := EncodeSettings(settings())
	a, _ := Encode(Frame{Kind: SettingsKind, Body: s})
	b, _ := Encode(Frame{Kind: DrainKind})
	c, _ := Encode(Frame{Kind: DataKind, ID: 1, Body: []byte("hello")})
	f.Add(append(a, b...), uint16(1))
	f.Add(c, uint16(17))
	f.Add([]byte{}, uint16(0))
	f.Fuzz(func(t *testing.T, p []byte, step uint16) {
		if len(p) > 2*(Header+MaxBody) {
			return
		}
		var d Decoder
		var encoded []byte
		accept := func(v Frame) error {
			q, e := Encode(v)
			if e != nil {
				t.Fatal(e)
			}
			encoded = append(encoded, q...)
			return nil
		}
		stride := 1 + int(step)%1024
		var e error
		for at := 0; at < len(p); at += stride {
			if e = d.Feed(p[at:min(at+stride, len(p))], accept); e != nil {
				break
			}
		}
		if e == nil {
			e = d.Finish()
		}
		if e == nil && !bytes.Equal(encoded, p) {
			t.Fatal("noncanonical record sequence")
		}
		if d.parsed > Header+MaxBody || d.need > Header+MaxBody {
			t.Fatal("parser bound")
		}
	})
}
