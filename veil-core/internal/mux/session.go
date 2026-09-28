package mux

import (
	"context"
	"encoding/binary"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

type Options struct {
	Server                    bool
	Profile                   Profile
	IdleTimeout, WriteTimeout time.Duration
	// Prefix is the caller's authentication frame, sent with the first OPEN.
	Prefix []byte
	// Padding is called only by the writer, with a bounded local send budget.
	Padding func(limit, records, bytes int) error
}

type control struct {
	frame
	done chan error
}

type Session struct {
	mu                        sync.Mutex
	conn                      net.Conn
	opts                      Options
	streams                   map[uint32]*Stream
	lastID, lastSent          uint32
	controls                  []*control
	err                       error
	wake                      chan struct{}
	done, readDone, writeDone chan struct{}
	accepted                  chan *Stream
	shape                     shape
	prefix                    []byte
	headerActivity            time.Time
}

func New(conn net.Conn, opts Options) (*Session, error) {
	if err := opts.Profile.Validate(); err != nil {
		return nil, err
	}
	s := &Session{conn: conn, opts: opts, streams: make(map[uint32]*Stream), wake: make(chan struct{}, 1), done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}), accepted: make(chan *Stream, MaxStreams), shape: newShape(opts.Profile), prefix: append([]byte(nil), opts.Prefix...)}
	s.opts.Prefix = nil
	go s.readLoop()
	go s.writeLoop()
	return s, nil
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Session) fail(err error) {
	if err == nil {
		err = net.ErrClosed
	}
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return
	}
	s.err = err
	close(s.done)
	for _, st := range s.streams {
		if st.peerDone {
			continue
		}
		e := err
		if e == io.EOF {
			e = io.ErrUnexpectedEOF
		}
		st.stopLocked(e)
	}
	for _, c := range s.controls {
		if c.done != nil {
			c.done <- err
		}
	}
	s.controls = nil
	s.mu.Unlock()
	s.conn.Close()
}

func (s *Session) Close() error { s.fail(net.ErrClosed); return nil }
func (s *Session) Wait()        { <-s.readDone; <-s.writeDone }
func (s *Session) Err() error   { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

// Snapshot reports active streams and whether any carries sustained traffic.
func (s *Session) Snapshot() (active int, bulk, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.streams {
		if st.transferred >= 256<<10 {
			bulk = true
		}
	}
	return len(s.streams), bulk, s.err != nil
}

func (s *Session) idleDeadlineLocked() {
	var deadline time.Time
	if len(s.streams) == 0 && s.opts.IdleTimeout > 0 {
		deadline = time.Now().Add(s.opts.IdleTimeout)
	}
	s.conn.SetReadDeadline(deadline)
}

func (s *Session) controlLocked(t byte, id uint32, p []byte, done chan error) error {
	if s.err != nil {
		return s.err
	}
	if t == credit {
		for _, c := range s.controls {
			if c.typ == credit && c.id == id {
				n := int(binary.BigEndian.Uint16(c.payload)) + int(binary.BigEndian.Uint16(p))
				if n > windowBlocks {
					return ErrProtocol
				}
				binary.BigEndian.PutUint16(c.payload, uint16(n))
				return nil
			}
		}
	}
	if len(s.controls) >= maxControls {
		return ErrFull
	}
	s.controls = append(s.controls, &control{frame: frame{t, id, append([]byte(nil), p...)}, done: done})
	signal(s.wake)
	return nil
}

func (s *Session) Open(ctx context.Context, metadata []byte) (*Stream, error) {
	if len(metadata) == 0 || len(metadata) > 512 || s.opts.Server {
		return nil, ErrProtocol
	}
	s.mu.Lock()
	if s.err != nil {
		e := s.err
		s.mu.Unlock()
		return nil, e
	}
	if len(s.streams) >= MaxStreams {
		s.mu.Unlock()
		return nil, ErrFull
	}
	if s.lastID >= ^uint32(0)-2 {
		s.mu.Unlock()
		return nil, ErrFull
	}
	id := s.lastID + 2
	if s.lastID == 0 {
		id = 1
	}
	s.lastID = id
	st := s.newStreamLocked(id, metadata)
	err := s.controlLocked(open, id, metadata, nil)
	s.mu.Unlock()
	if err != nil {
		st.Close()
		return nil, err
	}
	select {
	case <-ctx.Done():
		st.Close()
		return nil, ctx.Err()
	case <-st.stopped:
		st.Close()
		return nil, st.failure()
	case <-st.opened:
		s.mu.Lock()
		err = st.err
		s.mu.Unlock()
		if err != nil {
			st.Close()
			return nil, err
		}
		return st, nil
	}
}

func (s *Session) Accept(ctx context.Context) (*Stream, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.done:
			return nil, s.Err()
		case st := <-s.accepted:
			s.mu.Lock()
			alive := st.err == nil && !st.closed
			s.mu.Unlock()
			if alive {
				return st, nil
			}
		}
	}
}

func (s *Session) readPayload(st *Stream, p []byte) error {
	for len(p) > 0 {
		n, err := s.conn.Read(p)
		if n > 0 {
			p = p[n:]
			if st != nil {
				s.mu.Lock()
				st.activity = time.Now()
				s.mu.Unlock()
			}
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// Publish authenticated plaintext as it arrives, without waiting for a whole
// DATA frame. A cancelled stream leaves ownership of an unfinished block with
// this reader until the rest of the frame has been consumed.
func (s *Session) receive(st *Stream, id uint32, n int) error {
	c := blocks.Get().(*chunk)
	c.start, c.end, c.complete, c.discard = 0, 0, false, false
	s.mu.Lock()
	if st == nil || s.streams[id] != st || st.closed {
		c.discard = true
	} else {
		st.queue = append(st.queue, c)
	}
	s.mu.Unlock()
	filled := 0
	for filled < n {
		got, err := s.conn.Read(c.b[filled:n])
		filled += got
		if got == 0 && err == nil {
			err = io.ErrNoProgress
		}
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		s.mu.Lock()
		c.end = filled
		c.complete = filled == n || err != nil
		if !c.discard {
			st.activity = time.Now()
			st.transferred += uint64(got)
			signal(st.readable)
		}
		discard, complete := c.discard, c.complete
		s.mu.Unlock()
		if complete && discard {
			blocks.Put(c)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type headerReader struct{ s *Session }

func (r headerReader) Read(p []byte) (int, error) {
	n, err := r.s.conn.Read(p)
	if n > 0 {
		r.s.mu.Lock()
		r.s.headerActivity = time.Now()
		r.s.mu.Unlock()
	}
	return n, err
}

func (s *Session) readLoop() {
	defer close(s.readDone)
	var h [headerSize]byte
	for {
		s.mu.Lock()
		s.idleDeadlineLocked()
		s.mu.Unlock()
		t, id, n, err := readHeader(headerReader{s}, &h)
		if err != nil {
			s.fail(err)
			return
		}
		s.mu.Lock()
		st := s.streams[id]
		if st != nil && st.err != nil {
			st = nil
		}
		valid := true
		if t == open {
			valid = s.opts.Server && id > s.lastID
		} else {
			valid = id <= s.lastID
		}
		if t == data && st != nil {
			valid = valid && st.ready && !st.remoteFIN && st.recvCredit > 0
			if valid {
				st.recvCredit--
			}
		}
		s.mu.Unlock()
		if !valid {
			s.fail(ErrProtocol)
			return
		}
		if t == data {
			if err = s.receive(st, id, n); err != nil {
				s.fail(err)
				return
			}
			continue
		}
		p := make([]byte, n)
		if err = s.readPayload(st, p); err != nil {
			s.fail(err)
			return
		}
		s.mu.Lock()
		if t == open {
			s.lastID = id
			// A completed relay may still be waking from its final write. Its
			// identifier cannot be reused, and it no longer occupies an active slot.
			for oldID, old := range s.streams {
				if old.applicationDone {
					delete(s.streams, oldID)
				}
			}
			if len(s.streams) >= MaxStreams || len(s.accepted) == cap(s.accepted) {
				err = s.controlLocked(failed, id, []byte{1}, nil)
			} else {
				st = s.newStreamLocked(id, p)
				s.accepted <- st
			}
		} else if st != nil && s.streams[id] == st && !st.closed {
			switch t {
			case opened:
				if s.opts.Server || st.ready {
					err = ErrProtocol
				} else {
					st.ready = true
					close(st.opened)
				}
			case failed:
				if s.opts.Server || st.ready {
					err = ErrProtocol
				} else {
					st.closeLocked(OpenError(p[0]), false)
				}
			case fin:
				if !st.ready || st.remoteFIN {
					err = ErrProtocol
				} else {
					st.remoteFIN = true
					signal(st.readable)
				}
			case reset:
				st.closeLocked(ErrReset, false)
			case finished:
				if s.opts.Server || st.peerDone || !st.localFIN || !st.remoteFIN {
					err = ErrProtocol
				} else {
					st.peerDone = true
					close(st.completed)
				}
			case credit:
				amount := int(binary.BigEndian.Uint16(p))
				if amount == 0 || amount > windowBlocks-st.sendCredit {
					err = ErrProtocol
				} else {
					st.sendCredit += amount
					signal(s.wake)
				}
			}
		}
		s.mu.Unlock()
		if err != nil {
			s.fail(err)
			return
		}
	}
}

func (s *Session) writeLoop() {
	defer close(s.writeDone)
	buf := make([]byte, 0, batchSize)
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if s.err != nil {
				s.mu.Unlock()
				return
			}
			var readyStorage [MaxStreams]*Stream
			ready := readyStorage[:0]
			for _, st := range s.streams {
				if st.request != nil && !st.request.finished && st.request.off < len(st.request.p) && st.sendCredit > 0 && st.err == nil {
					ready = append(ready, st)
				}
			}
			if len(ready) == 0 && len(s.controls) == 0 {
				s.mu.Unlock()
				break
			}
			start := len(s.controls) > 0 && (s.controls[0].typ == open || s.controls[0].typ == opened)
			pad := start && len(s.streams) == 1
			var padLimit, padRecords, padBudget int
			if pad {
				padLimit, padRecords, padBudget = s.shape.limit.choose(), s.shape.writes.choose(), s.shape.budget.choose()
				s.shape.remaining = padRecords
			}
			if len(s.streams) > 1 {
				s.shape.remaining = 0
			}
			limit := batchSize
			if s.shape.remaining > 0 {
				limit = s.shape.startup.choose()
				s.shape.remaining--
			}
			buf = append(buf[:0], s.prefix...)
			s.prefix = nil
			var controlStorage [maxControls]*control
			sentControls := controlStorage[:0]
			for len(s.controls) > 0 {
				c := s.controls[0]
				if len(buf) > 0 && len(buf)+headerSize+len(c.payload) > limit && len(sentControls) > 0 {
					break
				}
				buf = appendFrame(buf, c.typ, c.id, c.payload)
				sentControls = append(sentControls, c)
				s.controls[0] = nil
				s.controls = s.controls[1:]
				if len(buf) >= limit {
					break
				}
			}
			var completedStorage [MaxStreams]*writeRequest
			completed := completedStorage[:0]
			if len(ready) > 1 {
				for i := 1; i < len(ready); i++ {
					for j := i; j > 0 && ready[j].id < ready[j-1].id; j-- {
						ready[j], ready[j-1] = ready[j-1], ready[j]
					}
				}
				start := 0
				if s.lastSent == 0 {
					start = rand.IntN(len(ready))
				} else {
					for i, st := range ready {
						if st.id > s.lastSent {
							start = i
							break
						}
					}
				}
				var rotated [MaxStreams]*Stream
				for i := range ready {
					rotated[i] = ready[(start+i)%len(ready)]
				}
				copy(ready, rotated[:len(ready)])
			}
			now := time.Now()
			for len(buf)+headerSize < limit && len(ready) > 0 {
				progress := false
				for _, st := range ready {
					r := st.request
					if r == nil || r.finished || r.off == len(r.p) || st.sendCredit == 0 {
						continue
					}
					share := blockSize
					if len(ready) > 1 {
						share = s.shape.share()
					}
					n := min(share, len(r.p)-r.off, limit-len(buf)-headerSize)
					if n <= 0 {
						break
					}
					buf = appendFrame(buf, data, st.id, r.p[r.off:r.off+n])
					r.off += n
					s.lastSent = st.id
					st.sendCredit--
					st.transferred += uint64(n)
					st.activity = now
					progress = true
					if r.off == len(r.p) {
						completed = append(completed, r)
					}
				}
				if !progress {
					break
				}
			}
			more := len(ready) > 0 || len(s.controls) > 0
			s.mu.Unlock()
			var err error
			if s.opts.Padding != nil {
				if pad {
					err = s.opts.Padding(padLimit, padRecords, padBudget)
				} else if len(ready) == 0 && len(sentControls) > 0 {
					// A tiny grant must not create a fixed-length, fixed-period
					// reverse-direction packet for every bulk transfer window.
					cap := s.shape.control.choose()
					err = s.opts.Padding(cap, 1, cap)
				} else if start || len(ready) > 1 {
					err = s.opts.Padding(0, 0, 0)
				}
			}
			if err == nil {
				if s.opts.WriteTimeout > 0 {
					s.conn.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout))
				}
				var n int
				n, err = s.conn.Write(buf)
				if err == nil && n != len(buf) {
					err = io.ErrShortWrite
				}
			}
			s.mu.Lock()
			for _, r := range completed {
				r.finishLocked(err)
			}
			for _, c := range sentControls {
				if c.done != nil {
					c.done <- err
				}
			}
			s.mu.Unlock()
			if err != nil {
				s.fail(err)
				return
			}
			if !more {
				break
			}
		}
	}
}
