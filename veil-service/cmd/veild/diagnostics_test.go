//go:build linux || freebsd || windows

package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"
	"veil/service"
)

func TestDiagnosticLogHopDedupAndRestart(t *testing.T) {
	var buffer bytes.Buffer
	l := diagnosticLog{logger: log.New(&buffer, "", 0), cursors: map[string]diagnosticCursor{}}
	now := time.Now().UTC()
	relay := service.Snapshot{State: "running", Diagnostics: &service.Diagnostics{StartedAt: now, Sequence: 4, Recent: []service.ErrorEvent{{Sequence: 4, FirstAt: now, At: now, Count: 4, Stage: "tunnel dial", Code: "timeout", Message: "failure\nnot a new log entry"}}}}
	s := service.Snapshot{State: "running", Relay: &relay}
	l.sample("test", "endpoint", s, map[string]bool{})
	first := buffer.Len()
	l.sample("test", "endpoint", s, map[string]bool{})
	if buffer.Len() != first {
		t.Fatal("unchanged snapshot logged twice")
	}
	relay.Diagnostics.Recent[0].Sequence++
	relay.Diagnostics.Sequence++
	relay.Diagnostics.Recent[0].Count++
	l.sample("test", "endpoint", s, map[string]bool{})
	if buffer.Len() == first {
		t.Fatal("repeated error count not logged")
	}
	relay.Diagnostics.StartedAt = now.Add(time.Second)
	relay.Diagnostics.Sequence = 1
	relay.Diagnostics.Recent[0].Sequence = 1
	l.sample("test", "endpoint", s, map[string]bool{})
	errors := 0
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatal("log injection", err)
		}
		if value["type"] == "connection_error" {
			errors++
			if value["connection_id"] != "test" || value["hop"] != "relay" {
				t.Fatal("hop identity lost")
			}
		}
	}
	if errors != 3 {
		t.Fatal("restart discarded error sequence", errors)
	}
}
