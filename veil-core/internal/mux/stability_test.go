package mux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Keep the large, deliberately unread windows out of routine CI. Unlike the
// forwarding soak, every round fills all sixteen directional windows before
// releasing readers or resetting a subset of streams.
func TestWindowSaturationSoak(t *testing.T) {
	text := os.Getenv("VEIL_WINDOW_STRESS_DURATION")
	if text == "" {
		t.Skip("set VEIL_WINDOW_STRESS_DURATION, for example 5m")
	}
	duration, err := time.ParseDuration(text)
	if err != nil || duration < time.Second {
		t.Fatal("invalid VEIL_WINDOW_STRESS_DURATION")
	}
	payload := bytes.Repeat([]byte("window-pressure!"), (windowBlocks+1)*blockSize/16)
	want := sha256.Sum256(payload)
	start := time.Now()
	var peak uint64
	rounds := 0
	for time.Since(start) < duration {
		if !t.Run(fmt.Sprintf("round-%d", rounds), func(t *testing.T) {
			live := saturatedRound(t, payload, want, rounds%2 != 0)
			peak = max(peak, live)
		}) {
			return
		}
		rounds++
	}
	runtime.GC()
	runtime.GC() // Expire pooled buffers before reporting live retained memory.
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("rounds=%d duration=%s peak_saturated_heap=%d final_live_heap=%d goroutines=%d", rounds, time.Since(start), peak, memory.HeapAlloc, runtime.NumGoroutine())
}

func saturatedRound(t *testing.T, payload []byte, want [sha256.Size]byte, reset bool) uint64 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	for _, conn := range []net.Conn{a, b} {
		if err := conn.(*net.TCPConn).SetWriteBuffer(64 << 10); err != nil {
			t.Fatal(err)
		}
	}
	client, err := New(a, Options{Profile: DefaultProfile(), WriteTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(b, Options{Server: true, Profile: DefaultProfile(), WriteTimeout: 15 * time.Second})
	if err != nil {
		client.Close()
		client.Wait()
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	stop := context.AfterFunc(ctx, func() { client.Close(); server.Close() })
	t.Cleanup(func() {
		cancel()
		stop()
		client.Close()
		server.Close()
		workers.Wait()
		client.Wait()
		server.Wait()
	})
	var streams [MaxStreams][2]*Stream
	var serverWrites [MaxStreams]chan struct{}
	results := make(chan error, 2*MaxStreams)
	for i := range streams {
		streams[i][0], streams[i][1] = openPair(t, client, server)
		serverWrites[i] = make(chan struct{})
		for side, st := range streams[i] {
			workers.Go(func() {
				if side == 1 {
					defer close(serverWrites[i])
				}
				n, err := st.Write(payload)
				if err == nil && n != len(payload) {
					err = io.ErrShortWrite
				}
				if err == nil {
					err = st.CloseWrite()
				}
				results <- err
			})
		}
	}
	for {
		full := true
		for _, pair := range streams {
			for _, st := range pair {
				st.s.mu.Lock()
				full = full && st.sendCredit == 0 && st.recvCredit == 0 && len(st.queue)-st.head == windowBlocks && st.queue[len(st.queue)-1].complete
				st.s.mu.Unlock()
			}
		}
		if full {
			break
		}
		select {
		case err := <-results:
			t.Fatalf("writer completed before all windows filled: %v", err)
		case <-ctx.Done():
			t.Fatal("full windows not reached")
		case <-time.After(time.Millisecond):
		}
	}
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	readResults := make(chan error, 2*MaxStreams)
	var finishes [MaxStreams]chan error
	resetDirections := 0
	for i, pair := range streams {
		if reset && i%2 == 0 {
			// Reset one endpoint while both directions have queued DATA.
			pair[0].Close()
			resetDirections += 2
			continue
		}
		serverRead := make(chan struct{})
		finishes[i] = make(chan error, 1)
		for side, st := range pair {
			workers.Go(func() {
				if side == 1 {
					defer close(serverRead)
				}
				h := sha256.New()
				n, err := io.CopyBuffer(h, st, make([]byte, 32<<10))
				if err == nil && (n != int64(len(payload)) || !bytes.Equal(h.Sum(nil), want[:])) {
					err = fmt.Errorf("payload mismatch: %d bytes", n)
				}
				readResults <- err
			})
		}
		// Like core.Server.serve, finish when the server's two pumps have
		// joined. Server CloseWrite may defer FIN until Finish, so waiting for
		// the client's EOF first would deadlock the test itself.
		workers.Go(func() {
			<-serverWrites[i]
			<-serverRead
			finishes[i] <- pair[1].Finish()
		})
	}
	failedWrites := 0
	for range 2 * MaxStreams {
		if err := <-results; err != nil {
			if !errors.Is(err, ErrReset) && !errors.Is(err, net.ErrClosed) {
				t.Fatalf("writer: %v", err)
			}
			failedWrites++
		}
	}
	if failedWrites != resetDirections {
		t.Fatalf("reset writes: %d, want %d", failedWrites, resetDirections)
	}
	for range 2*MaxStreams - resetDirections {
		if err := <-readResults; err != nil {
			t.Fatalf("reader: %v", err)
		}
	}
	for i, pair := range streams {
		if !reset || i%2 != 0 {
			if err := <-finishes[i]; err != nil {
				t.Fatalf("server Finish: %v", err)
			}
			if err := pair[0].WaitDone(ctx); err != nil {
				t.Fatalf("client DONE: %v", err)
			}
		}
		pair[0].Close()
		pair[1].Close()
	}
	for _, s := range []*Session{client, server} {
		if active, _, closed := s.Snapshot(); active != 0 || closed {
			t.Fatalf("session after drain: active=%d closed=%v", active, closed)
		}
	}
	return memory.HeapAlloc
}
