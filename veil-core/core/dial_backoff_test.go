package core

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
