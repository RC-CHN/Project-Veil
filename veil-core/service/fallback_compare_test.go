package service

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"
)

type fragmentedWrite struct{ net.Conn }

func (c fragmentedWrite) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := c.Conn.Write(p[:min(7, len(p))])
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

// Compare observable TLS/HTTP behavior with the SAME owned cover. Session
// tickets here belong to ordinary website fallback, not authenticated Veil.
func TestRealityFallbackMatchesHTTPS(t *testing.T) {
	cover, cfg := fallbackCoverConfig(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "reference body") }, &tls.Config{
		MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.X25519}, NextProtos: []string{"http/1.1"},
	})
	st, _ := fallbackKeys(t, cover)
	svc, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, alpn := range [][]string{nil, {"http/1.1"}, {"h2", "http/1.1"}} {
			for _, fragment := range []bool{false, true} {
				t.Run(fmt.Sprintf("%x/%v/fragment=%v", version, alpn, fragment), func(t *testing.T) {
					type observed struct {
						version, cipher uint16
						alpn            string
						cert            [32]byte
						resumed         bool
					}
					all := make([][]observed, 2)
					for index, endpoint := range []string{cover, addr} {
						config := cfg.Clone()
						config.MinVersion = version
						config.MaxVersion = version
						config.NextProtos = alpn
						config.ClientSessionCache = tls.NewLRUClientSessionCache(2)
						for attempt := 0; attempt < 2; attempt++ {
							raw, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
							if err != nil {
								t.Fatal(err)
							}
							raw.SetDeadline(time.Now().Add(3 * time.Second))
							if fragment {
								raw = fragmentedWrite{raw}
							}
							c := tls.Client(raw, config)
							if err := c.Handshake(); err != nil {
								c.Close()
								t.Fatal(endpoint, err)
							}
							if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: cover.test\r\nConnection: close\r\n\r\n"); err != nil {
								c.Close()
								t.Fatal(err)
							}
							response(t, c, "reference body")
							state := c.ConnectionState()
							all[index] = append(all[index], observed{state.Version, state.CipherSuite, state.NegotiatedProtocol, sha256.Sum256(state.PeerCertificates[0].Raw), state.DidResume})
							c.Close()
						}
					}
					if !reflect.DeepEqual(all[0], all[1]) {
						t.Fatalf("direct=%+v fallback=%+v", all[0], all[1])
					}
					if !all[1][1].resumed {
						t.Fatal("website ticket resumption not exercised")
					}
				})
			}
		}
	}
	if svc.Stats.Authenticated.Load() != 0 {
		t.Fatal("fallback authenticated as Veil")
	}
}

func TestRealityFallbackMatchesMalformedTLS(t *testing.T) {
	cover, _ := fallbackCover(t, func(http.ResponseWriter, *http.Request) {})
	st, _ := fallbackKeys(t, cover)
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	for _, payload := range [][]byte{
		[]byte("GET / HTTP/1.1\r\nHost: cover.test\r\n\r\n"),
		{22, 3, 1, 0, 4, 1, 0, 0, 0},  // complete but empty ClientHello
		{22, 3, 1, 0, 4, 20, 0, 0, 0}, // Finished before ClientHello
	} {
		var responses [][]byte
		for _, endpoint := range []string{cover, addr} {
			c, err := net.DialTimeout("tcp", endpoint, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			c.SetDeadline(time.Now().Add(2 * time.Second))
			c.Write(payload)
			b, err := io.ReadAll(c)
			c.Close()
			if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("probe stalled", endpoint, err)
			}
			responses = append(responses, b)
		}
		if !bytes.Equal(responses[0], responses[1]) {
			t.Fatalf("probe %x: direct=%x fallback=%x", payload, responses[0], responses[1])
		}
	}
}
