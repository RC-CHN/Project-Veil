package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestBoundsAndTruncation(t *testing.T) {
	for _, h := range [][]byte{{Data, 2, 0, 1}, {Auth, 0, 0, 48}, {Fin, 0, 0, 1}, {255, 0, 0, 0}, {Data, 0, 0, 0}} {
		r := Reader{R: bytes.NewReader(h)}
		if _, _, err := r.Read(); !errors.Is(err, ErrProtocol) {
			t.Fatalf("header %x: %v", h, err)
		}
		if cap(r.buf) != 0 {
			t.Fatal("allocated before header validation")
		}
	}
	r := Reader{R: bytes.NewReader([]byte{Data, 0, 0, 4, 1})}
	if _, _, err := r.Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
}
func TestProofBoundToTLS(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	a := bytes.Repeat([]byte{1}, 32)
	b := bytes.Repeat([]byte{2}, 32)
	p, err := AuthPayload(key, a)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyAuth(key, a, p) || VerifyAuth(key, b, p) {
		t.Fatal("exporter binding failed")
	}
	p[1] ^= 1
	if VerifyAuth(key, a, p) {
		t.Fatal("tampered nonce accepted")
	}
}
func TestAddresses(t *testing.T) {
	for _, a := range []string{"127.0.0.1:443", "[::1]:443", "example.com:443"} {
		p, e := EncodeAddress(a)
		if e != nil {
			t.Fatal(e)
		}
		got, e := DecodeAddress(p)
		if e != nil || got != a {
			t.Fatalf("%s => %s %v", a, got, e)
		}
		if _, e = DecodeAddress(append(p, 1)); e == nil {
			t.Fatal("trailing data accepted")
		}
	}
}
func FuzzRead(f *testing.F) {
	f.Add([]byte{Fin, 0, 0, 0})
	f.Add([]byte{Data, 0, 0, 1, 42})
	f.Fuzz(func(t *testing.T, b []byte) {
		r := Reader{R: bytes.NewReader(b)}
		_, p, _ := r.Read()
		if len(p) > MaxData {
			t.Fatal("unbounded payload")
		}
	})
}
