package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"veil/core"
	"veil/inbound"
	"veil/service"
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
	profilegen := flag.Bool("profilegen", false, "generate a bounded traffic profile for a new configuration")
	flag.Parse()
	if *profilegen {
		return json.NewEncoder(os.Stdout).Encode(service.GenerateTrafficProfile())
	}
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
		return fmt.Errorf("use -config path.json, -keygen or -profilegen")
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	cfg, err := service.Parse(f)
	if err != nil {
		return err
	}
	svc, err := service.New(cfg)
	if err != nil {
		return err
	}
	defer svc.Close()
	svc.OnError = func(err error) {
		var op *core.OpError
		if errors.As(err, &op) {
			log.Printf("connection: %v", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := inbound.Listen(cfg.Listen)
	if err != nil {
		return err
	}
	log.Printf("Veil v0.3 %s listening on %s (%s)", cfg.Role, ln.Addr(), cfg.TLS.Mode)
	err = svc.Serve(ctx, ln)
	log.Printf("stopped accepted=%d rejected=%d completed=%d failed=%d", svc.Stats.Accepted.Load(), svc.Stats.Rejected.Load(), svc.Stats.Completed.Load(), svc.Stats.Failed.Load())
	return err
}
