package streamlink

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestCreditObservationCoalescingAndAbandonment(t *testing.T) {
	var o creditObservation
	at := time.Unix(100, 0)
	o.consumed(at)
	o.consumed(at.Add(7 * time.Millisecond))
	o.leased(at.Add(10 * time.Millisecond))
	o.leased(at.Add(11 * time.Millisecond)) // FIN-only CREDIT has no ready sample.
	o.consumed(at.Add(12 * time.Millisecond))
	o.leased(at.Add(15 * time.Millisecond))
	o.consumed(at.Add(20 * time.Millisecond))
	o.closed(at.Add(25 * time.Millisecond))
	o.closed(at.Add(time.Second))
	o.waited(7, true)
	o.waited(11, false)
	want := CreditFlowStatus{ReadyLeased: 2, ReadyAbandoned: 1, ReadyLeaseNS: 13000000, ReadyLeaseMaxNS: 10000000, AbandonedNS: 5000000, SentFrames: 3, WaitSlices: 2, QueuedWaitSlices: 1, QueuedWaitNS: 7, AllLeasedWaitNS: 11}
	if o.status != want || !o.ready.IsZero() {
		t.Fatalf("coalesced credit/closure accounting: %+v", o)
	}
}

func TestCreditFlowSeparatesQueueAndPeerWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, _ := New(ctx, Config{Window: 8, MaxBytes: 64})
	b, _ := New(ctx, Config{Window: 8, MaxBytes: 64})
	defer a.Close(nil)
	defer b.Close(nil)
	if n, e := a.Write(ctx, []byte("12345678")); n != 8 || e != nil {
		t.Fatal(n, e)
	}
	blocked := func() {
		t.Helper()
		waitCtx, stop := context.WithTimeout(ctx, 10*time.Millisecond)
		defer stop()
		if n, e := a.Write(waitCtx, []byte("9")); n != 0 || !errors.Is(e, context.DeadlineExceeded) {
			t.Fatal("full window did not block", n, e)
		}
	}
	blocked() // Entire window still queued locally.
	p, _, e := a.Lease(MaxLease)
	if e != nil || b.Feed(p) != nil || a.Commit() != nil {
		t.Fatal("initial delivery", e)
	}
	blocked() // Outer receipt obtained; no consumption CREDIT yet.
	st := a.Status()
	if st.CreditFlow.QueuedWaitNS <= 0 || st.CreditFlow.AllLeasedWaitNS <= 0 || st.CreditWaitNS != st.CreditFlow.QueuedWaitNS+st.CreditFlow.AllLeasedWaitNS || st.Acked != 0 {
		t.Fatal("wait split or credit semantics", st)
	}
	done := make(chan error, 1)
	go func() { done <- b.Consume(ctx, io.Discard, func() error { return nil }) }()
	defer func() { cancel(); <-done }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		changed := b.Changes()
		if b.Status().Consumed == 8 {
			break
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("consumer did not advance")
		}
	}
	p, _, e = b.Lease(MaxLease)
	if e != nil || a.Feed(p) != nil || b.Commit() != nil {
		t.Fatal("consumer credit delivery", e)
	}
	if a.Status().CreditFlow.ReceivedFrames != 1 || a.Status().CreditFlow.AdvancingFrames != 1 || a.Status().Acked != 8 {
		t.Fatal("validated CREDIT not counted", a.Status())
	}
	st = b.Status()
	if st.CreditFlow.SentFrames != 1 || st.CreditFlow.ReadyLeased != 1 || st.CreditFlow.ReadyLeaseNS <= 0 || st.CreditFlow.ReadyLeaseMaxNS != st.CreditFlow.ReadyLeaseNS {
		t.Fatal("consumption to lease accounting", st)
	}
	if n, e := a.Write(ctx, []byte("9")); n != 1 || e != nil {
		t.Fatal("valid credit did not restore window", n, e)
	}
	// A duplicate valid offset is a frame, but not additional consumption.
	if e := a.Feed(frame(Credit, 0, 8, nil)); e != nil || a.Status().CreditFlow.ReceivedFrames != 2 || a.Status().CreditFlow.AdvancingFrames != 1 {
		t.Fatal("duplicate CREDIT classification", e, a.Status())
	}
	if e := a.Feed(frame(Credit, 0, 9, nil)); e == nil || a.Status().CreditFlow.ReceivedFrames != 2 {
		t.Fatal("invalid CREDIT counted as accepted", e, a.Status())
	}
}
