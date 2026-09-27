//go:build linux || freebsd

package local

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"veil-service/control"
)

func private(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPrivateSocketAndRecovery(t *testing.T) {
	dir := private(t)
	path := filepath.Join(dir, "c.sock")
	m, err := control.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if other, err := Listen(path); err == nil {
		other.Close()
		t.Fatal("second listener replaced live socket")
	}
	s, err := os.Stat(path)
	if err != nil || s.Mode().Perm() != 0600 {
		t.Fatal("socket is not private")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, m) }()
	r, err := Call(context.Background(), path, control.Request{Version: 1, Action: "status"})
	if err != nil || r.Error != nil || r.Status.State != "stopped" {
		t.Fatalf("status: %+v %v", r, err)
	}
	waiting, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer waiting.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("incomplete control request blocked shutdown")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("socket not removed on close")
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	recovered, err := Listen(path)
	if err != nil {
		t.Fatalf("stale socket not recovered: %v", err)
	}
	recovered.Close()
	os.WriteFile(path, []byte("keep"), 0600)
	if x, err := Listen(path); err == nil {
		x.Close()
		t.Fatal("regular file replaced")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "keep" {
		t.Fatal("non-socket modified")
	}
}

func TestBoundedStrictRequests(t *testing.T) {
	m, err := control.Open(private(t))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, b := range []string{
		`{"version":1,"action":"status","unknown":true}`,
		`{"version":1,"action":"status"} {}`,
		`{"version":1,"action":"save","config":"` + strings.Repeat("x", maxRequest) + `"}`,
	} {
		var q control.Request
		if err := readMessage(strings.NewReader(b+"\n"), &q); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if m.Status().SavedRevision != "" {
		t.Fatal("malformed request changed state")
	}
}
