package core

import (
	"sync"
	"sync/atomic"
	"time"
	"veil.local/core/internal/session"
	"veil.local/core/telemetry"
)

type observer struct {
	sink     telemetry.EventSink
	mu       sync.Mutex
	sequence uint64
	dropped  atomic.Int64
}

func newObserver(s telemetry.EventSink) *observer { return &observer{sink: s} }
func (o *observer) emit(e session.SessionEvent) {
	if o.sink == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sequence++
	if !o.sink.TryEmit(telemetry.Event{Sequence: o.sequence, At: time.Now().UTC(), Role: e.Role, Kind: e.EventType, Outcome: e.Outcome, ModelID: e.ModelID}) {
		o.dropped.Add(1)
	}
}
func snapshot(s session.Snapshot, state string, o *observer) telemetry.Snapshot {
	return telemetry.Snapshot{Role: s.Role, State: state, ModelID: s.ModelID, InnerProtocol: s.InnerProtocol, Listen: s.Listen, LocalReady: s.Ready && state == "ready", EndToEnd: "not_checked", ActiveStreams: s.ActiveStreams, ActiveCarriers: s.ActiveSessions, StartedStreams: s.StartedStreams, CompletedStreams: s.CompletedStreams, FailedStreams: s.FailedStreams, CleanupFailures: s.CleanupFailures, EventsDropped: s.EventsDropped + o.dropped.Load(), Updated: time.Now().UTC()}
}
