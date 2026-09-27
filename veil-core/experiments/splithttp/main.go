// splithttp is an isolated measurement fixture, not a supported Veil transport.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"veil/inbound"
	"veil/internal/socks"
)

type config struct {
	Role, Listen, Server, Mode, Secret string
	Certificate, PrivateKey, CAFile    string
}

func loopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err == nil && (host == "localhost" || net.ParseIP(host).IsLoopback())
}

func main() {
	path := flag.String("config", "", "owned fixture configuration")
	flag.Parse()
	ns, err := os.Readlink("/proc/self/ns/net")
	host, hostErr := os.Readlink("/proc/1/ns/net")
	if err != nil || hostErr != nil || ns == host || os.Getenv("VEIL_ISOLATED_NETNS") != "1" {
		log.Fatal("experiment requires a private Linux network namespace")
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatal(err)
	}
	if !loopback(cfg.Listen) || len(cfg.Secret) < 32 {
		log.Fatal("loopback listener and fixture secret required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	l, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.Role == "server" {
		h := &server{secret: cfg.Secret, sessions: make(map[string]*pair)}
		s := &http.Server{Handler: h, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
			DynamicRecordSizingDisabled: true}, HTTP2: h2Config(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
		go func() { <-ctx.Done(); h.close(); s.Close() }()
		if err := s.ServeTLS(l, cfg.Certificate, cfg.PrivateKey); err != http.ErrServerClosed {
			log.Print(err)
		}
	} else if cfg.Role == "client" {
		if !loopback(cfg.Server) {
			log.Fatal("loopback server required")
		}
		c, err := newClient(cfg)
		if err != nil {
			log.Fatal(err)
		}
		defer c.close()
		err = inbound.Serve(ctx, l, func(ctx context.Context, local net.Conn) error {
			defer local.Close()
			cancel := context.AfterFunc(ctx, func() { local.Close() })
			defer cancel()
			local.SetDeadline(time.Now().Add(10 * time.Second))
			address, err := socks.ReadConnect(local)
			if err != nil {
				return err
			}
			t, err := c.open(ctx, address)
			if err != nil {
				socks.Reply(local, 1)
				return err
			}
			defer t.Close()
			if err := socks.Reply(local, 0); err != nil {
				return err
			}
			local.SetDeadline(time.Time{})
			return relay(local, t)
		}, inbound.ServeOptions{MaxConnections: 128})
		if err != nil {
			log.Print(err)
		}
	} else {
		log.Fatal("invalid role")
	}
}
