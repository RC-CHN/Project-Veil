package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"veil-service/control"
	"veil-service/local"
	"veil/core"
	"veil/service"
)

func TestDesktopOwnsAndReleasesRuntime(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "desktop")
	a, err := openApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if second, err := openApp(dir); err == nil {
		second.close()
		t.Fatal("two desktop instances own the same profile")
	} else if !errors.Is(err, local.ErrStateLocked) {
		t.Fatalf("duplicate instance was not identified: %v", err)
	}
	b, err := json.Marshal(service.Config{Role: "client", Inbound: "mixed", Listen: "127.0.0.1:0", Server: "127.0.0.1:9",
		Secret: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), TLS: core.TLSConfig{Mode: "tls", ServerName: "test.example"}})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Request(control.Request{Version: 1, Action: "save", Config: b})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	revision := r.Revision
	r = a.Request(control.Request{Version: 1, Action: "start", ExpectedRevision: &revision})
	if r.Error != nil || r.Status.State != "running" {
		t.Fatalf("start: %+v", r)
	}
	addr := r.Status.Listen
	a.close()
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Fatal("closing desktop left proxy listening")
	}
	reopened, err := openApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	r = reopened.Request(control.Request{Version: 1, Action: "status"})
	if r.Status.State != "stopped" || r.Status.SavedRevision != revision {
		t.Fatal("reopen lost profile or auto-started it")
	}
}

func TestImportBound(t *testing.T) {
	p := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(p, bytes.Repeat([]byte(" "), service.MaxConfigSize+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfile(p); err == nil {
		t.Fatal("oversized import accepted")
	}
}
