package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
	"veil/core"
	"veil/inbound"
	"veil/internal/transport"
	"veil/internal/wire"
)

func startHandler(t *testing.T, handler inbound.Handler) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inbound.Serve(ctx, ln, handler, inbound.ServeOptions{}) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("listener did not stop")
		}
	})
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

func newCoreClient(t *testing.T, cfg core.ClientConfig) *core.Client {
	t.Helper()
	client, err := core.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestCoreReuseAfterQuietInterval(t *testing.T) {
	for _, mode := range []string{"tls", "reality"} {
		for _, closed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/closed=%v", mode, closed), func(t *testing.T) {
				st, ct := settings(t, mode)
				server, err := core.NewServer(core.ServerConfig{Config: core.Config{Secret: testKey, TLS: st}})
				if err != nil {
					t.Fatal(err)
				}
				accepted := make(chan net.Conn, 4)
				addr, _ := startHandler(t, func(ctx context.Context, c net.Conn) error { accepted <- c; return server.Handle(ctx, c) })
				client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct, MaxConnections: 1}, Server: addr})
				dst := target(t, func(c net.Conn) { p, _ := io.ReadAll(c); writeAll(c, p) })
				handler, _ := inbound.Forward(client, dst)
				entry, _ := startHandler(t, handler)
				for i := 0; i < 2; i++ {
					c, err := net.Dial("tcp", entry)
					if err != nil {
						t.Fatal(err)
					}
					halfEchoPayload(t, c, []byte("idle reuse"))
					await(t, func() bool { return client.PoolStats().Idle == 1 })
					if i == 0 {
						raw := <-accepted
						if closed {
							raw.Close()
						}
						time.Sleep(1100 * time.Millisecond)
					}
				}
				want := uint64(1)
				if closed {
					want = 2
				}
				if server.Stats.Authenticated.Load() != want {
					t.Fatal("unexpected redial or poisoned TLS read", server.Stats.Authenticated.Load())
				}
			})
		}
	}
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(end) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func halfEcho(t *testing.T, conn net.Conn) {
	t.Helper()
	halfEchoPayload(t, conn, bytes.Repeat([]byte("half-close"), 20000))
}

func halfEchoPayload(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := writeAll(conn, payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo len=%d: %v", len(got), err)
	}
}

func TestSharedCoreAcrossInbounds(t *testing.T) {
	st, ct := settings(t, "tls")
	dst := target(t, func(c net.Conn) { p, _ := io.ReadAll(c); writeAll(c, p) })
	dials := make(chan string, 8)
	server, err := core.NewServer(core.ServerConfig{
		Config: core.Config{Secret: testKey, TLS: st},
		DialContext: func(ctx context.Context, n, a string) (net.Conn, error) {
			dials <- a
			return (&net.Dialer{}).DialContext(ctx, n, a)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := startHandler(t, server.Handle)
	client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct, MaxConnections: 1}, Server: addr})
	forward, err := inbound.Forward(client, dst)
	if err != nil {
		t.Fatal(err)
	}
	fwd, stopForward := startHandler(t, forward)
	socks, _ := startHandler(t, inbound.SOCKS5(client, time.Second))
	for i := 0; i < 4; i++ {
		var conn net.Conn
		if i%2 == 0 {
			conn, err = net.Dial("tcp", fwd)
		} else {
			conn, err = socksDial(socks, dst)
		}
		if err != nil {
			t.Fatal(err)
		}
		halfEcho(t, conn)
		await(t, func() bool { return client.PoolStats().Idle == 1 && client.Stats.Completed.Load() == uint64(i+1) })
		if i == 2 {
			stopForward()
		} // stopping one inlet must not close the shared pool
	}
	if server.Stats.Authenticated.Load() != 1 || client.Stats.Completed.Load() != 4 {
		t.Fatal("inbounds did not share one reusable TLS connection")
	}
	if len(dials) != 4 {
		t.Fatal("destination dial hook bypassed")
	}
	for range 4 {
		if <-dials != dst {
			t.Fatal("wrong destination")
		}
	}
}

func TestForwardCLIConfig(t *testing.T) {
	st, ct := settings(t, "tls")
	dst := target(t, func(c net.Conn) { p, _ := io.ReadAll(c); writeAll(c, p) })
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, Target: dst})
	c, err := net.Dial("tcp", entry)
	if err != nil {
		t.Fatal(err)
	}
	halfEcho(t, c)
	for _, cfg := range []Config{
		{Role: "server", Secret: testKey, Target: dst},
		{Role: "client", Secret: testKey, Server: addr, Target: "missing-port"},
	} {
		if cfg.Defaults() == nil {
			t.Fatal("invalid forward config accepted")
		}
	}
}

func TestCoreCancelPendingDial(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "client-close"}[closeClient], func(t *testing.T) {
			_, ct := settings(t, "tls")
			entered := make(chan struct{})
			client := newCoreClient(t, core.ClientConfig{
				Config: core.Config{Secret: testKey, TLS: ct}, Server: "127.0.0.1:1",
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					close(entered)
					<-ctx.Done()
					return nil, ctx.Err()
				},
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.Open(ctx, "example.test:443"); done <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("dial hook not entered")
			}
			if closeClient {
				client.Close()
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("dial ignored cancellation")
			}
			if client.PoolStats().Total != 0 {
				t.Fatal("dial reservation leaked")
			}
		})
	}
}

func TestCoreAbandonOpen(t *testing.T) {
	st, ct := settings(t, "tls")
	dst := target(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct, MaxConnections: 1}, Server: addr})
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := client.Open(ctx, dst)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if excess, err := client.Open(ctx, dst); err == nil {
			excess.Close()
			t.Fatal("aggregate pool limit exceeded")
		}
		if i == 0 {
			stream.Close()
		} else if i == 1 {
			cancel()
		} else {
			client.Close()
		}
		await(t, func() bool { return client.PoolStats().Total == 0 })
		cancel()
		stream.Close()
	}
	if server.Stats.Authenticated.Load() != 3 {
		t.Fatal("abandoned stream was reused")
	}
}

func streamPair(t *testing.T, stream *core.Stream) (*net.TCPConn, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peer, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	local, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- stream.Relay(local) }()
	return peer.(*net.TCPConn), done
}

func relayResult(t *testing.T, done <-chan error, success bool) {
	t.Helper()
	select {
	case err := <-done:
		if (err == nil) != success {
			t.Fatalf("relay success=%v: %v", success, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not exit")
	}
}

func TestCoreLateCloseAndActiveCancel(t *testing.T) {
	st, ct := settings(t, "tls")
	dst := target(t, func(c net.Conn) { p, _ := io.ReadAll(c); writeAll(c, p) })
	server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct, MaxConnections: 1}, Server: addr})
	var previous *core.Stream
	for i := range 10 {
		stream, err := client.Open(context.Background(), dst)
		if err != nil {
			t.Fatal(err)
		}
		if previous != nil {
			previous.Close()
		} // the physical connection is now leased again
		peer, done := streamPair(t, stream)
		// Alternate contents and lengths across reuse, including a large-to-small
		// transition, so stale scratch bytes would fail the end-to-end comparison.
		halfEchoPayload(t, peer, bytes.Repeat([]byte{byte(i)}, (10-i)*19001))
		relayResult(t, done, true)
		previous = stream
	}
	previous.Close()
	if server.Stats.Authenticated.Load() != 1 {
		t.Fatal("late Close destroyed reused session")
	}
	stream, err := client.Open(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	_, aborted := streamPair(t, stream)
	stream.Close()
	relayResult(t, aborted, false)
	if client.PoolStats().Total != 0 {
		t.Fatal("aborted relay was retained")
	}
	stream, err = client.Open(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	peer, done := streamPair(t, stream)
	client.Close()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("active local socket stayed open")
	}
	relayResult(t, done, false)
	if client.PoolStats().Total != 0 {
		t.Fatal("closed client retained sessions")
	}
}

func TestSOCKSHandlerCancelNegotiation(t *testing.T) {
	_, ct := settings(t, "tls")
	client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct}, Server: "127.0.0.1:1"})
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inbound.SOCKS5(client, time.Minute)(ctx, a) }()
	cancel()
	relayResult(t, done, false)
}

func TestCoreCloseAfterRemoteFIN(t *testing.T) {
	st, ct := settings(t, "tls")
	dst := target(t, func(c net.Conn) {
		c.(*net.TCPConn).CloseWrite()
		io.Copy(io.Discard, c)
	})
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct}, Server: addr})
	stream, err := client.Open(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	peer, done := streamPair(t, stream)
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := io.Copy(io.Discard, peer); n != 0 || err != nil {
		t.Fatalf("remote FIN: %d, %v", n, err)
	}
	// The remote reader has exited successfully; the local reader is still
	// blocked. Closing only TLS would leave Relay waiting for the idle timeout.
	stream.Close()
	relayResult(t, done, false)
	if client.PoolStats().Total != 0 {
		t.Fatal("aborted half-closed stream was reused")
	}
}

// A FIN without DONE is not the reuse barrier. Also exercise cancellation
// while Open is waiting for OPEN_OK, rather than only during TCP dialing.
func TestCoreIncompleteControlExchange(t *testing.T) {
	for _, phase := range []string{"open", "done"} {
		t.Run(phase, func(t *testing.T) {
			st, ct := settings(t, "tls")
			h, err := transport.Server(st, time.Second, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			waiting := make(chan struct{})
			addr, _ := startHandler(t, func(ctx context.Context, raw net.Conn) error {
				stop := context.AfterFunc(ctx, func() { raw.Close() })
				defer stop()
				defer raw.Close()
				c, err := h(ctx, raw)
				if err != nil {
					return err
				}
				defer c.Close()
				r := wire.Reader{R: c}
				defer r.Release()
				if typ, _, err := r.Read(); err != nil || typ != wire.Auth {
					return wire.ErrProtocol
				}
				if typ, _, err := r.Read(); err != nil || typ != wire.Open {
					return wire.ErrProtocol
				}
				if phase == "done" {
					if err := wire.Write(c, wire.OpenOK, nil); err != nil {
						return err
					}
					if err := wire.Expect(&r, wire.Fin); err != nil {
						return err
					}
					if err := wire.Write(c, wire.Fin, nil); err != nil {
						return err
					}
				}
				close(waiting)
				_, err = io.Copy(io.Discard, c)
				return err
			})
			client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct}, Server: addr})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			if phase == "open" {
				go func() { _, err := client.Open(ctx, "example.test:443"); done <- err }()
			} else {
				stream, err := client.Open(ctx, "example.test:443")
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				peer, relayed := streamPair(t, stream)
				peer.CloseWrite()
				go func() { done <- <-relayed }()
			}
			select {
			case <-waiting:
			case <-time.After(3 * time.Second):
				t.Fatal("exchange stalled")
			}
			cancel()
			relayResult(t, done, false)
			if client.PoolStats().Total != 0 || client.Stats.Completed.Load() != 0 {
				t.Fatal("incomplete exchange reused")
			}
		})
	}
}
