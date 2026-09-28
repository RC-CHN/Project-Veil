// handshakebench measures fresh authenticated tunnels to owned loopback peers.
// Each sample includes TLS, AUTH+OPEN, a byte-checked echo, FIN/DONE, and close.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"time"
	"veil/internal/mux"
	"veil/internal/transport"
	"veil/internal/wire"
)

func sample(server string, target, key []byte, handshake transport.Handshake, padding bool, profile mux.Profile) (time.Duration, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", server)
	if err != nil {
		return 0, err
	}
	defer raw.Close()
	raw.SetDeadline(start.Add(5 * time.Second))
	c, err := handshake(ctx, raw)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	exporter, err := transport.Export(c)
	if err != nil {
		return 0, err
	}
	auth, err := wire.AuthPayload(key, exporter)
	if err != nil {
		return 0, err
	}
	var prefix bytes.Buffer
	if err := wire.Write(&prefix, wire.Auth, auth); err != nil {
		return 0, err
	}
	opts := mux.Options{Profile: profile, Prefix: prefix.Bytes(), WriteTimeout: 5 * time.Second}
	if padding {
		opts.Padding = func(limit, records, budget int) error { return transport.RecordBudget(c, limit, records, budget) }
	}
	m, err := mux.New(c, opts)
	if err != nil {
		return 0, err
	}
	defer func() { m.Close(); m.Wait() }()
	stop := context.AfterFunc(ctx, func() { m.Close() })
	defer stop()
	stream, err := m.Open(ctx, target)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	ready := time.Since(start)
	payload := bytes.Repeat([]byte{73}, 64)
	req := make([]byte, 9+len(payload))
	req[0] = 'E'
	binary.BigEndian.PutUint64(req[1:9], uint64(len(payload)))
	copy(req[9:], payload)
	if _, err := stream.Write(req); err != nil {
		return 0, err
	}
	if err := stream.CloseWrite(); err != nil {
		return 0, err
	}
	got, err := io.ReadAll(io.LimitReader(stream, int64(len(payload)+1)))
	if err != nil {
		return 0, err
	}
	if !bytes.Equal(got, payload) {
		return 0, fmt.Errorf("echo mismatch")
	}
	return ready, stream.WaitDone(ctx)
}

func main() {
	file := flag.String("config", "", "client JSON configuration")
	target := flag.String("target", "", "owned loopback benchmark backend")
	rounds := flag.Int("rounds", 2000, "fresh tunnels, one at a time")
	warmup := flag.Int("warmup", 16, "unmeasured fresh tunnels")
	flag.Parse()
	var cfg struct {
		Server  string             `json:"server"`
		Secret  string             `json:"secret"`
		TLS     transport.Settings `json:"tls"`
		Traffic *mux.Profile       `json:"traffic"`
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		panic(err)
	}
	for _, address := range []string{cfg.Server, *target} {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			panic("benchmark requires owned loopback endpoints")
		}
	}
	if *rounds < 1 || *warmup < 0 {
		panic("invalid rounds")
	}
	key, err := transport.DecodeKey(cfg.Secret)
	if err != nil {
		panic(err)
	}
	address, err := wire.EncodeAddress(*target)
	if err != nil {
		panic(err)
	}
	handshake, err := transport.Client(cfg.TLS)
	if err != nil {
		panic(err)
	}
	profile := mux.DefaultProfile()
	if cfg.Traffic != nil {
		profile = *cfg.Traffic
	}
	if err := profile.Validate(); err != nil {
		panic(err)
	}
	for range *warmup {
		if _, err := sample(cfg.Server, address, key, handshake, cfg.TLS.RecordPadding, profile); err != nil {
			panic(err)
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true})
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		panic(err)
	}
	start := time.Now()
	latencies := make([]int64, *rounds)
	for i := range latencies {
		ready, err := sample(cfg.Server, address, key, handshake, cfg.TLS.RecordPadding, profile)
		if err != nil {
			panic(err)
		}
		latencies[i] = ready.Nanoseconds()
	}
	elapsed := time.Since(start).Seconds()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"seconds": elapsed, "requests": len(latencies), "mode": "H", "bytes": 128 * len(latencies),
		"p50_us": float64(latencies[len(latencies)/2]) / 1e3,
		"p99_us": float64(latencies[len(latencies)*99/100]) / 1e3,
	})
	// Let the runner stop perf before process exit, retaining valid PID counters.
	io.Copy(io.Discard, os.Stdin)
}
