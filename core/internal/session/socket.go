package session

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
	sl "veil.local/core/internal/streamlink"
)

type linkSource struct {
	link    *sl.Link
	mu      sync.Mutex
	maximum int
}

func (s *linkSource) lease(n int) ([]byte, bool, error)               { return s.link.Lease(n) }
func (s *linkSource) commit() error                                   { return s.link.Commit() }
func (s *linkSource) wait(ctx context.Context, d time.Duration) error { return s.link.Wait(ctx, d) }
func (s *linkSource) close()                                          { s.link.Close(nil) }
func (s *linkSource) status() streamQueueStatus {
	st := s.link.Status()
	used := st.AvailableBytes + st.PendingBytes
	s.mu.Lock()
	s.maximum = max(s.maximum, used)
	maximum := s.maximum
	s.mu.Unlock()
	return streamQueueStatus{Used: used, Leased: st.PendingBytes, MaxUsed: maximum, Pending: st.Pending, InputEOF: st.SourceEOF, AckedEOF: st.OuterAckedEOF, Closed: st.Closed, BlockedNS: st.CreditWaitNS}
}
func (s *linkSource) deliver(p []byte, eof bool) error {
	if e := s.link.Feed(p); e != nil {
		return e
	}
	if eof {
		return s.link.PeerDone()
	}
	return nil
}

type socketPumpResult struct {
	ReadBytes             int
	ReadEOF               bool
	ReadError, WriteError string
	CloseWriteNS          int64
}
type streamSocket interface {
	net.Conn
	CloseWrite() error
	SetLinger(int) error
}
type socketLink struct {
	source *linkSource
	conn   streamSocket
	wg     sync.WaitGroup
	mu     sync.Mutex
	result socketPumpResult
	stop   func() bool
}

func newSocketLink(parent context.Context, conn streamSocket, window int, start <-chan struct{}) (*socketLink, error) {
	return newConfiguredSocketLink(parent, conn, sl.Config{Window: window, MaxBytes: 8 << 20}, start)
}
func newConfiguredSocketLink(parent context.Context, conn streamSocket, cfg sl.Config, start <-chan struct{}) (*socketLink, error) {
	l, e := sl.New(parent, cfg)
	if e != nil {
		return nil, e
	}
	return startSocketLink(l, conn, start), nil
}

// The mux owns the negotiated link; socket pumps retain their existing join and
// target Release semantics when attached to it.
func startSocketLink(l *sl.Link, conn streamSocket, start <-chan struct{}) *socketLink {
	p := &socketLink{source: &linkSource{link: l}, conn: conn}
	p.stop = context.AfterFunc(l.Context(), func() {
		if l.Status().Error != "" {
			conn.SetLinger(0)
		}
		conn.Close()
	})
	wait := func() error {
		select {
		case <-start:
			return nil
		case <-l.Context().Done():
			return l.Context().Err()
		}
	}
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		e := wait()
		nTotal := 0
		eof := false
		if e == nil {
			buf := make([]byte, sl.MaxData)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					written, writeErr := l.Write(l.Context(), buf[:n])
					nTotal += written
					if writeErr != nil {
						e = writeErr
						break
					}
				}
				if err != nil {
					if err == io.EOF {
						e = l.Finish()
						eof = e == nil
					} else {
						e = err
					}
					break
				}
			}
		}
		p.mu.Lock()
		p.result.ReadBytes = nTotal
		p.result.ReadEOF = eof
		if e != nil {
			p.result.ReadError = e.Error()
		}
		p.mu.Unlock()
		if e != nil {
			l.Close(e)
		}
	}()
	go func() {
		defer p.wg.Done()
		e := wait()
		if e == nil {
			e = l.Consume(l.Context(), conn, func() error {
				e := conn.CloseWrite()
				if e == nil {
					p.mu.Lock()
					p.result.CloseWriteNS = time.Now().UnixNano()
					p.mu.Unlock()
				}
				return e
			})
		}
		if e != nil {
			p.mu.Lock()
			p.result.WriteError = e.Error()
			p.mu.Unlock()
			l.Close(e)
		}
	}()
	return p
}
func (p *socketLink) close(cause error) socketPumpResult {
	p.source.link.Close(cause)
	p.stop()
	if cause != nil {
		p.conn.SetLinger(0)
	}
	p.conn.Close()
	p.wg.Wait()
	if held, ok := p.conn.(interface{ Release() }); ok {
		held.Release()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result
}
