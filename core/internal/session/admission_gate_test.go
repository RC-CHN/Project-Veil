package session

import (
	"net"
	"sync"
	"testing"
)

type admissionListener struct {
	inputs chan net.Conn
	done   chan struct{}
	once   sync.Once
}

func (l *admissionListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.inputs:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *admissionListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*admissionListener) Addr() net.Addr { return &net.TCPAddr{} }
func TestSocketCloseRetainsWorkAdmission(t *testing.T) {
	raw := &admissionListener{inputs: make(chan net.Conn, 1), done: make(chan struct{})}
	defer raw.Close()
	listener := &cappedListener{Listener: raw, slots: make(chan struct{}, 1)}
	a, b := net.Pipe()
	defer b.Close()
	raw.inputs <- a
	accepted, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer accepted.(*socketCounted).release()
	accepted.Close()
	if len(listener.slots) != 1 {
		t.Fatal("socket close recycled connection work admission")
	}
}
