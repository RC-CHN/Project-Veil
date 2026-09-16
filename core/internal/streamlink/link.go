// Package streamlink carries one ordered, independently half-closed byte stream
// over an ordered outer byte carrier. Outer receipt and consumer credit differ.
package streamlink

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	fw "veil.local/core/internal/flightwindow"
)

const MaxWindow = 1 << 20
const MaxData = 16 << 10
const Header = 16
const MaxLease = 256 << 10

var ErrLeaseBusy = fw.ErrFull

type linkLease struct {
	bytes       int
	eof, legacy bool
}

const (
	Data   byte = 1
	Credit byte = 2
	Fin    byte = 3
)

type ring struct {
	p       []byte
	head, n int
}

func (r *ring) put(p []byte) {
	at := (r.head + r.n) % len(r.p)
	first := copy(r.p[at:], p)
	copy(r.p, p[first:])
	r.n += len(p)
}
func (r *ring) peek(n int) []byte {
	p := make([]byte, n)
	first := copy(p, r.p[r.head:])
	copy(p[first:], r.p)
	return p
}
func (r *ring) drop(n int) {
	first := min(n, len(r.p)-r.head)
	clear(r.p[r.head : r.head+first])
	clear(r.p[:n-first])
	r.head = (r.head + n) % len(r.p)
	r.n -= n
}

type Config struct {
	Window   int
	MaxBytes uint64
}
type Status struct {
	CreditFlow                                                                                                  CreditFlowStatus
	PendingLeases                                                                                               int    `json:",omitempty"`
	LeaseIssued, LeaseCommitted                                                                                 uint64 `json:",omitempty"`
	Sent, Acked, Received, Consumed                                                                             uint64
	Queued, ReceiveQueued, PendingBytes, AvailableBytes, MaxSendOutstanding, MaxReceiveOutstanding              int
	WriteEOF, FinSent, FinAcked, RemoteFIN, RemoteDelivered, PeerEOF, SourceEOF, OuterAckedEOF, Pending, Closed bool
	CreditWaitNS, ConsumerWriteNS                                                                               int64
	Error                                                                                                       string `json:",omitempty"`
}
type Link struct {
	creditObservation                                                                 creditObservation
	consumerStarted                                                                   bool
	pullConsumer                                                                      bool
	mu, feedMu                                                                        sync.Mutex
	ctx                                                                               context.Context
	cancel                                                                            context.CancelFunc
	stop                                                                              func() bool
	cfg                                                                               Config
	tx, rx                                                                            ring
	sent, acked, received, consumed, creditQueued                                     uint64
	writeEOF, finSent, finAcked, remoteFIN, remoteDelivered, finCreditQueued, peerEOF bool
	pending, outerAckedEOF, closed                                                    bool
	leases                                                                            fw.Window[linkLease]
	pendingBytes, maxSend, maxReceive                                                 int
	changed                                                                           chan struct{}
	err                                                                               error
	creditWaitNS, consumerWriteNS                                                     int64
	parser                                                                            [Header + MaxData]byte
	parsed, need                                                                      int
}

func New(parent context.Context, cfg Config) (*Link, error) {
	return NewWithPending(parent, cfg, 1)
}

func NewWithPending(parent context.Context, cfg Config, maxPending int) (*Link, error) {
	leases, e := fw.New[linkLease](maxPending)
	if e != nil {
		return nil, e
	}
	if parent == nil || cfg.Window < 1 || cfg.Window > MaxWindow || cfg.MaxBytes < 1 || cfg.MaxBytes > 1<<40 {
		return nil, errors.New("link configuration bound")
	}
	if e := parent.Err(); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(parent)
	l := &Link{ctx: ctx, cancel: cancel, cfg: cfg, leases: leases, changed: make(chan struct{}), need: Header, tx: ring{p: make([]byte, cfg.Window)}, rx: ring{p: make([]byte, cfg.Window)}}
	l.mu.Lock()
	l.stop = context.AfterFunc(ctx, func() { l.Close(ctx.Err()) })
	l.mu.Unlock()
	return l, nil
}
func (l *Link) Context() context.Context { return l.ctx }
func (l *Link) signal()                  { close(l.changed); l.changed = make(chan struct{}) }
func (l *Link) failLocked(e error) error {
	if !l.closed {
		l.creditObservation.closed(time.Now())
		l.closed = true
		l.err = e
		l.tx.drop(l.tx.n)
		l.rx.drop(l.rx.n)
		l.pending = false
		l.pendingBytes = 0
		l.leases.Clear()
		if l.stop != nil {
			l.stop()
		}
		l.signal()
		l.cancel()
	}
	return e
}
func (l *Link) Close(e error) { l.mu.Lock(); defer l.mu.Unlock(); l.failLocked(e) }
func (l *Link) live() error {
	if l.closed {
		if l.err != nil {
			return l.err
		}
		return io.ErrClosedPipe
	}
	if e := l.ctx.Err(); e != nil {
		return l.failLocked(e)
	}
	return nil
}

// One application producer calls Write and then Finish. Receipts from the outer
// carrier do not release these bytes; only validated CREDIT does.
func (l *Link) Write(ctx context.Context, p []byte) (int, error) {
	if ctx == nil {
		return 0, errors.New("nil write context")
	}
	at := 0
	for at < len(p) {
		l.mu.Lock()
		if e := l.live(); e != nil {
			l.mu.Unlock()
			return at, e
		}
		if e := ctx.Err(); e != nil {
			l.mu.Unlock()
			return at, e
		}
		if l.writeEOF {
			l.mu.Unlock()
			return at, io.ErrClosedPipe
		}
		total := l.sent + uint64(l.tx.n)
		if total == l.cfg.MaxBytes {
			e := l.failLocked(errors.New("link send byte limit"))
			l.mu.Unlock()
			return at, e
		}
		space := l.cfg.Window - int(l.sent-l.acked) - l.tx.n
		if space > 0 {
			n := min(len(p)-at, space, int(l.cfg.MaxBytes-total))
			l.tx.put(p[at : at+n])
			at += n
			l.maxSend = max(l.maxSend, int(l.sent-l.acked)+l.tx.n)
			l.signal()
			l.mu.Unlock()
			continue
		}
		changed := l.changed
		queued := l.tx.n > 0
		l.mu.Unlock()
		start := time.Now()
		select {
		case <-changed:
		case <-ctx.Done():
		case <-l.ctx.Done():
		}
		l.mu.Lock()
		elapsed := time.Since(start).Nanoseconds()
		l.creditWaitNS += elapsed
		l.creditObservation.waited(elapsed, queued)
		l.mu.Unlock()
	}
	return at, nil
}
func (l *Link) Finish() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.live(); e != nil {
		return e
	}
	if l.writeEOF {
		return errors.New("duplicate local EOF")
	}
	l.writeEOF = true
	l.signal()
	return nil
}
func frame(kind, flags byte, offset uint64, p []byte) []byte {
	out := make([]byte, Header+len(p))
	out[0] = kind
	out[1] = flags
	out[2] = 1
	binary.BigEndian.PutUint32(out[4:8], uint32(len(p)))
	binary.BigEndian.PutUint64(out[8:], offset)
	copy(out[Header:], p)
	return out
}
func (l *Link) available() int {
	n := l.tx.n
	if n > 0 {
		n += ((n + MaxData - 1) / MaxData) * Header
	}
	if l.consumed > l.creditQueued || l.remoteDelivered && !l.finCreditQueued {
		n += Header
	}
	if l.writeEOF && !l.finSent {
		n += Header
	}
	return n
}
func (l *Link) sourceEOF() bool {
	return l.writeEOF && l.finSent && l.finAcked && l.remoteDelivered && l.finCreditQueued && l.creditQueued == l.consumed && l.tx.n == 0
}
func (l *Link) Lease(capacity int) ([]byte, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, p, eof, e := l.leaseLocked(capacity, true)
	return p, eof, e
}

// LeaseNext issues an ordered byte slice with a receipt token. Callers must
// deliver slices in issue order even when outer exchanges complete out of order.
func (l *Link) LeaseNext(capacity int) (uint64, []byte, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leaseLocked(capacity, false)
}

func (l *Link) leaseLocked(capacity int, legacy bool) (uint64, []byte, bool, error) {
	if e := l.live(); e != nil {
		return 0, nil, false, e
	}
	front, held := l.leases.Front()
	if legacy && l.pending || held && front.Value.legacy || capacity < Header+1 || capacity > MaxLease {
		return 0, nil, false, l.failLocked(errors.New("link lease bound or phase"))
	}
	if l.leases.Full() {
		return 0, nil, false, ErrLeaseBusy
	}
	out := make([]byte, 0, capacity)
	if l.consumed > l.creditQueued || l.remoteDelivered && !l.finCreditQueued {
		flags := byte(0)
		if l.remoteDelivered {
			flags = 1
		}
		out = append(out, frame(Credit, flags, l.consumed, nil)...)
		l.creditObservation.leased(time.Now())
		l.creditQueued = l.consumed
		l.finCreditQueued = l.remoteDelivered
	}
	for l.tx.n > 0 && capacity-len(out) > Header {
		n := min(MaxData, l.tx.n, capacity-len(out)-Header)
		p := l.tx.peek(n)
		out = append(out, frame(Data, 0, l.sent, p)...)
		l.tx.drop(n)
		l.sent += uint64(n)
	}
	if l.writeEOF && l.tx.n == 0 && !l.finSent && capacity-len(out) >= Header {
		out = append(out, frame(Fin, 0, l.sent, nil)...)
		l.finSent = true
	}
	eof := l.sourceEOF()
	id, e := l.leases.Push(linkLease{bytes: len(out), eof: eof, legacy: legacy})
	if e != nil {
		return 0, nil, false, l.failLocked(e)
	}
	l.pending = true
	l.pendingBytes += len(out)
	return id, out, eof, nil
}
func (l *Link) Commit() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.live(); e != nil {
		return e
	}
	front, held := l.leases.Front()
	if !held || l.leases.Count() != 1 || !front.Value.legacy {
		return l.failLocked(errors.New("outer receipt without lease"))
	}
	return l.ackLocked(front.ID)
}

func (l *Link) AckLease(id uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.live(); e != nil {
		return e
	}
	if front, held := l.leases.Front(); held && front.Value.legacy {
		return l.failLocked(errors.New("numbered receipt for legacy lease"))
	}
	return l.ackLocked(id)
}

func (l *Link) ackLocked(id uint64) error {
	ready, e := l.leases.Ack(id)
	if e != nil {
		return l.failLocked(e)
	}
	for _, entry := range ready {
		l.outerAckedEOF = l.outerAckedEOF || entry.Value.eof
		l.pendingBytes -= entry.Value.bytes
	}
	l.pending = l.leases.Count() > 0
	l.signal()
	return nil
}
func (l *Link) Wait(ctx context.Context, limit time.Duration) error {
	if ctx == nil || limit < 0 {
		return errors.New("invalid wait")
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		l.mu.Lock()
		if e := l.live(); e != nil {
			l.mu.Unlock()
			return e
		}
		ready := l.available() > 0 || l.sourceEOF()
		changed := l.changed
		l.mu.Unlock()
		if e := ctx.Err(); e != nil {
			return e
		}
		if ready {
			return nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-l.ctx.Done():
			return l.ctx.Err()
		}
	}
}

// Feed never waits on application I/O. A peer exceeding advertised credit fails.
func (l *Link) Feed(p []byte) error {
	l.feedMu.Lock()
	defer l.feedMu.Unlock()
	for len(p) > 0 {
		l.mu.Lock()
		e := l.live()
		peerEOF := l.peerEOF
		l.mu.Unlock()
		if e != nil {
			return e
		}
		if peerEOF {
			e = errors.New("wire bytes after peer EOF")
			l.Close(e)
			return e
		}
		n := copy(l.parser[l.parsed:l.need], p)
		l.parsed += n
		p = p[n:]
		if l.parsed < l.need {
			continue
		}
		if l.need == Header {
			kind, flags := l.parser[0], l.parser[1]
			size := binary.BigEndian.Uint32(l.parser[4:8])
			if l.parser[2] != 1 || l.parser[3] != 0 || kind < Data || kind > Fin || flags > 1 || kind != Credit && flags != 0 || size > MaxData || kind == Data && size == 0 || kind != Data && size != 0 {
				e = errors.New("inner frame header")
				l.Close(e)
				return e
			}
			l.need = Header + int(size)
			if l.parsed < l.need {
				continue
			}
		}
		if e = l.accept(l.parser[:l.need]); e != nil {
			return e
		}
		clear(l.parser[:l.need])
		l.parsed = 0
		l.need = Header
	}
	return nil
}
func (l *Link) accept(p []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.live(); e != nil {
		return e
	}
	offset := binary.BigEndian.Uint64(p[8:])
	data := p[Header:]
	switch p[0] {
	case Data:
		if l.remoteFIN || offset != l.received || len(data) > l.cfg.Window-l.rx.n || uint64(len(data)) > l.cfg.MaxBytes-l.received || l.received+uint64(len(data)) > l.creditQueued+uint64(l.cfg.Window) {
			return l.failLocked(errors.New("data order, credit or byte limit"))
		}
		l.rx.put(data)
		l.received += uint64(len(data))
		l.maxReceive = max(l.maxReceive, l.rx.n)
	case Credit:
		if offset < l.acked || offset > l.sent || p[1] == 1 && (!l.finSent || offset != l.sent) {
			return l.failLocked(errors.New("invalid consumer credit"))
		}
		l.creditObservation.status.ReceivedFrames++
		if offset > l.acked {
			l.creditObservation.status.AdvancingFrames++
		}
		l.acked = offset
		l.finAcked = l.finAcked || p[1] == 1
	case Fin:
		if l.remoteFIN || offset != l.received {
			return l.failLocked(errors.New("invalid FIN boundary"))
		}
		l.remoteFIN = true
	}
	l.signal()
	return nil
}
func (l *Link) PeerDone() error {
	l.feedMu.Lock()
	defer l.feedMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.live(); e != nil {
		return e
	}
	if l.parsed != 0 || !l.sourceEOF() {
		return l.failLocked(errors.New("outer EOF before inner completion"))
	}
	l.peerEOF = true
	return nil
}

// Consume runs separately from Feed. Caller must arrange for context cancellation
// to close a blocking writer (e.g. the owned TCP connection).
func (l *Link) Consume(ctx context.Context, w io.Writer, closeWrite func() error) error {
	if ctx == nil || w == nil || closeWrite == nil {
		return errors.New("invalid consumer")
	}
	l.mu.Lock()
	if l.consumerStarted {
		l.mu.Unlock()
		return errors.New("duplicate consumer")
	}
	l.consumerStarted = true
	l.mu.Unlock()
	for {
		l.mu.Lock()
		if e := l.live(); e != nil {
			l.mu.Unlock()
			return e
		}
		if e := ctx.Err(); e != nil {
			l.mu.Unlock()
			return e
		}
		if l.rx.n > 0 {
			p := l.rx.peek(min(MaxData, l.rx.n))
			l.mu.Unlock()
			start := time.Now()
			n, e := w.Write(p)
			l.mu.Lock()
			l.consumerWriteNS += time.Since(start).Nanoseconds()
			if n < 0 || n > len(p) {
				e = errors.New("invalid sink write count")
				n = 0
			}
			if !l.closed && n > 0 {
				l.creditObservation.consumed(time.Now())
				l.rx.drop(n)
				l.consumed += uint64(n)
				l.signal()
			}
			if e == nil && n == 0 {
				e = io.ErrNoProgress
			}
			if e != nil {
				l.failLocked(e)
			}
			l.mu.Unlock()
			if e != nil {
				return e
			}
			continue
		}
		if l.remoteFIN {
			l.mu.Unlock()
			e := closeWrite()
			l.mu.Lock()
			if e == nil && !l.closed {
				l.remoteDelivered = true
				l.signal()
			}
			if e != nil {
				l.failLocked(e)
			}
			l.mu.Unlock()
			return e
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-l.ctx.Done():
			return l.ctx.Err()
		}
	}
}
func (l *Link) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.ctx.Err(); e != nil && !l.closed {
		l.failLocked(e)
	}
	s := Status{Sent: l.sent, Acked: l.acked, Received: l.received, Consumed: l.consumed, Queued: l.tx.n, ReceiveQueued: l.rx.n, PendingBytes: l.pendingBytes, AvailableBytes: l.available(), MaxSendOutstanding: l.maxSend, MaxReceiveOutstanding: l.maxReceive, WriteEOF: l.writeEOF, FinSent: l.finSent, FinAcked: l.finAcked, RemoteFIN: l.remoteFIN, RemoteDelivered: l.remoteDelivered, PeerEOF: l.peerEOF, SourceEOF: l.sourceEOF(), OuterAckedEOF: l.outerAckedEOF, Pending: l.pending, Closed: l.closed, CreditWaitNS: l.creditWaitNS, ConsumerWriteNS: l.consumerWriteNS}
	s.PendingLeases, s.LeaseIssued, s.LeaseCommitted = l.leases.Count(), l.leases.Issued(), l.leases.Committed()
	s.CreditFlow = l.creditObservation.status
	if l.err != nil {
		s.Error = l.err.Error()
	}
	if l.closed {
		s.AvailableBytes = 0
		s.SourceEOF = false
	}
	return s
}
