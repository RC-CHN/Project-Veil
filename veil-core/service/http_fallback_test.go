package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func httpFallbackTLS(t *testing.T, cert string) *tls.Config {
	t.Helper()
	data, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(data)
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "cover.test"}
}

func TestHTTPFallbackConfiguration(t *testing.T) {
	st, _ := settings(t, "tls")
	for _, test := range []struct {
		name, role, mode, raw string
		valid                 bool
	}{
		{"h1", "server", "tls", `{"http1":"127.0.0.1:8080"}`, true},
		{"fixed-hostname", "server", "tls", `{"http1":"owned.example:8080","h2c":"[::1]:8081"}`, true},
		{"https", "server", "tls", `{"https":{"address":"127.0.0.1:8443","server_name":"owned.example","http2":true}}`, true},
		{"https-mixed-h1", "server", "tls", `{"http1":"127.0.0.1:8080","https":{"address":"127.0.0.1:8443","server_name":"owned.example"}}`, false},
		{"https-mixed-h2", "server", "tls", `{"h2c":"127.0.0.1:8080","https":{"address":"127.0.0.1:8443","server_name":"owned.example"}}`, false},
		{"https-no-name", "server", "tls", `{"https":{"address":"127.0.0.1:8443"}}`, false},
		{"https-no-address", "server", "tls", `{"https":{"server_name":"owned.example"}}`, false},
		{"https-bad-address", "server", "tls", `{"https":{"address":"127.0.0.1:0","server_name":"owned.example"}}`, false},
		{"empty", "server", "tls", `{}`, false},
		{"h2-only", "server", "tls", `{"h2c":"127.0.0.1:8080"}`, false},
		{"bad-address", "server", "tls", `{"http1":"127.0.0.1"}`, false},
		{"zero-port", "server", "tls", `{"http1":"127.0.0.1:0"}`, false},
		{"bad-h2-address", "server", "tls", `{"http1":"127.0.0.1:8080","h2c":"bad/path:80"}`, false},
		{"client", "client", "tls", `{"http1":"127.0.0.1:8080"}`, false},
		{"reality", "server", "reality", `{"http1":"127.0.0.1:8080"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Role: test.role, Server: "127.0.0.1:1", Secret: testKey, TLS: st}
			cfg.TLS.Mode = test.mode
			if err := json.Unmarshal([]byte(test.raw), &cfg.HTTPFallback); err != nil {
				t.Fatal(err)
			}
			original := cfg.HTTPFallback
			err := cfg.Defaults()
			if (err == nil) != test.valid {
				t.Fatal(err)
			}
			if err == nil {
				original.HTTP1 = "mutated.invalid:1"
				if cfg.HTTPFallback.HTTP1 == original.HTTP1 {
					t.Fatal("config alias retained")
				}
				if original.HTTPS != nil {
					original.HTTPS.Address = "mutated.invalid:1"
					if cfg.HTTPFallback.HTTPS.Address == original.HTTPS.Address {
						t.Fatal("HTTPS config alias retained")
					}
				}
			}
		})
	}
}

func TestHTTPFallbackConfiguredRouting(t *testing.T) {
	origin := func(label string, h2 bool) *httptest.Server {
		site := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				w.WriteHeader(405)
				return
			}
			io.WriteString(w, label+":"+r.URL.Path)
		}))
		site.Config.Protocols = new(http.Protocols)
		site.Config.Protocols.SetHTTP1(true)
		site.Config.Protocols.SetUnencryptedHTTP2(h2)
		site.Start()
		t.Cleanup(site.Close)
		return site
	}
	h1, h2 := origin("http1", false), origin("h2c", true)
	for _, dual := range []bool{false, true} {
		t.Run(map[bool]string{false: "h1-only", true: "h1-h2c"}[dual], func(t *testing.T) {
			st, ct := settings(t, "tls")
			cfg := Config{Role: "server", Secret: testKey, TLS: st, HTTPFallback: &HTTPFallbackConfig{HTTP1: h1.Listener.Addr().String()}}
			if dual {
				cfg.HTTPFallback.H2C = h2.Listener.Addr().String()
			}
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Config
			if err = json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			svc, addr := start(t, decoded)
			transport := &http.Transport{TLSClientConfig: httpFallbackTLS(t, st.Certificate), ForceAttemptHTTP2: true}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			req, _ := http.NewRequest("GET", "https://"+addr+"/owned", nil)
			req.Host = "unowned.invalid"
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			protocol, label := "HTTP/1.1", "http1"
			if dual {
				protocol, label = "HTTP/2.0", "h2c"
			}
			if res.Proto != protocol || string(body) != label+":/owned" {
				t.Fatal(res.Proto, string(body))
			}
			// No ALPN is explicitly mapped to the mandatory HTTP/1 backend.
			plainCfg := httpFallbackTLS(t, st.Certificate)
			c, err := tls.Dial("tcp", addr, plainCfg)
			if err != nil {
				t.Fatal(err)
			}
			c.SetDeadline(time.Now().Add(time.Second))
			io.WriteString(c, "GET /no-alpn HTTP/1.1\r\nHost: elsewhere.invalid\r\nConnection: close\r\n\r\n")
			data, err := io.ReadAll(c)
			c.Close()
			if err != nil || !strings.Contains(string(data), "http1:/no-alpn") {
				t.Fatal(string(data), err)
			}
			// Ordinary authenticated Veil still opens an actual backend through this service.
			_, proxy := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr})
			local, err := socksDial(proxy, h1.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			io.WriteString(local, "GET /veil HTTP/1.1\r\nHost: owned\r\nConnection: close\r\n\r\n")
			local.CloseWrite()
			data, err = io.ReadAll(local)
			local.Close()
			if err != nil || !strings.Contains(string(data), "http1:/veil") || svc.Stats.Authenticated.Load() != 1 {
				t.Fatal("authenticated Veil failed", string(data), err)
			}
		})
	}
}

func TestHTTPFallbackBackendFailureAndStop(t *testing.T) {
	for _, silent := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "stop"}[silent], func(t *testing.T) {
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			address := backend.Addr().String()
			accepted := make(chan net.Conn, 1)
			if silent {
				go func() { c, _ := backend.Accept(); accepted <- c }()
			} else {
				backend.Close()
			}
			st, _ := settings(t, "tls")
			svc, err := New(Config{Role: "server", Secret: testKey, TLS: st, HTTPFallback: &HTTPFallbackConfig{HTTP1: address}, DialSeconds: 1, IdleSeconds: 1})
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
			c, err := tls.Dial("tcp", ln.Addr().String(), httpFallbackTLS(t, st.Certificate))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			io.WriteString(c, "GET / HTTP/1.1\r\nHost: owned\r\n\r\n")
			if silent {
				target := <-accepted
				defer target.Close()
				cancel()
			}
			data, _ := io.ReadAll(c)
			if len(data) != 0 {
				t.Fatal("manufactured fallback response", string(data))
			}
			cancel()
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("service did not join")
			}
			if svc.Stats.ActiveConnections.Load() != 0 {
				t.Fatal("fallback connection retained")
			}
		})
	}
}
