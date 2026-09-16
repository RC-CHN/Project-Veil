package streamopen

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	d "veil.local/core/internal/destination"
)

func example() Request {
	return Request{Address: d.Address{Host: "owned.test", Port: 443}, Limits: Limits{65536, 8 << 20}}
}
func TestPairedOpenAndBoundedResult(t *testing.T) {
	client, _ := NewClient(example())
	server, _ := NewServer(Limits{32768, 4 << 20})
	defer client.Close(nil)
	defer server.Close(nil)
	p, e := client.TakePrefix(4096)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range p {
		rest, e := server.Feed([]byte{v})
		if e != nil || len(rest) != 0 {
			t.Fatal(e)
		}
	}
	r, e := server.WaitRequest(context.Background())
	if e != nil || r != example() {
		t.Fatal(r, e)
	}
	if e = server.Respond(Result{OK, Limits{32768, 4 << 20}}); e != nil {
		t.Fatal(e)
	}
	p, e = server.TakePrefix(4096)
	if e != nil {
		t.Fatal(e)
	}
	data := []byte{1, 2, 3}
	p = append(p, data...)
	rest, e := client.Feed(p)
	if e != nil || !bytes.Equal(rest, data) {
		t.Fatal(rest, e)
	}
	got, e := client.WaitResult(context.Background())
	if e != nil || got.Limits.Window != 32768 || !client.Status().Established || !server.Status().Established {
		t.Fatal(got, e)
	}
}
func TestRejectCanFinishOuter(t *testing.T) {
	c, _ := NewClient(example())
	s, _ := NewServer(example().Limits)
	p, _ := c.TakePrefix(4096)
	s.Feed(p)
	if e := s.Respond(Result{Code: Denied}); e != nil {
		t.Fatal(e)
	}
	p, _ = s.TakePrefix(4096)
	if _, e := c.Feed(p); e != nil {
		t.Fatal(e)
	}
	r, e := c.WaitResult(context.Background())
	var reject Rejection
	if !errors.As(e, &reject) || r.Code != Denied {
		t.Fatal(r, e)
	}
	for _, h := range []*Handshake{c, s} {
		if e := h.PeerDone(); e != nil {
			t.Fatal(e)
		}
		if h.Status().Established || !h.Status().Rejected {
			t.Fatal(h.Status())
		}
		h.Close(nil)
	}
}
func TestMalformedAndEarlyData(t *testing.T) {
	for _, name := range []string{"version", "reserved", "oversize", "network", "zero_port", "window", "uppercase", "early_data", "duplicate_open", "partial_eof"} {
		t.Run(name, func(t *testing.T) {
			s, _ := NewServer(example().Limits)
			defer s.Close(nil)
			p, _ := EncodeRequest(example())
			switch name {
			case "version":
				p[0] = 2
			case "reserved":
				p[2] = 1
			case "oversize":
				binary.BigEndian.PutUint32(p[4:8], MaxBody+1)
			case "network":
				p[8] = 2
			case "zero_port":
				clear(p[10:12])
			case "window":
				clear(p[12:16])
			case "uppercase":
				p[24] = 'O'
			case "early_data":
				p = append(p, 1)
			case "duplicate_open":
				if _, e := s.Feed(p); e != nil {
					t.Fatal(e)
				}
			case "partial_eof":
				s.Feed(p[:9])
				if e := s.PeerDone(); e == nil {
					t.Fatal("truncated accepted")
				}
				return
			}
			if _, e := s.Feed(p); e == nil || !s.Status().Closed {
				t.Fatal(name, e)
			}
		})
	}
	c, _ := NewClient(example())
	p, _ := EncodeResult(Result{OK, example().Limits})
	if _, e := c.Feed(p); e == nil {
		t.Fatal("unsolicited result")
	}
	c, _ = NewClient(example())
	c.TakePrefix(4096)
	p, _ = EncodeResult(Result{OK, Limits{131072, 8 << 20}})
	if _, e := c.Feed(p); e == nil {
		t.Fatal("expanded result")
	}
}
func TestWaitingCancellationAndCleanup(t *testing.T) {
	s, _ := NewServer(example().Limits)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.WaitRequest(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	p, _ := EncodeRequest(example())
	s.Feed(p[:12])
	s.Close(context.Canceled)
	st := s.Status()
	if !st.Closed || st.ParserBytes != 0 || st.QueuedBytes != 0 {
		t.Fatal(st)
	}
}
