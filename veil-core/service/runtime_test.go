package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeRelayAndLifecycle(t *testing.T) {
	st, ct := settings(t, "tls")
	dst := target(t, func(c net.Conn) { b, _ := io.ReadAll(c); writeAll(c, b) })
	var server, client Runtime
	t.Cleanup(func() { client.Close(); server.Close() })
	if err := server.Start(Config{Role: "server", Listen: "127.0.0.1:0", Secret: testKey, TLS: st}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Role: "client", Listen: "127.0.0.1:0", Server: server.Snapshot().Listen, Secret: testKey, TLS: ct}
	if err := client.Start(cfg); err != nil {
		t.Fatal(err)
	}
	addr := client.Snapshot().Listen
	for range 3 {
		c, err := socksDial(addr, dst)
		if err != nil {
			t.Fatal(err)
		}
		halfEcho(t, c)
	}
	await(t, func() bool { return client.Snapshot().Stats.Completed == 3 })
	if n := server.Snapshot().Stats.Authenticated; n != 1 {
		t.Fatalf("TLS pool not reused: %d", n)
	}
	bad := cfg
	bad.TLS.Mode = "bad"
	if client.Restart(bad) == nil || client.Snapshot().Listen != addr || client.Snapshot().State != "running" {
		t.Fatal("invalid candidate interrupted the running instance")
	}
	// Stop must close a SOCKS negotiation which is waiting for further input.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	await(t, func() bool { return client.Snapshot().Stats.Accepted == 4 })
	client.Stop()
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("active socket survived stop")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener not released: %v", err)
	}
	ln.Close()
	if err := client.Start(cfg); err != nil {
		t.Fatal(err)
	}
	// A bind failure after a requested restart leaves an honest stopped state.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg.Listen = busy.Addr().String()
	if client.Restart(cfg) == nil || client.Snapshot().State != "stopped" {
		t.Fatal("bind conflict not reported")
	}
	client.Close()
	if client.Start(cfg) == nil {
		t.Fatal("closed runtime restarted")
	}
}

func TestRuntimeConcurrentClose(t *testing.T) {
	_, ct := settings(t, "tls")
	cfg := Config{Role: "client", Listen: "127.0.0.1:0", Server: "127.0.0.1:9", Secret: testKey, TLS: ct}
	var r Runtime
	defer r.Close()
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			switch i % 4 {
			case 0:
				r.Start(cfg)
			case 1:
				r.Stop()
			case 2:
				r.Snapshot()
			case 3:
				r.Close()
			}
		})
	}
	wg.Wait()
	if r.Snapshot().State != "stopped" || r.Start(cfg) == nil {
		t.Fatal("close not terminal")
	}
}

func TestRuntimeReportsConnectionFailure(t *testing.T) {
	st, ct := settings(t, "tls")
	var server, client Runtime
	defer server.Close()
	defer client.Close()
	if err := server.Start(Config{Role: "server", Listen: "127.0.0.1:0", Secret: testKey, TLS: st}); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(Config{Role: "client", Listen: "127.0.0.1:0", Server: server.Snapshot().Listen, Secret: testKey, TLS: ct}); err != nil {
		t.Fatal(err)
	}
	unavailable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dst := unavailable.Addr().String()
	unavailable.Close()
	if c, err := socksDial(client.Snapshot().Listen, dst); err == nil {
		c.Close()
		t.Fatal("failed target accepted")
	}
	await(t, func() bool {
		return strings.Contains(client.Snapshot().LastConnectionError, "target connection refused") && strings.Contains(server.Snapshot().LastConnectionError, "target dial")
	})
	client.Stop()
	server.Stop() // callback must not acquire Runtime's lifecycle lock
	if client.Snapshot().LastConnectionError == "" {
		t.Fatal("failure lost at stop")
	}
}

func TestStrictConfig(t *testing.T) {
	_, ct := settings(t, "tls")
	b, _ := json.Marshal(Config{Role: "client", Secret: testKey, TLS: ct, Server: "127.0.0.1:9"})
	for _, input := range [][]byte{
		[]byte("null"), append(bytes.Clone(b), []byte(" {}")...),
		[]byte(`{"unknown":true}`), append(bytes.Clone(b), []byte(strings.Repeat(" ", MaxConfigSize))...),
	} {
		if _, err := Parse(bytes.NewReader(input)); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	cfg, err := Parse(bytes.NewReader(b))
	if err != nil || Validate(cfg) != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cfg.Listen = "127.0.0.1:not-a-port"
	if Validate(cfg) == nil {
		t.Fatal("invalid listener accepted")
	}
}
