package session

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	b "veil.local/core/internal/behavior"
)

// This deliberately noncompliant authenticated peer advances valid outer VMs
// without sending inner HELLO/OPEN. The container fixture is an owned server;
// ordinary unit runs skip this external-runtime check.
type unestablishedControlSource struct{ pending bool }

func (s *unestablishedControlSource) lease(int) ([]byte, bool, error) {
	if s.pending {
		return nil, false, errors.New("control lease still pending")
	}
	s.pending = true
	return nil, false, nil
}
func (s *unestablishedControlSource) commit() error {
	if !s.pending {
		return errors.New("missing control lease")
	}
	s.pending = false
	return nil
}
func (s *unestablishedControlSource) wait(ctx context.Context, _ time.Duration) error {
	return ctx.Err()
}
func (s *unestablishedControlSource) status() streamQueueStatus {
	return streamQueueStatus{Pending: s.pending}
}
func (s *unestablishedControlSource) close() {}

func TestGenerationRuntimeRejectsRenewingUnestablishedClient(t *testing.T) {
	path := os.Getenv("VEIL_GENERATION_GUARD_CONFIG")
	if path == "" {
		t.Skip("requires owned generation-runtime container fixture")
	}
	var cfg ClientConfig
	if err := ReadPrivateJSON(path, &cfg, 65536); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !client.flight.renewable() {
		t.Fatal("expected renewing runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started := time.Now()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", client.dialAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	outer := tls.Client(raw, client.tls.Clone())
	if err := outer.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	state := outer.ConnectionState()
	master, err := client.flight.material(&state)
	if err != nil {
		t.Fatal(err)
	}
	var dialed atomic.Bool
	transport := &http.Transport{ForceAttemptHTTP2: true, MaxConnsPerHost: 1, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: client.tls.Clone(), DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
		if !dialed.CompareAndSwap(false, true) {
			return nil, errors.New("replacement guard connection prohibited")
		}
		return outer, nil
	}}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect prohibited") }}
	material, _ := client.flight.generationMaterial(master, 0, 0)
	var generations, batches uint64
	for {
		session, createErr := b.NewBatchSession(ctx, client.program, client.seed, material)
		if createErr != nil {
			t.Fatal(createErr)
		}
		value, driveErr := driveAdaptive(ctx, session, httpClient, client.endpoint, sessionPrefix(client.cfg.Bucket, client.seed, material), &unestablishedControlSource{}, adaptiveHooks{Prepared: true, Pause: func(st b.BatchStatus) bool { return st.Completed == 2 }})
		batches += session.Status().Completed
		if driveErr == nil {
			material, driveErr = requestFlightHandoff(ctx, client.flight, client.seed, master, 0, generations, session.Status(), value, httpClient, client.endpoint, client.cfg.Bucket)
		}
		session.Close()
		if driveErr != nil {
			err = driveErr
			break
		}
		generations++
	}
	elapsed := time.Since(started)
	if ctx.Err() != nil || elapsed < 7*time.Second || elapsed > 10*time.Second || generations < 5 || batches < 10 {
		t.Fatalf("unexpected establishment expiry: elapsed=%v generations=%d batches=%d local=%v remote=%v", elapsed, generations, batches, ctx.Err(), err)
	}
	t.Logf("owned runtime terminated unestablished TLS carrier after %v; completed generations=%d batches=%d; no inner HELLO/OPEN; error=%v", elapsed, generations, batches, err)
}
