package streammux

import (
	"bytes"
	"context"
	"testing"
	"time"

	fw "veil.local/core/internal/flightwindow"
	so "veil.local/core/internal/streamopen"
)

func TestReorderedFlightDeliversMuxBytesBeforeReceipts(t *testing.T) {
	c, s := flightPair(t)
	a, b := openPair(t, c, s, so.OK)
	payload := make([]byte, 1536)
	for i := range payload {
		payload[i] = byte(i*131 + 17)
	}
	if _, e := a.Link().Write(context.Background(), payload); e != nil {
		t.Fatal(e)
	}
	a.Link().Finish()
	var frames [4]fw.Frame
	for i := range frames {
		id, p, eof, e := c.LeaseNext(512)
		if e != nil {
			t.Fatal(e)
		}
		// Existing SETTINGS/OPEN exchanges consumed legacy IDs. A production
		// coordinator starts numbered transport at construction, never rebases
		// IDs. This test's receiver starts at the already-delivered prefix.
		frames[i] = fw.Frame{ID: id, Body: p, EOF: eof}
	}
	r, _ := fw.NewReceiver(4)
	for id := uint64(1); id < frames[0].ID; id++ {
		if e := r.Put(id, nil, false); e != nil {
			t.Fatal(e)
		}
		r.Pop()
	}
	for _, i := range []int{3, 1, 2} {
		if e := r.Put(frames[i].ID, frames[i].Body, frames[i].EOF); e != nil {
			t.Fatal(e)
		}
		if _, ok := r.Pop(); ok {
			t.Fatal("receive order crossed gap")
		}
	}
	if b.Link().Status().Received != 0 || c.Status().PendingLeases != 4 {
		t.Fatal("buffered completions advanced inner state")
	}
	if e := r.Put(frames[0].ID, frames[0].Body, frames[0].EOF); e != nil {
		t.Fatal(e)
	}
	for range frames {
		frame, ok := r.Pop()
		if !ok {
			t.Fatal("complete prefix missing")
		}
		if e := s.Feed(frame.Body); e != nil {
			t.Fatal(e)
		}
		if e := c.AckLease(frame.ID); e != nil {
			t.Fatal(e)
		}
	}
	if c.Status().PendingLeases != 0 || a.Link().Status().Acked != 0 {
		t.Fatal("outer receipt released application credit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var output bytes.Buffer
	if e := b.Link().Consume(ctx, &output, func() error { return nil }); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(output.Bytes(), payload) {
		t.Fatal("reordered transport corrupted stream")
	}
}
