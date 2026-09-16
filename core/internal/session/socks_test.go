package session

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
	d "veil.local/core/internal/destination"
)

type memorySocket struct {
	input    *bytes.Reader
	output   bytes.Buffer
	fragment int
}

func (s *memorySocket) Read(p []byte) (int, error) {
	if s.fragment > 0 && len(p) > s.fragment {
		p = p[:s.fragment]
	}
	return s.input.Read(p)
}
func (s *memorySocket) Write(p []byte) (int, error) {
	if s.fragment > 0 && len(p) > s.fragment {
		p = p[:s.fragment]
	}
	return s.output.Write(p)
}
func (*memorySocket) Close() error                     { return nil }
func (*memorySocket) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*memorySocket) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*memorySocket) SetDeadline(time.Time) error      { return nil }
func (*memorySocket) SetReadDeadline(time.Time) error  { return nil }
func (*memorySocket) SetWriteDeadline(time.Time) error { return nil }

func TestSOCKSFragmentationAndPayloadBoundary(t *testing.T) {
	input := append([]byte{5, 1, 0, 5, 1, 0, 3, 10}, []byte("Owned.Test")...)
	input = append(input, 1, 187)
	input = append(input, []byte("early application bytes")...)
	for _, fragment := range []int{0, 1, 3} {
		s := &memorySocket{input: bytes.NewReader(input), fragment: fragment}
		a, e := socksDestination(s)
		if e != nil || a.Host != "owned.test" || a.Port != 443 {
			t.Fatal(a, e)
		}
		rest, _ := io.ReadAll(s.input)
		if string(rest) != "early application bytes" || !bytes.Equal(s.output.Bytes(), []byte{5, 0}) {
			t.Fatal("payload consumed or premature success")
		}
	}
}
func TestSOCKSRejectsWithoutSuccess(t *testing.T) {
	for _, v := range []struct{ input, output []byte }{
		{[]byte{5, 1, 2}, []byte{5, 255}},
		{[]byte{5, 1, 0, 5, 3, 0, 1}, []byte{5, 0, 5, 7, 0, 1, 0, 0, 0, 0, 0, 0}},
		{[]byte{5, 1, 0, 5, 1, 0, 1, 127, 0, 0, 1, 0, 0}, []byte{5, 0, 5, 8, 0, 1, 0, 0, 0, 0, 0, 0}},
		{[]byte{5, 1, 0, 5, 1, 0, 3, 0, 1, 187}, []byte{5, 0, 5, 8, 0, 1, 0, 0, 0, 0, 0, 0}},
	} {
		s := &memorySocket{input: bytes.NewReader(v.input), fragment: 1}
		if _, e := socksDestination(s); e == nil || !bytes.Equal(s.output.Bytes(), v.output) {
			t.Fatal(e, s.output.Bytes())
		}
	}
}
func FuzzSOCKSDestination(f *testing.F) {
	for _, p := range [][]byte{{}, {5, 1, 0, 5, 1, 0, 1, 127, 0, 0, 1, 1, 187}, {5, 1, 2}, {5, 1, 0, 5, 1, 0, 3, 1, 'a', 0, 80}, {5, 1, 0, 5, 1, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 187}} {
		f.Add(p, uint8(1))
	}
	f.Fuzz(func(t *testing.T, p []byte, fragment uint8) {
		if len(p) > 4096 {
			t.Skip()
		}
		s := &memorySocket{input: bytes.NewReader(p), fragment: int(fragment)%16 + 1}
		a, e := socksDestination(s)
		if len(p)-s.input.Len() > 522 || s.output.Len() > 12 {
			t.Fatal("SOCKS parser bound")
		}
		if e == nil {
			canonical, err := d.Parse(a.Host, a.Port)
			if err != nil || canonical != a || !bytes.Equal(s.output.Bytes(), []byte{5, 0}) {
				t.Fatal("invalid accepted destination", a, err)
			}
		}
	})
}
func FuzzStreamObject(f *testing.F) {
	f.Add([]byte{})
	f.Add(streamEncode(1, []byte("sample"), false))
	f.Add(streamEncode(2, nil, true))
	f.Fuzz(func(t *testing.T, p []byte) {
		if len(p) > maxObject+1 {
			t.Skip()
		}
		seq, data, eof, e := streamDecode(p)
		if e == nil && !bytes.Equal(streamEncode(seq, data, eof), p) {
			t.Fatal("noncanonical object accepted")
		}
	})
}
