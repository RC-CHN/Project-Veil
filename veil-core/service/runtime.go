package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"veil/core"
	"veil/inbound"
)

// Runtime serializes application lifecycle operations. Its zero value is ready
// for use. It adds no work to the relay path. Close is terminal; Stop is not.
type Runtime struct {
	mu     sync.Mutex
	run    *running
	closed bool
}

type running struct {
	svc                 *Service
	listen              string
	role                string
	cancel              context.CancelFunc
	done                chan struct{}
	err                 error // published by closing done
	lastConnectionError atomic.Pointer[string]
}

type Counters struct {
	Accepted      uint64 `json:"accepted"`
	Rejected      uint64 `json:"rejected"`
	Completed     uint64 `json:"completed"`
	Failed        uint64 `json:"failed"`
	Authenticated uint64 `json:"authenticated"`
}

type Snapshot struct {
	State               string   `json:"state"`
	Listen              string   `json:"listen,omitempty"`
	Role                string   `json:"role,omitempty"`
	Stats               Counters `json:"stats"`
	Error               string   `json:"error,omitempty"`
	LastConnectionError string   `json:"last_connection_error,omitempty"`
}

func (r *Runtime) active() bool {
	if r.run == nil {
		return false
	}
	select {
	case <-r.run.done:
		return false
	default:
		return true
	}
}

// Start is idempotent while running; a new configuration requires Restart.
func (r *Runtime) Start(cfg Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	if r.active() {
		return nil
	}
	s, err := New(cfg)
	if err != nil {
		return err
	}
	return r.start(s)
}

func (r *Runtime) start(s *Service) error {
	ln, err := inbound.Listen(s.cfg.Listen)
	if err != nil {
		s.Close()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	x := &running{svc: s, listen: ln.Addr().String(), role: s.cfg.Role, cancel: cancel, done: make(chan struct{})}
	s.OnError = func(err error) {
		var op *core.OpError
		if errors.As(err, &op) {
			message := err.Error()
			x.lastConnectionError.Store(&message)
		}
	}
	r.run = x
	go func() {
		defer close(x.done)
		defer cancel()
		x.err = s.Serve(ctx, ln)
	}()
	return nil
}

func (r *Runtime) stop() {
	if r.run != nil {
		r.run.cancel()
		<-r.run.done
	}
}

// Stop closes the listener and all active streams, then joins the handlers.
func (r *Runtime) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stop()
}

// Restart validates transport settings before interrupting the old instance.
// Binding can still fail after Stop (e.g. a port conflict); then it stays stopped.
func (r *Runtime) Restart(cfg Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	s, err := New(cfg)
	if err != nil {
		return err
	}
	r.stop()
	return r.start(s)
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.stop()
	return nil
}

func (r *Runtime) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{State: "stopped"}
	if x := r.run; x != nil {
		s.Listen, s.Role = x.listen, x.role
		if r.active() {
			s.State = "running"
		} else if x.err != nil {
			s.Error = x.err.Error()
		}
		c := x.svc.Stats
		if message := x.lastConnectionError.Load(); message != nil {
			s.LastConnectionError = *message
		}
		s.Stats = Counters{c.Accepted.Load(), c.Rejected.Load(), c.Completed.Load(), c.Failed.Load(), c.Authenticated.Load()}
	}
	return s
}
