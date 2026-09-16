package destination

import (
	"errors"
	"sync"
)

var ErrBusy = errors.New("destination capacity")

type Gate struct {
	mu                          sync.Mutex
	global, per, total, maximum int
	counts                      map[string]int
}
type GateStatus struct{ Active, Maximum, Principals int }

func NewGate(global, per int) (*Gate, error) {
	if global < 1 || global > 4096 || per < 1 || per > global {
		return nil, errors.New("admission configuration")
	}
	return &Gate{global: global, per: per, counts: make(map[string]int)}, nil
}
func (g *Gate) Acquire(principal string) (func(), error) {
	if len(principal) < 1 || len(principal) > 128 {
		return nil, errors.New("authenticated identity bound")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.total == g.global || g.counts[principal] == g.per {
		return nil, ErrBusy
	}
	g.total++
	g.counts[principal]++
	g.maximum = max(g.maximum, g.total)
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.total--
			g.counts[principal]--
			if g.counts[principal] == 0 {
				delete(g.counts, principal)
			}
		})
	}, nil
}
func (g *Gate) Status() GateStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return GateStatus{g.total, g.maximum, len(g.counts)}
}
