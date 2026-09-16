package session

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	b "veil.local/core/internal/behavior"
)

func muxTLSStates(t *testing.T) (tls.ConnectionState, tls.ConnectionState) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"owned.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	cert, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	server := tls.Server(left, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	client := tls.Client(right, &tls.Config{RootCAs: roots, ServerName: "owned.test", MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.HandshakeContext(ctx) }()
	if e = client.HandshakeContext(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	return client.ConnectionState(), server.ConnectionState()
}
func TestMuxFrontendUsesRuntimeExporterDomain(t *testing.T) {
	clientTLS, serverTLS := muxTLSStates(t)
	program, e := b.CompileBatch(adaptiveModel())
	if e != nil {
		t.Fatal(e)
	}
	want, e := runtimeMaterial(&clientTLS, program.ID(), 2)
	if e != nil {
		t.Fatal(e)
	}
	other, e := runtimeMaterial(&serverTLS, program.ID(), 2)
	if e != nil || other != want {
		t.Fatal("TLS exporter peers differ", e)
	}
	legacy, e := parallelMaterial(&serverTLS, program.ID())
	if e != nil || legacy == want {
		t.Fatal("exporter domains not separated", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{program: program, cfg: ServerConfig{Version: 2, Window: 65536, MaxBytes: 8 << 20, Mux: MuxConfig{Streams: 4, Opened: 64, ConnectionBytes: 64 << 20, CarrierIdleMS: 5000}}}
	c := &serverConnection{server: s, ctx: ctx, cancel: cancel, prefix: "owned/prefix", material: want}
	if e = c.initializeMux(); e != nil {
		t.Fatal(e)
	}
	defer func() { c.muxSource.close(); cancel(); c.stopIdle(); <-c.muxAcceptDone; c.muxWorkers.Wait() }()
	request := &http.Request{Method: "HEAD", URL: &url.URL{Path: "/owned/prefix/upload"}, TLS: &serverTLS}
	if _, _, e = c.front.claim(request); e != nil {
		t.Fatal(e)
	}
	if c.front.material != want {
		t.Fatal("frontend used a different TLS exporter domain")
	}
}
