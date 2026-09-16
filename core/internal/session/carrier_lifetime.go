package session

import (
	"context"
	"sync"
	"time"
)

const carrierSetupLimit = 8 * time.Second

// A renewing carrier has a bounded setup phase starting before dialing or at
// accept, then follows its finite child generations and unchanged stream/idle
// budgets. Legacy carriers retain their original absolute lifetime.
type carrierLifetime struct {
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	ready       bool
	deadline    time.Time
	established chan struct{}
}

func newCarrierLifetime(parent context.Context, program *flightProgram) *carrierLifetime {
	if !program.renewable() {
		ctx, cancel := context.WithTimeout(parent, 610*time.Second)
		return &carrierLifetime{ctx: ctx, cancel: cancel}
	}
	return newRenewingCarrierLifetime(parent, carrierSetupLimit)
}

func newRenewingCarrierLifetime(parent context.Context, setup time.Duration) *carrierLifetime {
	ctx, cancel := context.WithCancel(parent)
	l := &carrierLifetime{ctx: ctx, deadline: time.Now().Add(setup), established: make(chan struct{})}
	done := make(chan struct{})
	l.cancel = func() { cancel(); <-done }
	go func() {
		defer close(done)
		timer := time.NewTimer(time.Until(l.deadline))
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-l.established:
		case <-timer.C:
			l.mu.Lock()
			if !l.ready {
				cancel()
			}
			l.mu.Unlock()
		}
	}()
	return l
}

// Readiness cannot revive an expired setup even if timer delivery is delayed.
// This method only closes the establishment channel; cancel joins the watcher.
func (l *carrierLifetime) establish() bool {
	if l == nil || l.established == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx.Err() != nil {
		return false
	}
	if l.ready {
		return true
	}
	if !time.Now().Before(l.deadline) {
		return false
	}
	l.ready = true
	close(l.established)
	return true
}
