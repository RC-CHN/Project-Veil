package core

import (
	"context"
	"net"
	"testing"
	"time"
	"veil/internal/mux"
)

func pooled(t *testing.T, c net.Conn) *session {
	t.Helper()
	m, e := mux.New(c, mux.Options{Profile: mux.DefaultProfile()})
	if e != nil {
		t.Fatal(e)
	}
	return &session{mux: m, at: time.Now()}
}
func testPool(t *testing.T) *pool {
	t.Helper()
	cfg := ClientConfig{}
	if e := cfg.Config.defaults(); e != nil {
		t.Fatal(e)
	}
	p := newPool(cfg, nil, nil)
	t.Cleanup(p.close)
	return p
}
func TestPoolDiscardsClosedBeforeOpen(t *testing.T) {
	a, b := net.Pipe()
	dead := pooled(t, a)
	b.Close()
	deadline := time.Now().Add(time.Second)
	for dead.mux.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("EOF not observed")
		}
		time.Sleep(time.Millisecond)
	}
	hot, peer := net.Pipe()
	defer peer.Close()
	live := pooled(t, hot)
	p := testPool(t)
	p.all[dead] = true
	p.all[live] = true
	p.total = 2
	got, e := p.get(context.Background())
	if e != nil || got != live || p.total != 1 {
		t.Fatalf("session=%p err=%v total=%d", got, e, p.total)
	}
}

type waitingClose struct {
	net.Conn
	entered, release chan struct{}
}

func (c waitingClose) Close() error { close(c.entered); <-c.release; return c.Conn.Close() }
func TestPoolDoesNotHoldLockDuringClose(t *testing.T) {
	a, b := net.Pipe()
	wrapped := waitingClose{a, make(chan struct{}), make(chan struct{})}
	s := pooled(t, wrapped)
	p := testPool(t)
	p.all[s] = true
	p.total = 1
	b.Close()
	<-wrapped.entered
	done := make(chan error, 1)
	go func() { _, e := p.get(context.Background()); done <- e }()
	// get must remove the closed session before waiting for its reader to join.
	deadline := time.Now().Add(time.Second)
	for {
		if !p.mu.TryLock() {
			if time.Now().After(deadline) {
				close(wrapped.release)
				t.Fatal("close held pool mutex")
			}
			time.Sleep(time.Millisecond)
			continue
		}
		removed := p.total == 0
		if removed {
			p.closed = true
		}
		p.mu.Unlock()
		if removed {
			break
		}
		if time.Now().After(deadline) {
			close(wrapped.release)
			t.Fatal("closed session was not removed")
		}
		time.Sleep(time.Millisecond)
	}
	close(wrapped.release)
	if e := <-done; e != net.ErrClosed {
		t.Fatal(e)
	}
}
