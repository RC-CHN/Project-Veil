//go:build linux || freebsd

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"veil-service/control"
	"veil-service/internal/buildinfo"
	"veil-service/local"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("state-dir", "", "private directory containing the saved config")
	socket := flag.String("socket", "", "private Unix control socket")
	autostart := flag.Bool("autostart", false, "start saved profile when the daemon starts")
	version := flag.Bool("version", false, "print release version")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String())
		return nil
	}
	if *dir == "" || *socket == "" || flag.NArg() != 0 {
		return errors.New("use -state-dir DIR -socket PATH [-autostart]")
	}
	if err := control.PrivateDir(*dir); err != nil {
		return err
	}
	lock, err := local.Lock(filepath.Join(*dir, ".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	m, err := control.Open(*dir)
	if err != nil {
		return err
	}
	defer func() {
		m.Close()
		s := m.Status().Stats
		log.Printf("stopped accepted=%d rejected=%d completed=%d failed=%d", s.Accepted, s.Rejected, s.Completed, s.Failed)
	}()
	ln, err := local.Listen(*socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if *autostart && m.Status().SavedRevision != "" {
		r := m.Handle(control.Request{Version: control.Version, Action: "start"})
		if r.Error != nil {
			// Keep the control socket available so a failed autostart can be
			// diagnosed and fixed by saving a new profile, without a crash loop.
			log.Printf("autostart failed: %s", r.Error.Message)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("control listening on %s", *socket)
	return local.Serve(ctx, ln, m)
}
