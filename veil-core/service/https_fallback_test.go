package service

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func httpsFallbackSite(t *testing.T, handler http.Handler) (*httptest.Server, *HTTPSFallbackConfig) {
	t.Helper()
	cp, kp := certs(t)
	pair, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	site := httptest.NewUnstartedServer(handler)
	site.EnableHTTP2 = true
	site.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	site.Config.ErrorLog = log.New(io.Discard, "", 0)
	site.StartTLS()
	t.Cleanup(site.Close)
	return site, &HTTPSFallbackConfig{Address: site.Listener.Addr().String(), ServerName: "cover.test", CAFile: cp, HTTP2: true}
}

func TestHTTPSFallbackRoutingAndServerFirst(t *testing.T) {
	site, backend := httpsFallbackSite(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		io.WriteString(w, "owned:"+r.URL.Path)
	}))
	st, ct := settings(t, "tls")
	svc, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HTTPFallback: &HTTPFallbackConfig{HTTPS: backend}})
	for _, protocol := range []string{"h2", "http/1.1", ""} {
		t.Run(protocol, func(t *testing.T) {
			cfg := httpFallbackTLS(t, st.Certificate)
			if protocol != "" {
				cfg.NextProtos = []string{protocol}
			}
			tr := &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: protocol == "h2"}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
			req, _ := http.NewRequest("GET", "https://"+addr+"/fixed", nil)
			req.Host = "other.invalid"
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil || res.StatusCode != 200 || string(data) != "owned:/fixed" || (res.ProtoMajor == 2) != (protocol == "h2") {
				t.Fatal("website response changed", res.Proto, string(data), err)
			}
		})
	}
	cfg := httpFallbackTLS(t, st.Certificate)
	cfg.NextProtos = []string{"h2"}
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(time.Second))
	var header [9]byte
	_, err = io.ReadFull(c, header[:])
	if err != nil {
		c.Close()
		t.Fatal("no server-first SETTINGS", err)
	}
	length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
	if header[3] != 4 || header[4] != 0 || header[5] != 0 || header[6] != 0 || header[7] != 0 || header[8] != 0 || length%6 != 0 || length > 16384 {
		c.Close()
		t.Fatalf("invalid initial SETTINGS %x", header)
	}
	_, err = io.CopyN(io.Discard, c, int64(length))
	c.Close()
	if err != nil {
		t.Fatal("truncated SETTINGS", err)
	}
	if svc.Stats.Authenticated.Load() != 0 {
		t.Fatal("website authenticated as Veil")
	}
	// This server also remains a working Veil proxy, with the same destination semantics.
	_, proxy := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr})
	raw, err := socksDial(proxy, site.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	upstream := tls.Client(raw, httpFallbackTLS(t, backend.CAFile))
	defer upstream.Close()
	upstream.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = io.WriteString(upstream, "GET /veil HTTP/1.1\r\nHost: cover.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(upstream)
	if err != nil || !strings.Contains(string(data), "owned:/veil") || svc.Stats.Authenticated.Load() != 1 {
		t.Fatal(string(data), err)
	}
}

func TestHTTPSFallbackVerification(t *testing.T) {
	var requests atomic.Int64
	_, backend := httpsFallbackSite(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	st, _ := settings(t, "tls")
	wrongCA, _ := certs(t)
	for _, test := range []struct{ name, serverName, ca string }{
		{"wrong-name", "wrong.invalid", backend.CAFile},
		{"wrong-ca", "cover.test", wrongCA},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := *backend
			bad.ServerName, bad.CAFile = test.serverName, test.ca
			svc, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HTTPFallback: &HTTPFallbackConfig{HTTPS: &bad}})
			cfg := httpFallbackTLS(t, st.Certificate)
			cfg.NextProtos = []string{"h2"}
			c, err := tls.Dial("tcp", addr, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			var one [1]byte
			if n, err := c.Read(one[:]); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatal("unverified backend relayed", n, err)
			}
			if svc.Stats.Authenticated.Load() != 0 {
				t.Fatal("failed website authenticated")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("unverified backend received HTTP")
	}
	badPEM := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPEM, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ca := range []string{badPEM, badPEM + ".missing"} {
		bad := *backend
		bad.CAFile = ca
		if svc, err := New(Config{Role: "server", Secret: testKey, TLS: st, HTTPFallback: &HTTPFallbackConfig{HTTPS: &bad}}); err == nil {
			svc.Close()
			t.Fatal("invalid CA accepted at startup")
		}
	}
}

func TestHTTPSFallbackRequiresHTTP2ALPN(t *testing.T) {
	cp, kp := certs(t)
	pair, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// A TLS website without ALPN is legal for HTTP/1, never for HTTP/2.
	for _, protocol := range []string{"h2", "http/1.1"} {
		t.Run(protocol, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				raw, err := ln.Accept()
				if err != nil {
					done <- err
					return
				}
				c := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{pair}})
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				if err := c.Handshake(); err != nil {
					done <- err
					return
				}
				var one [1]byte
				n, err := c.Read(one[:])
				if protocol == "h2" {
					if n != 0 || err != io.EOF {
						done <- errors.New("H2 bytes reached non-H2 backend")
						return
					}
				} else if n != 1 || one[0] != 'G' || err != nil {
					done <- errors.New("HTTP/1 not relayed")
					return
				}
				done <- nil
			}()
			cfg := Config{DialSeconds: 1, IdleSeconds: 1, HTTPFallback: &HTTPFallbackConfig{HTTPS: &HTTPSFallbackConfig{Address: ln.Addr().String(), ServerName: "cover.test", CAFile: cp, HTTP2: true}}}
			fallback, err := httpFallback(cfg)
			if err != nil {
				t.Fatal(err)
			}
			front, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer front.Close()
			right, err := net.Dial("tcp", front.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer right.Close()
			left, err := front.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer left.Close()
			right.SetDeadline(time.Now().Add(2 * time.Second))
			result := make(chan error, 1)
			go func() { result <- fallback.Handler(context.Background(), left, protocol) }()
			if protocol == "http/1.1" {
				if _, err = right.Write([]byte("G")); err != nil {
					t.Fatal(err)
				}
				if err = right.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err = <-result:
				if protocol == "h2" && (err == nil || !strings.Contains(err.Error(), "ALPN")) {
					t.Fatal(err)
				}
				if protocol == "http/1.1" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("fallback did not return")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTPSFallbackHandshakeBoundedAndCanceled(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[stop], func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			accepted := make(chan net.Conn, 1)
			go func() { c, _ := ln.Accept(); accepted <- c }()
			fallback, err := httpFallback(Config{DialSeconds: 1, IdleSeconds: 1, HTTPFallback: &HTTPFallbackConfig{HTTPS: &HTTPSFallbackConfig{Address: ln.Addr().String(), ServerName: "cover.test", HTTP2: true}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			done := make(chan error, 1)
			go func() { done <- fallback.Handler(ctx, left, "h2") }()
			var target net.Conn
			select {
			case target = <-accepted:
				if target == nil {
					t.Fatal("backend accept failed")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("backend not contacted")
			}
			defer target.Close()
			begin := time.Now()
			if stop {
				cancel()
			}
			select {
			case err = <-done:
				if stop {
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				} else {
					var timeout net.Error
					if !errors.As(err, &timeout) || !timeout.Timeout() {
						t.Fatal(err)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stalled TLS handshake not bounded")
			}
			if stop && time.Since(begin) > 500*time.Millisecond {
				t.Fatal("slow handshake cancellation")
			}
			target.SetReadDeadline(time.Now().Add(time.Second))
			if _, err = io.Copy(io.Discard, target); err != nil {
				t.Fatal("backend connection leaked", err)
			}
		})
	}
}
