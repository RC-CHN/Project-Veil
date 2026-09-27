package core

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestIdleConnectionCheck(t *testing.T) {
	for _, outcome := range []string{"alive", "closed", "unexpected data"} {
		t.Run(outcome, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			s := session{Conn: a}
			switch outcome {
			case "closed":
				b.Close()
			case "unexpected data":
				go b.Write([]byte{1})
			}
			if got := s.idleUsable(context.Background()); got != (outcome == "alive") {
				t.Fatalf("usable=%v", got)
			}
			if outcome == "alive" {
				go b.Write([]byte{7})
				var v [1]byte
				if _, err := a.Read(v[:]); err != nil || v[0] != 7 {
					t.Fatal("deadline not cleared", err)
				}
			}
		})
	}
}

func TestPoolDiscardsClosedBeforeOpen(t *testing.T) {
	a, b := net.Pipe()
	b.Close()
	hot, peer := net.Pipe()
	defer peer.Close()
	dead := &session{Conn: a, at: time.Now().Add(-2 * time.Second)}
	live := &session{Conn: hot, at: time.Now()}
	p := &pool{cfg: ClientConfig{Config: Config{PoolTimeout: time.Minute}}, idle: []*session{live, dead}, all: map[*session]bool{live: true, dead: true}, total: 2}
	defer p.close()
	s, err := p.get(context.Background())
	if err != nil || s != live || p.total != 1 {
		t.Fatalf("session=%p err=%v total=%d", s, err, p.total)
	}
}

type waitingClose struct {
	net.Conn
	entered, release chan struct{}
}

func (c waitingClose) Close() error { close(c.entered); <-c.release; return nil }

func TestPoolDoesNotHoldLockDuringClose(t *testing.T) {
	c := waitingClose{entered: make(chan struct{}), release: make(chan struct{})}
	s := &session{Conn: c, at: time.Now().Add(-time.Hour)}
	p := &pool{cfg: ClientConfig{Config: Config{PoolTimeout: time.Second}}, idle: []*session{s}, all: map[*session]bool{s: true}, total: 1}
	done := make(chan error, 1)
	go func() { _, err := p.get(context.Background()); done <- err }()
	<-c.entered
	if !p.mu.TryLock() {
		close(c.release)
		<-done
		t.Fatal("close held pool mutex")
	}
	p.closed = true
	p.mu.Unlock()
	close(c.release)
	if err := <-done; err != net.ErrClosed {
		t.Fatal(err)
	}
}
