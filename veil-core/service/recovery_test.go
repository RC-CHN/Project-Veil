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

// A missing OPEN reply retires the lane for new work without aborting an
// existing transfer. Explicit destination errors and caller deadlines do not.
func TestCoreRecoveryAfterOpenFailure(t *testing.T) {
	for _, failure := range []string{"missing reply", "caller deadline", "target timeout", "target rejected"} {
		t.Run(failure, func(t *testing.T) {
			st, ct := settings(t, "tls")
			dst := target(t, func(c net.Conn) { io.Copy(c, c) })
			server, err := core.NewServer(core.ServerConfig{
				Config: core.Config{Secret: testKey, TLS: st, DialTimeout: 3 * time.Second},
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					if address == "failure.invalid:443" {
						switch failure {
						case "target timeout":
							return nil, context.DeadlineExceeded
						case "target rejected":
							return nil, errors.New("injected destination failure")
						default:
							<-ctx.Done()
							return nil, ctx.Err()
						}
					}
					return (&net.Dialer{}).DialContext(ctx, network, address)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			addr, _ := startHandler(t, server.Handle)
			client := newCoreClient(t, core.ClientConfig{
				Config: core.Config{Secret: testKey, TLS: ct, MaxConnections: 2, HandshakeTimeout: 300 * time.Millisecond, DialTimeout: 50 * time.Millisecond}, Server: addr,
			})
			forward, err := inbound.Forward(client, dst)
			if err != nil {
				t.Fatal(err)
			}
			entry, _ := startHandler(t, forward)
			healthy, err := net.Dial("tcp", entry)
			if err != nil {
				t.Fatal(err)
			}
			defer healthy.Close()
			healthy.SetDeadline(time.Now().Add(3 * time.Second))
			echo := func() {
				t.Helper()
				if _, err := healthy.Write([]byte("live")); err != nil {
					t.Fatal(err)
				}
				var b [4]byte
				if _, err := io.ReadFull(healthy, b[:]); err != nil || string(b[:]) != "live" {
					t.Fatalf("existing transfer was interrupted: %q %v", b, err)
				}
			}
			echo()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if failure == "caller deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 30*time.Millisecond)
				defer stop()
			}
			if stream, err := client.Open(ctx, "failure.invalid:443"); err == nil {
				stream.Close()
				t.Fatal("injected failure succeeded")
			}
			fresh, err := client.Open(context.Background(), dst)
			if err != nil {
				t.Fatal("subsequent open did not recover:", err)
			}
			fresh.Close()
			want := uint64(1)
			if failure == "missing reply" {
				want = 2
			}
			if got := server.Stats.Authenticated.Load(); got != want {
				t.Fatalf("physical connections: got %d want %d", got, want)
			}
			echo()
			healthy.Close()
			await(t, func() bool { p := client.PoolStats(); return p.Total == 1 && p.Streams == 0 })
		})
	}
}

// Drop bytes after a healthy handshake without delivering EOF/RST. A real
// blackhole must not be modeled as a clean close, which the pool already detects.
type blackholeConn struct {
	net.Conn
	drop atomic.Bool
}

func (c *blackholeConn) Read(p []byte) (int, error) {
	for {
		n, err := c.Conn.Read(p)
		if !c.drop.Load() {
			return n, err
		}
		if err != nil {
			return 0, err
		}
	}
}

func (c *blackholeConn) Write(p []byte) (int, error) {
	if c.drop.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func TestDoubleHopRecoversFromBlackhole(t *testing.T) {
	st, ct := settings(t, "reality")
	dst := target(t, func(c net.Conn) { p, _ := io.ReadAll(c); writeAll(c, p) })
	newServer := func() *core.Server {
		t.Helper()
		s, err := core.NewServer(core.ServerConfig{Config: core.Config{Secret: testKey, TLS: st}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	exit := newServer()
	exitAddr, _ := startHandler(t, exit.Handle)
	relay := newServer()
	accepted := make(chan *blackholeConn, 8)
	relayAddr, _ := startHandler(t, func(ctx context.Context, c net.Conn) error {
		wire := &blackholeConn{Conn: c}
		accepted <- wire
		return relay.Handle(ctx, wire)
	})
	outer := newCoreClient(t, core.ClientConfig{
		Config: core.Config{Secret: testKey, TLS: ct, HandshakeTimeout: 300 * time.Millisecond, DialTimeout: 100 * time.Millisecond}, Server: relayAddr,
	})
	forward, err := inbound.Forward(outer, exitAddr)
	if err != nil {
		t.Fatal(err)
	}
	hop, _ := startHandler(t, forward)
	inner := newCoreClient(t, core.ClientConfig{
		Config: core.Config{Secret: testKey, TLS: ct, HandshakeTimeout: time.Second, DialTimeout: 100 * time.Millisecond}, Server: hop,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	warm, err := inner.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	warm.Close()
	(<-accepted).drop.Store(true)
	if s, err := inner.Open(ctx, dst); err == nil {
		s.Close()
		t.Fatal("blackhole unexpectedly replied")
	}
	// Subsequent user requests may expose the outer lane's missing OPEN reply
	// before both pools recover. No daemon restart or application replay occurs.
	var recovered *core.Stream
	for range 3 {
		recovered, err = inner.Open(ctx, dst)
		if err == nil {
			break
		}
		// Retrying immediately is intentionally coalesced after a TCP/TLS
		// failure. Wait for the advertised budget before the next user request.
		time.Sleep(time.Duration(inner.PoolStats().RetryAfterMillis) * time.Millisecond)
	}
	if err != nil {
		t.Fatal("double hop stayed on the blackholed connection:", err)
	}
	defer recovered.Close()
	if relay.Stats.Authenticated.Load() < 2 || exit.Stats.Authenticated.Load() < 2 {
		t.Fatal("recovery did not replace both physical layers")
	}
	entry, _ := startHandler(t, func(_ context.Context, c net.Conn) error { return recovered.Relay(c) })
	app, err := net.Dial("tcp", entry)
	if err != nil {
		t.Fatal(err)
	}
	halfEchoPayload(t, app, []byte("recovered through two REALITY layers"))
}

func TestCoreDialBackoffRecovery(t *testing.T) {
	st, ct := settings(t, "tls")
	server, err := core.NewServer(core.ServerConfig{Config: core.Config{Secret: testKey, TLS: st}})
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := startHandler(t, server.Handle)
	dst := target(t, func(c net.Conn) { io.Copy(c, c) })
	var available atomic.Bool
	var attempts atomic.Int32
	failure := errors.New("injected temporary network failure")
	client := newCoreClient(t, core.ClientConfig{
		Config: core.Config{Secret: testKey, TLS: ct}, Server: addr,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			attempts.Add(1)
			if !available.Load() {
				return nil, failure
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for range 2 {
		if stream, err := client.Open(ctx, dst); !errors.Is(err, failure) {
			if stream != nil {
				stream.Close()
			}
			t.Fatal("expected temporary dial failure:", err)
		}
	}
	if attempts.Load() != 1 {
		t.Fatal("requests did not share dial cooldown")
	}
	available.Store(true)
	time.Sleep(time.Duration(client.PoolStats().RetryAfterMillis) * time.Millisecond)
	stream, err := client.Open(ctx, dst)
	if err != nil {
		t.Fatal("network recovery required a restart:", err)
	}
	defer stream.Close()
	if attempts.Load() != 2 || client.PoolStats().RetryAfterMillis != 0 {
		t.Fatal("successful handshake did not clear cooldown")
	}
}
