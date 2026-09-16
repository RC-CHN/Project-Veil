package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"runtime"
	"time"

	"veil.local/node/internal/compose"
	"veil.local/node/internal/prepare"
	"veil.local/node/internal/probe"
)

func prepareCommand(args []string) error {
	if runtime.GOOS != "linux" {
		return errors.New("prepare currently requires Linux private-directory support")
	}
	var o prepare.Options
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	fs.StringVar(&o.Output, "out", "", "new private directory; existing paths are refused")
	fs.StringVar(&o.ServerAddress, "server-address", "", "numeric server IP:port used by the client")
	fs.StringVar(&o.ServerName, "server-name", "", "certificate IP/DNS name; defaults to server IP")
	fs.StringVar(&o.ServerListen, "server-listen", "", "server bind address; defaults to wildcard at server port")
	fs.StringVar(&o.ClientListen, "client-listen", "127.0.0.1:1080", "loopback SOCKS listener")
	fs.StringVar(&o.ServerCertificate, "server-cert", "", "server PEM certificate chain")
	fs.StringVar(&o.ServerKey, "server-key", "", "private server PEM key (0600)")
	fs.StringVar(&o.ServerCA, "server-ca", "", "explicit server trust roots; omitted uses system trust")
	fs.BoolVar(&o.LocalTest, "local-test", false, "generate a loopback-only test server identity and permit loopback destinations")
	fs.IntVar(&o.Lanes, "lanes", 4, "model lane count (1-4)")
	fs.Uint64Var(&o.Limits.StreamBytes, "stream-bytes", 0, "per-stream lifetime byte cap; 0 preserves 8 MiB default")
	fs.Uint64Var(&o.Limits.CarrierBytes, "carrier-bytes", 0, "per-carrier encoded byte cap; 0 preserves 64 MiB default")
	fs.DurationVar(&o.MinValidity, "min-validity", 48*time.Hour, "required server chain validity (local-test default 1h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected prepare arguments")
	}
	if o.LocalTest {
		explicit := false
		fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "min-validity" })
		if !explicit {
			o.MinValidity = time.Hour
		}
	}
	report, err := prepare.Create(o, compose.FileReader(), compose.BundleWriter())
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

func probeCommand(args []string) error {
	var o probe.Options
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.StringVar(&o.SOCKS, "socks", "127.0.0.1:1080", "running local SOCKS node")
	fs.StringVar(&o.TCPEcho, "tcp-echo", "", "controlled TCP echo host:port (must support half-close)")
	fs.StringVar(&o.UDPEcho, "udp-echo", "", "controlled UDP echo host:port")
	fs.DurationVar(&o.Timeout, "timeout", 10*time.Second, "total probe deadline (100ms..2m)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected probe arguments")
	}
	ctx, cancel := compose.SignalContext()
	defer cancel()
	report, err := probe.Run(ctx, o)
	if outputErr := json.NewEncoder(os.Stdout).Encode(report); outputErr != nil {
		return outputErr
	}
	return err
}
