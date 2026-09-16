package streamlink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestInFlightReceiptsDoNotReleaseConsumerCredit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, e := NewWithPending(ctx, Config{Window: 64, MaxBytes: 1024}, 4)
	if e != nil {
		t.Fatal(e)
	}
	s, _ := New(ctx, Config{Window: 64, MaxBytes: 1024})
	t.Cleanup(func() { c.Close(nil); s.Close(nil) })
	c.Write(ctx, bytes.Repeat([]byte{1}, 64))
	var ids []uint64
	for i := 0; i < 4; i++ {
		id, p, _, e := c.LeaseNext(Header + 16)
		if e != nil || s.Feed(p) != nil {
			t.Fatal("issue/delivery", e)
		}
		ids = append(ids, id)
	}
	before := c.Status()
	if _, _, _, e := c.LeaseNext(Header + 16); !errors.Is(e, ErrLeaseBusy) || c.Status() != before {
		t.Fatal("full window changed link", e)
	}
	for i := 3; i > 0; i-- {
		if e := c.AckLease(ids[i]); e != nil {
			t.Fatal(e)
		}
	}
	if st := c.Status(); st.PendingLeases != 4 || st.LeaseCommitted != 0 || st.PendingBytes != 4*(Header+16) {
		t.Fatal("receipt crossed gap", st)
	}
	if e := c.AckLease(ids[0]); e != nil {
		t.Fatal(e)
	}
	if st := c.Status(); st.Pending || st.PendingBytes != 0 || st.LeaseCommitted != 4 || st.Acked != 0 {
		t.Fatal("outer receipt fabricated credit", st)
	}
	done := make(chan error, 1)
	go func() { _, e := c.Write(ctx, []byte{2}); done <- e }()
	select {
	case e := <-done:
		t.Fatal("producer bypassed consumption credit", e)
	case <-time.After(10 * time.Millisecond):
	}
	consumer := make(chan error, 1)
	go func() { consumer <- s.Consume(ctx, io.Discard, func() error { return nil }) }()
	t.Cleanup(func() { cancel(); <-consumer })
	deadline := time.After(time.Second)
	for {
		ch := s.Changes()
		if s.Status().Consumed == 64 {
			break
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatal("consumer did not progress")
		}
	}
	p, _, e := s.Lease(MaxLease)
	if e != nil || c.Feed(p) != nil || s.Commit() != nil {
		t.Fatal("credit delivery", e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("credit did not wake producer")
	}
	if st := c.Status(); st.Acked != 64 || st.MaxSendOutstanding > 64 {
		t.Fatal("consumer window changed", st)
	}
}

func TestNumberedReceiptFailuresAndLegacyIsolation(t *testing.T) {
	for _, mode := range []string{"duplicate", "unknown", "legacy-commit", "legacy-lease"} {
		t.Run(mode, func(t *testing.T) {
			l, _ := NewWithPending(context.Background(), Config{Window: 64, MaxBytes: 128}, 4)
			defer l.Close(nil)
			a, _, _, _ := l.LeaseNext(64)
			b, _, _, _ := l.LeaseNext(64)
			var e error
			switch mode {
			case "duplicate":
				l.AckLease(b)
				e = l.AckLease(b)
			case "unknown":
				e = l.AckLease(b + 1)
			case "legacy-commit":
				e = l.Commit()
			case "legacy-lease":
				_, _, e = l.Lease(64)
			}
			if a == 0 || e == nil || !l.Status().Closed || l.Status().PendingLeases != 0 {
				t.Fatal("invalid receipt/API mix accepted", e)
			}
		})
	}
}
