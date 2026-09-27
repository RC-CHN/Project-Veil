package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
	"veil/internal/transport"
)

func fallbackKeys(t *testing.T, cover string) (transport.Settings, transport.Settings) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := transport.Settings{Mode: "reality", ServerName: "cover.test", ShortID: "0102030405060708", CoverAddress: cover,
		RealityPrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes())}
	c := transport.Settings{Mode: "reality", ServerName: s.ServerName, ShortID: s.ShortID,
		RealityPublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())}
	return s, c
}

func fallbackCover(t *testing.T, handler http.HandlerFunc) (string, *tls.Config) {
	t.Helper()
	cp, kp := certs(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{ReadHeaderTimeout: 6 * time.Second, Handler: handler, TLSConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519}, NextProtos: []string{"http/1.1"},
	}}
	done := make(chan error, 1)
	go func() { done <- s.ServeTLS(ln, cp, kp) }()
	t.Cleanup(func() {
		s.Close()
		if err := <-done; err != http.ErrServerClosed {
			t.Error(err)
		}
	})
	roots := x509.NewCertPool()
	pem, err := os.ReadFile(cp)
	if err != nil || !roots.AppendCertsFromPEM(pem) {
		t.Fatal("cannot load cover certificate", err)
	}
	return ln.Addr().String(), &tls.Config{ServerName: "cover.test", RootCAs: roots, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}}
}

func website(t *testing.T, addr string, cfg *tls.Config) *tls.Conn {
	t.Helper()
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(8 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

func response(t *testing.T, c net.Conn, want string) {
	t.Helper()
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil || r.StatusCode != 200 || string(body) != want {
		t.Fatalf("HTTP status=%d body=%q error=%v", r.StatusCode, body, err)
	}
}

func TestRealityFallbackLongHTTP(t *testing.T) {
	cover, tlsCfg := fallbackCover(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			for range 7 {
				fmt.Fprint(w, "x")
				w.(http.Flusher).Flush()
				time.Sleep(400 * time.Millisecond)
			}
			return
		}
		body, err := io.ReadAll(r.Body)
		if err == nil {
			fmt.Fprintf(w, "ok:%s", body)
		}
	})
	st, _ := fallbackKeys(t, cover)
	svc, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HandshakeSeconds: 1, IdleSeconds: 2})
	t.Run("delayed request", func(t *testing.T) {
		c := website(t, addr, tlsCfg)
		time.Sleep(1300 * time.Millisecond) // exceeds the absolute handshake bound
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: cover.test\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response(t, c, "ok:")
	})
	t.Run("download keeps idle upload alive", func(t *testing.T) {
		c := website(t, addr, tlsCfg)
		if _, err := io.WriteString(c, "GET /download HTTP/1.1\r\nHost: cover.test\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response(t, c, "xxxxxxx") // total duration exceeds both configured timeouts
	})
	t.Run("upload keeps idle download alive", func(t *testing.T) {
		c := website(t, addr, tlsCfg)
		if _, err := io.WriteString(c, "POST / HTTP/1.1\r\nHost: cover.test\r\nConnection: close\r\nContent-Length: 8\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		for range 8 {
			if _, err := c.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(400 * time.Millisecond)
		}
		response(t, c, "ok:xxxxxxxx")
	})
	if svc.Stats.Authenticated.Load() != 0 {
		t.Fatal("website fallback counted as a Veil session")
	}
}

func TestRealityFallbackTimeouts(t *testing.T) {
	cover, tlsCfg := fallbackCover(t, func(w http.ResponseWriter, r *http.Request) {})
	st, ct := fallbackKeys(t, cover)
	svc, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HandshakeSeconds: 1, IdleSeconds: 3})
	for _, payload := range [][]byte{nil, {22, 3, 1}} {
		t.Run(fmt.Sprintf("incomplete hello %x", payload), func(t *testing.T) {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(4 * time.Second))
			if len(payload) != 0 {
				c.Write(payload)
			}
			boundedClose(t, c, 700*time.Millisecond, 2*time.Second)
		})
	}
	t.Run("no business authentication", func(t *testing.T) {
		raw, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(4 * time.Second))
		handshake, err := transport.Client(ct)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := handshake(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		boundedClose(t, c, 500*time.Millisecond, 2*time.Second)
	})
	t.Run("idle website", func(t *testing.T) {
		c := website(t, addr, tlsCfg)
		boundedClose(t, c, 2500*time.Millisecond, 4500*time.Millisecond)
	})
	if svc.Stats.Authenticated.Load() != 0 {
		t.Fatal("unauthenticated connection accepted")
	}
}

func boundedClose(t *testing.T, c net.Conn, min, max time.Duration) {
	t.Helper()
	start := time.Now()
	_, err := io.Copy(io.Discard, c)
	elapsed := time.Since(start)
	if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatalf("client deadline fired instead of server close: %v", err)
	}
	if elapsed < min || elapsed > max {
		t.Fatalf("close after %s, want %s..%s (error=%v)", elapsed, min, max, err)
	}
}

func TestRealityFallbackHalfClose(t *testing.T) {
	request := []byte("plain owned reference request\n")
	reply := bytes.Repeat([]byte("response after EOF\x00"), 10000)
	cover := target(t, func(c net.Conn) {
		b, err := io.ReadAll(c) // reply only after forwarded client EOF
		if err == nil && bytes.Equal(b, request) {
			writeAll(c, reply)
			c.(*net.TCPConn).CloseWrite()
		}
	})
	st, _ := fallbackKeys(t, cover)
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HandshakeSeconds: 2, IdleSeconds: 3})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Second))
	if err := writeAll(c, request); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(b, reply) {
		t.Fatalf("half-close response: bytes=%d want=%d error=%v", len(b), len(reply), err)
	}
}

func TestRealitySilentCoverTimeout(t *testing.T) {
	cover := target(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	st, _ := fallbackKeys(t, cover)
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HandshakeSeconds: 1, IdleSeconds: 30})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Second))
	// Rejected locally, but a silent reference must not grant this connection
	// the much longer fallback idle timeout.
	if _, err := io.WriteString(c, "not a TLS ClientHello"); err != nil {
		t.Fatal(err)
	}
	boundedClose(t, c, 700*time.Millisecond, 2*time.Second)
}

func TestRealityFallbackCancellation(t *testing.T) {
	cover, tlsCfg := fallbackCover(t, func(w http.ResponseWriter, r *http.Request) {})
	st, _ := fallbackKeys(t, cover)
	svc, err := New(Config{Role: "server", Secret: testKey, TLS: st, HandshakeSeconds: 1, IdleSeconds: 60})
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
	go func() { done <- svc.Serve(ctx, ln) }()
	c := website(t, ln.Addr().String(), tlsCfg)
	time.Sleep(1200 * time.Millisecond) // handshake context has expired in fallback
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not join fallback pumps")
	}
	boundedClose(t, c, 0, time.Second)
}

func TestRealityClientRejectsWebsite(t *testing.T) {
	cover, _ := fallbackCover(t, func(w http.ResponseWriter, r *http.Request) {})
	_, settings := fallbackKeys(t, cover)
	for _, name := range []string{"chrome120", "chrome131", "chrome133", "chrome149"} {
		t.Run(name, func(t *testing.T) {
			settings.Fingerprint = name
			rejectWebsite(t, settings, cover)
		})
	}
}

func TestRealityHybridKeyExchange(t *testing.T) {
	for _, name := range []string{"chrome131", "chrome133", "chrome149"} {
		t.Run(name, func(t *testing.T) {
			cp, kp := certs(t)
			// A hybrid-only cover forces both the offer and the actual exchange.
			cover := coverGroup(t, cp, kp, "X25519MLKEM768")
			st, ct := fallbackKeys(t, cover)
			ct.Fingerprint = name
			_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
			_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr})
			dst := target(t, func(c net.Conn) { io.Copy(c, c) })
			c, err := socksDial(entry, dst)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			payload := bytes.Repeat([]byte("hybrid TLS integrity\x00"), 1024)
			if err := writeAll(c, payload); err != nil {
				t.Fatal(err)
			}
			c.CloseWrite()
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("hybrid echo: bytes=%d error=%v", len(got), err)
			}
		})
	}
}

func rejectWebsite(t *testing.T, settings transport.Settings, cover string) {
	t.Helper()
	handshake, err := transport.Client(settings)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Dial("tcp", cover)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c, err := handshake(ctx, raw); err == nil {
		c.Close()
		t.Fatal("normal website certificate accepted as REALITY authentication")
	}
}

func TestRecordPaddingWithStandardTLS(t *testing.T) {
	st, ct := settings(t, "tls")
	cert, err := tls.LoadX509KeyPair(st.Certificate, st.PrivateKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	dst := target(t, func(raw net.Conn) {
		c := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}})
		defer c.Close()
		io.Copy(c, c)
	})
	c := directTLS(t, ct, dst)
	for range 2 {
		if err := transport.RecordPadding(c, true); err != nil {
			t.Fatal(err)
		}
		for _, size := range []int{4, 257, 1024, 16384, 17} {
			p := bytes.Repeat([]byte{9}, size)
			p[len(p)-1] = 0
			if err := writeAll(c, p); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(p))
			if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, p) {
				t.Fatal("standard TLS padding interoperability", err)
			}
		}
	}
}
