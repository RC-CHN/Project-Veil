// Package telemetry contains bounded public runtime observations, without
// protocol-private types or credential material.
package telemetry

import (
	"context"
	"sync/atomic"
	"time"
)

type Event struct {
	Sequence                     uint64
	At                           time.Time
	Role, Kind, Outcome, ModelID string
}
type EventSink interface{ TryEmit(Event) bool }
type Snapshot struct {
	Role, State, ModelID, InnerProtocol, Listen                                                                    string
	LocalReady                                                                                                     bool
	EndToEnd                                                                                                       string
	ActiveStreams, ActiveCarriers, StartedStreams, CompletedStreams, FailedStreams, CleanupFailures, EventsDropped int64
	RejectedStreams, RejectedConnections, FailedCarriers, AuthDenied                                               int64
	Updated                                                                                                        time.Time
}

// Buffer is bounded and non-blocking for producers. It never closes its channel;
// consumer lifetime is controlled by context so producers cannot panic on close.
type Buffer struct {
	events  chan Event
	dropped atomic.Uint64
}

func NewBuffer(capacity int) *Buffer {
	if capacity < 1 || capacity > 65536 {
		panic("event buffer capacity")
	}
	return &Buffer{events: make(chan Event, capacity)}
}
func (b *Buffer) TryEmit(e Event) bool {
	select {
	case b.events <- e:
		return true
	default:
		b.dropped.Add(1)
		return false
	}
}
func (b *Buffer) Next(ctx context.Context) (Event, error) {
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case e := <-b.events:
		return e, nil
	}
}
func (b *Buffer) Dropped() uint64 { return b.dropped.Load() }
