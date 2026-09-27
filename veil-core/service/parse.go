package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const MaxConfigSize = 1 << 20

// Parse reads exactly one bounded JSON object, retaining the CLI's defaults.
// It neither binds a listener nor connects to the network.
func Parse(r io.Reader) (Config, error) {
	var cfg Config
	b, err := io.ReadAll(io.LimitReader(r, MaxConfigSize+1))
	if err != nil {
		return cfg, err
	}
	if len(b) > MaxConfigSize {
		return cfg, errors.New("configuration exceeds 1 MiB")
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return cfg, errors.New("configuration must be a JSON object of at most 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return cfg, errors.New("configuration must contain one JSON object")
	}
	return cfg, cfg.Defaults()
}

// Validate checks transport settings and local certificate files without
// listening or dialing. Successful validation does not imply reachability.
func Validate(cfg Config) error {
	if err := cfg.Defaults(); err != nil {
		return err
	}
	for _, field := range []struct {
		name, addr string
		zero       bool
	}{
		{"listen", cfg.Listen, true}, {"server", cfg.Server, false},
	} {
		if field.name == "server" && cfg.Role != "client" {
			continue
		}
		host, port, err := net.SplitHostPort(field.addr)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 || (!field.zero && (n == 0 || host == "")) {
			return fmt.Errorf("%s: invalid host or numeric port", field.name)
		}
	}
	s, err := New(cfg)
	if err != nil {
		return err
	}
	return s.Close()
}
