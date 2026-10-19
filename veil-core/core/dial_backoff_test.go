package core

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"veil/internal/mux"
)

func TestPoolParallelExpansionIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		requests, limit, dials int
		confirm, cancel        bool
	}{
		{"one lane is enough", 8, 32, 1, false, false},
		{"demand grows during dial", 9, 32, 2, false, false},
		{"third lane tail", 17, 32, 3, false, false},
		{"fourth lane tail", 25, 32, 4, false, false},
		{"eight lanes", 64, 32, 8, false, false},
		{"parallel ceiling", 256, 64, 16, false, false},
		{"physical ceiling", 32, 4, 3, false, false},
		{"first OPENED wakes expansion", 128, 32, 16, true, false},
		{"cancel pending expansion", 256, 64, 16, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, peer := net.Pipe()
			defer peer.Close()
			live := pooled(t, a)
			live.active = mux.MaxStreams
			live.established.Store(!tc.confirm)
			p := testPool(t)
			p.all[live], p.total = true, 1
			p.cfg.MaxConnections = tc.limit
			entered := make(chan struct{}, tc.requests)
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			failure := errors.New("injected expansion failure")
			p.cfg.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				entered <- struct{}{}
				select {
				case <-release:
					return nil, failure
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			results := make(chan error, tc.requests)
			for range cap(results) {
				go func() { _, err := p.get(ctx); results <- err }()
			}
			if tc.confirm {
				waitPoolQueue(t, p, tc.requests-1)
				p.confirm(live)
			}
			// All callers settle into dial reservations or the bounded queue.
			// No dial completes before demand growth and OPENED are observed.
			waitPoolQueue(t, p, tc.requests-tc.dials)
			for range tc.dials {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("reserved expansion did not enter the dialer")
				}
			}
			started := tc.dials + len(entered)
			want := failure
			if tc.cancel {
				want = context.Canceled
				cancel()
			} else {
				unblock()
			}
			for range cap(results) {
				if err := <-results; !errors.Is(err, want) {
					t.Error("expansion failure was lost:", err)
				}
			}
			if started != tc.dials {
				t.Fatalf("started %d concurrent dials, want %d", started, tc.dials)
			}
			waitPoolQueue(t, p, 0)
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.total != 1 || p.dialing != 0 || tc.cancel && p.dialError != nil {
				t.Fatal("failed or canceled dials retained capacity or cancellation triggered backoff")
			}
		})
	}
}

func TestPoolCoalescesDialFailures(t *testing.T) {
	p := testPool(t)
	failure := errors.New("injected unreachable server")
	var attempts atomic.Int32
	p.cfg.DialContext = func(context.Context, string, string) (net.Conn, error) {
		attempts.Add(1)
		return nil, failure
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			<-start
			if _, err := p.get(ctx); !errors.Is(err, failure) {
				t.Errorf("dial failure was lost: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := attempts.Load(); got != 1 {
		t.Fatalf("one burst caused %d physical dial attempts", got)
	}
}

func TestPoolOptionalDialCooldown(t *testing.T) {
	a, peer := net.Pipe()
	defer peer.Close()
	live := pooled(t, a)
	live.active = 4
	p := testPool(t)
	p.all[live], p.total = true, 1
	var attempts int
	p.cfg.DialContext = func(context.Context, string, string) (net.Conn, error) {
		attempts++
		return nil, errors.New("injected expansion failure")
	}
	for range 16 {
		got, err := p.get(context.Background())
		if err != nil || got != live {
			t.Fatalf("cooldown prevented use of healthy lane: %v", err)
		}
		p.put(got)
	}
	if attempts != 1 {
		t.Fatalf("retried failed expansion for every request: %d attempts", attempts)
	}
}

func TestPoolCallerCancellationDoesNotBackoff(t *testing.T) {
	p := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	failure := errors.New("injected network error")
	p.cfg.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		attempts++
		if attempts == 1 {
			cancel()
			return nil, ctx.Err()
		}
		return nil, failure
	}
	if _, err := p.get(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := p.get(context.Background()); !errors.Is(err, failure) || attempts != 2 {
		t.Fatalf("caller cancellation suppressed another request: %v attempts=%d", err, attempts)
	}
}

func TestPoolRetriesAfterCooldown(t *testing.T) {
	p := testPool(t)
	attempts := 0
	failure := errors.New("injected network error")
	p.cfg.DialContext = func(context.Context, string, string) (net.Conn, error) {
		attempts++
		return nil, failure
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for range 2 {
		if _, err := p.get(ctx); !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
	if attempts != 1 {
		t.Fatal("missing failure cooldown")
	}
	for attempts == 1 {
		select {
		case <-ctx.Done():
			t.Fatal("dial cooldown never allowed recovery")
		case <-time.After(10 * time.Millisecond):
		}
		if _, err := p.get(ctx); !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
}
