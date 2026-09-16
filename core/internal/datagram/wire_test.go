package datagram

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"
	d "veil.local/core/internal/destination"
)

func TestPacketFragmentationAndEmpty(t *testing.T) {
	var stream []byte
	var want []Packet
	for _, v := range []struct {
		host string
		n    int
	}{{"127.0.0.2", 0}, {"2001:db8::1", 1}, {"owned.test", 1200}, {"owned.test", MaxPayload}} {
		p := Packet{d.Address{Host: v.host, Port: 53}, bytes.Repeat([]byte{7}, v.n)}
		raw, e := Encode(p)
		if e != nil {
			t.Fatal(e)
		}
		stream = append(stream, raw...)
		want = append(want, p)
	}
	for _, step := range []int{1, 7, 16, 16384, len(stream)} {
		var decoder Decoder
		var got []Packet
		for at := 0; at < len(stream); {
			n := min(step, len(stream)-at)
			used, e := decoder.Write(stream[at:at+n], func(p Packet) error { p.Payload = append([]byte{}, p.Payload...); got = append(got, p); return nil })
			if e != nil || used != n {
				t.Fatal(e, used)
			}
			at += n
		}
		if e := decoder.Finish(); e != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("packet boundary", e)
		}
	}
}
func TestMalformedRecordsAndSOCKS(t *testing.T) {
	p := Packet{d.Address{Host: "127.0.0.2", Port: 53}, []byte("data")}
	valid, _ := Encode(p)
	for _, at := range []int{0, 1, 2, 3, 4, 8, 9} {
		raw := append([]byte(nil), valid...)
		raw[at] ^= 255
		var dec Decoder
		_, e := dec.Write(raw, func(Packet) error { return nil })
		if e == nil {
			e = dec.Finish()
		}
		if e == nil {
			t.Fatal("bad field accepted", at)
		}
	}
	zeroPort := append([]byte(nil), valid...)
	zeroPort[10] = 0
	zeroPort[11] = 0
	if _, e := Decode(zeroPort); e == nil {
		t.Fatal("zero destination port")
	}
	var partial Decoder
	partial.Write(valid[:len(valid)-1], func(Packet) error { return nil })
	if partial.Finish() == nil {
		t.Fatal("truncated packet")
	}
	raw, _ := SOCKS(p)
	decoded, e := ParseSOCKS(raw)
	if e != nil || !reflect.DeepEqual(decoded, p) {
		t.Fatal(e)
	}
	raw[2] = 1
	if _, e = ParseSOCKS(raw); e == nil {
		t.Fatal("SOCKS fragment accepted")
	}
	if _, e = SOCKS(Packet{p.Address, make([]byte, MaxPayload)}); e == nil {
		t.Fatal("oversized SOCKS envelope")
	}
	if _, e = ParseSOCKS(make([]byte, 65508)); e == nil {
		t.Fatal("oversized local UDP")
	}
}
func TestQueueBoundsAndClose(t *testing.T) {
	q := NewQueue()
	for i := 0; i < QueuePackets; i++ {
		if !q.Push([]byte{byte(i)}) {
			t.Fatal(i)
		}
	}
	if q.Push([]byte{1}) || q.Status().Dropped != 1 {
		t.Fatal("packet cap")
	}
	q.Close(false)
	for i := 0; i < QueuePackets; i++ {
		p, e := q.Pop(context.Background())
		if e != nil || len(p) != 1 || p[0] != byte(i) {
			t.Fatal("queue order", e)
		}
	}
	if _, e := q.Pop(context.Background()); e != io.EOF {
		t.Fatal(e)
	}
	q = NewQueue()
	large := bytes.Repeat([]byte{9}, MaxRecord)
	for q.Push(large) {
	}
	st := q.Status()
	if st.Bytes > QueueBytes || st.Packets != 3 {
		t.Fatal(st)
	}
	q.Close(true)
	if st = q.Status(); st.Bytes != 0 || st.Packets != 0 || !st.Closed {
		t.Fatal(st)
	}
}
func FuzzRecords(f *testing.F) {
	empty, _ := Encode(Packet{Address: d.Address{Host: "127.0.0.2", Port: 53}})
	full, _ := Encode(Packet{Address: d.Address{Host: "owned.test", Port: 443}, Payload: []byte("sample")})
	for _, p := range [][]byte{{}, empty, full, append(append([]byte{}, empty...), full...), {1, 1, 0, 0, 255, 255, 255, 255}} {
		f.Add(p, uint8(7))
	}
	f.Fuzz(func(t *testing.T, p []byte, step uint8) {
		if len(p) > 2*MaxRecord+8 {
			t.Skip()
		}
		var output [2][][]byte
		var errs [2]bool
		for i := 0; i < 2; i++ {
			var decoder Decoder
			size := len(p)
			if i == 1 {
				size = int(step) + 1
			}
			for at := 0; at < len(p); {
				n := min(size, len(p)-at)
				_, e := decoder.Write(p[at:at+n], func(v Packet) error {
					raw, e := Encode(v)
					if e != nil {
						t.Fatal(e)
					}
					output[i] = append(output[i], raw)
					return nil
				})
				at += n
				if e != nil {
					errs[i] = true
					break
				}
			}
			if decoder.Finish() != nil {
				errs[i] = true
			}
			if decoder.Buffered() > MaxRecord {
				t.Fatal("parser bound")
			}
		}
		if errs[0] != errs[1] || !reflect.DeepEqual(output[0], output[1]) {
			t.Fatal("fragment mismatch")
		}
	})
}
func FuzzSOCKSUDP(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 127, 0, 0, 2, 0, 53})
	f.Add([]byte{})
	f.Add([]byte{0, 0, 1, 1, 127, 0, 0, 2, 0, 53})
	f.Fuzz(func(t *testing.T, p []byte) {
		if len(p) > 65508 {
			t.Skip()
		}
		packet, e := ParseSOCKS(p)
		if e == nil {
			raw, e := SOCKS(packet)
			if e != nil {
				t.Fatal(e)
			}
			other, e := ParseSOCKS(raw)
			if e != nil || !reflect.DeepEqual(packet, other) {
				t.Fatal("SOCKS packet canonicalization")
			}
		}
	})
}
