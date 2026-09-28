// Package rpc adapts rpcd executable calls to Veil's private control socket.
// Each method has its own rpcd ACL; callers cannot choose a socket or command.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"veil-service/control"
	"veil/service"
)

const Socket = "/var/run/veil/control.sock"

var methods = map[string]map[string]any{
	"connections":       {},
	"connection_get":    {"id": ""},
	"connection_export": {"id": ""},
	"connection_test":   {"id": ""},
	"connection_save":   {"profile": map[string]any{}, "relay": map[string]any{}, "expected_revision": "", "apply": true},
	"connection_start":  {"id": "", "expected_revision": ""},
	"connection_stop":   {"id": "", "expected_revision": ""},
	"connection_delete": {"id": "", "expected_revision": ""},
	"status":            {}, "config": {}, "stop": {},
	"validate": {"config": map[string]any{}},
	"save":     {"config": map[string]any{}, "expected_revision": ""},
	"start":    {"expected_revision": ""},
	"restart":  {"expected_revision": ""},
}

type Call func(context.Context, string, control.Request) (control.Response, error)

// Run speaks rpcd's list/call convention. Returned domain errors remain JSON.
func Run(ctx context.Context, args []string, input io.Reader, output io.Writer, call Call) error {
	encoder := json.NewEncoder(output)
	if len(args) == 1 && args[0] == "list" {
		return encoder.Encode(methods)
	}
	if len(args) != 2 || args[0] != "call" {
		return errors.New("usage: veil-rpc list | call METHOD")
	}
	fields, exists := methods[args[1]]
	if !exists {
		return encoder.Encode(control.Fail("unknown_action", errors.New("unsupported RPC method")))
	}
	body, err := io.ReadAll(io.LimitReader(input, service.MaxConfigSize+1025))
	if err != nil {
		return err
	}
	if len(body) > service.MaxConfigSize+1024 {
		return encoder.Encode(control.Fail("invalid_request", errors.New("request too large")))
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(body, &params); err != nil || params == nil {
		return encoder.Encode(control.Fail("invalid_request", errors.New("expected one JSON object")))
	}
	for name := range params {
		// rpcd can attach its authenticated session identifier. It is never forwarded.
		if name == "ubus_rpc_session" {
			continue
		}
		if _, exists := fields[name]; !exists {
			return encoder.Encode(control.Fail("invalid_request", fmt.Errorf("unexpected parameter: %s", name)))
		}
	}
	q := control.Request{Version: control.Version, Action: args[1], Config: params["config"]}
	if b, exists := params["id"]; exists {
		if err := json.Unmarshal(b, &q.ID); err != nil || string(b) == "null" {
			return encoder.Encode(control.Fail("invalid_request", errors.New("id must be a string")))
		}
	}
	if b, exists := params["profile"]; exists {
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(&q.Profile); err != nil || q.Profile == nil {
			return encoder.Encode(control.Fail("invalid_request", errors.New("invalid profile")))
		}
	}
	if b, exists := params["relay"]; exists && string(b) != "null" {
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(&q.Relay); err != nil || q.Relay == nil {
			return encoder.Encode(control.Fail("invalid_request", errors.New("invalid relay")))
		}
	}
	if b, exists := params["apply"]; exists {
		if err := json.Unmarshal(b, &q.Apply); err != nil || string(b) == "null" {
			return encoder.Encode(control.Fail("invalid_request", errors.New("apply must be a boolean")))
		}
	}
	if b, exists := params["expected_revision"]; exists {
		var revision string
		if err := json.Unmarshal(b, &revision); err != nil || string(b) == "null" {
			return encoder.Encode(control.Fail("invalid_request", errors.New("expected_revision must be a string")))
		}
		q.ExpectedRevision = &revision
	}
	result, err := call(ctx, Socket, q)
	if err != nil {
		return encoder.Encode(control.Fail("unavailable", errors.New("Veil control service is unavailable; check the service and system log")))
	}
	return encoder.Encode(result)
}
