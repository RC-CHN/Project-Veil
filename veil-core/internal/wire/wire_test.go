package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestBoundsAndTruncation(t *testing.T) {
	for _, h := range [][]byte{{Data, 2, 0, 1}, {Auth, 0, 0, 48}, {2, 0, 0, 0}, {Fin, 0, 0, 1}, {255, 0, 0, 0}, {Data, 0, 0, 0}} {
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
	p[0] = 0
	copy(p[17:], proof(key, a, p[:17]))
	if VerifyAuth(key, a, p) {
		t.Fatal("old protocol version accepted")
	}
}

type recordingWriter struct {
	bytes.Buffer
	writes int
	short  bool
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.short {
		return len(p) - 1, nil
	}
	return w.Buffer.Write(p)
}
func TestPipelinedOpen(t *testing.T) {
	auth, err := AuthPayload(make([]byte, 32), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	address, _ := EncodeAddress("127.0.0.1:443")
	for _, first := range []bool{true, false} {
		w := new(recordingWriter)
		var proof []byte
		if first {
			proof = auth
		}
		if err := WriteOpen(w, proof, address); err != nil {
			t.Fatal(err)
		}
		if w.writes != 1 {
			t.Fatalf("separate TLS writes: %d", w.writes)
		}
		r := Reader{R: w}
		if first {
			typ, p, err := r.Read()
			if err != nil || typ != Auth || !bytes.Equal(p, auth) {
				t.Fatal("AUTH not first", err)
			}
		}
		typ, p, err := r.Read()
		if err != nil || typ != Open || !bytes.Equal(p, address) {
			t.Fatal("OPEN lost", err)
		}
		if w.Len() != 0 {
			t.Fatal("unexpected trailing frames")
		}
	}
	w := &recordingWriter{short: true}
	if err := WriteOpen(w, auth, address); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	w = new(recordingWriter)
	if err := WriteOpen(w, auth[:48], address); err == nil || w.writes != 0 {
		t.Fatal("invalid AUTH emitted")
	}
}

func TestFullDataFrameFitsEightTLSRecords(t *testing.T) {
	w := new(recordingWriter)
	b := make([]byte, HeaderSize+MaxData)
	if err := WriteBuffer(w, Data, b, MaxData); err != nil {
		t.Fatal(err)
	}
	if w.Len() != 8*16384 {
		t.Fatalf("frame leaves a TLS tail: %d", w.Len())
	}
	r := Reader{R: w}
	typ, p, err := r.Read()
	if err != nil || typ != Data || len(p) != MaxData {
		t.Fatal("max frame rejected", err)
	}
	r = Reader{R: bytes.NewReader([]byte{Data, 2, 0, 0})}
	if _, _, err := r.Read(); !errors.Is(err, ErrProtocol) {
		t.Fatal("old oversized frame accepted", err)
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
