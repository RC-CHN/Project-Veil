package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"veil/core"
	"veil/inbound"
)

// Opt in to sustained real TCP/TLS load; ordinary CI exercises the focused
// regressions instead. The duration excludes startup and the final drain.
func TestStabilitySoak(t *testing.T) {
	durationText := os.Getenv("VEIL_STRESS_DURATION")
	if durationText == "" {
		t.Skip("set VEIL_STRESS_DURATION, for example 60m")
	}
	duration, err := time.ParseDuration(durationText)
	if err != nil || duration < time.Second {
		t.Fatal("invalid VEIL_STRESS_DURATION")
	}
	workerCount := 128
	if text := os.Getenv("VEIL_STRESS_WORKERS"); text != "" {
		workerCount, err = strconv.Atoi(text)
		if err != nil || workerCount < 1 || workerCount > 512 {
			t.Fatal("VEIL_STRESS_WORKERS must be 1..512")
		}
	}
	st, ct := settings(t, "reality")
	st.RecordPadding, ct.RecordPadding = true, true
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var diagnosticMu sync.Mutex
	diagnosticCounts := make(map[string]int)
	reportError := func(err error) {
		code := errorCode(err)
		diagnosticMu.Lock()
		diagnosticCounts[code]++
		count := diagnosticCounts[code]
		diagnosticMu.Unlock()
		if count <= 2 {
			t.Logf("proxy diagnostic (%s): %v", code, err)
		}
	}
	var runners sync.WaitGroup
	var listeners []net.Listener
	var inner, outer *core.Client
	closeAll := sync.OnceFunc(func() {
		stop()
		if inner != nil {
			inner.Close()
		}
		if outer != nil {
			outer.Close()
		}
		for _, ln := range listeners {
			ln.Close()
		}
		runners.Wait()
	})
	t.Cleanup(closeAll)
	serve := func(handle inbound.Handler, stats *core.Stats) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, ln)
		runners.Go(func() {
			if err := inbound.Serve(ctx, ln, handle, inbound.ServeOptions{MaxConnections: max(512, 2*workerCount), Stats: stats, OnError: reportError}); err != nil {
				t.Error(err)
			}
		})
		return ln.Addr().String()
	}
	newServer := func() (*core.Server, string) {
		t.Helper()
		s, err := core.NewServer(core.ServerConfig{Config: core.Config{Secret: testKey, TLS: st}})
		if err != nil {
			t.Fatal(err)
		}
		s.OnStreamError = reportError
		return s, serve(s.Handle, &s.Stats)
	}
	exit, exitAddr := newServer()
	relay, relayAddr := newServer()
	// Leave admission headroom while the previous request completes FIN/DONE.
	// Saturated admission and recovery are exercised separately by overload tests.
	clientConfig := core.Config{Secret: testKey, TLS: ct, MaxConnections: max(64, workerCount/8+8)}
	outer = newCoreClient(t, core.ClientConfig{Config: clientConfig, Server: relayAddr})
	forward, err := inbound.Forward(outer, exitAddr)
	if err != nil {
		t.Fatal(err)
	}
	hop := serve(forward, nil)
	inner = newCoreClient(t, core.ClientConfig{Config: clientConfig, Server: hop})
	entry := serve(inbound.Mixed(inner, 10*time.Second), nil)
	echo := serve(func(ctx context.Context, c net.Conn) error {
		defer c.Close()
		cancel := context.AfterFunc(ctx, func() { c.Close() })
		defer cancel()
		_, err := io.Copy(c, c)
		return err
	}, nil)

	var completed, canceled, transferred, failed, worstMicros atomic.Int64
	loadCtx, endLoad := context.WithTimeout(ctx, duration)
	defer endLoad()
	var workers sync.WaitGroup
	for worker := range workerCount {
		workers.Go(func() {
			payload := make([]byte, 65536)
			received := make([]byte, len(payload))
			for i := range payload {
				payload[i] = byte(i*31 + worker)
			}
			for round := 0; loadCtx.Err() == nil; round++ {
				began := time.Now()
				progress := 0
				err := func() error {
					c, err := net.DialTimeout("tcp", entry, 10*time.Second)
					if err != nil {
						return err
					}
					defer c.Close()
					c.SetDeadline(time.Now().Add(15 * time.Second))
					var reader io.Reader = c
					if (worker+round)%2 == 0 {
						if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo, echo); err != nil {
							return err
						}
						r := bufio.NewReader(c)
						response, err := http.ReadResponse(r, nil)
						if err != nil {
							return err
						}
						if response.StatusCode != 200 {
							return fmt.Errorf("CONNECT returned %d", response.StatusCode)
						}
						reader = r
					} else {
						if err := soakSOCKS(c, echo); err != nil {
							return err
						}
					}
					if round%31 == 30 {
						// An application's abort is expected; adjacent streams must survive.
						if err := writeAll(c, payload[:1024]); err != nil {
							return err
						}
						if err := c.(*net.TCPConn).SetLinger(0); err != nil {
							return err
						}
						if err := c.Close(); err != nil {
							return err
						}
						canceled.Add(1)
						return nil
					}
					length := []int{1, 63, 64, 4095, 16383, 16384, 32769, 65536}[(worker+round)%8]
					chunks := 1
					if worker < 8 {
						chunks = 256
						length = len(payload)
					}
					for range chunks {
						if err := writeAll(c, payload[:length]); err != nil {
							return err
						}
						if _, err := io.ReadFull(reader, received[:length]); err != nil {
							return err
						}
						if !bytes.Equal(payload[:length], received[:length]) {
							return fmt.Errorf("payload mismatch, worker=%d round=%d", worker, round)
						}
						transferred.Add(int64(length))
						progress += length
					}
					if err := c.(*net.TCPConn).CloseWrite(); err != nil {
						return err
					}
					var tail [1]byte
					if n, err := reader.Read(tail[:]); n != 0 || err != io.EOF {
						return fmt.Errorf("half-close: n=%d err=%v", n, err)
					}
					completed.Add(1)
					return nil
				}()
				elapsed := time.Since(began).Microseconds()
				for prev := worstMicros.Load(); elapsed > prev && !worstMicros.CompareAndSwap(prev, elapsed); prev = worstMicros.Load() {
				}
				if err != nil {
					failed.Add(1)
					t.Errorf("worker=%d round=%d validated_bytes=%d: %v", worker, round, progress, err)
					endLoad()
					return
				}
			}
		})
	}
	report := func(phase string) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		fds, _ := os.ReadDir("/proc/self/fd")
		v := map[string]any{"phase": phase, "at": time.Now().UTC(), "completed": completed.Load(), "canceled": canceled.Load(), "failed": failed.Load(), "bytes_each_direction": transferred.Load(), "worst_us": worstMicros.Load(), "heap": mem.HeapAlloc, "heap_inuse": mem.HeapInuse, "goroutines": runtime.NumGoroutine(), "fds": len(fds), "inner": inner.PoolStats(), "outer": outer.PoolStats(), "exit_streams": exit.Stats.ActiveStreams.Load(), "relay_streams": relay.Stats.ActiveStreams.Load()}
		v["workers"], v["gc_cycles"], v["gc_cpu_fraction"] = workerCount, mem.NumGC, mem.GCCPUFraction
		diagnosticMu.Lock()
		counts := make(map[string]int, len(diagnosticCounts))
		for code, n := range diagnosticCounts {
			counts[code] = n
		}
		diagnosticMu.Unlock()
		v["diagnostics"] = counts
		b, _ := json.Marshal(v)
		t.Log(string(b))
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	report("started")
	for loadCtx.Err() == nil {
		select {
		case <-ticker.C:
			report("load")
		case <-loadCtx.Done():
		}
	}
	workers.Wait()
	report("draining")
	closeAll()
	runtime.GC()
	report("closed")
	// net's TCP-to-TCP splice path in the echo fixture caches pipe descriptors
	// in sync.Pool. A second collection expires the pool's victim generation.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	report("settled")
	if inner.PoolStats().Streams != 0 || outer.PoolStats().Streams != 0 || exit.Stats.ActiveStreams.Load() != 0 || relay.Stats.ActiveStreams.Load() != 0 {
		t.Fatal("streams retained after shutdown")
	}
	for _, p := range []core.PoolStats{inner.PoolStats(), outer.PoolStats()} {
		if p.Total != 0 || p.Idle != 0 || p.Dialing {
			t.Fatal("physical connections retained after shutdown", p)
		}
	}
	if failed.Load() != 0 || completed.Load() == 0 {
		t.Fatal("healthy workload did not complete without unexpected errors")
	}
}

func soakSOCKS(c net.Conn, target string) error {
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	var method [2]byte
	if _, err := io.ReadFull(c, method[:]); err != nil {
		return err
	}
	if method != [2]byte{5, 0} {
		return fmt.Errorf("SOCKS method %x", method)
	}
	address, portText, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		return err
	}
	request := append([]byte{5, 1, 0, 3, byte(len(address))}, address...)
	request = append(request, byte(port>>8), byte(port))
	if _, err := c.Write(request); err != nil {
		return err
	}
	var reply [10]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return err
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fmt.Errorf("SOCKS reply %x", reply)
	}
	return nil
}
