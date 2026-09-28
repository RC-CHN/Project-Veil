package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"veil-service/control"
)

func TestRPCBoundary(t *testing.T) {
	for _, tc := range []struct {
		method, body string
		wantCall     bool
	}{
		{"status", `{}`, true},
		{"config", `{"ubus_rpc_session":"session"}`, true},
		{"save", `{"config":{"role":"client"},"expected_revision":""}`, true},
		{"status", `{"socket":"/other.sock"}`, false},
		{"status", `{"config":{}}`, false},
		{"save", `{"expected_revision":null}`, false},
		{"save", `{"expected_revision":3}`, false},
		{"restart", `[]`, false},
		{"restart", `null`, false},
		{"stop", `{} {}`, false},
		{"exec", `{}`, false},
	} {
		t.Run(tc.method+tc.body, func(t *testing.T) {
			var output bytes.Buffer
			called := false
			err := Run(context.Background(), []string{"call", tc.method}, strings.NewReader(tc.body), &output,
				func(_ context.Context, socket string, q control.Request) (control.Response, error) {
					called = true
					if socket != Socket || q.Action != tc.method || q.Version != control.Version {
						t.Fatalf("invalid forwarding: %q %+v", socket, q)
					}
					if tc.method == "save" && (q.ExpectedRevision == nil || *q.ExpectedRevision != "" || len(q.Config) == 0) {
						t.Fatal("lost save revision/configuration")
					}
					return control.Response{Version: control.Version}, nil
				})
			if err != nil || called != tc.wantCall || !json.Valid(output.Bytes()) {
				t.Fatalf("called=%v output=%s err=%v", called, output.String(), err)
			}
		})
	}
}
