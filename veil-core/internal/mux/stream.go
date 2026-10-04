package mux

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

type writeResult struct {
	n   int
	err error
}
type writeRequest struct {
	p        []byte
	off      int
	finished bool
	done     chan writeResult
}

func (r *writeRequest) finishLocked(err error) {
	if !r.finished {
		r.finished = true
		r.done <- writeResult{r.off, err}
	}
}

// Stream has independent FIN, reset, receive storage and send credit. The
// session reader never waits for an application's Read or target socket Write.
type Stream struct {
	refundAt                              int
	applicationDone, peerDone, pendingFIN bool
	completed                             chan struct{}
	s                                     *Session
	id                                    uint32
	metadata                              []byte
	writeMu                               sync.Mutex
	ready, closed, localFIN, remoteFIN    bool
	err                                   error
	opened, readable                      chan struct{}
	stopped                               <-chan struct{}
	ctx                                   context.Context
	cancel                                context.CancelCauseFunc
	sendCredit, recvCredit, refund        int
	queue                                 []*chunk
	head                                  int
	request                               *writeRequest
	writeRequest                          writeRequest
	controlDone                           chan error
	transferred                           uint64
	activity                              time.Time
}

func (s *Session) newStreamLocked(id uint32, metadata []byte) *Stream {
	parent := s.opts.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancelCause(parent)
	st := &Stream{s: s, id: id, metadata: append([]byte(nil), metadata...), stopped: ctx.Done(), ctx: ctx, cancel: cancel, readable: make(chan struct{}, 1), sendCredit: windowBlocks, recvCredit: windowBlocks, activity: time.Now()}
	if !s.opts.Server {
		st.opened = make(chan struct{})
		st.completed = make(chan struct{})
	}
	s.streams[id] = st
	st.refundAt = s.shape.credit.choose()
	s.idleDeadlineLocked()
	return st
}

func (st *Stream) Metadata() []byte         { return append([]byte(nil), st.metadata...) }
func (st *Stream) Context() context.Context { return st.ctx }
func (st *Stream) LastActivity() time.Time {
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	if len(st.s.streams) == 1 && st.s.headerActivity.After(st.activity) {
		return st.s.headerActivity
	}
	return st.activity
}
func (st *Stream) failure() error {
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	if st.err != nil {
		return st.err
	}
	return net.ErrClosed
}

func (st *Stream) stopLocked(err error) {
	if st.err == nil {
		st.err = err
		st.cancel(err)
	}
	if st.request != nil {
		st.request.finishLocked(err)
	}
}

// Respond is called only after the real destination dial has completed. A
// nonzero code rejects this stream while preserving unrelated streams.
func (st *Stream) Respond(code byte) error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	s := st.s
	s.mu.Lock()
	if st.err != nil {
		err := st.err
		s.mu.Unlock()
		return err
	}
	if !s.opts.Server || st.ready || st.closed {
		s.mu.Unlock()
		return ErrProtocol
	}
	if st.controlDone == nil {
		st.controlDone = make(chan error, 1)
	}
	done := st.controlDone
	t := opened
	var payload []byte
	if code != 0 {
		t = failed
		payload = []byte{code}
	} else {
		st.ready = true
	}
	err := s.controlLocked(t, st.id, payload, done)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case err = <-done:
		return err
	case <-st.stopped:
		return st.failure()
	case <-s.done:
		return s.Err()
	}
}

func (st *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s := st.s
	for {
		s.mu.Lock()
		total := 0
		var grantErr error
		// Drain only bytes already published by the decoder. This reduces local
		// writes without withholding a partial frame or waiting to fill p.
		for st.head < len(st.queue) && total < len(p) {
			c := st.queue[st.head]
			if c.start == c.end && !c.complete {
				break
			}
			n := copy(p[total:], c.b[c.start:c.end])
			total += n
			c.start += n
			if c.start != c.end || !c.complete {
				break
			}
			putChunk(c)
			st.queue[st.head] = nil
			st.head++
			st.refund++
			if st.head == len(st.queue) {
				st.queue = st.queue[:0]
				st.head = 0
			} else if st.head >= windowBlocks {
				n := copy(st.queue, st.queue[st.head:])
				clear(st.queue[n:])
				st.queue = st.queue[:n]
				st.head = 0
			}
			if st.refund >= st.refundAt && st.err == nil && !st.closed {
				var b [2]byte
				binary.BigEndian.PutUint16(b[:], uint16(st.refund))
				grantErr = s.controlLocked(credit, st.id, b[:], nil)
				if grantErr != nil {
					break
				}
				st.recvCredit += st.refund
				st.refund = 0
				st.refundAt = s.shape.credit.choose()
			}
		}
		var err error
		if grantErr != nil {
			err = grantErr
		} else if st.err != nil {
			err = st.err
		} else if st.closed {
			err = net.ErrClosed
		} else if st.remoteFIN {
			err = io.EOF
		}
		s.mu.Unlock()
		if grantErr != nil {
			s.fail(grantErr)
		}
		if total > 0 {
			return total, nil
		}
		if err != nil {
			return 0, err
		}
		select {
		case <-st.readable:
		case <-st.stopped:
			return 0, st.failure()
		}
	}
}

func (st *Stream) Write(p []byte) (int, error) {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	s := st.s
	s.mu.Lock()
	if st.err != nil {
		err := st.err
		s.mu.Unlock()
		return 0, err
	}
	if st.closed || st.localFIN {
		s.mu.Unlock()
		return 0, net.ErrClosed
	}
	if !st.ready {
		s.mu.Unlock()
		return 0, ErrProtocol
	}
	if len(p) == 0 {
		s.mu.Unlock()
		return 0, nil
	}
	r := &st.writeRequest
	if r.done == nil {
		r.done = make(chan writeResult, 1)
	}
	r.p, r.off, r.finished = p, 0, false
	st.request = r
	signal(s.wake)
	s.mu.Unlock()
	var result writeResult
	select {
	case result = <-r.done:
	case <-st.stopped:
		// Closing under the session lock removes this request from scheduling
		// before returning ownership of the caller's buffer. An in-flight
		// socket write uses the writer's own batch buffer.
		st.Close()
		result = <-r.done
	}
	s.mu.Lock()
	if st.request == r {
		st.request = nil
	}
	r.p = nil
	s.mu.Unlock()
	return result.n, result.err
}

func (st *Stream) CloseWrite() error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	s := st.s
	s.mu.Lock()
	if st.err != nil {
		err := st.err
		s.mu.Unlock()
		return err
	}
	if st.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	if st.localFIN {
		s.mu.Unlock()
		return nil
	}
	if !st.ready {
		s.mu.Unlock()
		return ErrProtocol
	}
	st.localFIN = true
	if s.opts.Server && st.remoteFIN {
		st.pendingFIN = true
		s.mu.Unlock()
		return nil
	}
	done := make(chan error, 1)
	err := s.controlLocked(fin, st.id, nil, done)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case err = <-done:
		return err
	case <-st.stopped:
		return st.failure()
	}
}

func (st *Stream) closeLocked(reason error, notify bool) error {
	s := st.s
	if st.closed {
		return nil
	}
	st.closed = true
	var err error
	if notify && !(st.applicationDone || st.peerDone) && st.err == nil {
		err = s.controlLocked(reset, st.id, nil, nil)
	}
	st.stopLocked(reason)
	for i := st.head; i < len(st.queue); i++ {
		c := st.queue[i]
		if c.complete {
			putChunk(c)
		} else {
			c.discard = true
		}
		st.queue[i] = nil
	}
	st.queue = nil
	st.head = 0
	delete(s.streams, st.id)
	s.idleDeadlineLocked()
	return err
}

func (st *Stream) Close() error {
	s := st.s
	s.mu.Lock()
	err := st.closeLocked(net.ErrClosed, true)
	s.mu.Unlock()
	if err != nil && err != net.ErrClosed {
		s.fail(err)
	}
	return nil
}

// Finish confirms the server's relay has completed both directions. FIN alone
// is insufficient: another pump can still be writing bytes to the target.
func (st *Stream) Finish() error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	s := st.s
	s.mu.Lock()
	if st.err != nil {
		e := st.err
		s.mu.Unlock()
		return e
	}
	if !s.opts.Server || !st.localFIN || !st.remoteFIN || st.applicationDone {
		s.mu.Unlock()
		return ErrProtocol
	}
	st.applicationDone = true
	var err error
	if st.pendingFIN {
		err = s.controlLocked(fin, st.id, nil, nil)
		st.pendingFIN = false
	}
	if st.controlDone == nil {
		st.controlDone = make(chan error, 1)
	}
	done := st.controlDone
	if err == nil {
		err = s.controlLocked(finished, st.id, nil, done)
	}
	s.mu.Unlock()
	if err != nil {
		s.fail(err)
		return err
	}
	select {
	case err = <-done:
		return err
	case <-st.stopped:
		return st.failure()
	}
}

func (st *Stream) WaitDone(ctx context.Context) error {
	select {
	case <-st.completed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-st.stopped:
		st.s.mu.Lock()
		done := st.peerDone
		st.s.mu.Unlock()
		if done {
			return nil
		}
		return st.failure()
	}
}

// Done allows the caller to avoid allocating a timeout after FIN/DONE were
// already decoded together. It does not retire a stream or bypass WaitDone.
func (st *Stream) Done() bool {
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	return st.peerDone
}
