package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
	"veil/core"
	"veil/internal/mux"
)

// Hold real application connections open together; sequential short requests
// alone never exercise the distinction between inbound and physical limits.
func TestConnectionConcurrentCapacity(t *testing.T) {
	count := 128
	if v := os.Getenv("VEIL_CONCURRENT_CONNECTIONS"); v != "" {
		var err error
		count, err = strconv.Atoi(v)
		if err != nil || count < 65 || count > 4096 {
			t.Fatal("VEIL_CONCURRENT_CONNECTIONS must be 65..4096")
		}
	}
	transport := os.Getenv("VEIL_CAPACITY_TRANSPORT")
	if transport == "" {
		transport = "tls"
	}
	if transport != "tls" && transport != "reality" {
		t.Fatal("VEIL_CAPACITY_TRANSPORT must be tls or reality")
	}
	// Leave default-capacity runs on the actual defaults. Larger runs explicitly
	// configure both ends, as an operator must when increasing physical capacity.
	physical := 0
	if count > core.DefaultMaxConnections*mux.MaxStreams {
		physical = (count + mux.MaxStreams - 1) / mux.MaxStreams
	}
	st, ct := settings(t, transport)
	server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, MaxConnections: physical})
	c, err := OpenConnection(Config{Role: "client", Server: addr, Secret: testKey, TLS: ct, MaxConnections: physical}, []Inlet{{"socks", "127.0.0.1:0"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	echo := target(t, func(n net.Conn) {
		// Keep the fixture out of net's splice-pipe cache so FD counts describe
		// live sockets, rather than pipes waiting for runtime cleanups after GC.
		var b [4096]byte
		for {
			count, err := n.Read(b[:])
			if count > 0 {
				if err := writeAll(n, b[:count]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})
	entry := c.Inlets()[0].Listen
	baselineFDs, fdErr := os.ReadDir("/proc/self/fd")
	for round := range 3 {
		connections := make([]*net.TCPConn, count)
		failures := make(chan error, 2*count)
		var workers sync.WaitGroup
		start := time.Now()
		for i := range count {
			workers.Go(func() {
				n, err := socksDial(entry, echo)
				if err == nil {
					connections[i] = n
					_, err = n.Write([]byte{byte(i)})
					if err == nil {
						var b [1]byte
						_, err = io.ReadFull(n, b[:])
						if err == nil && b[0] != byte(i) {
							err = fmt.Errorf("stream %d payload mismatch", i)
						}
					}
				}
				if err != nil {
					failures <- err
				}
			})
		}
		workers.Wait()
		elapsed := time.Since(start)
		snapshot := c.Snapshot()
		// Saturated admission rejects excess work without interrupting any of
		// the healthy held streams. The next round must regain the full capacity.
		if len(failures) == 0 && count == c.cfg.MaxInboundConnections {
			n, err := socksDial(entry, echo)
			if err == nil {
				n.Close()
				t.Error("saturated inlet accepted an extra connection")
			}
			await(t, func() bool { return c.Snapshot().Stats.Rejected == uint64(round+1) })
		}
		if len(failures) == 0 && count == c.cfg.MaxConnections*mux.MaxStreams {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			s, err := c.client.Open(ctx, echo)
			cancel()
			if s != nil {
				s.Close()
			}
			if !errors.Is(err, core.ErrPoolFull) {
				t.Errorf("saturated shared pool: %v", err)
			}
		}
		for i, n := range connections {
			if n != nil {
				workers.Go(func() {
					_, err := n.Write([]byte{byte(i + 1)})
					var b [1]byte
					if err == nil {
						_, err = io.ReadFull(n, b[:])
					}
					if err != nil || b[0] != byte(i+1) {
						failures <- fmt.Errorf("held stream %d: payload=%x, error=%v", i, b, err)
					}
				})
			}
		}
		workers.Wait()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		fds, _ := os.ReadDir("/proc/self/fd")
		t.Logf("round=%d requested=%d active=%d physical=%d failures=%d open=%v heap=%d goroutines=%d fds=%d", round, count, snapshot.Stats.ActiveStreams, snapshot.Diagnostics.Pool.Total, len(failures), elapsed, mem.HeapAlloc, runtime.NumGoroutine(), len(fds))
		// Release every accepted connection even when the admission assertion fails.
		for _, n := range connections {
			if n != nil {
				n.Close()
			}
		}
		if len(failures) != 0 || snapshot.Stats.ActiveStreams != int64(count) {
			errorCount := len(failures)
			if len(failures) != 0 {
				t.Log("first connection error:", <-failures)
			}
			t.Fatalf("concurrent admission failed: %d errors, %d/%d active", errorCount, snapshot.Stats.ActiveStreams, count)
		}
		if snapshot.Diagnostics.Pool.Total > c.cfg.MaxConnections {
			t.Fatal("physical pool exceeded its own limit")
		}
		await(t, func() bool {
			s := c.Snapshot()
			return s.Stats.ActiveConnections == 0 && s.Stats.ActiveStreams == 0 && server.Stats.ActiveStreams.Load() == 0 && s.Diagnostics.Pool.Total <= c.cfg.MaxIdle
		})
		runtime.GC()
		if fdErr == nil {
			await(t, func() bool {
				fds, err := os.ReadDir("/proc/self/fd")
				return err == nil && len(fds) <= len(baselineFDs)+2*c.cfg.MaxIdle
			})
		}
		runtime.ReadMemStats(&mem)
		fds, _ = os.ReadDir("/proc/self/fd")
		t.Logf("round=%d drained physical=%d heap=%d goroutines=%d fds=%d", round, c.Snapshot().Diagnostics.Pool.Total, mem.HeapAlloc, runtime.NumGoroutine(), len(fds))
	}
}

func TestIndependentAdmissionLimits(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		for _, inbound := range []int{0, 3, -1, 4097} {
			cfg := Config{Role: role, Server: "127.0.0.1:443", Secret: testKey, MaxConnections: 2, MaxInboundConnections: inbound}
			err := cfg.Defaults()
			if inbound < 0 || inbound > 4096 {
				if err == nil {
					t.Fatal("invalid inbound limit accepted")
				}
				continue
			}
			want := inbound
			if want == 0 {
				want = 2
				if role == "client" {
					want = 16
				}
			}
			if err != nil || cfg.MaxInboundConnections != want || cfg.MaxConnections != 2 || cfg.MaxIdle != 2 {
				t.Fatalf("%s: incorrect independent limits: %+v, %v", role, cfg, err)
			}
		}
	}
}
