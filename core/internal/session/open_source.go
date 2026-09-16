package session

import (
	"context"
	"errors"
	"sync"
	"time"
	sl "veil.local/core/internal/streamlink"
	so "veil.local/core/internal/streamopen"
)

// One outer lease covers a prefix and, after successful negotiation, an inner
// lease. Outer receipts never imply destination connection success.
type openSource struct {
	mu                                                  sync.Mutex
	ctx                                                 context.Context
	handshake                                           *so.Handshake
	socket                                              *socketLink
	makeClient                                          func(so.Limits) (*socketLink, error)
	changed                                             chan struct{}
	pending, pendingInner, pendingEOF, ackedEOF, closed bool
	pendingBytes, maximum                               int
	resultNS                                            int64
}

func newOpenSource(ctx context.Context, h *so.Handshake) *openSource {
	return &openSource{ctx: ctx, handshake: h, changed: make(chan struct{})}
}
func (s *openSource) signal() { close(s.changed); s.changed = make(chan struct{}) }
func (s *openSource) attach(p *socketLink, r so.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("OPEN source closed")
	}
	if e := s.handshake.Respond(r); e != nil {
		return e
	}
	s.socket = p
	s.resultNS = time.Now().UnixNano()
	s.signal()
	return nil
}
func (s *openSource) lease(n int) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.pending {
		return nil, false, errors.New("OPEN source lease phase")
	}
	prefix, e := s.handshake.TakePrefix(n)
	if e != nil {
		return nil, false, e
	}
	st := s.handshake.Status()
	eof := st.Rejected && st.PrefixSent
	var body []byte
	if st.Established {
		if s.socket == nil {
			return nil, false, errors.New("successful OPEN without socket")
		}
		body, eof, e = s.socket.source.link.Lease(n - len(prefix))
		if e != nil {
			return nil, false, e
		}
		s.pendingInner = true
	}
	s.pending = true
	s.pendingEOF = eof
	s.pendingBytes = len(prefix) + len(body)
	return append(prefix, body...), eof, nil
}
func (s *openSource) commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.pending {
		return errors.New("OPEN receipt phase")
	}
	if s.pendingInner {
		if e := s.socket.source.link.Commit(); e != nil {
			return e
		}
	}
	s.ackedEOF = s.ackedEOF || s.pendingEOF
	s.pending = false
	s.pendingInner = false
	s.pendingBytes = 0
	s.signal()
	return nil
}
func (s *openSource) deliver(p []byte, eof bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("OPEN delivery closed")
	}
	rest, e := s.handshake.Feed(p)
	if e != nil {
		return e
	}
	st := s.handshake.Status()
	if st.Client && st.ResultKnown && s.resultNS == 0 {
		s.resultNS = time.Now().UnixNano()
	}
	if st.Established && s.socket == nil {
		if !st.Client || s.makeClient == nil {
			return errors.New("missing successful OPEN consumer")
		}
		s.socket, e = s.makeClient(st.Result.Limits)
		if e != nil {
			return e
		}
	}
	if len(rest) > 0 {
		if s.socket == nil {
			return errors.New("data without open socket")
		}
		if e = s.socket.source.link.Feed(rest); e != nil {
			return e
		}
	}
	if eof {
		if e = s.handshake.PeerDone(); e != nil {
			return e
		}
		if !st.Rejected {
			if s.socket == nil {
				return errors.New("EOF without socket")
			}
			if e = s.socket.source.link.PeerDone(); e != nil {
				return e
			}
		}
	}
	s.signal()
	return nil
}
func (s *openSource) status() streamQueueStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.handshake.Status()
	used := h.QueuedBytes + s.pendingBytes
	blocked := int64(0)
	eof := h.Rejected && h.PrefixSent
	if s.socket != nil {
		st := s.socket.source.link.Status()
		used += st.AvailableBytes
		blocked = st.CreditWaitNS
		eof = st.SourceEOF
	}
	if s.closed {
		used = 0
	}
	s.maximum = max(s.maximum, used)
	return streamQueueStatus{Used: used, Leased: s.pendingBytes, MaxUsed: s.maximum, Pending: s.pending, InputEOF: eof, AckedEOF: s.ackedEOF, Closed: s.closed, BlockedNS: blocked}
}
func (s *openSource) wait(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errors.New("OPEN source closed")
		}
		h := s.handshake.Status()
		p := s.socket
		changed := s.changed
		s.mu.Unlock()
		if h.QueuedBytes > 0 || h.Rejected {
			return nil
		}
		if p != nil {
			return p.source.link.Wait(ctx, max(time.Duration(0), time.Until(deadline)))
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (s *openSource) close() { s.finish(nil) }
func (s *openSource) finish(cause error) socketPumpResult {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.pending = false
		s.pendingInner = false
		s.pendingBytes = 0
		s.handshake.Close(cause)
		s.signal()
	}
	p := s.socket
	s.mu.Unlock()
	if p != nil {
		return p.close(cause)
	}
	return socketPumpResult{}
}
func (s *openSource) linkStatus() sl.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.socket != nil {
		return s.socket.source.link.Status()
	}
	return sl.Status{}
}

func (s *openSource) resultTime() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.resultNS }

func (s *openSource) snapshot() (so.Status, sl.Status, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.handshake.Status()
	l := sl.Status{}
	if s.socket != nil {
		l = s.socket.source.link.Status()
	}
	return h, l, s.resultNS
}

func (s *openSource) udpStatus() *UDPStatus {
	s.mu.Lock()
	p := s.socket
	s.mu.Unlock()
	if p != nil {
		if u, ok := p.conn.(*datagramSocket); ok {
			v := u.status()
			return &v
		}
	}
	return nil
}
