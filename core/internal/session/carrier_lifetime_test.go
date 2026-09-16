package session

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCarrierLifetimeLegacyDeadlineIsolation(t *testing.T) {
	for _, version := range []int{0, 1, 2, 3, 4} {
		var p *flightProgram
		if version != 0 {
			p = &flightProgram{version: version}
		}
		started := time.Now()
		l := newCarrierLifetime(context.Background(), p)
		deadline, ok := l.ctx.Deadline()
		if version == 4 {
			if ok || l.deadline.Sub(started) < carrierSetupLimit || l.deadline.Sub(started) > carrierSetupLimit+time.Second {
				t.Fatal("renewing setup bound or absolute deadline", ok, l.deadline)
			}
		} else if !ok || deadline.Sub(started) < 610*time.Second || deadline.Sub(started) > 611*time.Second {
			t.Fatal("legacy lifetime changed", version, deadline)
		}
		l.cancel()
		if l.ctx.Err() != context.Canceled {
			t.Fatal(l.ctx.Err())
		}
	}
}

func TestCarrierLifetimeUnreadyExpiresAndCannotRevive(t *testing.T) {
	for _, limit := range []time.Duration{0, 20 * time.Millisecond} {
		l := newRenewingCarrierLifetime(context.Background(), limit)
		select {
		case <-l.ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("unready carrier held resources past setup")
		}
		if l.establish() {
			t.Fatal("expired establishment revived")
		}
		l.cancel()
	}
}

func TestCarrierLifetimeReadySurvivesSetupButHonorsParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newRenewingCarrierLifetime(parent, 30*time.Millisecond)
	defer l.cancel()
	if !l.establish() {
		t.Fatal("valid ready transition rejected")
	}
	select {
	case <-l.ctx.Done():
		t.Fatal("setup deadline truncated ready carrier")
	case <-time.After(60 * time.Millisecond):
	}
	if !l.establish() {
		t.Fatal("repeated readiness extended or lost setup state")
	}
	cancel()
	if l.ctx.Err() == nil || l.establish() {
		t.Fatal("parent cancellation lost")
	}
	parentDeadline, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	inherited := newRenewingCarrierLifetime(parentDeadline, carrierSetupLimit)
	defer inherited.cancel()
	want, _ := parentDeadline.Deadline()
	if got, ok := inherited.ctx.Deadline(); !ok || !got.Equal(want) {
		t.Fatal("external deadline removed")
	}
}

func TestCarrierLifetimeConcurrentReadyAndCancelJoin(t *testing.T) {
	for i := 0; i < 20; i++ {
		l := newRenewingCarrierLifetime(context.Background(), carrierSetupLimit)
		var workers sync.WaitGroup
		workers.Add(2)
		go func() { defer workers.Done(); l.establish() }()
		go func() { defer workers.Done(); l.cancel() }()
		workers.Wait()
		if l.ctx.Err() == nil || l.establish() {
			t.Fatal("cancelled carrier became ready")
		}
	}
}
