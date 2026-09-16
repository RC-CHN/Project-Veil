// Package lifecycle joins components and propagates cancellation without
// placing operating-system signal handling in application components.
package lifecycle

import (
	"context"
	"errors"
	"sync"
)

type Runner interface{ Run(context.Context) error }
type Readiness interface{ WaitReady(context.Context) error }
type Component interface {
	Runner
	Readiness
}
type Group struct {
	components []Component
	mu         sync.Mutex
	started    bool
}

func New(components ...Component) (*Group, error) {
	if len(components) == 0 || len(components) > 16 {
		return nil, errors.New("component count")
	}
	for _, c := range components {
		if c == nil {
			return nil, errors.New("nil component")
		}
	}
	return &Group{components: append([]Component(nil), components...)}, nil
}
func (g *Group) Run(parent context.Context) error {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return errors.New("group already run")
	}
	g.started = true
	g.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan error, len(g.components))
	for _, c := range g.components {
		go func(c Component) { done <- c.Run(ctx) }(c)
	}
	var failures []error
	for range g.components {
		e := <-done
		cancel()
		if e != nil && !errors.Is(e, context.Canceled) {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}
func (g *Group) WaitReady(ctx context.Context) error {
	for _, c := range g.components {
		if e := c.WaitReady(ctx); e != nil {
			return e
		}
	}
	return nil
}

type Gate struct {
	once sync.Once
	done chan struct{}
	err  error
}

func NewGate() *Gate             { return &Gate{done: make(chan struct{})} }
func (g *Gate) Finish(err error) { g.once.Do(func() { g.err = err; close(g.done) }) }
func (g *Gate) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.done:
		return g.err
	}
}
