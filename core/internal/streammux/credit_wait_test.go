package streammux

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	so "veil.local/core/internal/streamopen"
)

func creditOnlyPair(t *testing.T) (*Session, *Session, *Stream, *Stream) {
	t.Helper()
	c, s := pair(t, 1)
	a, b := openPair(t, c, s, so.OK)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Link().Consume(ctx, io.Discard, func() error { return nil }) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, e := b.Link().Write(ctx, []byte{1}); e != nil {
		t.Fatal(e)
	}
	xfer(t, s, c)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		changed := a.Link().Changes()
		if a.Link().Status().Consumed == 1 {
			break
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatal("consumer did not produce credit")
		}
	}
	return c, s, a, b
}

func TestCreditOnlyWaitCombinesFollowingData(t *testing.T) {
	c, s, a, b := creditOnlyPair(t)
	done := make(chan error, 1)
	go func() { done <- c.WaitCoalesced(context.Background(), time.Second, 500*time.Millisecond) }()
	select {
	case e := <-done:
		t.Fatal("credit-only output escaped before data", e)
	case <-time.After(10 * time.Millisecond):
	}
	if _, e := a.Link().Write(context.Background(), []byte{2}); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("DATA did not interrupt credit delay")
	}
	xfer(t, c, s)
	if b.Link().Status().Acked != 1 || b.Link().Status().Received != 1 {
		t.Fatal("credit and data were not delivered by one lease", b.Link().Status())
	}
}

func TestCreditDelayHonorsDeadlineCancellationAndFIN(t *testing.T) {
	t.Run("overall deadline", func(t *testing.T) {
		c, _, _, _ := creditOnlyPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if e := c.WaitCoalesced(ctx, 10*time.Millisecond, time.Second); e != nil {
			t.Fatal("credit timer extended original wait deadline", e)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		c, _, _, _ := creditOnlyPair(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- c.WaitCoalesced(ctx, time.Second, time.Second) }()
		select {
		case e := <-done:
			t.Fatal("credit wait ended before cancellation", e)
		case <-time.After(10 * time.Millisecond):
		}
		cancel()
		select {
		case e := <-done:
			if !errors.Is(e, context.Canceled) {
				t.Fatal("credit coalescing ignored cancellation", e)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatal("credit coalescing did not wake on cancellation")
		}
	})
	t.Run("FIN", func(t *testing.T) {
		c, _, a, _ := creditOnlyPair(t)
		if e := a.Link().Finish(); e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if e := c.WaitCoalesced(ctx, time.Second, time.Second); e != nil {
			t.Fatal("FIN was delayed behind credit timer", e)
		}
	})
}
