package service

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"veil/core"
	"veil/inbound"
	"veil/internal/mux"
)

// ErrorEvent describes an operation, never application bytes or configuration.
// Consecutive identical failures share a bounded entry and retain their count.
type ErrorEvent struct {
	Sequence uint64    `json:"sequence"`
	FirstAt  time.Time `json:"first_at"`
	At       time.Time `json:"at"`
	Count    uint64    `json:"count"`
	Stage    string    `json:"stage"`
	Code     string    `json:"code"`
	Message  string    `json:"message"`
}

type Diagnostics struct {
	StartedAt       time.Time       `json:"started_at"`
	ConnectionLimit int             `json:"connection_limit"`
	Sequence        uint64          `json:"sequence"`
	Recent          []ErrorEvent    `json:"recent,omitempty"`
	Pool            *core.PoolStats `json:"pool,omitempty"`
}

type errorHistory struct {
	mu       sync.Mutex
	started  time.Time
	sequence uint64
	recent   []ErrorEvent
}

func errorCode(err error) string {
	var dns *net.DNSError
	var network net.Error
	var target core.TargetError
	switch {
	case errors.Is(err, inbound.ErrConnectionLimit):
		return "connection_limit"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, core.ErrIdleTimeout):
		return "idle_timeout"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.Is(err, mux.ErrReset):
		return "stream_reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	case errors.As(err, &target):
		switch target {
		case core.TargetDNS:
			return "dns"
		case core.TargetTimeout:
			return "timeout"
		case core.TargetRefused:
			return "refused"
		}
	}
	return "io"
}

func (h *errorHistory) record(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	stage := "inbound"
	for e := err; e != nil; e = errors.Unwrap(e) {
		if op, ok := e.(*core.OpError); ok {
			stage = op.Op
		}
	}
	message := strings.ToValidUTF8(err.Error(), "?")
	if r := []rune(message); len(r) > 256 {
		message = string(r[:256]) + "…"
	}
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sequence++
	event := ErrorEvent{Sequence: h.sequence, FirstAt: now, At: now, Count: 1, Stage: stage, Code: errorCode(err), Message: message}
	if n := len(h.recent); n > 0 {
		last := &h.recent[n-1]
		if last.Stage == stage && last.Message == message && last.Code == event.Code {
			last.Sequence, last.At = event.Sequence, now
			last.Count++
			return
		}
	}
	if len(h.recent) == 16 {
		copy(h.recent, h.recent[1:])
		h.recent = h.recent[:15]
	}
	h.recent = append(h.recent, event)
}

func (h *errorHistory) snapshot(limit int, client *core.Client) *Diagnostics {
	h.mu.Lock()
	d := &Diagnostics{StartedAt: h.started, ConnectionLimit: limit, Sequence: h.sequence, Recent: slices.Clone(h.recent)}
	h.mu.Unlock()
	if client != nil {
		p := client.PoolStats()
		d.Pool = &p
	}
	return d
}

func counters(c *core.Stats) Counters {
	return Counters{Accepted: c.Accepted.Load(), Rejected: c.Rejected.Load(), Completed: c.Completed.Load(), Failed: c.Failed.Load(), Authenticated: c.Authenticated.Load(), ActiveConnections: c.ActiveConnections.Load(), ActiveStreams: c.ActiveStreams.Load()}
}

func (s *Snapshot) withDiagnostics(h *errorHistory, limit int, client *core.Client) {
	s.Diagnostics = h.snapshot(limit, client)
	if recent := s.Diagnostics.Recent; len(recent) > 0 {
		s.LastConnectionError = recent[len(recent)-1].Message
	}
}
