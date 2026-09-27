// probepeer provides owned-loopback reference endpoints for differential tests.
package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	utls "github.com/metacubex/utls"
	"veil/internal/transport"
)

func main() {
	role := flag.String("role", "cover", "cover or upstream-reality")
	listen := flag.String("listen", "127.0.0.1:0", "loopback address")
	cert := flag.String("cert", "", "reference certificate")
	key := flag.String("key", "", "reference private key")
	config := flag.String("config", "", "Veil server configuration for upstream REALITY")
	timeout := flag.Duration("timeout", 2*time.Second, "reference HTTP header/handshake timeout")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("reference endpoint requires loopback")
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	if *role == "cover" {
		s := &http.Server{ReadHeaderTimeout: *timeout, TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
			CurvePreferences: []tls.CurveID{tls.X25519}, NextProtos: []string{"http/1.1"},
		}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Connection", "close")
			fmt.Fprintln(w, "owned Go TLS reference")
		})}
		log.Fatal(s.ServeTLS(ln, *cert, *key))
	}
	if *role != "upstream-reality" {
		log.Fatal("unknown role")
	}
	var input struct {
		TLS transport.Settings `json:"tls"`
	}
	b, err := os.ReadFile(*config)
	if err != nil {
		log.Fatal(err)
	}
	if err = json.Unmarshal(b, &input); err != nil {
		log.Fatal(err)
	}
	private, err := transport.DecodeKey(input.TLS.RealityPrivateKey)
	if err != nil {
		log.Fatal(err)
	}
	id, err := hex.DecodeString(input.TLS.ShortID)
	if err != nil || len(id) != 8 {
		log.Fatal("invalid short ID")
	}
	var short [8]byte
	copy(short[:], id)
	d := net.Dialer{Timeout: 3 * time.Second}
	cfg := &utls.RealityConfig{Type: "tcp", Dest: input.TLS.CoverAddress,
		DialContext: d.DialContext, PrivateKey: private, ShortIds: map[[8]byte]bool{short: true},
		ServerNames: map[string]bool{input.TLS.ServerName: true}, MaxTimeDiff: time.Minute}
	cfg.MinVersion, cfg.MaxVersion = utls.VersionTLS13, utls.VersionTLS13
	cfg.SessionTicketsDisabled = true
	slots := make(chan struct{}, 64)
	for {
		raw, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		select {
		case slots <- struct{}{}:
		default:
			raw.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			defer raw.Close()
			// No Veil deadlines or business framing: this isolates upstream fallback.
			c, err := utls.RealityServer(context.Background(), raw, cfg)
			if err == nil {
				c.Close()
			}
		}()
	}
}
