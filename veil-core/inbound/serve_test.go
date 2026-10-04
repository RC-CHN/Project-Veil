package inbound

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

type acceptResult struct {
	conn net.Conn
	err  error
}

type testListener struct {
	results chan acceptResult
	closed  chan struct{}
	once    sync.Once
}

func (l *testListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.results:
		return r.conn, r.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *testListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *testListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestServeRecoversTemporaryAcceptError(t *testing.T) {
	for _, cause := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE} {
		t.Run(cause.Error(), func(t *testing.T) {
			if !cause.Temporary() {
				t.Skip("not a temporary accept error on this platform")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ln := &testListener{results: make(chan acceptResult, 10), closed: make(chan struct{})}
			for i := range 3 {
				local, peer := net.Pipe()
				defer local.Close()
				defer peer.Close()
				ln.results <- acceptResult{conn: local}
				failures := 0
				if i == 0 {
					failures = 6
				} else if i == 1 {
					failures = 1
				}
				for range failures {
					ln.results <- acceptResult{err: &net.OpError{Op: "accept", Net: "tcp", Err: cause}}
				}
			}
			started := make(chan struct{}, 3)
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, ln, func(ctx context.Context, c net.Conn) error {
					defer c.Close()
					started <- struct{}{}
					<-ctx.Done()
					return nil
				}, ServeOptions{})
			}()
			for range 2 {
				select {
				case <-started:
				case err := <-done:
					t.Fatalf("temporary accept error stopped existing handlers: %v", err)
				case <-time.After(time.Second):
					t.Fatal("listener did not recover")
				}
			}
			select {
			case <-started:
			case <-time.After(150 * time.Millisecond):
				t.Fatal("successful accept did not reset retry delay")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("handlers did not join on stop")
			}
		})
	}
}

func TestServeAcceptRetryStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := &testListener{results: make(chan acceptResult, 10), closed: make(chan struct{})}
	for range cap(ln.results) {
		ln.results <- acceptResult{err: &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}}
	}
	retries := make(chan error, 10)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, func(context.Context, net.Conn) error { return nil }, ServeOptions{OnError: func(err error) { retries <- err }})
	}()
	start := time.Now()
	for range 8 {
		select {
		case err := <-retries:
			if !errors.Is(err, syscall.EMFILE) {
				t.Fatal(err)
			}
		case err := <-done:
			t.Fatalf("resource exhaustion stopped listener: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("retry not reported")
		}
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("accept failure retries busy-looped")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stop waited for retry backoff")
	}
}

func TestServePermanentAcceptErrorClosesHandlers(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	ln := &testListener{results: make(chan acceptResult, 2), closed: make(chan struct{})}
	want := errors.New("broken listener")
	ln.results <- acceptResult{conn: local}
	ln.results <- acceptResult{err: want}
	joined := make(chan struct{})
	err := Serve(context.Background(), ln, func(ctx context.Context, c net.Conn) error {
		defer close(joined)
		defer c.Close()
		<-ctx.Done()
		return nil
	}, ServeOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("permanent accept error: %v", err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("failed listener left a handler running")
	}
}
