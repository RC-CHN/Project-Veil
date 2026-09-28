package main

import (
	"bytes"
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
	id := flag.String("id", "", "connection ID for connection_* actions")
	apply := flag.Bool("apply", false, "apply a saved connection immediately")
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
		return errors.New("use -socket PATH [-config FILE|-] [-if-revision HASH] status|config|validate|save|start|stop|restart, connections|connection_get|connection_export|connection_save|connection_start|connection_stop|connection_delete|connection_test (-id ID; connection_save uses -config FILE and optional -apply), or profilegen without a socket")
	}
	q := control.Request{Version: control.Version, Action: flag.Arg(0), ID: *id, Apply: *apply}
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
		if q.Action == "connection_save" {
			d := json.NewDecoder(bytes.NewReader(b))
			d.DisallowUnknownFields()
			var shape map[string]json.RawMessage
			if err := json.Unmarshal(b, &shape); err != nil {
				return err
			}
			if _, bundle := shape["profile"]; bundle {
				var v struct {
					Version int              `json:"version"`
					Profile *control.Profile `json:"profile"`
					Relay   *control.Profile `json:"relay,omitempty"`
				}
				if err := d.Decode(&v); err != nil {
					return err
				}
				if v.Version != 1 {
					return errors.New("unsupported connection bundle version")
				}
				q.Profile, q.Relay = v.Profile, v.Relay
			} else if err := d.Decode(&q.Profile); err != nil {
				return err
			}
			if d.Decode(new(any)) != io.EOF {
				return errors.New("expected one configuration object")
			}
		} else {
			q.Config = b
		}
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
