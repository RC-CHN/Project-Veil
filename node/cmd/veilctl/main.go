package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	core "veil.local/core"
	"veil.local/core/model"
	"veil.local/node/internal/compose"
	"veil.local/node/internal/config"
)

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: veilctl version|capabilities|modelgen|configcheck|prepare|probe")
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(core.Version)
		return nil
	case "capabilities":
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"platform": runtime.GOOS, "foreground": true, "socks_tcp": true, "socks_udp": true, "private_preparation": runtime.GOOS == "linux", "echo_probe": true, "periodic_status": true, "tun": false, "service_installation": false, "control_ipc": false})
	case "prepare":
		return prepareCommand(os.Args[2:])
	case "probe":
		return probeCommand(os.Args[2:])
	case "modelgen":
		fs := flag.NewFlagSet("modelgen", flag.ContinueOnError)
		lanes := fs.Int("lanes", 4, "model lanes (1-4)")
		if e := fs.Parse(os.Args[2:]); e != nil {
			return e
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		m, e := model.Generate(*lanes)
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(append(m.Bytes(), '\n'))
		return e
	case "configcheck":
		fs := flag.NewFlagSet("configcheck", flag.ContinueOnError)
		path := fs.String("config", "", "node configuration file")
		if e := fs.Parse(os.Args[2:]); e != nil {
			return e
		}
		if *path == "" || fs.NArg() != 0 {
			return errors.New("configcheck --config PATH")
		}
		loaded, e := config.Load(*path, compose.FileReader())
		if e != nil {
			return e
		}
		_, e = compose.New(loaded)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(loaded.Report())
	default:
		return errors.New("unknown command")
	}
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
