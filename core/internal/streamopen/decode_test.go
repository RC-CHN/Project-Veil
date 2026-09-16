package streamopen

import (
	"bytes"
	"testing"
)

func TestCompletePrefixesAreStrict(t *testing.T) {
	r := Request{Network: NetworkUDP, Limits: Limits{Window: 16384, MaxBytes: 65536}}
	p, e := EncodeRequest(r)
	if e != nil {
		t.Fatal(e)
	}
	got, e := DecodeRequest(p)
	if e != nil || got != r {
		t.Fatalf("%+v %v", got, e)
	}
	result := Result{Code: OK, Limits: r.Limits}
	q, e := EncodeResult(result)
	if e != nil {
		t.Fatal(e)
	}
	g, e := DecodeResult(q)
	if e != nil || g != result {
		t.Fatalf("%+v %v", g, e)
	}
	for _, v := range [][]byte{p, q} {
		for i := 0; i < len(v); i++ {
			if _, e := DecodeRequest(v[:i]); e == nil {
				t.Fatal("partial request")
			}
			if _, e := DecodeResult(v[:i]); e == nil {
				t.Fatal("partial result")
			}
		}
		for _, bad := range [][]byte{append(bytes.Clone(v), 0), append(bytes.Clone(v), v...)} {
			if _, e := DecodeRequest(bad); e == nil {
				t.Fatal("request trailing bytes")
			}
			if _, e := DecodeResult(bad); e == nil {
				t.Fatal("result trailing bytes")
			}
		}
	}
}
func FuzzCompletePrefixes(f *testing.F) {
	p, _ := EncodeRequest(Request{Network: NetworkUDP, Limits: Limits{16384, 65536}})
	q, _ := EncodeResult(Result{Code: Busy})
	f.Add(p)
	f.Add(q)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, p []byte) {
		if len(p) > 4096 {
			return
		}
		if r, e := DecodeRequest(p); e == nil {
			out, e := EncodeRequest(r)
			if e != nil || !bytes.Equal(p, out) {
				t.Fatal("noncanonical OPEN")
			}
		}
		if r, e := DecodeResult(p); e == nil {
			out, e := EncodeResult(r)
			if e != nil || !bytes.Equal(p, out) {
				t.Fatal("noncanonical RESULT")
			}
		}
	})
}
