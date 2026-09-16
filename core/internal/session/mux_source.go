package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"sync"
	"time"
	sm "veil.local/core/internal/streammux"
)

type muxSource struct {
	txHash, rxHash                        hash.Hash
	mu                                    sync.Mutex
	mux                                   *sm.Session
	pending, pendingEOF, ackedEOF, closed bool
	maximum                               int
}

func (s *muxSource) lease(n int) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.pending {
		return nil, false, errors.New("mux outer lease phase")
	}
	p, eof, e := s.mux.Lease(n)
	if e != nil {
		return nil, false, e
	}
	s.pending = true
	s.pendingEOF = eof
	if s.txHash == nil {
		s.txHash = sha256.New()
	}
	s.txHash.Write(p)
	return p, eof, nil
}
func (s *muxSource) commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.pending {
		return errors.New("mux outer receipt phase")
	}
	if e := s.mux.Commit(); e != nil {
		return e
	}
	s.ackedEOF = s.ackedEOF || s.pendingEOF
	s.pending = false
	s.pendingEOF = false
	return nil
}
func (s *muxSource) deliver(p []byte, eof bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.mux.Feed(p); e != nil {
		return e
	}
	if s.rxHash == nil {
		s.rxHash = sha256.New()
	}
	s.rxHash.Write(p)
	if eof {
		return s.mux.PeerDone()
	}
	return nil
}
func (s *muxSource) hashes() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, b := "", ""
	if s.txHash != nil {
		a = hex.EncodeToString(s.txHash.Sum(nil))
	}
	if s.rxHash != nil {
		b = hex.EncodeToString(s.rxHash.Sum(nil))
	}
	return a, b
}
func (s *muxSource) wait(ctx context.Context, d time.Duration) error {
	return s.mux.WaitCoalesced(ctx, d, time.Millisecond)
}
func (s *muxSource) status() streamQueueStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.mux.Output()
	used := v.AvailableBytes + v.PendingBytes
	if s.closed {
		used = 0
	}
	s.maximum = max(s.maximum, used)
	return streamQueueStatus{Used: used, Leased: v.PendingBytes, MaxUsed: s.maximum, Pending: s.pending, InputEOF: v.EOF, AckedEOF: s.ackedEOF, Closed: s.closed || v.Closed}
}
func (s *muxSource) close() { s.finish(nil) }
func (s *muxSource) finish(e error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.pending = false
	s.pendingEOF = false
	s.mux.Close(e)
}
func watchMuxIdle(ctx context.Context, m *sm.Session, limit time.Duration) func() {
	child, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(min(100*time.Millisecond, limit/2))
		defer ticker.Stop()
		last := time.Now()
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
				st := m.Status()
				if st.Closed || st.Draining {
					return
				}
				if !st.Ready || st.Active > 0 {
					last = time.Now()
					continue
				}
				if time.Since(last) >= limit {
					m.Drain()
					return
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
func watchMuxStreamIdle(stream *sm.Stream, limit time.Duration) func() {
	ctx, cancel := context.WithCancel(stream.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(min(time.Second, limit/2))
		defer ticker.Stop()
		last := time.Now()
		var total uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l := stream.Link()
				if l == nil {
					last = time.Now()
					continue
				}
				st := l.Status()
				n := st.Sent + st.Received
				if n != total {
					total = n
					last = time.Now()
				}
				if time.Since(last) >= limit {
					_ = stream.Reset(sm.EndBudget)
					return
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
