package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
	"veil/core"
	"veil/inbound"
)

func TestTLSFallbackListenerLimitAndStop(t *testing.T) {
	for _, protocol := range []string{"http/1.1", "h2"} {
		t.Run(protocol, func(t *testing.T) { testTLSFallbackListenerLimitAndStop(t, protocol) })
	}
}

func testTLSFallbackListenerLimitAndStop(t *testing.T, protocol string) {
	t.Helper()
	serverTLS, _ := settings(t, "tls")
	var active, peak atomic.Int64
	entered := make(chan struct{}, 2)
	s, err := core.NewServer(core.ServerConfig{Config: core.Config{Secret: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), TLS: serverTLS, HandshakeTimeout: time.Second}, Fallback: &core.Fallback{Protocols: []string{protocol}, Handler: func(ctx context.Context, c net.Conn, _ string) error {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- inbound.Serve(ctx, ln, s.Handle, inbound.ServeOptions{MaxConnections: 2, Stats: &s.Stats})
	}()
	pem, err := os.ReadFile(serverTLS.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "cover.test", NextProtos: []string{protocol}}
	for range 2 {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", ln.Addr().String(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if protocol == "http/1.1" {
			if _, err = c.Write([]byte("GET ")); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("fallback did not start")
		}
	}
	for range 8 {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", ln.Addr().String(), cfg)
		if err == nil {
			c.Close()
			t.Fatal("connection limit did not reject")
		}
	}
	if active.Load() != 2 || peak.Load() != 2 || s.Stats.Rejected.Load() != 8 {
		t.Fatal("incorrect resource counts", active.Load(), peak.Load(), s.Stats.Rejected.Load())
	}
	start := time.Now()
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not join fallback handlers")
	}
	if active.Load() != 0 || s.Stats.ActiveConnections.Load() != 0 || time.Since(start) > 300*time.Millisecond {
		t.Fatal("resources not released", active.Load(), s.Stats.ActiveConnections.Load())
	}
}
