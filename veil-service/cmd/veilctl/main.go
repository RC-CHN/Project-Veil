package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"veil-service/control"
	"veil-service/internal/buildinfo"
	"veil-service/local"
	"veil/service"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	socket := flag.String("socket", "", "private control socket or Windows named pipe")
	config := flag.String("config", "", "configuration path, or - for stdin (validate/save)")
	expected := flag.String("if-revision", "", "optional saved revision guard")
	version := flag.Bool("version", false, "print release version")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String())
		return nil
	}
	if flag.NArg() == 1 && flag.Arg(0) == "profilegen" {
		return json.NewEncoder(os.Stdout).Encode(service.GenerateTrafficProfile())
	}
	if *socket == "" || flag.NArg() != 1 {
		return errors.New("use -socket PATH [-config FILE|-] [-if-revision HASH] status|config|validate|save|start|stop|restart, or profilegen without a socket")
	}
	q := control.Request{Version: control.Version, Action: flag.Arg(0)}
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "if-revision" {
			q.ExpectedRevision = expected
		}
	})
	if *config != "" {
		var r io.Reader = os.Stdin
		if *config != "-" {
			f, err := os.Open(*config)
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		b, err := io.ReadAll(io.LimitReader(r, service.MaxConfigSize+1))
		if err != nil {
			return err
		}
		if len(b) > service.MaxConfigSize {
			return errors.New("configuration exceeds 1 MiB")
		}
		q.Config = b
	}
	r, err := local.Call(context.Background(), *socket, q)
	if err != nil {
		return err
	}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err := e.Encode(r); err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("control: %s", r.Error.Code)
	}
	return nil
}
