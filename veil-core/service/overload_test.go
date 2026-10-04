package service

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
	"veil/core"
	"veil/inbound"
	"veil/internal/mux"
)

// Repeated admission failures and canceled target dials must not poison a
// shared lane or displace a healthy, low-frequency connection on that lane.
func TestPoolOverloadRecovery(t *testing.T) {
	for _, mode := range []string{"tls", "reality"} {
		for _, pendingDial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pending-dial=%v", mode, pendingDial), func(t *testing.T) {
				st, ct := settings(t, mode)
				st.RecordPadding, ct.RecordPadding = true, true
				dst, _ := startHandler(t, func(ctx context.Context, c net.Conn) error {
					defer c.Close()
					stop := context.AfterFunc(ctx, func() { c.Close() })
					defer stop()
					_, err := io.Copy(c, c)
					return err
				})
				entered := make(chan struct{}, mux.MaxStreams)
				server, err := core.NewServer(core.ServerConfig{
					Config: core.Config{Secret: testKey, TLS: st},
					DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
						if address == "waiting.invalid:443" {
							entered <- struct{}{}
							<-ctx.Done()
							return nil, ctx.Err()
						}
						return (&net.Dialer{}).DialContext(ctx, network, address)
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				addr, stopServer := startHandler(t, server.Handle)
				client := newCoreClient(t, core.ClientConfig{
					Config: core.Config{Secret: testKey, TLS: ct, MaxConnections: 1}, Server: addr,
				})
				forward, err := inbound.Forward(client, dst)
				if err != nil {
					t.Fatal(err)
				}
				entry, stopEntry := startHandler(t, forward)
				healthy, err := net.Dial("tcp", entry)
				if err != nil {
					t.Fatal(err)
				}
				defer healthy.Close()
				probe := func(t *testing.T) {
					t.Helper()
					healthy.SetDeadline(time.Now().Add(3 * time.Second))
					if err := writeAll(healthy, []byte("live")); err != nil {
						t.Fatal(err)
					}
					var p [4]byte
					if _, err := io.ReadFull(healthy, p[:]); err != nil || string(p[:]) != "live" {
						t.Fatalf("healthy lane interrupted: %q %v", p, err)
					}
				}
				probe(t)
				for round := range 16 {
					if !t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						var workers sync.WaitGroup
						defer workers.Wait()
						// Cleanup runs before joining pending opens, including on Fatal.
						defer cancel()
						results := make(chan error, mux.MaxStreams-1)
						for range mux.MaxStreams - 1 {
							if pendingDial {
								workers.Go(func() {
									s, err := client.Open(ctx, "waiting.invalid:443")
									if s != nil {
										s.Close()
									}
									results <- err
								})
								select {
								case <-entered:
								case <-ctx.Done():
									t.Fatal("target dial did not start")
								}
							} else {
								s, err := client.Open(ctx, dst)
								if err != nil {
									t.Fatal(err)
								}
								defer s.Close()
							}
						}
						if p := client.PoolStats(); p.Total != 1 || p.Streams != mux.MaxStreams {
							t.Fatal("pool was not full", p)
						}
						failures := make(chan error, 64)
						for range cap(failures) {
							workers.Go(func() {
								s, err := client.Open(ctx, dst)
								if s != nil {
									s.Close()
								}
								failures <- err
							})
						}
						for range cap(failures) {
							if err := <-failures; err == nil || ctx.Err() != nil {
								t.Fatalf("overload did not reject within budget: %v", err)
							}
						}
						probe(t)
						cancel()
						if pendingDial {
							for range mux.MaxStreams - 1 {
								if err := <-results; err == nil {
									t.Fatal("canceled target dial succeeded")
								}
							}
						}
					}) {
						return
					}
					await(t, func() bool {
						p := client.PoolStats()
						return p.Total == 1 && p.Streams == 1 && !p.Dialing && server.Stats.ActiveStreams.Load() == 1
					})
					probe(t)
				}
				if n := server.Stats.Authenticated.Load(); n != 1 {
					t.Fatalf("overload replaced healthy lane: %d handshakes", n)
				}
				healthy.Close()
				stopEntry()
				client.Close()
				stopServer()
				if p := client.PoolStats(); p.Total != 0 || p.Streams != 0 || p.Dialing || server.Stats.ActiveStreams.Load() != 0 {
					t.Fatal("resources retained after stop", p)
				}
			})
		}
	}
}
