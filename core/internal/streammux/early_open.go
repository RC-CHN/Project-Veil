package streammux

import (
	"errors"

	so "veil.local/core/internal/streamopen"
)

// OpenFirst queues exactly one first request before the initial SETTINGS lease.
// It allocates no link or application data window, and never implies a RESULT.
// Callers must bind Config.EarlyOpen to an authenticated protocol version before
// enabling this extension on either side. Legacy runtime configurations do not.
func (m *Session) OpenFirst(r so.Request) (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.live(); e != nil {
		return nil, e
	}
	if !m.earlyOpen || !m.client || m.settingsSent || m.settingsReceived || m.nextID != 0 || m.earlyOpenQueued {
		return nil, errors.New("mux early OPEN phase or capability")
	}
	if m.draining {
		return nil, ErrDraining
	}
	if _, e := so.EncodeRequest(r); e != nil {
		return nil, e
	}
	r.Limits.Window = min(r.Limits.Window, m.local.Window)
	r.Limits.MaxBytes = min(r.Limits.MaxBytes, m.local.StreamBytes)
	m.nextID, m.remoteUsed = 1, 1
	m.earlyOpenQueued = true
	if m.local.Opened == 1 {
		m.draining = true
	}
	return m.newStream(1, r), nil
}

// pendingEarlyOpen is called with m.mu held. An unsent cancelled first request
// never goes on the wire and its ID is never reused.
func (m *Session) pendingEarlyOpen() *Stream {
	if !m.earlyOpenQueued || m.earlyOpenSent {
		return nil
	}
	s := m.streams[1]
	if s == nil || s.openSent || s.endWanted {
		return nil
	}
	return s
}

// awaitEarlyResult suppresses a SETTINGS-only wakeup while the first target is
// being checked. Wait's existing overall timer still expires; no new timeout is
// introduced and Lease may always emit SETTINGS when that budget is exhausted.
func (m *Session) awaitEarlyResult() bool {
	if m.client || !m.earlyOpenReceived || m.settingsSent {
		return false
	}
	s := m.streams[1]
	return s != nil && !s.resultKnown && !s.endWanted
}

func (m *Session) receiveEarlyOpen(f Frame) error {
	if !m.earlyOpen || m.client || !m.settingsReceived || m.earlyOpenReceived || m.lastPeerID != 0 || f.ID != 1 || m.peerDrain || len(m.order) != 0 {
		return errors.New("mux early OPEN direction, phase or capability")
	}
	r, e := so.DecodeRequest(f.Body)
	if e != nil {
		return e
	}
	// The advertised first request is a ceiling. Peer SETTINGS have already
	// been intersected with local limits; no link exists until bounded RESULT.
	r.Limits.Window = min(r.Limits.Window, m.limits.Window)
	r.Limits.MaxBytes = min(r.Limits.MaxBytes, m.limits.StreamBytes)
	m.lastPeerID, m.earlyOpenReceived = 1, true
	s := m.newStream(1, r)
	s.openSent = true
	select {
	case m.accept <- s:
		return nil
	default:
		return errors.New("mux early OPEN accept queue bound")
	}
}
