package streammux

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOutputCoalescingCollectsDataUntilFIN(t *testing.T) {
	c, s, a, b := creditOnlyPair(t)
	if _, e := a.Link().Write(context.Background(), []byte{2}); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- c.WaitCoalescedOutput(ctx, time.Second, 500*time.Millisecond) }()
	for i := 0; i < 2; i++ {
		select {
		case e := <-done:
			t.Fatal("DATA escaped bounded coalescing", e)
		case <-time.After(10 * time.Millisecond):
		}
		if i == 0 {
			if _, e := a.Link().Write(ctx, []byte{3}); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := a.Link().Finish(); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("FIN was delayed by DATA timer")
	}
	xfer(t, c, s)
	if st := b.Link().Status(); st.Received != 2 || st.Acked != 1 || !st.RemoteFIN {
		t.Fatal("combined CREDIT/DATA/FIN changed bytes or credit", st)
	}
}

func TestOutputCoalescingBoundAndCancel(t *testing.T) {
	for _, mode := range []string{"budget", "cancel", "END"} {
		t.Run(mode, func(t *testing.T) {
			c, _, a, _ := creditOnlyPair(t)
			if _, e := a.Link().Write(context.Background(), []byte{2}); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if mode == "budget" {
				if e := c.WaitCoalescedOutput(ctx, 10*time.Millisecond, time.Second); e != nil {
					t.Fatal("DATA delay extended original budget", e)
				}
				return
			}
			if mode == "END" {
				a.Reset(EndCancelled)
				if e := c.WaitCoalescedOutput(ctx, time.Second, time.Second); e != nil {
					t.Fatal("END was delayed by DATA timer", e)
				}
				return
			}
			cancel()
			if e := c.WaitCoalescedOutput(ctx, time.Second, time.Second); !errors.Is(e, context.Canceled) {
				t.Fatal("DATA wait ignored cancellation", e)
			}
		})
	}
}
