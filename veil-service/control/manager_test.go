package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"veil/core"
	"veil/service"
)

func profile(t *testing.T, target string) []byte {
	t.Helper()
	b, err := json.Marshal(service.Config{Role: "client", Listen: "127.0.0.1:0", Server: "127.0.0.1:9",
		Secret: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), Target: target,
		TLS: core.TLSConfig{Mode: "tls", ServerName: "cover.test"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func manager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, dir
}

func perform(t *testing.T, m *Manager, action string, b []byte) Response {
	t.Helper()
	r := m.Handle(Request{Version: Version, Action: action, Config: b})
	if r.Error != nil {
		t.Fatalf("%s: %+v", action, r.Error)
	}
	return r
}

func TestSavedAndRunningAreSeparate(t *testing.T) {
	m, dir := manager(t)
	a := profile(t, "")
	r := perform(t, m, "validate", a)
	if r.Status.SavedRevision != "" || r.Status.State != "stopped" {
		t.Fatal("validate mutated state")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatal("validate wrote a profile")
	}
	r = perform(t, m, "save", a)
	first := r.Revision
	if r.Status.State != "stopped" {
		t.Fatal("save started the runtime")
	}
	s, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || s.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v %v", s, err)
	}
	perform(t, m, "start", nil)
	addr := m.Status().Listen
	b := profile(t, "example.test:443")
	r = perform(t, m, "save", b)
	if !r.Status.RestartRequired || r.Status.ActiveRevision != first || r.Status.Listen != addr {
		t.Fatal("save changed the live runtime")
	}
	perform(t, m, "start", nil)
	if !m.Status().RestartRequired || m.Status().Listen != addr {
		t.Fatal("start silently applied pending config")
	}
	before, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	bad := m.Handle(Request{Version: Version, Action: "save", Config: []byte(`{"role":"bad"}`)})
	if bad.Error == nil {
		t.Fatal("bad config saved")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed save changed disk")
	}
	conflict := m.Handle(Request{Version: Version, Action: "restart", ExpectedRevision: &first})
	if conflict.Error == nil || conflict.Error.Code != "conflict" {
		t.Fatal("stale restart accepted")
	}
	perform(t, m, "restart", nil)
	if m.Status().RestartRequired || m.Status().ActiveRevision != r.Revision {
		t.Fatal("restart did not apply saved config")
	}
	perform(t, m, "stop", nil)
	perform(t, m, "stop", nil)
	m.Close()
	loaded, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if loaded.Status().SavedRevision != r.Revision || loaded.Status().State != "stopped" {
		t.Fatal("profile did not survive reopening")
	}
	status, _ := json.Marshal(loaded.Status())
	if bytes.Contains(status, []byte("secret")) || bytes.Contains(status, []byte("private_key")) {
		t.Fatal("status exposes credentials")
	}
}

func TestConcurrentOperationsAndClose(t *testing.T) {
	m, _ := manager(t)
	perform(t, m, "save", profile(t, ""))
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			actions := []string{"start", "stop", "status", "restart"}
			m.Handle(Request{Version: Version, Action: actions[i%len(actions)]})
		})
	}
	wg.Wait()
	m.Close()
	if m.Status().State != "stopped" {
		t.Fatal("manager still running")
	}
	if r := m.Handle(Request{Version: Version, Action: "start"}); r.Error == nil || r.Error.Code != "closed" {
		t.Fatal("manager restarted after close")
	}
}

func TestPrivateDirectoryAndVersion(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0755)
	if _, err := Open(dir); err == nil {
		t.Fatal("shared directory accepted")
	}
	m, _ := manager(t)
	if r := m.Handle(Request{Version: 2, Action: "start"}); r.Error == nil || r.Error.Code != "unsupported_version" {
		t.Fatal("unknown protocol accepted")
	}
}
