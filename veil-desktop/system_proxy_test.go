package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"veil-desktop/internal/systemproxy"
	"veil-service/control"
	"veil/core"
	"veil/service"
)

type desktopProxyBackend struct{ current systemproxy.Snapshot }

func (*desktopProxyBackend) Name() string                          { return "test" }
func (b *desktopProxyBackend) Read() (systemproxy.Snapshot, error) { return b.current, nil }
func (b *desktopProxyBackend) Write(s systemproxy.Snapshot) error  { b.current = s; return nil }
func (*desktopProxyBackend) Manual(s systemproxy.Snapshot, endpoint string) systemproxy.Snapshot {
	s["server"] = endpoint
	return s
}
func (*desktopProxyBackend) Direct(s systemproxy.Snapshot) systemproxy.Snapshot {
	s["server"] = ""
	return s
}

func proxyApp(t *testing.T) (*App, *desktopProxyBackend) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "desktop")
	a, err := openApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.close)
	b := &desktopProxyBackend{current: systemproxy.Snapshot{"server": "original"}}
	a.proxy, err = systemproxy.Open(filepath.Join(dir, "test-proxy.json"), b)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func saveProxyProfile(t *testing.T, a *App, inbound, listen string) string {
	t.Helper()
	b, err := json.Marshal(service.Config{Role: "client", Inbound: inbound, Listen: listen, Server: "127.0.0.1:9",
		Secret: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), TLS: core.TLSConfig{Mode: "tls", ServerName: "test.example"}})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Request(control.Request{Version: 1, Action: "save", Config: b})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	return r.Revision
}

func TestSystemProxyUsesRunningProfile(t *testing.T) {
	a, b := proxyApp(t)
	revision := saveProxyProfile(t, a, "mixed", "127.0.0.1:0")
	r := a.Request(control.Request{Version: 1, Action: "start", ExpectedRevision: &revision})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	listen := r.Status.Listen
	if b.current["server"] != "original" {
		t.Fatal("default mode changed system settings")
	}
	// Saving a different inbound must not change the active listener's type.
	revision = saveProxyProfile(t, a, "socks", "127.0.0.1:0")
	if err := a.SetSystemProxyMode("auto"); err != nil {
		t.Fatal(err)
	}
	if b.current["server"] != listen {
		t.Fatal("did not select the running HTTP listener")
	}
	r = a.Request(control.Request{Version: 1, Action: "restart", ExpectedRevision: &revision})
	if r.Error == nil || r.Status.State != "stopped" {
		t.Fatalf("automatic SOCKS-only start: %+v", r)
	}
	if b.current["server"] != "original" {
		t.Fatal("failed automatic start did not restore settings")
	}
}

func TestFailedRestartRestoresSystemProxy(t *testing.T) {
	a, b := proxyApp(t)
	if err := a.SetSystemProxyMode("auto"); err != nil {
		t.Fatal(err)
	}
	revision := saveProxyProfile(t, a, "http", "127.0.0.1:0")
	r := a.Request(control.Request{Version: 1, Action: "start", ExpectedRevision: &revision})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	revision = saveProxyProfile(t, a, "http", occupied.Addr().String())
	r = a.Request(control.Request{Version: 1, Action: "restart", ExpectedRevision: &revision})
	if r.Error == nil {
		t.Fatal("restart on occupied port succeeded")
	}
	if b.current["server"] != "original" || a.SystemProxy().Managed {
		t.Fatal("failed restart left a dead system proxy")
	}
}
