package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"
)

type testComponent struct{ run func(context.Context) error }

func (c testComponent) Run(ctx context.Context) error   { return c.run(ctx) }
func (c testComponent) WaitReady(context.Context) error { return nil }
func TestGroupJoinsAfterFailure(t *testing.T) {
	want := errors.New("controlled startup failure")
	joined := make(chan struct{})
	g, e := New(testComponent{func(context.Context) error { return want }}, testComponent{func(ctx context.Context) error { <-ctx.Done(); close(joined); return nil }})
	if e != nil {
		t.Fatal(e)
	}
	if e = g.Run(context.Background()); !errors.Is(e, want) {
		t.Fatal(e)
	}
	select {
	case <-joined:
	default:
		t.Fatal("returned before worker exited")
	}
	if e = g.Run(context.Background()); e == nil {
		t.Fatal("instance ran twice")
	}
}
func TestGateFailureAndWaitCancellation(t *testing.T) {
	g := NewGate()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(g.WaitReady(ctx), context.Canceled) {
		t.Fatal("wait cancellation")
	}
	want := errors.New("startup failed")
	g.Finish(want)
	g.Finish(nil)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !errors.Is(g.WaitReady(ctx), want) {
		t.Fatal("startup result changed")
	}
}
