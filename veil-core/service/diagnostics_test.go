package service

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"veil/core"
)

func TestDiagnosticHistoryBoundedAndDetached(t *testing.T) {
	h := errorHistory{started: time.Now().UTC()}
	err := &core.OpError{Op: "acquire tunnel", Err: &core.OpError{Op: "TLS handshake", Err: context.DeadlineExceeded}}
	var workers sync.WaitGroup
	for range 50 {
		workers.Go(func() { h.record(err) })
	}
	workers.Wait()
	h.record(context.Canceled)
	d := h.snapshot(64, nil)
	if d.Sequence != 50 || len(d.Recent) != 1 || d.Recent[0].Count != 50 || d.Recent[0].Stage != "TLS handshake" || d.Recent[0].Code != "timeout" || d.Recent[0].At.Before(d.Recent[0].FirstAt) {
		t.Fatalf("bad diagnostic: %+v", d)
	}
	d.Recent[0].Message = "modified copy"
	if h.snapshot(64, nil).Recent[0].Message == "modified copy" {
		t.Fatal("snapshot aliases runtime history")
	}
	for i := range 40 {
		h.record(fmt.Errorf("event %d %s", i, strings.Repeat("x", 4096)))
	}
	d = h.snapshot(64, nil)
	if len(d.Recent) != 16 || d.Sequence != 90 || len([]rune(d.Recent[15].Message)) != 257 || d.Recent[0].Sequence != 75 {
		t.Fatalf("unbounded history: %+v", d)
	}
}

func TestDiagnosticsIncludeAdmissionFailure(t *testing.T) {
	_, ct := settings(t, "tls")
	var runtime Runtime
	defer runtime.Close()
	if err := runtime.Start(Config{Role: "client", Listen: "127.0.0.1:0", Server: "127.0.0.1:9", Secret: testKey, TLS: ct, MaxConnections: 1, MaxIdle: 1}); err != nil {
		t.Fatal(err)
	}
	a, err := net.Dial("tcp", runtime.Snapshot().Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	await(t, func() bool { return runtime.Snapshot().Stats.ActiveConnections == 1 })
	b, err := net.Dial("tcp", runtime.Snapshot().Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	await(t, func() bool {
		return runtime.Snapshot().Stats.Rejected == 1 && runtime.Snapshot().Diagnostics.Sequence > 0
	})
	s := runtime.Snapshot()
	if s.Diagnostics.Recent[0].Code != "connection_limit" || s.Diagnostics.ConnectionLimit != 1 || s.Diagnostics.Pool == nil || s.Diagnostics.Pool.Total != 0 {
		t.Fatalf("limit not explained: %+v", s)
	}
	runtime.Stop()
	if s = runtime.Snapshot(); s.Stats.ActiveConnections != 0 || s.Diagnostics.Sequence == 0 {
		t.Fatal("stopped state lost diagnostics or active count")
	}
}
