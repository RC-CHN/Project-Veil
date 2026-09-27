package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"veil/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	file := flag.String("config", "", "JSON configuration path")
	keygen := flag.Bool("keygen", false, "generate a business secret and REALITY keypair")
	flag.Parse()
	if *keygen {
		key, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		secret := make([]byte, 32)
		rand.Read(secret)
		id := make([]byte, 8)
		rand.Read(id)
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"secret": base64.RawURLEncoding.EncodeToString(secret), "reality_private_key": base64.RawURLEncoding.EncodeToString(key.Bytes()), "reality_public_key": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "short_id": hex.EncodeToString(id)})
	}
	if *file == "" {
		return fmt.Errorf("use -config path.json or -keygen")
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	var cfg proxy.Config
	if err = dec.Decode(&cfg); err != nil {
		return err
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("configuration must contain one JSON object")
	}
	if err = cfg.Defaults(); err != nil {
		return err
	}
	svc, err := proxy.New(cfg)
	if err != nil {
		return err
	}
	defer svc.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Printf("Veil v0.1 %s listening on %s (%s)", cfg.Role, ln.Addr(), cfg.TLS.Mode)
	err = svc.Serve(ctx, ln)
	log.Printf("stopped accepted=%d rejected=%d completed=%d failed=%d", svc.Stats.Accepted.Load(), svc.Stats.Rejected.Load(), svc.Stats.Completed.Load(), svc.Stats.Failed.Load())
	return err
}
