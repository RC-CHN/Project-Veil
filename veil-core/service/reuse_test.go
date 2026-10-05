package service

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
	"veil/core"
	"veil/inbound"
)

// The peer can disappear after pool selection and before OPEN is written.
// Only the first physical connection is cut, with no application DATA sent.
type cutOnWriteConn struct {
	net.Conn
	cut atomic.Bool
}

func (c *cutOnWriteConn) Write(p []byte) (int, error) {
	if c.cut.Load() {
		c.Conn.Close()
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

func TestReusedTransportFailureBeforeData(t *testing.T) {
	for _, mode := range []string{"tls", "reality"} {
		t.Run(mode, func(t *testing.T) {
			st, ct := settings(t, mode)
			server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
			dst := target(t, func(c net.Conn) { io.Copy(c, c) })
			var first *cutOnWriteConn
			var attempts atomic.Int32
			client := newCoreClient(t, core.ClientConfig{
				Config: core.Config{Secret: testKey, TLS: ct}, Server: addr,
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					c, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err == nil && attempts.Add(1) == 1 {
						first = &cutOnWriteConn{Conn: c}
						return first, nil
					}
					return c, err
				},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			warm, err := client.Open(ctx, dst)
			if err != nil {
				t.Fatal(err)
			}
			// Finish a real stream so no delayed RESET write can trigger the cut.
			entry, _ := startHandler(t, func(_ context.Context, c net.Conn) error { return warm.Relay(c) })
			app, err := net.Dial("tcp", entry)
			if err != nil {
				t.Fatal(err)
			}
			halfEchoPayload(t, app, []byte("before"))
			await(t, func() bool { return client.PoolStats().Streams == 0 })
			first.cut.Store(true)
			forward, err := inbound.Forward(client, dst)
			if err != nil {
				t.Fatal(err)
			}
			entry, _ = startHandler(t, forward)
			app, err = net.Dial("tcp", entry)
			if err != nil {
				t.Fatal(err)
			}
			halfEchoPayload(t, app, []byte("after: delivered exactly once"))
			if attempts.Load() != 2 || server.Stats.Authenticated.Load() != 2 {
				t.Fatal("expected exactly one replacement transport")
			}
			if client.PoolStats().OpenRetries != 1 {
				t.Fatal("transport replacement was not counted")
			}
		})
	}
}

func TestOpenRetryDoesNotLoopOrRetryAuthentication(t *testing.T) {
	for _, badAuth := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement dial fails", true: "fresh authentication fails"}[badAuth], func(t *testing.T) {
			st, ct := settings(t, "tls")
			_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
			dst := target(t, func(c net.Conn) { io.Copy(c, c) })
			key := testKey
			if badAuth {
				key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			}
			var first *cutOnWriteConn
			var attempts atomic.Int32
			client := newCoreClient(t, core.ClientConfig{
				Config: core.Config{Secret: key, TLS: ct}, Server: addr,
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					if attempts.Add(1) > 1 {
						return nil, errors.New("replacement dial failed")
					}
					c, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err == nil {
						first = &cutOnWriteConn{Conn: c}
						return first, nil
					}
					return nil, err
				},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if !badAuth {
				warm, err := client.Open(ctx, dst)
				if err != nil {
					t.Fatal(err)
				}
				entry, _ := startHandler(t, func(_ context.Context, c net.Conn) error { return warm.Relay(c) })
				app, err := net.Dial("tcp", entry)
				if err != nil {
					t.Fatal(err)
				}
				halfEchoPayload(t, app, []byte("warm"))
				await(t, func() bool { return client.PoolStats().Streams == 0 })
				first.cut.Store(true)
			}
			if s, err := client.Open(ctx, dst); err == nil {
				s.Close()
				t.Fatal("failed transport unexpectedly opened a stream")
			}
			want := int32(2)
			if badAuth {
				want = 1
			}
			if attempts.Load() != want || client.PoolStats().OpenRetries != uint64(want-1) {
				t.Fatalf("unbounded or unsafe retry: %+v", client.PoolStats())
			}
		})
	}
}
