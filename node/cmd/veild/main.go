package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"
	core "veil.local/core"
	"veil.local/core/telemetry"
	"veil.local/node/internal/compose"
	"veil.local/node/internal/config"
)

type statusRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Version       string `json:"version"`
	RunID         string `json:"run_id"`
	PID           int    `json:"pid"`
	Phase         string `json:"phase"`
	Error         string `json:"error,omitempty"`
	telemetry.Snapshot
}

func run() error {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(core.Version)
		return nil
	}
	fs := flag.NewFlagSet("veild", flag.ContinueOnError)
	path := fs.String("config", "", "node configuration file")
	interval := fs.Duration("status-interval", 0, "periodic status interval: 0 disables, otherwise 100ms..1h")
	if e := fs.Parse(os.Args[1:]); e != nil {
		return e
	}
	if *path == "" || fs.NArg() != 0 {
		return errors.New("usage: veild --config PATH")
	}
	if *interval != 0 && (*interval < 100*time.Millisecond || *interval > time.Hour) {
		return errors.New("status-interval must be zero or 100ms..1h")
	}
	loaded, e := config.Load(*path, compose.FileReader())
	if e != nil {
		return e
	}
	node, e := compose.New(loaded)
	if e != nil {
		return e
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return e
	}
	runID := hex.EncodeToString(nonce[:])
	output := func(phase string, failure error) error {
		record := statusRecord{SchemaVersion: 1, Kind: "node_status", Version: core.Version, RunID: runID, PID: os.Getpid(), Phase: phase, Snapshot: node.Snapshot()}
		if failure != nil {
			record.Error = failure.Error()
		}
		return json.NewEncoder(os.Stdout).Encode(record)
	}
	parent, stop := compose.SignalContext()
	defer stop()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan error, 1)
	go func() { done <- node.Run(ctx) }()
	go func() { ready <- node.WaitReady(ctx) }()
	select {
	case e = <-done:
		cancel()
		<-ready
		if outputErr := output("stopped", e); e == nil {
			e = outputErr
		}
		return e
	case e = <-ready:
		if e != nil {
			cancel()
			<-done
			_ = output("failed", e)
			return e
		}
	}
	if e = output("ready", nil); e != nil {
		cancel()
		<-done
		return e
	}
	var ticks <-chan time.Time
	if *interval != 0 {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case e = <-done:
			cancel()
			if outErr := output("stopped", e); e == nil {
				e = outErr
			}
			return e
		case <-ticks:
			if e = output("running", nil); e != nil {
				cancel()
				<-done
				return e
			}
		}
	}
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
