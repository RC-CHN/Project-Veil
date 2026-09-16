package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"sync"

	fw "veil.local/core/internal/flightwindow"
	sm "veil.local/core/internal/streammux"
)

// flightMuxSource owns numbered leases from a fresh Mux. The future HTTP
// coordinator must validate connection/model/receipt fields before calling it.
// It is intentionally separate from the legacy adaptiveSource contract.
type flightMuxSource struct {
	mu                    sync.Mutex
	mux                   *sm.Session
	receiver              *fw.Receiver
	txHash, rxHash        hash.Hash
	firstTxEOF, delivered uint64
	rxEOF, closed         bool
	err                   error
	stop                  func() bool
	changed               chan struct{}
}

type flightSourceStatus struct {
	Mux                   sm.Status
	Receive               fw.ReceiveStatus
	Delivered, FirstTxEOF uint64
	AckedEOF, PeerEOF     bool
	Closed                bool
	Error                 string
}

func newFlightMuxSource(ctx context.Context, cfg sm.Config) (*flightMuxSource, error) {
	if cfg.MaxPending == 0 {
		cfg.MaxPending = 1
	}
	r, e := fw.NewReceiver(cfg.MaxPending)
	if e != nil {
		return nil, e
	}
	m, e := sm.New(ctx, cfg)
	if e != nil {
		return nil, e
	}
	s := &flightMuxSource{mux: m, receiver: r, txHash: sha256.New(), rxHash: sha256.New(), changed: make(chan struct{})}
	s.mu.Lock()
	s.stop = context.AfterFunc(ctx, func() { s.finish(ctx.Err()) })
	s.mu.Unlock()
	return s, nil
}

func (s *flightMuxSource) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *flightMuxSource) failLocked(e error) error {
	if !s.closed {
		s.closed, s.err = true, e
		if s.stop != nil {
			s.stop()
		}
		s.receiver.Close()
		s.mux.Close(e)
		s.signalLocked()
		if e == nil {
			if st := s.mux.Status(); st.Error != "" {
				s.err = errors.New(st.Error)
			}
		}
	}
	if s.err != nil {
		return s.err
	}
	return errors.New("numbered mux source closed")
}

func (s *flightMuxSource) liveLocked() error {
	if s.closed {
		return s.failLocked(s.err)
	}
	if st := s.mux.Status(); st.Closed {
		return s.failLocked(errors.New("numbered mux closed: " + st.Error))
	}
	return nil
}

func (s *flightMuxSource) lease(capacity int) (fw.Frame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaseLocked(capacity)
}

func (s *flightMuxSource) leaseLocked(capacity int) (fw.Frame, error) {
	if e := s.liveLocked(); e != nil {
		return fw.Frame{}, e
	}
	id, body, eof, e := s.mux.LeaseNext(capacity)
	if errors.Is(e, sm.ErrLeaseBusy) {
		return fw.Frame{}, e
	}
	if e != nil {
		return fw.Frame{}, s.failLocked(e)
	}
	if eof && s.firstTxEOF == 0 {
		s.firstTxEOF = id
	}
	s.txHash.Write(body)
	s.signalLocked()
	return fw.Frame{ID: id, Body: body, EOF: eof}, nil
}

// nextFlight reserves at most one global EOF. Empty controls have no lease ID
// and remain available while receipts hold the data window full.
func (s *flightMuxSource) nextFlight(capacity int) (fw.Frame, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.liveLocked(); e != nil {
		return fw.Frame{}, false, e
	}
	if s.firstTxEOF != 0 {
		return fw.Frame{}, true, nil
	}
	out := s.mux.Output()
	if out.AvailableBytes == 0 && !out.EOF {
		return fw.Frame{}, false, nil
	}
	frame, e := s.leaseLocked(capacity)
	if errors.Is(e, sm.ErrLeaseBusy) {
		return fw.Frame{}, false, nil
	}
	return frame, frame.EOF, e
}

func (s *flightMuxSource) ack(id uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.liveLocked(); e != nil {
		return e
	}
	if e := s.mux.AckLease(id); e != nil {
		return s.failLocked(e)
	}
	s.signalLocked()
	return nil
}

// receive returns the successful delivery prefix, not the received-frame count.
// An out-of-order frame may be accepted without advancing that prefix at all.
func (s *flightMuxSource) receive(frame fw.Frame) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.liveLocked(); e != nil {
		return 0, e
	}
	if e := s.receiver.Put(frame.ID, frame.Body, frame.EOF); e != nil {
		return 0, s.failLocked(e)
	}
	for {
		next, ok := s.receiver.Pop()
		if !ok {
			break
		}
		if e := s.mux.Feed(next.Body); e != nil {
			return 0, s.failLocked(e)
		}
		if next.EOF {
			if e := s.mux.PeerDone(); e != nil {
				return 0, s.failLocked(e)
			}
		}
		s.rxHash.Write(next.Body)
		s.rxEOF = s.rxEOF || next.EOF
		s.delivered = next.ID
	}
	s.signalLocked()
	return s.delivered, nil
}

func (s *flightMuxSource) closingReadyLocked() bool {
	m := s.mux.Status()
	return !s.closed && !m.Closed && s.rxEOF && s.receiver.Status().Pending == 0 && s.firstTxEOF != 0 && m.LeaseCommitted >= s.firstTxEOF && !m.Pending && m.SourceEOF && m.PeerEOF
}

func (s *flightMuxSource) waitFlightState(ctx context.Context, id uint64, closing bool) error {
	for {
		s.mu.Lock()
		if e := s.liveLocked(); e != nil {
			s.mu.Unlock()
			return e
		}
		ready := s.delivered >= id
		if closing {
			ready = s.closingReadyLocked()
		}
		changed := s.changed
		s.mu.Unlock()
		if ready {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (s *flightMuxSource) status() flightSourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Observe inner cancellation here as well; the parent callback and this
	// observation both converge on the same idempotent cleanup path.
	if !s.closed && s.mux.Status().Closed {
		s.liveLocked()
	}
	m := s.mux.Status()
	st := flightSourceStatus{Mux: m, Receive: s.receiver.Status(), Delivered: s.delivered, FirstTxEOF: s.firstTxEOF, AckedEOF: s.firstTxEOF != 0 && m.LeaseCommitted >= s.firstTxEOF, PeerEOF: s.rxEOF, Closed: s.closed}
	if s.err != nil {
		st.Error = s.err.Error()
	}
	return st
}

func (s *flightMuxSource) hashes() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return hex.EncodeToString(s.txHash.Sum(nil)), hex.EncodeToString(s.rxHash.Sum(nil))
}

func (s *flightMuxSource) finish(e error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	m := s.mux.Status()
	if e == nil && (s.receiver.Status().Pending != 0 || !s.rxEOF || s.firstTxEOF == 0 || m.LeaseCommitted < s.firstTxEOF || m.Pending || !m.SourceEOF || !m.PeerEOF || m.Closed) {
		e = errors.New("numbered mux close before delivered and acknowledged EOF")
	}
	s.failLocked(e)
}
