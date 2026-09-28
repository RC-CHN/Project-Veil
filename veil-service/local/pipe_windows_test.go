package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"veil-service/control"
	"veil-service/internal/winsec"

	"github.com/Microsoft/go-winio"
)

func TestPipeLifecycleAndLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	m, err := control.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	lockPath := filepath.Join(dir, ".lock")
	lock, err := Lock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if duplicate, err := Lock(lockPath); err == nil {
		duplicate.Close()
		t.Fatal("second daemon acquired state lock")
	}
	lock.Close()
	lock, err = Lock(lockPath)
	if err != nil {
		t.Fatalf("released lock not recovered: %v", err)
	}
	defer lock.Close()
	path := fmt.Sprintf(`\\.\pipe\veil-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if other, err := Listen(path); err == nil {
		other.Close()
		t.Fatal("live pipe replaced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, m) }()
	r, err := Call(ctx, path, control.Request{Version: 1, Action: "status"})
	if err != nil || r.Error != nil || r.Status.State != "stopped" {
		t.Fatalf("status: %+v %v", r, err)
	}
	waiting, err := dial(ctx, path)
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
	case <-time.After(2 * time.Second):
		t.Fatal("incomplete request blocked shutdown")
	}
	recovered, err := Listen(path)
	if err != nil {
		t.Fatalf("closed pipe not recovered: %v", err)
	}
	recovered.Close()
}

func TestOnlyLocalPipes(t *testing.T) {
	for _, path := range []string{"", "127.0.0.1:80", `\\server\pipe\veil`, `\\.\pipe\`, `\\.\pipe\a\b`} {
		if _, err := Listen(path); err == nil {
			t.Fatalf("invalid endpoint accepted: %q", path)
		}
		if _, err := dial(context.Background(), path); err == nil {
			t.Fatalf("invalid endpoint dialed: %q", path)
		}
	}
}

func TestSharedPipeRejectedBeforeRequest(t *testing.T) {
	sddl, err := winsec.Descriptor(false)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf(`\\.\pipe\veil-untrusted-%d-%d`, os.Getpid(), time.Now().UnixNano())
	// A pipe with our name but a shared ACL must not receive configuration.
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl + "(A;;FA;;;WD)"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			received <- 0
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		b, _ := io.ReadAll(conn)
		received <- len(b)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Call(ctx, path, control.Request{Version: 1, Action: "status"}); err == nil || !strings.Contains(err.Error(), "grants access") {
		t.Fatal("shared pipe was not rejected by the ACL check:", err)
	}
	select {
	case n := <-received:
		if n != 0 {
			t.Fatal("request was sent before authenticating the pipe")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rejected pipe connection was left open")
	}
}
