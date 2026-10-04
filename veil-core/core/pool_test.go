package core

import (
	"context"
	"errors"
	"io"
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

func TestPoolUsesLiveLaneDuringExpansion(t *testing.T) {
	for _, dialing := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed dial", true: "pending dial"}[dialing], func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			live := pooled(t, a)
			live.active = 4 // Trigger the optional second lane.
			p := testPool(t)
			p.all[live], p.total, p.dialing = true, 1, dialing
			if dialing {
				p.total++
			}
			p.cfg.DialContext = func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("injected secondary dial failure")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := p.get(ctx)
			if err != nil || got != live {
				t.Fatalf("healthy lane unavailable: session=%p err=%v", got, err)
			}
			p.put(got)
		})
	}
}

func TestPoolWaitForDialIsBounded(t *testing.T) {
	p := testPool(t)
	p.cfg.HandshakeTimeout = 30 * time.Millisecond
	p.total, p.dialing = 1, true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := p.get(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("pool wait escaped its own handshake budget: %v (caller: %v)", err, ctx.Err())
	}
}

func waitPoolQueue(t *testing.T, p *pool, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		n := p.waiters.Len()
		p.mu.Unlock()
		if n == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued opens: got %d, want %d", n, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPoolQueuedAdmission(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	live := pooled(t, a)
	p := testPool(t)
	p.cfg.MaxConnections = 1
	p.all[live], p.total = true, 1
	// Hold an older admission as if its goroutine has not been scheduled yet.
	// A free lane must not let newcomers overtake it indefinitely.
	older := p.waiters.PushBack(make(chan struct{}, 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := p.get(ctx)
		result <- err
	}()
	waitPoolQueue(t, p, 2)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("queued cancellation:", err)
	}
	waitPoolQueue(t, p, 1)
	go func() {
		s, err := p.get(context.Background())
		if err == nil {
			p.put(s)
		}
		result <- err
	}()
	waitPoolQueue(t, p, 2)
	p.mu.Lock()
	p.waiters.Remove(older)
	p.notifyLocked()
	p.mu.Unlock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal("admission did not recover:", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued open was not woken")
	}
	waitPoolQueue(t, p, 0)
}

func TestPoolCloseWakesQueuedOpens(t *testing.T) {
	p := testPool(t)
	p.total, p.dialing = 1, true
	results := make(chan error, 32)
	for range cap(results) {
		go func() { _, err := p.get(context.Background()); results <- err }()
	}
	waitPoolQueue(t, p, cap(results))
	p.close()
	for range cap(results) {
		select {
		case err := <-results:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("close left an open queued")
		}
	}
	waitPoolQueue(t, p, 0)
}

func TestDrainingLaneCountsAgainstPoolLimit(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	s := pooled(t, a)
	s.active = 1
	p := testPool(t)
	p.cfg.MaxConnections = 1
	p.all[s], p.total = true, 1
	p.drain(s)
	p.expire()
	if s.mux.Err() != nil || p.total != 1 {
		t.Fatal("draining aborted an active stream")
	}
	if _, err := p.get(context.Background()); err == nil {
		t.Fatal("draining bypassed pool limit or accepted new work")
	}
	p.put(s)
	if p.total != 0 || s.mux.Err() == nil {
		t.Fatal("last stream did not release draining lane")
	}
}

func TestMissingDoneRetiresLane(t *testing.T) {
	a, b := net.Pipe()
	s := pooled(t, a)
	peer, err := mux.New(b, mux.Options{Server: true, Profile: mux.DefaultProfile(), WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { peer.Close(); peer.Wait() }()
	p := testPool(t)
	p.cfg.HandshakeTimeout = 30 * time.Millisecond
	p.all[s], p.total = true, 1
	c := &Client{cfg: p.cfg, pool: p, ctx: context.Background()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	opened := make(chan *Stream, 1)
	go func() {
		st, err := c.Open(ctx, "localhost:80")
		if err != nil {
			t.Error(err)
		}
		opened <- st
	}()
	remote, err := peer.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err := remote.Respond(0); err != nil {
		t.Fatal(err)
	}
	stream := <-opened
	if stream == nil {
		t.Fatal("open failed")
	}
	defer stream.Close()
	local, app := tcpPair(t)
	result := make(chan error, 1)
	go func() { result <- stream.Relay(local) }()
	if err := remote.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, app); err != nil {
		t.Fatal(err)
	}
	app.CloseWrite()
	if _, err := io.Copy(io.Discard, remote); err != nil {
		t.Fatal(err)
	}
	// Both FINs arrived, but the peer never confirms completion with DONE.
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("missing DONE was not reported:", err)
	}
	if got := c.PoolStats(); got.Total != 0 {
		t.Fatal("unconfirmed lane remained reusable:", got)
	}
}
