package service

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
	"veil/core"
	"veil/internal/mux"
)

// Every burst starts together and holds all streams until the slowest Open
// completes. The fixed dial delay isolates pool expansion from WAN variability.
func BenchmarkBurstOpen(b *testing.B) {
	transport := os.Getenv("VEIL_BURST_TRANSPORT")
	if transport == "" {
		transport = "tls"
	}
	if transport != "tls" && transport != "reality" {
		b.Fatal("VEIL_BURST_TRANSPORT must be tls or reality")
	}
	counts := []int{128, 512}
	if v := os.Getenv("VEIL_BURST_CONNECTIONS"); v != "" {
		count, err := strconv.Atoi(v)
		if err != nil || count < 1 || count > 4096 {
			b.Fatal("VEIL_BURST_CONNECTIONS must be 1..4096")
		}
		counts = []int{count}
	}
	for _, count := range counts {
		for _, mode := range []string{"cold", "repeat", "local"} {
			b.Run(fmt.Sprintf("%s-%d", mode, count), func(b *testing.B) {
				st, ct := settings(b, transport)
				physical := max(64, (count+mux.MaxStreams-1)/mux.MaxStreams)
				_, server := start(b, Config{Role: "server", Secret: testKey, TLS: st, MaxInboundConnections: 2 * physical})
				dst := target(b, func(n net.Conn) { io.Copy(io.Discard, n) })
				cfg := core.ClientConfig{
					Config: core.Config{Secret: testKey, TLS: ct, MaxConnections: physical}, Server: server,
					DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
						if mode != "local" {
							timer := time.NewTimer(20 * time.Millisecond)
							defer timer.Stop()
							select {
							case <-ctx.Done():
								return nil, ctx.Err()
							case <-timer.C:
							}
						}
						return (&net.Dialer{}).DialContext(ctx, network, address)
					},
				}
				newClient := func() *core.Client {
					client, err := core.NewClient(cfg)
					if err != nil {
						b.Fatal(err)
					}
					return client
				}
				burst := func(client *core.Client) (time.Duration, time.Duration, uint64) {
					previous := client.PoolStats().DialAttempts
					streams := make([]*core.Stream, count)
					latencies := make([]time.Duration, count)
					failures := make(chan error, count)
					ready := make(chan struct{})
					var workers sync.WaitGroup
					start := time.Now()
					for i := range streams {
						workers.Go(func() {
							<-ready
							s, err := client.Open(b.Context(), dst)
							latencies[i] = time.Since(start)
							if err != nil {
								failures <- err
							}
							streams[i] = s
						})
					}
					close(ready)
					workers.Wait()
					elapsed := time.Since(start)
					dials := client.PoolStats().DialAttempts - previous
					slices.Sort(latencies)
					p95 := latencies[(count*95+99)/100-1]
					for _, s := range streams {
						if s != nil {
							s.Close()
						}
					}
					if len(failures) > 0 {
						client.Close()
						b.Fatal(<-failures)
					}
					return elapsed, p95, dials
				}
				var client *core.Client
				if mode == "repeat" {
					client = newClient()
					defer client.Close()
					burst(client) // Each measured burst begins with the same eight idle lanes.
				}
				var p95, elapsed time.Duration
				var dials uint64
				b.ReportAllocs()
				for b.Loop() {
					if mode != "repeat" {
						client = newClient()
					}
					e, p, d := burst(client)
					elapsed, p95, dials = elapsed+e, p95+p, dials+d
					if mode != "repeat" {
						client.Close()
					}
				}
				b.ReportMetric(float64(dials)/float64(b.N), "dials/burst")
				b.ReportMetric(float64(elapsed)/float64(b.N)/1e6, "open-ms")
				b.ReportMetric(float64(p95)/float64(b.N)/1e6, "p95-ms")
			})
		}
	}
}
