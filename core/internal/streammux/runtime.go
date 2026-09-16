package streammux

import (
	"context"
	"errors"
	so "veil.local/core/internal/streamopen"
)

type OutputStatus struct {
	AvailableBytes, PendingBytes int
	EOF, Closed                  bool
}

// Output includes terminal/control records generated lazily by Lease. It never
// calls application code or waits on application I/O.
func (m *Session) Output() OutputStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := OutputStatus{PendingBytes: m.pendingBytes, EOF: m.eof(), Closed: m.closed}
	if m.closed {
		return v
	}
	if !m.settingsSent {
		v.AvailableBytes += Header + SettingsSize
	}
	if !m.ready() {
		if s := m.pendingEarlyOpen(); s != nil {
			p, _ := so.EncodeRequest(s.request)
			v.AvailableBytes += Header + len(p)
		}
		return v
	}
	if !m.client && m.retired > m.grantSent {
		v.AvailableBytes += Header + 4
	}
	for _, s := range m.order {
		if s.endSent {
			continue
		}
		if m.client && !s.openSent {
			if !s.endWanted {
				p, _ := so.EncodeRequest(s.request)
				v.AvailableBytes += Header + len(p)
			}
			continue
		}
		if !m.client && s.resultKnown && !s.resultSent && !s.endWanted {
			v.AvailableBytes += Header + so.Header + 16
		}
		if s.endWanted || s.resultKnown && s.result.Code != so.OK {
			v.AvailableBytes += Header + 1
			continue
		}
		if s.link != nil {
			st := s.link.Status()
			if st.SourceEOF || st.Closed {
				v.AvailableBytes += Header + 1
			} else if st.AvailableBytes > 0 {
				v.AvailableBytes += st.AvailableBytes + Header*((st.AvailableBytes+Quantum-1)/Quantum)
			}
		}
	}
	if m.draining && !m.drainSent {
		v.AvailableBytes += Header
	}
	return v
}
func (m *Session) WaitReady(ctx context.Context) (Settings, error) {
	if ctx == nil {
		return Settings{}, errors.New("nil settings wait context")
	}
	for {
		m.mu.Lock()
		e := m.live()
		ready, limits, ch := m.ready(), m.limits, m.changed
		m.mu.Unlock()
		if e != nil {
			return Settings{}, e
		}
		if ready {
			return limits, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return Settings{}, ctx.Err()
		case <-m.ctx.Done():
			return Settings{}, m.ctx.Err()
		}
	}
}
