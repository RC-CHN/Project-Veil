package session

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"
	b "veil.local/core/internal/behavior"
)

type notifyClosed struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *notifyClosed) Close() error {
	e := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return e
}
func waitAdmission(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("admission lifecycle timeout")
	}
}
func TestPermitWaitsForHTTPAndObserverCleanup(t *testing.T) {
	raw := &admissionListener{inputs: make(chan net.Conn, 16), done: make(chan struct{})}
	defer raw.Close()
	rejected := make(chan struct{}, 16)
	listener := &cappedListener{Listener: raw, slots: make(chan struct{}, 1), reject: func() { rejected <- struct{}{} }}
	a, peer := net.Pipe()
	defer peer.Close()
	tracked := &notifyClosed{Conn: a, closed: make(chan struct{})}
	raw.inputs <- tracked
	accepted, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	conn := tls.Server(accepted, &tls.Config{})
	program, e := b.CompileBatch(adaptiveModel())
	if e != nil {
		t.Fatal(e)
	}
	entered, allow := make(chan struct{}), make(chan struct{})
	s := &Server{program: program, entries: make(map[net.Conn]*serverConnection), observer: func(SessionEvent) { close(entered); <-allow }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entry := &serverConnection{server: s, conn: conn, ctx: ctx, cancel: cancel, httpDone: make(chan struct{}), started: time.Now().UnixNano()}
	s.entries[conn] = entry
	s.stats.openConnection()
	s.workers.Add(1)
	defer func() {
		entry.httpOnce.Do(func() { close(entry.httpDone) })
		select {
		case <-allow:
		default:
			close(allow)
		}
		s.workers.Wait()
	}()
	entry.stop()
	waitAdmission(t, tracked.closed)
	if len(listener.slots) != 1 {
		t.Fatal("permit released before HTTP completion")
	}
	entry.httpOnce.Do(func() { close(entry.httpDone) })
	waitAdmission(t, entered)
	if len(listener.slots) != 1 || s.stats.connections.Load() != 1 {
		t.Fatal("permit released before observer returned")
	}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		c, _ := listener.Accept()
		if c != nil {
			c.Close()
			releaseAccepted(c)
		}
	}()
	for i := 0; i < 8; i++ {
		a, p := net.Pipe()
		raw.inputs <- a
		defer p.Close()
		waitAdmission(t, rejected)
	}
	if len(listener.slots) != 1 || s.stats.maxConnections.Load() != 1 {
		t.Fatal("closing worker admission expanded")
	}
	close(allow)
	s.workers.Wait()
	if len(listener.slots) != 0 || s.stats.connections.Load() != 0 {
		t.Fatal("cleanup did not release")
	}
	releaseAccepted(conn)
	if len(listener.slots) != 0 {
		t.Fatal("duplicate release")
	}
	listener.Close()
	waitAdmission(t, acceptDone)
}
