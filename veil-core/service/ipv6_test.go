package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
	"veil/core"
	"veil/inbound"
)

func TestIPv6SOCKSAndCONNECT(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	probe.Close()
	for _, mode := range []string{"tls", "reality"} {
		t.Run(mode, func(t *testing.T) {
			serve := func(handler inbound.Handler) string {
				t.Helper()
				ln, err := net.Listen("tcp6", "[::1]:0")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- inbound.Serve(ctx, ln, handler, inbound.ServeOptions{}) }()
				t.Cleanup(func() {
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(3 * time.Second):
						t.Error("IPv6 listener failed to join")
					}
				})
				return ln.Addr().String()
			}
			st, ct := settings(t, mode)
			s, err := core.NewServer(core.ServerConfig{Config: core.Config{Secret: testKey, TLS: st}})
			if err != nil {
				t.Fatal(err)
			}
			server := serve(s.Handle)
			client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct}, Server: server})
			entry := serve(inbound.Mixed(client, 3*time.Second))
			target := serve(func(ctx context.Context, c net.Conn) error {
				defer c.Close()
				stop := context.AfterFunc(ctx, func() { c.Close() })
				defer stop()
				p, err := io.ReadAll(io.LimitReader(c, 65537))
				if err != nil {
					return err
				}
				return writeAll(c, p)
			})
			for _, kind := range []string{"SOCKS IPv6", "SOCKS invalid domain", "HTTP CONNECT"} {
				t.Run(kind, func(t *testing.T) {
					conn, err := net.DialTimeout("tcp6", entry, 3*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(3 * time.Second))
					var reader io.Reader = conn
					switch kind {
					case "HTTP CONNECT":
						if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
							t.Fatal(err)
						}
						r := bufio.NewReader(conn)
						response, err := http.ReadResponse(r, nil)
						if err != nil {
							t.Fatal(err)
						}
						if response.StatusCode != http.StatusOK {
							t.Fatal("CONNECT status", response.StatusCode)
						}
						reader = r
					case "SOCKS invalid domain":
						// ATYP=3 carries a domain, not a colon-containing IPv6
						// literal. Reject it without disturbing the shared tunnel.
						if err := soakSOCKS(conn, target); !errors.Is(err, io.EOF) {
							t.Fatalf("invalid SOCKS domain: %v", err)
						}
						return
					default:
						if err := writeAll(conn, []byte{5, 1, 0}); err != nil {
							t.Fatal(err)
						}
						var method [2]byte
						if _, err := io.ReadFull(conn, method[:]); err != nil || method != [2]byte{5, 0} {
							t.Fatalf("SOCKS method %x: %v", method, err)
						}
						host, port, _ := net.SplitHostPort(target)
						n, _ := strconv.Atoi(port)
						request := append([]byte{5, 1, 0, 4}, net.ParseIP(host).To16()...)
						request = binary.BigEndian.AppendUint16(request, uint16(n))
						if err := writeAll(conn, request); err != nil {
							t.Fatal(err)
						}
						var reply [10]byte
						if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[0] != 5 || reply[1] != 0 || reply[3] != 1 {
							t.Fatalf("SOCKS reply %x: %v", reply, err)
						}
					}
					payload := bytes.Repeat([]byte{0, 255, 37, 128}, 16384)
					if err := writeAll(conn, payload); err != nil {
						t.Fatal(err)
					}
					if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(reader)
					if err != nil || !bytes.Equal(got, payload) {
						t.Fatalf("IPv6 half-close echo: %d bytes, %v", len(got), err)
					}
				})
			}
			await(t, func() bool { return client.PoolStats().Streams == 0 && s.Stats.ActiveStreams.Load() == 0 })
			if got := s.Stats.Authenticated.Load(); got != 1 {
				t.Fatalf("IPv6 requests did not reuse their TLS connection: %d handshakes", got)
			}
		})
	}
}
