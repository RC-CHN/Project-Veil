package mux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
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

type notifiedWriter struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *notifiedWriter) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestRespondCancellationDuringBlockedWrite(t *testing.T) {
	a, peer := net.Pipe()
	defer peer.Close()
	w := &notifiedWriter{Conn: a, started: make(chan struct{})}
	s, err := New(w, Options{Server: true, Profile: DefaultProfile(), WriteTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close(); s.Wait() }()
	if _, err := peer.Write(appendFrame(nil, open, 1, []byte("target"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := s.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- stream.Respond(0) }()
	select {
	case <-w.started:
	case <-ctx.Done():
		t.Fatal("response write did not start")
	}
	// The remote peer is not reading. Cancel only this stream while the
	// physical writer remains blocked until its own deadline.
	stream.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled response succeeded")
		}
	case <-ctx.Done():
		t.Fatal("canceled response held its server worker until the socket timeout")
	}
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

func TestParentCancellationReleasesFlowControlledWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
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
	done := make(chan error, 1)
	go func() {
		_, err := peer.Write(make([]byte, (windowBlocks+1)*blockSize))
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		blocked := peer.sendCredit == 0
		s.mu.Unlock()
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("send window did not fill")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("parent cancellation left Write waiting for peer credit")
	}
	if s.Err() != nil {
		t.Fatal("stream cancellation closed the session:", s.Err())
	}
}

type gatedDataWriter struct {
	net.Conn
	started, release chan struct{}
	unchanged        chan bool
	once             sync.Once
}

func (c *gatedDataWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == data {
		c.once.Do(func() {
			before := bytes.Clone(p)
			close(c.started)
			<-c.release
			c.unchanged <- bytes.Equal(p, before)
		})
	}
	return c.Conn.Write(p)
}

func TestCanceledWriteReturnsBufferOwnership(t *testing.T) {
	// Cover both a request fully copied into the blocked batch and one whose
	// remainder is still owned by the scheduler's pending request.
	for _, size := range []int{64, 4096} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a, b := net.Pipe()
			w := &gatedDataWriter{Conn: b, started: make(chan struct{}), release: make(chan struct{}), unchanged: make(chan bool, 1)}
			c, err := New(a, Options{Profile: DefaultProfile()})
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(w, Options{Server: true, Context: ctx, Profile: DefaultProfile()})
			if err != nil {
				t.Fatal(err)
			}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(w.release) }); c.Close(); s.Close(); c.Wait(); s.Wait() })
			_, peer := openPair(t, c, s)
			payload := bytes.Repeat([]byte{0x7b}, size)
			done := make(chan error, 1)
			go func() { _, err := peer.Write(payload); done <- err }()
			select {
			case <-w.started:
			case <-time.After(time.Second):
				t.Fatal("socket write did not start")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled write succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation waited for blocked socket")
			}
			// The caller may immediately recycle p while the physical writer resumes.
			clear(payload)
			release.Do(func() { close(w.release) })
			if !<-w.unchanged {
				t.Fatal("physical writer retained the caller's returned buffer")
			}
		})
	}
}

func TestStreamChurnAtCapacity(t *testing.T) {
	c, s := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { c.Close(); s.Close() })
	defer stop()
	payload := bytes.Repeat([]byte("churn"), 20<<10)
	for round := range 24 {
		var streams [MaxStreams][2]*Stream
		for i := range streams {
			a, b := openPair(t, c, s)
			streams[i] = [2]*Stream{a, b}
		}
		if _, err := c.Open(ctx, []byte("over capacity")); !errors.Is(err, ErrFull) {
			t.Fatalf("round %d: capacity was not enforced: %v", round, err)
		}
		var workers sync.WaitGroup
		for i, peers := range streams {
			a, b := peers[0], peers[1]
			if i%2 != 0 {
				// Race queued writes and FIN against RESET while other streams
				// complete normally on the same full physical connection.
				workers.Go(func() { _, _ = a.Write(payload) })
				workers.Go(func() { _ = a.CloseWrite() })
				workers.Go(func() { a.Close(); b.Close() })
				continue
			}
			workers.Go(func() {
				defer b.Close()
				p, err := io.ReadAll(b)
				if err != nil || !bytes.Equal(p, payload) {
					t.Errorf("server payload: %d bytes, %v", len(p), err)
					return
				}
				if _, err = b.Write(p); err == nil {
					err = b.CloseWrite()
				}
				if err == nil {
					err = b.Finish()
				}
				if err != nil {
					t.Error("server completion:", err)
				}
			})
			workers.Go(func() {
				defer a.Close()
				if _, err := a.Write(payload); err != nil {
					t.Error("client write:", err)
					return
				}
				if err := a.CloseWrite(); err != nil {
					t.Error("client FIN:", err)
					return
				}
				p, err := io.ReadAll(a)
				if err != nil || !bytes.Equal(p, payload) {
					t.Errorf("client payload: %d bytes, %v", len(p), err)
					return
				}
				if err := a.WaitDone(ctx); err != nil {
					t.Error("client DONE:", err)
				}
			})
		}
		workers.Wait()
		for _, session := range []*Session{c, s} {
			if active, _, closed := session.Snapshot(); active != 0 || closed {
				t.Fatalf("round %d: active=%d closed=%v err=%v", round, active, closed, session.Err())
			}
		}
		if t.Failed() {
			return
		}
	}
}
