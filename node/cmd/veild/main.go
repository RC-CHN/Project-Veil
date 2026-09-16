package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	core "veil.local/core"
	"veil.local/node/internal/compose"
	"veil.local/node/internal/config"
)

func run() error {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(core.Version)
		return nil
	}
	fs := flag.NewFlagSet("veild", flag.ContinueOnError)
	path := fs.String("config", "", "node configuration file")
	if e := fs.Parse(os.Args[1:]); e != nil {
		return e
	}
	if *path == "" || fs.NArg() != 0 {
		return errors.New("usage: veild --config PATH")
	}
	loaded, e := config.Load(*path, compose.FileReader())
	if e != nil {
		return e
	}
	node, e := compose.New(loaded)
	if e != nil {
		return e
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
		return e
	case e = <-ready:
		if e != nil {
			cancel()
			<-done
			return e
		}
	}
	if e = json.NewEncoder(os.Stdout).Encode(node.Snapshot()); e != nil {
		cancel()
		<-done
		return e
	}
	e = <-done
	cancel()
	if outErr := json.NewEncoder(os.Stdout).Encode(node.Snapshot()); e == nil {
		e = outErr
	}
	return e
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
