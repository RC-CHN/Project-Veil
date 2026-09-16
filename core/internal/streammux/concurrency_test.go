package streammux

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	so "veil.local/core/internal/streamopen"
)

func TestNormalEndCannotBeRewrittenByLateCancellation(t *testing.T) {
	c, s := pair(t, 1)
	a, b := openPair(t, c, s, so.OK)
	var workers sync.WaitGroup
	workers.Add(2)
	for _, v := range []*Stream{a, b} {
		go func(v *Stream) {
			defer workers.Done()
			_ = v.Link().Consume(v.Context(), io.Discard, func() error { return nil })
		}(v)
		if e := v.Link().Finish(); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() { c.Close(nil); s.Close(nil); workers.Wait() })
	reset := false
	deadline := time.Now().Add(2 * time.Second)
	for !a.Status().EndAcked || !b.Status().EndAcked {
		if time.Now().After(deadline) {
			t.Fatal("normal finish timeout", a.Status(), b.Status())
		}
		p, _, e := c.Lease(65536)
		if e != nil {
			t.Fatal(e)
		}
		if a.Status().EndSent && !reset {
			reset = true
			if e = a.Reset(EndCancelled); e != nil {
				t.Fatal(e)
			}
		}
		if e = s.Feed(p); e != nil {
			t.Fatal(e)
		}
		if e = c.Commit(); e != nil {
			t.Fatal(e)
		}
		xfer(t, s, c)
		runtime.Gosched()
	}
	workers.Wait()
	if !reset || a.Status().EndCode != EndNormal || a.Status().Error != "" {
		t.Fatal("normal terminal rewritten", a.Status())
	}
	a.Release()
	b.Release()
}
func TestAcceptShutdownOwnershipRace(t *testing.T) {
	for i := 0; i < 32; i++ {
		c, s := pair(t, 1)
		a, e := c.Open(request())
		if e != nil {
			t.Fatal(e)
		}
		xfer(t, c, s)
		start := make(chan struct{})
		accepted := make(chan *Stream, 1)
		stopped := make(chan struct{})
		go func() { <-start; v, _ := s.Accept(context.Background()); accepted <- v }()
		go func() { <-start; s.Close(errors.New("shutdown race")); close(stopped) }()
		close(start)
		v := <-accepted
		<-stopped
		if v != nil {
			if s.Status().Active != 1 {
				t.Fatal("handed-off work lost")
			}
			v.Release()
		}
		if s.Status().Active != 0 {
			t.Fatal("unclaimed work leaked")
		}
		a.Reset(EndCancelled)
		a.Release()
		c.Close(nil)
	}
}
func TestWaitCombinesLinkReadinessAndCancellation(t *testing.T) {
	c, s := pair(t, 1)
	a, _ := openPair(t, c, s, so.OK)
	done := make(chan error, 1)
	go func() { done <- c.Wait(context.Background(), time.Second) }()
	if _, e := a.Link().Write(context.Background(), []byte{1}); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lost readiness wakeup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := c.Wait(ctx, time.Second); !errors.Is(e, context.Canceled) {
		t.Fatal("ignored wait cancellation", e)
	}
}
