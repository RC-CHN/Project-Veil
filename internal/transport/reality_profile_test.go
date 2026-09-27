package transport

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestRealityProfileConfig(t *testing.T) {
	for _, s := range []Settings{
		{Fingerprint: "unknown"},
		{Fingerprints: []string{}},
		{Fingerprint: "chrome", Fingerprints: []string{"chrome120"}},
		{Fingerprints: []string{"chrome133", "unknown"}},
		{Fingerprints: []string{""}},
		{Fingerprints: []string{"chrome", "chrome133"}},
	} {
		if _, err := realityProfiles(s); err == nil {
			t.Errorf("accepted invalid profile configuration: %+v", s)
		}
	}
}

// Exercise the actual shared handshake factory concurrently. A selection pool
// must never share mutable ClientHello state or ephemeral material across calls.
func TestRealityClientHelloFresh(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Client(Settings{
		Mode: "reality", ServerName: "cover.test", ShortID: "0000000000000000",
		RealityPublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		Fingerprints:     []string{"chrome120", "chrome131", "chrome133"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seenRandom, seenSession := map[string]bool{}, map[string]bool{}
	for range 16 {
		t.Run("connection", func(t *testing.T) {
			t.Parallel()
			raw, peer := net.Pipe()
			defer peer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				defer raw.Close()
				c, err := h(ctx, raw)
				if c != nil {
					c.Close()
				}
				done <- err
			}()
			peer.SetDeadline(time.Now().Add(2 * time.Second))
			var hdr [5]byte
			if _, err := io.ReadFull(peer, hdr[:]); err != nil {
				t.Fatal(err)
			}
			b := make([]byte, binary.BigEndian.Uint16(hdr[3:5]))
			if _, err := io.ReadFull(peer, b); err != nil {
				t.Fatal(err)
			}
			peer.Close()
			if err := <-done; err == nil {
				t.Fatal("authenticated a peer that sent no TLS handshake")
			}
			if hdr[0] != 22 || len(b) < 71 || b[0] != 1 || b[38] != 32 {
				t.Fatal("invalid REALITY ClientHello layout")
			}
			mu.Lock()
			defer mu.Unlock()
			random, session := string(b[6:38]), string(b[39:71])
			if seenRandom[random] || seenSession[session] {
				t.Fatal("repeated random or authenticated session ID")
			}
			seenRandom[random], seenSession[session] = true, true
		})
	}
}
