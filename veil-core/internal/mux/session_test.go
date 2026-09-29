package mux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func pair(t *testing.T) (*Session, *Session) {
	t.Helper()
	a, b := net.Pipe()
	c, err := New(a, Options{Profile: DefaultProfile(), WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(b, Options{Server: true, Profile: DefaultProfile(), WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close(); c.Wait(); s.Wait() })
	return c, s
}

func openPair(t *testing.T, c, s *Session) (*Stream, *Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type result struct {
		stream *Stream
		err    error
	}
	ch := make(chan result, 1)
	go func() { st, e := c.Open(ctx, []byte("owned test target")); ch <- result{st, e} }()
	server, e := s.Accept(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = server.Respond(0); e != nil {
		t.Fatal(e)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { r.stream.Close(); server.Close() })
	return r.stream, server
}

func TestConcurrentHalfClose(t *testing.T) {
	c, s := pair(t)
	var wg sync.WaitGroup
	for i := range MaxStreams {
		a, b := openPair(t, c, s)
		payload := bytes.Repeat([]byte{byte(i)}, 3*blockSize+i+1)
		wg.Go(func() {
			defer b.Close()
			p, e := io.ReadAll(b)
			if e != nil || !bytes.Equal(p, payload) {
				t.Errorf("server receive: %d %v", len(p), e)
				return
			}
			if _, e = b.Write(p); e != nil {
				t.Error(e)
			}
			if e = b.CloseWrite(); e != nil {
				t.Error(e)
			}
			if e = b.Finish(); e != nil {
				t.Error(e)
			}
		})
		wg.Go(func() {
			defer a.Close()
			if _, e := a.Write(payload); e != nil {
				t.Error(e)
				return
			}
			if e := a.CloseWrite(); e != nil {
				t.Error(e)
				return
			}
			p, e := io.ReadAll(a)
			if e != nil || !bytes.Equal(p, payload) {
				t.Errorf("client receive: %d %v", len(p), e)
			}
			if e = a.WaitDone(context.Background()); e != nil {
				t.Error(e)
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("half-close stalled")
	}
}

func TestSlowReaderAndResetAreIsolated(t *testing.T) {
	c, s := pair(t)
	blocked, slow := openPair(t, c, s)
	finished := make(chan error, 1)
	go func() { _, e := blocked.Write(make([]byte, (windowBlocks+8)*blockSize)); finished <- e }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		full := slow.recvCredit == 0
		s.mu.Unlock()
		if full {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receiver window did not fill")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case e := <-finished:
		t.Fatalf("write bypassed flow control: %v", e)
	default:
	}
	fast, peer := openPair(t, c, s)
	go func() {
		p := make([]byte, 4)
		_, e := io.ReadFull(peer, p)
		if e == nil {
			_, e = peer.Write(p)
		}
		if e != nil {
			peer.Close()
		}
	}()
	if _, e := fast.Write([]byte("ping")); e != nil {
		t.Fatal(e)
	}
	buf := make([]byte, 4)
	if _, e := io.ReadFull(fast, buf); e != nil || string(buf) != "ping" {
		t.Fatalf("unrelated stream blocked: %q %v", buf, e)
	}
	blocked.Close()
	select {
	case e := <-finished:
		if e == nil {
			t.Fatal("cancelled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled write stayed blocked")
	}
	if _, _, closed := c.Snapshot(); closed {
		t.Fatal("reset destroyed physical connection")
	}
}

func TestOpenCancellationAndDialFailure(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "reject"}[reject], func(t *testing.T) {
			c, s := pair(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				st, e := c.Open(ctx, []byte("target"))
				if st != nil {
					st.Close()
				}
				result <- e
			}()
			peer, e := s.Accept(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer peer.Close()
			if reject {
				if e = peer.Respond(3); e != nil {
					t.Fatal(e)
				}
			} else {
				cancel()
			}
			select {
			case e = <-result:
				if reject && !errors.Is(e, OpenError(3)) {
					t.Fatal(e)
				}
				if !reject && !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("open stayed blocked")
			}
			if _, _, closed := c.Snapshot(); closed {
				t.Fatal("stream failure closed session")
			}
		})
	}
}

// An OPEN timeout cancels only that stream; the same physical session remains
// usable, and the remote dial context observes RESET.
func TestOpenTimeoutKeepsSessionUsable(t *testing.T) {
	c, s := pair(t)
	c.opts.OpenTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := c.Open(ctx, []byte("timeout target")); result <- err }()
	peer, err := s.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	select {
	case err = <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("OPEN timeout did not fire")
	}
	select {
	case <-peer.Context().Done():
	case <-ctx.Done():
		t.Fatal("remote dial was not canceled")
	}
	c.opts.OpenTimeout = time.Second
	openPair(t, c, s)
}

func TestAcceptedStreamParentCancellation(t *testing.T) {
	type key struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "dial policy"))
	defer cancel()
	a, b := net.Pipe()
	c, err := New(a, Options{Profile: DefaultProfile()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(b, Options{Server: true, Context: ctx, Profile: DefaultProfile()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close(); c.Wait(); s.Wait() })
	_, peer := openPair(t, c, s)
	if peer.Context().Value(key{}) != "dial policy" {
		t.Fatal("lost parent values")
	}
	done := make(chan error, 1)
	go func() { var b [1]byte; _, err := peer.Read(b[:]); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled read stayed blocked")
	}
}
