package service

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestConnectionSeparateInletsAndRelay(t *testing.T) {
	for _, chain := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "relay"}[chain], func(t *testing.T) {
			st, ct := settings(t, "tls")
			server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
			cfg := Config{Role: "client", Server: addr, Secret: testKey, TLS: ct}
			var relay *Config
			if chain {
				_, raddr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
				r := Config{Role: "client", Server: raddr, Secret: testKey, TLS: ct}
				ca, err := os.ReadFile(ct.CAFile)
				if err != nil {
					t.Fatal(err)
				}
				r.TLS.CAPEM, r.TLS.CAFile = string(ca), ""
				relay = &r
			}
			c, err := OpenConnection(cfg, []Inlet{{"socks", connectionAddress(t)}, {"http", connectionAddress(t)}}, relay)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			echo := target(t, func(n net.Conn) { b, _ := io.ReadAll(n); writeAll(n, b) })
			conn, err := socksDial(c.inlets[0].Listen, echo)
			if err != nil {
				t.Fatal(err)
			}
			halfEcho(t, conn)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			defer origin.Close()
			proxy, _ := url.Parse("http://" + c.inlets[1].Listen)
			tr := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			response, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Get(origin.URL)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 204 {
				t.Fatal(response.Status)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err = c.ProbeHTTP(ctx, origin.URL); err != nil {
				t.Fatal(err)
			}
			if server.Stats.Authenticated.Load() != 1 {
				t.Fatal("inlets and probe did not share one tunnel", server.Stats.Authenticated.Load())
			}
			await(t, func() bool { return c.Snapshot().Stats.ActiveStreams == 0 })
			snapshot := c.Snapshot()
			if snapshot.Diagnostics.Pool.Total != 1 || snapshot.Diagnostics.Pool.Idle != 1 || snapshot.Diagnostics.Pool.Streams != 0 {
				t.Fatalf("pool accounting after transfer: %+v", snapshot.Diagnostics.Pool)
			}
			if chain && (snapshot.Relay == nil || snapshot.Relay.Diagnostics.Pool == nil || snapshot.Relay.Stats.ActiveStreams != 1) {
				t.Fatalf("missing independent relay diagnostics: %+v", snapshot.Relay)
			}
			if !chain && snapshot.Relay != nil {
				t.Fatal("direct connection reports a relay")
			}
		})
	}
}
func TestConnectionBindFailureReleasesOtherListeners(t *testing.T) {
	_, ct := settings(t, "tls")
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	_, err = OpenConnection(Config{Role: "client", Server: "127.0.0.1:9", Secret: testKey, TLS: ct}, []Inlet{{"socks", "127.0.0.1:0"}, {"http", occupied.Addr().String()}}, nil)
	if err == nil {
		t.Fatal("bind conflict ignored")
	}
}

func connectionAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}
func TestRemoveOneInletKeepsOtherStreamAndPool(t *testing.T) {
	st, ct := settings(t, "tls")
	_, server := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	entries := []Inlet{{"socks", connectionAddress(t)}, {"http", connectionAddress(t)}}
	c, err := OpenConnection(Config{Role: "client", Server: server, Secret: testKey, TLS: ct}, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	dst := target(t, func(n net.Conn) { b, _ := io.ReadAll(n); writeAll(n, b) })
	conn, err := socksDial(entries[0].Listen, dst)
	if err != nil {
		t.Fatal(err)
	}
	client := c.client
	if err = c.SetInlets(entries[:1]); err != nil {
		t.Fatal(err)
	}
	halfEcho(t, conn)
	if c.client != client {
		t.Fatal("removing HTTP replaced shared pool")
	}
	if err = c.SetInlets(entries); err != nil {
		t.Fatal(err)
	}
	if c.client != client {
		t.Fatal("adding HTTP replaced shared pool")
	}
}
