package service

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"veil/core"
	"veil/inbound"
)

// Both layers use production defaults; in particular the outer stream must
// outlive quiet periods inside the inner TLS connection.
func doubleHopEcho(t *testing.T) (*net.TCPConn, *core.Client, *core.Client, *core.Server, *core.Server) {
	t.Helper()
	st, ct := settings(t, "reality")
	newServer := func() (*core.Server, string) {
		s, err := core.NewServer(core.ServerConfig{Config: core.Config{Secret: testKey, TLS: st}})
		if err != nil {
			t.Fatal(err)
		}
		addr, _ := startHandler(t, s.Handle)
		return s, addr
	}
	exit, exitAddr := newServer()
	relay, relayAddr := newServer()
	outer := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct}, Server: relayAddr})
	forward, err := inbound.Forward(outer, exitAddr)
	if err != nil {
		t.Fatal(err)
	}
	hop, _ := startHandler(t, forward)
	inner := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct}, Server: hop})
	entry, _ := startHandler(t, inbound.SOCKS5(inner, 10*time.Second))
	dst, _ := startHandler(t, func(ctx context.Context, c net.Conn) error {
		defer c.Close()
		stop := context.AfterFunc(ctx, func() { c.Close() })
		defer stop()
		_, err := io.Copy(c, c)
		return err
	})
	c, err := socksDial(entry, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, inner, outer, exit, relay
}

func echoProgress(t *testing.T, c net.Conn, payload []byte, timeout time.Duration) {
	t.Helper()
	c.SetDeadline(time.Now().Add(timeout))
	if err := writeAll(c, payload); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, len(payload))
	if _, err := io.ReadFull(c, p); err != nil || !bytes.Equal(p, payload) {
		t.Fatalf("long-lived stream lost data: %v", err)
	}
	c.SetDeadline(time.Time{})
}

// Opt in: exercises real wall-clock silence, beyond the old two-minute limit.
func TestQuietDoubleHopDefaults(t *testing.T) {
	text := os.Getenv("VEIL_QUIET_DURATION")
	if text == "" {
		t.Skip("set VEIL_QUIET_DURATION=3m for the quiet connection regression")
	}
	pause, err := time.ParseDuration(text)
	if err != nil || pause <= 2*time.Minute || pause >= core.DefaultPoolTimeout {
		t.Fatal("quiet duration must be >2m and < default pool timeout")
	}
	c, inner, outer, exit, relay := doubleHopEcho(t)
	echoProgress(t, c, []byte("before silence"), 3*time.Second)
	time.Sleep(pause)
	echoProgress(t, c, []byte("same stream after silence"), 3*time.Second)
	halfEchoPayload(t, c, []byte("half-close after quiet period"))
	await(t, func() bool { return inner.PoolStats().Streams == 0 })
	if exit.Stats.Authenticated.Load() != 1 || relay.Stats.Authenticated.Load() != 1 || inner.PoolStats().DialAttempts != 1 || outer.PoolStats().DialAttempts != 1 {
		t.Fatal("quiet connection was replaced instead of preserved")
	}
}

// Opt in and confined to an explicitly created test namespace. Kernel packet
// loss exercises retransmission, unlike a userspace wrapper that discards bytes
// after the TCP stack already acknowledged them. Never alter the host qdisc.
func TestRecoverableNetworkPauses(t *testing.T) {
	text := os.Getenv("VEIL_NETEM_PAUSES")
	if text == "" {
		t.Skip("set VEIL_NETEM_PAUSES=5s,15s,30s inside a veil-test- netns")
	}
	namespace, err := exec.Command("ip", "netns", "identify", strconv.Itoa(os.Getpid())).Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(namespace)), "veil-test-") {
		t.Fatal("packet loss requires an isolated veil-test- network namespace")
	}
	for _, text := range strings.Split(text, ",") {
		pause, err := time.ParseDuration(text)
		if err != nil || pause <= 0 || pause > 30*time.Second {
			t.Fatal("network pauses must be >0 and <=30s")
		}
		t.Run(text, func(t *testing.T) {
			c, inner, outer, exit, relay := doubleHopEcho(t)
			echoProgress(t, c, []byte("before packet loss"), 3*time.Second)
			if out, err := exec.Command("tc", "qdisc", "replace", "dev", "lo", "root", "netem", "loss", "100%").CombinedOutput(); err != nil {
				t.Fatalf("install packet loss: %v: %s", err, out)
			}
			restored := make(chan error, 1)
			go func() {
				time.Sleep(pause)
				restored <- exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run()
			}()
			t.Cleanup(func() {
				if err := <-restored; err != nil {
					t.Error("restore test network:", err)
				}
			})
			start := time.Now()
			echoProgress(t, c, bytes.Repeat([]byte("sequence/integrity\x00"), 1024), 100*time.Second)
			t.Logf("%s total packet loss; original stream resumed in %s", pause, time.Since(start))
			halfEchoPayload(t, c, []byte("completed without replay"))
			await(t, func() bool { return inner.PoolStats().Streams == 0 })
			if exit.Stats.Authenticated.Load() != 1 || relay.Stats.Authenticated.Load() != 1 || inner.PoolStats().DialAttempts != 1 || outer.PoolStats().DialAttempts != 1 {
				t.Fatal("recovered traffic opened a replacement connection")
			}
		})
	}
}
