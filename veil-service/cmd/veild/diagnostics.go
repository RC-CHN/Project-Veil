//go:build linux || freebsd || windows

package main

import (
	"context"
	"encoding/json"
	"log"
	"time"
	"veil-service/control"
	"veil/service"
)

// A bounded poll keeps logging out of the packet path. Status retains the last
// 16 error groups; OS service managers own log persistence and rotation.
type diagnosticLog struct {
	logger  *log.Logger
	cursors map[string]diagnosticCursor
}

type diagnosticCursor struct {
	started  time.Time
	sequence uint64
	state    string
}

func (d *diagnosticLog) sample(id, hop string, s service.Snapshot, seen map[string]bool) {
	key := id + "/" + hop
	seen[key] = true
	previous := d.cursors[key]
	current := diagnosticCursor{state: s.State + ":" + s.Error}
	if v := s.Diagnostics; v != nil {
		current.started, current.sequence = v.StartedAt, v.Sequence
		for _, event := range v.Recent {
			if previous.started.Equal(v.StartedAt) && event.Sequence <= previous.sequence {
				continue
			}
			value := struct {
				Type      string    `json:"type"`
				ID        string    `json:"connection_id"`
				Hop       string    `json:"hop"`
				StartedAt time.Time `json:"started_at"`
				service.ErrorEvent
			}{"connection_error", id, hop, v.StartedAt, event}
			b, _ := json.Marshal(value)
			d.logger.Print(string(b))
		}
	}
	if previous.state != current.state || !previous.started.Equal(current.started) {
		b, _ := json.Marshal(struct {
			Type  string `json:"type"`
			ID    string `json:"connection_id"`
			Hop   string `json:"hop"`
			State string `json:"state"`
			Error string `json:"error,omitempty"`
		}{"connection_state", id, hop, s.State, s.Error})
		d.logger.Print(string(b))
	}
	d.cursors[key] = current
	if s.Relay != nil {
		d.sample(id, "relay", *s.Relay, seen)
	}
}

func (d *diagnosticLog) poll(m *control.Manager) {
	seen := map[string]bool{}
	s := m.Status()
	if s.Role != "" || s.Error != "" {
		d.sample("standalone", "endpoint", s.Snapshot, seen)
	}
	for _, row := range m.Handle(control.Request{Version: control.Version, Action: "connections"}).Connections {
		if row.Kind == "connection" {
			d.sample(row.ID, "endpoint", row.Snapshot, seen)
		}
	}
	for key := range d.cursors {
		if !seen[key] {
			delete(d.cursors, key)
		}
	}
}

func observe(ctx context.Context, m *control.Manager, logger *log.Logger) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d := diagnosticLog{logger: logger, cursors: map[string]diagnosticCursor{}}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		d.poll(m)
		for {
			select {
			case <-ctx.Done():
				d.poll(m)
				return
			case <-ticker.C:
				d.poll(m)
			}
		}
	}()
	return func() { cancel(); <-done }
}
