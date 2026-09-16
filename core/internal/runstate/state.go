package runstate

import (
	"context"
	"errors"
	"sync"
)

var ErrAlreadyRun = errors.New("instance can only run once")
var ErrStopped = errors.New("instance stopped before ready")

type State struct {
	mu                       sync.Mutex
	started, ready, finished bool
	err                      error
	done                     chan struct{}
}

func New() *State { return &State{done: make(chan struct{})} }
func (s *State) Begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return ErrAlreadyRun
	}
	s.started = true
	return nil
}
func (s *State) Ready() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready && !s.finished {
		s.ready = true
		close(s.done)
	}
}
func (s *State) Finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	s.err = err
	if !s.ready {
		if s.err == nil {
			s.err = ErrStopped
		}
		close(s.done)
	}
}
func (s *State) WaitReady(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil readiness context")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	return s.err
}
func (s *State) Current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		if s.err != nil {
			return "failed"
		}
		return "stopped"
	}
	if s.ready {
		return "ready"
	}
	if s.started {
		return "starting"
	}
	return "created"
}
