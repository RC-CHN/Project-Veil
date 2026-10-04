package core

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"veil/internal/wire"
)

type fallbackTestConfig struct {
	Config
	DialContext DialFunc
	Fallback    FallbackFunc
}

func (c fallbackTestConfig) server() ServerConfig {
	out := ServerConfig{Config: c.Config, DialContext: c.DialContext}
	if c.Fallback != nil {
		out.Fallback = &Fallback{Handler: c.Fallback, Protocols: []string{"h2", "http/1.1"}}
	}
	return out
}

type fallbackFixture struct {
	cfg Config
	tls *tls.Config
}

func fallbackSettings(t *testing.T) fallbackFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cover.test"}, DNSNames: []string{"cover.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(cp, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return fallbackFixture{Config{Secret: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), TLS: TLSConfig{Mode: "tls", ServerName: "cover.test", Certificate: cp, PrivateKeyFile: kp, CAFile: cp}, HandshakeTimeout: 300 * time.Millisecond, IdleTimeout: 500 * time.Millisecond}, &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "cover.test", RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}}}
}

func fallbackListen(t *testing.T, cfg fallbackTestConfig) (string, *Server, context.CancelFunc, <-chan error) {
	t.Helper()
	s, err := NewServer(cfg.server())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	errs := make(chan error, 256)
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		defer wg.Wait()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); errs <- s.Handle(ctx, c) }()
		}
	}()
	stop := func() { cancel(); ln.Close() }
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("handlers did not join")
		}
	})
	return ln.Addr().String(), s, stop, errs
}

func fallbackBridge(address string, idle time.Duration, called *atomic.Int64) FallbackFunc {
	return func(ctx context.Context, c net.Conn, protocol string) error {
		if called != nil {
			called.Add(1)
		}
		// Deliberately fixed destination: Host and CONNECT never select a target.
		target, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp", address)
		if err != nil {
			return err
		}
		defer target.Close()
		return relay(ctx, c, target, idle)
	}
}

func fallbackOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Origin-Protocol", r.Proto)
		if r.Body != nil && r.ContentLength != 0 {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "request read failed", 400)
				return
			}
			w.Write(body)
			return
		}
		io.WriteString(w, "self-owned-site:"+r.URL.Path)
	}))
	origin.Config.Protocols = new(http.Protocols)
	origin.Config.Protocols.SetHTTP1(true)
	origin.Config.Protocols.SetUnencryptedHTTP2(true)
	origin.Config.ReadHeaderTimeout = 300 * time.Millisecond
	origin.Config.IdleTimeout = 500 * time.Millisecond
	origin.Start()
	t.Cleanup(origin.Close)
	return origin
}

func TestTLSFallbackHTTP(t *testing.T) {
	for _, protocol := range []string{"http/1.1", "h2"} {
		t.Run(protocol, func(t *testing.T) {
			f := fallbackSettings(t)
			origin := fallbackOrigin(t)
			var calls atomic.Int64
			addr, s, _, _ := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: fallbackBridge(origin.Listener.Addr().String(), time.Second, &calls)})
			tlsCfg := f.tls.Clone()
			tlsCfg.NextProtos = []string{protocol}
			tr := &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: protocol == "h2", MaxConnsPerHost: 1}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
			for _, size := range []int{17, 1 << 20, 3} {
				body := bytes.Repeat([]byte{byte(size)}, size)
				res, err := client.Post("https://"+addr+"/echo", "application/octet-stream", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil || !bytes.Equal(got, body) {
					t.Fatal("body changed", len(got), err)
				}
				want := "HTTP/1.1"
				if protocol == "h2" {
					want = "HTTP/2.0"
				}
				if res.Proto != want || res.Header.Get("X-Origin-Protocol") != want {
					t.Fatal("protocol mismatch", res.Proto, res.Header)
				}
			}
			if calls.Load() != 1 || s.Stats.Authenticated.Load() != 0 {
				t.Fatal("fallback/auth counts", calls.Load(), s.Stats.Authenticated.Load())
			}
		})
	}
}

func TestTLSFallbackFragmentedAndFixedDestination(t *testing.T) {
	f := fallbackSettings(t)
	origin := fallbackOrigin(t)
	addr, _, _, _ := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: fallbackBridge(origin.Listener.Addr().String(), time.Second, nil)})
	cfg := f.tls.Clone()
	cfg.NextProtos = []string{"http/1.1"}
	for _, request := range []string{"GET http://unowned.invalid/fragment HTTP/1.1\r\nHost: unowned.invalid\r\nConnection: close\r\n\r\n", "CONNECT unowned.invalid:443 HTTP/1.1\r\nHost: unowned.invalid\r\nConnection: close\r\n\r\n"} {
		c, err := tls.Dial("tcp", addr, cfg)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		// Cross the four-byte sniff boundary using separate TLS application writes.
		for i := 0; i < 8; i++ {
			if _, err = c.Write([]byte(request[i : i+1])); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = c.Write([]byte(request[8:])); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(c)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if request[:3] == "GET" {
			if !bytes.Contains(data, []byte("self-owned-site:/fragment")) {
				t.Fatal(string(data))
			}
		} else if !bytes.Contains(data, []byte("405 Method Not Allowed")) {
			t.Fatal(string(data))
		}
	}
}

func TestTLSFallbackWrongAuthenticationPreservesEveryByte(t *testing.T) {
	f := fallbackSettings(t)
	var seen []byte
	handler := func(ctx context.Context, c net.Conn, protocol string) error {
		if protocol != "http/1.1" {
			return fmt.Errorf("unexpected ALPN %q", protocol)
		}
		var err error
		seen, err = io.ReadAll(c)
		if err == nil {
			_, err = c.Write(seen)
		}
		return err
	}
	addr, s, _, errs := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: handler})
	cfg := f.tls.Clone()
	cfg.NextProtos = []string{"http/1.1"}
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	data := append([]byte{wire.Auth, 0, 0, wire.AuthSize}, bytes.Repeat([]byte{0xa5}, wire.AuthSize+32768)...)
	for _, part := range [][]byte{data[:2], data[2:17], data[17:53], data[53:]} {
		if _, err = c.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(reply, data) {
		t.Fatal("wrong auth replay changed", len(reply), err)
	}
	if err = <-errs; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seen, data) || s.Stats.Authenticated.Load() != 0 {
		t.Fatal("wrong auth accepted/lost")
	}
}

func TestTLSFallbackFragmentedHTTP2Preface(t *testing.T) {
	f := fallbackSettings(t)
	origin := fallbackOrigin(t)
	handler := fallbackBridge(origin.Listener.Addr().String(), time.Second, nil)
	addr, _, _, _ := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: func(ctx context.Context, c net.Conn, protocol string) error {
		if protocol != "h2" {
			return fmt.Errorf("wrong negotiated protocol %q", protocol)
		}
		return handler(ctx, c, protocol)
	}})
	cfg := f.tls.Clone()
	cfg.NextProtos = []string{"h2"}
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	preface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	for _, b := range preface {
		if _, err = c.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = c.Write([]byte{0, 0, 0, 4, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	} // empty SETTINGS
	var head [9]byte
	if _, err = io.ReadFull(c, head[:]); err != nil {
		t.Fatal(err)
	}
	if head[3] != 4 {
		t.Fatalf("expected backend SETTINGS, got %x", head)
	}
}

func TestTLSFallbackTimeoutAndCancellation(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("aborted-reset-%t", reset), func(t *testing.T) {
			f := fallbackSettings(t)
			f.tls.NextProtos = []string{"http/1.1"}
			var calls atomic.Int64
			addr, _, _, errs := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: func(context.Context, net.Conn, string) error { calls.Add(1); return nil }})
			c, err := tls.Dial("tcp", addr, f.tls)
			if err != nil {
				t.Fatal(err)
			}
			if reset {
				if _, err := c.Write([]byte("G")); err != nil {
					t.Fatal(err)
				}
				raw := c.NetConn().(*net.TCPConn)
				if err := raw.SetLinger(0); err != nil {
					t.Fatal(err)
				}
				if err := raw.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				c.Close()
			}
			select {
			case <-errs:
			case <-time.After(time.Second):
				t.Fatal("aborted authentication did not return")
			}
			if calls.Load() != 0 {
				t.Fatal("closed/reset peer initiated fallback work")
			}
		})
	}
	for _, input := range [][]byte{{'G'}, {wire.Auth, 0, 0, wire.AuthSize, 3}} {
		t.Run(fmt.Sprintf("partial-%d", len(input)), func(t *testing.T) {
			f := fallbackSettings(t)
			f.tls.NextProtos = []string{"http/1.1"}
			f.cfg.HandshakeTimeout = 90 * time.Millisecond
			var called atomic.Int64
			addr, _, _, errs := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: func(context.Context, net.Conn, string) error { called.Add(1); return nil }})
			c, err := tls.Dial("tcp", addr, f.tls)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			c.Write(input)
			if _, err = c.Read(make([]byte, 1)); err == nil {
				t.Fatal("partial auth remained usable")
			}
			select {
			case err := <-errs:
				var n net.Error
				if !errors.As(err, &n) || !n.Timeout() {
					t.Fatal("not auth timeout", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timeout did not finish")
			}
			if called.Load() != 0 {
				t.Fatal("expired input entered fallback")
			}
		})
	}
	t.Run("callback-read-cancel", func(t *testing.T) {
		f := fallbackSettings(t)
		started := make(chan struct{})
		returned := make(chan struct{})
		addr, _, cancel, errs := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: func(ctx context.Context, c net.Conn, _ string) error {
			close(started)
			defer close(returned)
			_, err := io.Copy(io.Discard, c)
			return err
		}})
		c, err := tls.Dial("tcp", addr, f.tls)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("GET "))
		<-started
		start := time.Now()
		cancel()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("callback read did not cancel")
		}
		<-errs
		if time.Since(start) > 300*time.Millisecond {
			t.Fatal("slow cancellation")
		}
	})
	t.Run("relay-idle", func(t *testing.T) {
		f := fallbackSettings(t)
		backend, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close()
		accepted := make(chan net.Conn, 1)
		go func() { c, _ := backend.Accept(); accepted <- c }()
		addr, _, _, errs := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: fallbackBridge(backend.Addr().String(), 80*time.Millisecond, nil)})
		c, err := tls.Dial("tcp", addr, f.tls)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("GET "))
		target := <-accepted
		defer target.Close()
		select {
		case err := <-errs:
			if !errors.Is(err, ErrIdleTimeout) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("idle fallback hung")
		}
	})
}

func TestTLSFallbackHTTP2StartsBeforeClientData(t *testing.T) {
	f := fallbackSettings(t)
	f.tls.NextProtos = []string{"h2"}
	seen := make(chan []byte, 1)
	addr, s, _, errs := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: func(ctx context.Context, c net.Conn, protocol string) error {
		if protocol != "h2" {
			return fmt.Errorf("unexpected ALPN %q", protocol)
		}
		if _, err := c.Write([]byte("website-first")); err != nil {
			return err
		}
		data, err := io.ReadAll(c)
		seen <- data
		return err
	}})
	c, err := tls.Dial("tcp", addr, f.tls)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	first := make([]byte, len("website-first"))
	if _, err = io.ReadFull(c, first); err != nil || string(first) != "website-first" {
		t.Fatal("website did not speak first", string(first), err)
	}
	// Even a valid Veil AUTH on h2 belongs to the website, byte for byte.
	state := c.ConnectionState()
	exporter, err := state.ExportKeyingMaterial("EXPORTER-Veil-v0.3", nil, 32)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := wire.AuthPayload(s.key, exporter)
	if err != nil {
		t.Fatal(err)
	}
	var frame bytes.Buffer
	if err = wire.Write(&frame, wire.Auth, auth); err != nil {
		t.Fatal(err)
	}
	// The website is no longer subject to the completed AUTH deadline.
	time.Sleep(f.cfg.HandshakeTimeout + 20*time.Millisecond)
	if _, err = c.Write(frame.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, c); err != nil {
		t.Fatal(err)
	}
	if err = <-errs; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(<-seen, frame.Bytes()) || s.Stats.Authenticated.Load() != 0 {
		t.Fatal("HTTP/2 data was interpreted as Veil authentication")
	}
}

func TestTLSFallbackFragmentedValidAuthentication(t *testing.T) {
	for _, split := range []int{1, 4, 17, 32, 52} {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			f := fallbackSettings(t)
			f.tls.NextProtos = []string{"http/1.1"}
			var calls atomic.Int64
			addr, s, _, errs := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: func(context.Context, net.Conn, string) error { calls.Add(1); return nil }})
			c, err := tls.Dial("tcp", addr, f.tls)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			state := c.ConnectionState()
			exporter, err := state.ExportKeyingMaterial("EXPORTER-Veil-v0.3", nil, 32)
			if err != nil {
				t.Fatal(err)
			}
			auth, err := wire.AuthPayload(s.key, exporter)
			if err != nil {
				t.Fatal(err)
			}
			var frame bytes.Buffer
			if err = wire.Write(&frame, wire.Auth, auth); err != nil {
				t.Fatal(err)
			}
			for _, part := range [][]byte{frame.Bytes()[:split], frame.Bytes()[split:]} {
				if _, err = c.Write(part); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if _, err = io.Copy(io.Discard, c); err != nil {
				t.Fatal(err)
			}
			if err = <-errs; err != nil {
				t.Fatal(err)
			}
			if s.Stats.Authenticated.Load() != 1 || calls.Load() != 0 {
				t.Fatal("fragmented AUTH changed")
			}
		})
	}
}

func TestTLSFallbackDisabledAndVeilSemantics(t *testing.T) {
	f := fallbackSettings(t)
	t.Run("disabled", func(t *testing.T) {
		addr, _, _, errs := fallbackListen(t, fallbackTestConfig{Config: f.cfg})
		c, err := tls.Dial("tcp", addr, f.tls)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if c.ConnectionState().NegotiatedProtocol != "http/1.1" {
			t.Fatal("nil fallback changed ALPN")
		}
		c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
		c.SetDeadline(time.Now().Add(time.Second))
		data, err := io.ReadAll(c)
		if len(data) != 0 || err != nil {
			t.Fatal("baseline EOF changed", len(data), err)
		}
		if err = <-errs; !errors.Is(err, wire.ErrProtocol) {
			t.Fatal(err)
		}
	})
	t.Run("authenticated-target-failure", func(t *testing.T) {
		var calls atomic.Int64
		addr, s, _, _ := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: func(context.Context, net.Conn, string) error { calls.Add(1); return nil }, DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, context.DeadlineExceeded }})
		client, err := NewClient(ClientConfig{Config: f.cfg, Server: addr})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		_, err = client.Open(context.Background(), "127.0.0.1:1")
		if !errors.Is(err, TargetTimeout) || calls.Load() != 0 || s.Stats.Authenticated.Load() != 1 {
			t.Fatal("Open semantics changed", err, calls.Load(), s.Stats.Authenticated.Load())
		}
	})

	t.Run("authenticated-success", func(t *testing.T) {
		origin := fallbackOrigin(t)
		var calls atomic.Int64
		addr, s, _, _ := fallbackListen(t, fallbackTestConfig{Config: f.cfg, Fallback: func(context.Context, net.Conn, string) error { calls.Add(1); return nil }})
		client, err := NewClient(ClientConfig{Config: f.cfg, Server: addr})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stream, err := client.Open(ctx, origin.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		local, peer := tcpPair(t)
		done := make(chan error, 1)
		go func() { done <- stream.Relay(local) }()
		if _, err = io.WriteString(peer, "GET /veil HTTP/1.1\r\nHost: cover.test\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		if err = peer.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(peer)
		if err != nil || !bytes.Contains(data, []byte("self-owned-site:/veil")) {
			t.Fatal(string(data), err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 0 || s.Stats.Authenticated.Load() != 1 {
			t.Fatal("valid Veil entered fallback")
		}
	})
	t.Run("reject-reality-api", func(t *testing.T) {
		cfg := f.cfg
		cfg.TLS.Mode = "reality"
		_, err := NewServer((fallbackTestConfig{Config: cfg, Fallback: func(context.Context, net.Conn, string) error { return nil }}).server())
		if err == nil {
			t.Fatal("REALITY application fallback accepted")
		}
	})
}

func TestTLSFallbackProtocolDeclaration(t *testing.T) {
	f := fallbackSettings(t)
	for _, protocols := range [][]string{nil, {"http/1.1"}, {"h2", "http/1.1"}, {"h3"}, {"h2", "h2"}} {
		fallback := &Fallback{Handler: func(context.Context, net.Conn, string) error { return nil }, Protocols: protocols}
		server, err := NewServer(ServerConfig{Config: f.cfg, Fallback: fallback})
		invalid := len(protocols) > 0 && (protocols[0] == "h3" || len(protocols) == 2 && protocols[0] == protocols[1])
		if (err != nil) != invalid {
			t.Fatal(protocols, err)
		}
		if invalid {
			continue
		}
		if len(protocols) == 0 && server.cfg.Fallback.Protocols[0] != "http/1.1" {
			t.Fatal("default ALPN is not H1")
		}
		fallback.Handler = nil
		if server.cfg.Fallback.Handler == nil {
			t.Fatal("caller retained callback ownership")
		}
		if len(protocols) > 0 {
			first := server.cfg.Fallback.Protocols[0]
			protocols[0] = "mutated"
			if server.cfg.Fallback.Protocols[0] != first {
				t.Fatal("ALPN slice not copied")
			}
		}
	}
	if _, err := NewServer(ServerConfig{Config: f.cfg, Fallback: &Fallback{}}); err == nil {
		t.Fatal("nil handler accepted")
	}
}
