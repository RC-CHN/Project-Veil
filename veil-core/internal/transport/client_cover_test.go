package transport

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type clientCoverServer struct {
	*httptest.Server
	connections atomic.Int64
}

func coverTestServer(t *testing.T, h2 bool, handle http.HandlerFunc) (*clientCoverServer, Settings) {
	t.Helper()
	server := &clientCoverServer{Server: httptest.NewUnstartedServer(handle)}
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			server.connections.Add(1)
		}
	}
	server.EnableHTTP2 = h2
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := server.Certificate()
	name := "example.com"
	if len(cert.DNSNames) > 0 {
		name = cert.DNSNames[0]
	}
	return server, Settings{Mode: "reality", ServerName: name, RealityPublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), ShortID: "0102030405060708", CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})), Fingerprint: "chrome133"}
}

func coverHandshake(t *testing.T, ctx context.Context, server *clientCoverServer, s Settings) error {
	t.Helper()
	handshake, err := Client(s)
	if err != nil {
		return err
	}
	raw, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	c, err := handshake(ctx, raw)
	if c != nil {
		c.Close()
		t.Fatal("cover connection escaped to core and could carry AUTH")
	}
	if err == nil {
		t.Fatal("cover certificate authenticated as REALITY")
	}
	return err
}

func TestClientCoverTrustedH1H2(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "h1"
		if h2 {
			name = "h2"
		}
		t.Run(name, func(t *testing.T) {
			seen := make(chan *http.Request, 1)
			server, s := coverTestServer(t, h2, func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Clone(context.Background())
				w.WriteHeader(http.StatusNoContent)
			})
			if err := coverHandshake(t, context.Background(), server, s); !strings.Contains(err.Error(), "REALITY authentication failed") {
				t.Fatal(err)
			}
			select {
			case r := <-seen:
				if server.connections.Load() != 1 {
					t.Fatal("extra connection", server.connections.Load())
				}
				// The Go h2 server sets Request.TLS only for :scheme=https;
				// Request.URL itself remains in origin form.
				if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 {
					t.Fatal("request did not remain on TLS")
				}
				want := 1
				if h2 {
					want = 2
				}
				if r.ProtoMajor != want || r.Method != "GET" || r.URL.Path != "/" || r.Host != s.ServerName || r.ContentLength != 0 {
					t.Fatalf("unexpected cover request: %s %s %s %s %d", r.Proto, r.Method, r.URL, r.Host, r.ContentLength)
				}
				if !strings.Contains(r.UserAgent(), "Chrome/133.") {
					t.Fatal(r.UserAgent())
				}
			case <-time.After(time.Second):
				t.Fatal("no real HTTPS request")
			}
		})
	}
}

func TestClientCoverUntrustedAndWrongName(t *testing.T) {
	for _, mode := range []string{"untrusted", "wrong SNI", "invalid CA"} {
		t.Run(mode, func(t *testing.T) {
			var seen atomic.Int64
			server, s := coverTestServer(t, true, func(w http.ResponseWriter, r *http.Request) { seen.Add(1); w.WriteHeader(204) })
			switch mode {
			case "untrusted":
				s.CAPEM = ""
			case "wrong SNI":
				s.ServerName = "mismatch.invalid"
			case "invalid CA":
				s.CAPEM = "not PEM"
			}
			coverHandshake(t, context.Background(), server, s)
			if seen.Load() != 0 {
				t.Fatal("sent HTTP without validating cover identity")
			}
		})
	}
}

func TestClientCoverDoesNotRedirect(t *testing.T) {
	var seen atomic.Int64
	server, s := coverTestServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		w.Header().Set("Location", "https://must-not-resolve.invalid/other")
		w.WriteHeader(302)
	})
	coverHandshake(t, context.Background(), server, s)
	if seen.Load() != 1 {
		t.Fatalf("requests: %d", seen.Load())
	}
}

func TestClientCoverBoundedResponseAndCancellation(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, mode := range []string{"body budget", "header budget", "reset", "blackhole", "cancel"} {
			name := "h1/" + mode
			if h2 {
				name = "h2/" + mode
			}
			t.Run(name, func(t *testing.T) {
				entered := make(chan struct{})
				finished := make(chan struct{})
				server, s := coverTestServer(t, h2, func(w http.ResponseWriter, r *http.Request) {
					defer close(finished)
					close(entered)
					if mode == "reset" {
						panic(http.ErrAbortHandler)
					}
					if mode == "header budget" {
						w.Header().Set("Large", strings.Repeat("x", 128<<10))
						w.WriteHeader(200)
						w.Write([]byte("ok"))
						return
					}
					if mode == "body budget" {
						w.WriteHeader(200)
						p := make([]byte, 16384)
						for {
							if _, err := w.Write(p); err != nil {
								return
							}
						}
					}
					<-r.Context().Done()
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "cancel" {
					go func() { <-entered; cancel() }()
				}
				start := time.Now()
				err := coverHandshake(t, ctx, server, s)
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("lost parent cancellation", err)
				}
				elapsed := time.Since(start)
				if elapsed > 3*time.Second {
					t.Fatal("unbounded fallback", elapsed)
				}
				if mode != "blackhole" && elapsed > time.Second {
					t.Fatal("did not stop promptly", mode, elapsed)
				}
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Fatal("server request not canceled")
				}
			})
		}
	}
}

func TestClientCoverRejectExpiredCertificate(t *testing.T) {
	server, s := coverTestServer(t, false, func(w http.ResponseWriter, r *http.Request) { t.Error("expired certificate reached HTTP") })
	cert := *server.Certificate()
	cert.NotAfter = time.Now().Add(-time.Hour)
	// Verification checks parsed validity; signature remains tied to the same DER.
	if err := verifyCoverCertificate(s, []*x509.Certificate{&cert}); err == nil {
		t.Fatal("expired certificate accepted")
	}
}

func TestClientCoverParentDeadline(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "h1"
		if h2 {
			name = "h2"
		}
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			server, s := coverTestServer(t, h2, func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() })
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			start := time.Now()
			if err := coverHandshake(t, ctx, server, s); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("lost parent deadline", err)
			}
			if time.Since(start) > 750*time.Millisecond {
				t.Fatal("fallback extended the parent deadline")
			}
			select {
			case <-entered:
			default:
				t.Fatal("deadline expired before the fallback path was exercised")
			}
		})
	}
}
