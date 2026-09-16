package streammux

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"sync"
	"time"

	fw "veil.local/core/internal/flightwindow"
	sl "veil.local/core/internal/streamlink"
	so "veil.local/core/internal/streamopen"
)

var (
	ErrNotReady  = errors.New("mux settings not exchanged")
	ErrBusy      = errors.New("mux stream work limit")
	ErrDraining  = errors.New("mux draining")
	ErrLeaseBusy = fw.ErrFull
)

const MinLease = Header + so.Header + so.MaxBody

type Config struct {
	EarlyOpen  bool
	MaxPending int
	Client     bool
	Limits     Settings
}
type Status struct {
	EarlyOpenEnabled, EarlyOpenSent, EarlyOpenReceived                         bool   `json:",omitempty"`
	PendingLeases                                                              int    `json:",omitempty"`
	LeaseIssued, LeaseCommitted                                                uint64 `json:",omitempty"`
	Client, Ready, Draining, PeerDraining, Pending, SourceEOF, PeerEOF, Closed bool
	Active, MaximumActive, ReservedWindow                                      int
	Opened, OpenSent, Retired, RemoteGranted                                   uint32
	SentBytes, ReceivedBytes                                                   uint64
	Limits                                                                     Settings
	Error                                                                      string
}
type StreamStatus struct {
	ID                                                                        uint32
	Request                                                                   so.Request
	Result                                                                    so.Result
	ResultKnown, OpenSent, ResultSent, EndSent, EndAcked, PeerEnded, Released bool
	EndCode, PeerEndCode                                                      byte
	Link                                                                      sl.Status
	Error                                                                     string
}
type Stream struct {
	accepted                                                      bool
	m                                                             *Session
	id                                                            uint32
	ctx                                                           context.Context
	cancel                                                        context.CancelFunc
	request                                                       so.Request
	result                                                        so.Result
	resultKnown, openSent, resultSent                             bool
	endWanted, endSent, endAcked, peerEnded, released, doneClosed bool
	endCode, peerEndCode                                          byte
	link                                                          *sl.Link
	err                                                           error
	done                                                          chan struct{}
}
type linkReceipt struct {
	stream *Stream
	id     uint64
}
type pendingLease struct {
	settings, drain, legacy bool
	bytes                   int
	ends                    []*Stream
	links                   []linkReceipt
}
type Session struct {
	earlyOpen, earlyOpenQueued, earlyOpenSent, earlyOpenReceived                        bool
	pendingBytes                                                                        int
	mu, feedMu                                                                          sync.Mutex
	ctx                                                                                 context.Context
	cancel                                                                              context.CancelFunc
	stop                                                                                func() bool
	client                                                                              bool
	local, limits                                                                       Settings
	settingsSent, settingsReceived, draining, drainSent, drainAcked, peerDrain, peerEOF bool
	closed, pending                                                                     bool
	err                                                                                 error
	changed                                                                             chan struct{}
	decoder                                                                             Decoder
	streams                                                                             map[uint32]*Stream
	order                                                                               []*Stream
	accept                                                                              chan *Stream
	nextID, lastPeerID, lastServed, openSent, retired, grantSent, remoteGranted         uint32
	remoteUsed, maximum                                                                 int
	sent, received                                                                      uint64
	leases                                                                              fw.Window[pendingLease]
}

func New(parent context.Context, cfg Config) (*Session, error) {
	if cfg.MaxPending == 0 {
		cfg.MaxPending = 1
	}
	leases, e := fw.New[pendingLease](cfg.MaxPending)
	if e != nil {
		return nil, e
	}
	if parent == nil || !cfg.Limits.Valid() {
		return nil, errors.New("mux configuration")
	}
	if e := parent.Err(); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(parent)
	m := &Session{ctx: ctx, cancel: cancel, leases: leases, earlyOpen: cfg.EarlyOpen, client: cfg.Client, local: cfg.Limits, limits: cfg.Limits, changed: make(chan struct{}), streams: make(map[uint32]*Stream), accept: make(chan *Stream, cfg.Limits.Streams)}
	m.mu.Lock()
	m.stop = context.AfterFunc(ctx, func() { m.Close(ctx.Err()) })
	m.mu.Unlock()
	return m, nil
}
func (m *Session) signal() { close(m.changed); m.changed = make(chan struct{}) }
func (m *Session) live() error {
	if m.closed {
		if m.err != nil {
			return m.err
		}
		return io.ErrClosedPipe
	}
	if e := m.ctx.Err(); e != nil {
		return e
	}
	return nil
}
func (m *Session) ready() bool { return m.settingsSent && m.settingsReceived }
func (m *Session) fail(e error) error {
	if !m.closed {
		m.closed = true
		m.err = e
		m.pending = false
		m.pendingBytes = 0
		m.leases.Clear()
		if m.stop != nil {
			m.stop()
		}
		m.cancel()
		for _, s := range m.order {
			m.abort(s, EndInternal, e)
			m.markDone(s)
			if !m.client && !s.accepted {
				s.released = true
			}
		}
		for _, s := range append([]*Stream(nil), m.order...) {
			m.retire(s)
		}
		m.signal()
	}
	return e
}
func (m *Session) Close(e error) {
	m.feedMu.Lock()
	defer m.feedMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if e == nil && !(m.eof() && m.peerEOF && !m.pending) {
		e = io.ErrClosedPipe
	}
	m.fail(e)
	m.decoder.Close()
}
func (m *Session) newStream(id uint32, r so.Request) *Stream {
	ctx, cancel := context.WithCancel(m.ctx)
	s := &Stream{m: m, id: id, ctx: ctx, cancel: cancel, request: r, done: make(chan struct{})}
	m.streams[id] = s
	m.order = append(m.order, s)
	m.maximum = max(m.maximum, len(m.order))
	m.signal()
	return s
}
func (m *Session) Open(r so.Request) (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.live(); e != nil {
		return nil, e
	}
	if !m.client {
		return nil, errors.New("server initiated stream")
	}
	if !m.ready() {
		return nil, ErrNotReady
	}
	if m.draining || m.nextID >= m.limits.Opened {
		return nil, ErrDraining
	}
	if len(m.order) >= m.limits.Streams || m.remoteUsed >= m.limits.Streams {
		return nil, ErrBusy
	}
	if _, e := so.EncodeRequest(r); e != nil {
		return nil, e
	}
	r.Limits.Window = min(r.Limits.Window, m.limits.Window)
	r.Limits.MaxBytes = min(r.Limits.MaxBytes, m.limits.StreamBytes)
	m.nextID++
	m.remoteUsed++
	if m.nextID == m.limits.Opened {
		m.draining = true
	}
	return m.newStream(m.nextID, r), nil
}
func (m *Session) Accept(ctx context.Context) (*Stream, error) {
	if ctx == nil {
		return nil, errors.New("nil accept context")
	}
	if m.client {
		return nil, errors.New("client accept")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	select {
	case s := <-m.accept:
		m.mu.Lock()
		defer m.mu.Unlock()
		if e := m.live(); e != nil {
			m.fail(e)
			return nil, e
		}
		// The caller now owns this entry's cancellation and cleanup, even if
		// its Accept context is cancelled immediately after this handoff.
		s.accepted = true
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, m.ctx.Err()
	}
}
func (m *Session) Drain()                  { m.mu.Lock(); defer m.mu.Unlock(); m.draining = true; m.signal() }
func (s *Stream) ID() uint32               { return s.id }
func (s *Stream) Context() context.Context { return s.ctx }
func (s *Stream) Request() so.Request      { return s.request }
func (s *Stream) Done() <-chan struct{}    { return s.done }
func (s *Stream) Respond(r so.Result) error {
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.live(); e != nil {
		return e
	}
	if m.client || s.resultKnown || s.endWanted || m.streams[s.id] != s {
		return errors.New("mux RESULT phase")
	}
	if _, e := so.EncodeResult(r); e != nil {
		return e
	}
	if r.Code == so.OK {
		if r.Limits.Window > s.request.Limits.Window || r.Limits.Window > m.limits.Window || r.Limits.MaxBytes > s.request.Limits.MaxBytes || r.Limits.MaxBytes > m.limits.StreamBytes {
			return errors.New("mux RESULT expands limits")
		}
		l, e := sl.NewWithPending(s.ctx, sl.Config{Window: r.Limits.Window, MaxBytes: r.Limits.MaxBytes}, m.leases.Limit())
		if e != nil {
			return e
		}
		s.link = l
	}
	s.result = r
	s.resultKnown = true
	m.signal()
	return nil
}
func (s *Stream) WaitResult(ctx context.Context) (so.Result, *sl.Link, error) {
	if ctx == nil {
		return so.Result{}, nil, errors.New("nil result context")
	}
	m := s.m
	for {
		m.mu.Lock()
		r, l, known, e, ch := s.result, s.link, s.resultKnown, s.err, m.changed
		m.mu.Unlock()
		if e != nil {
			return r, nil, e
		}
		if known {
			if r.Code != so.OK {
				return r, nil, so.Rejection{Code: r.Code}
			}
			return r, l, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return r, nil, ctx.Err()
		case <-s.ctx.Done():
			return r, nil, s.ctx.Err()
		}
	}
}
func (s *Stream) Link() *sl.Link { s.m.mu.Lock(); defer s.m.mu.Unlock(); return s.link }
func (m *Session) abort(s *Stream, code byte, e error) {
	if s.err == nil {
		s.err = e
	}
	// Preserve the first abnormal terminal even before it is leased. Cleanup
	// cancellation must not relabel an idle/byte-budget reset or a peer reset.
	if !s.endSent && (!s.endWanted || s.endCode == EndNormal) {
		s.endWanted = true
		s.endCode = code
	}
	s.cancel()
	if s.link != nil {
		s.link.Close(e)
	}
	m.signal()
}
func (s *Stream) Reset(code byte) error {
	if code < EndCancelled || code > EndInternal {
		return errors.New("mux reset code")
	}
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams[s.id] != s {
		return io.ErrClosedPipe
	}
	// An emitted normal END already certifies complete bidirectional inner I/O.
	// A later local cancellation cannot rewrite that wire terminal or invalidate
	// the still-in-flight peer END while application cleanup retains the slot.
	if s.endSent && s.endCode == EndNormal {
		return nil
	}
	m.abort(s, code, errors.New("inner stream reset"))
	if m.client && !s.openSent {
		m.markDone(s)
	}
	m.retire(s)
	return nil
}

// Release is called only after the application worker, socket pumps and target
// cleanup have joined. Calling it early aborts the stream rather than reusing it.
func (s *Stream) Release() {
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.released {
		return
	}
	s.released = true
	if !s.doneClosed && !s.endWanted {
		m.abort(s, EndCancelled, errors.New("stream released before terminal"))
	}
	if m.client && !s.openSent {
		m.markDone(s)
	}
	m.retire(s)
	m.signal()
}
func (m *Session) markDone(s *Stream) {
	if !s.doneClosed {
		s.doneClosed = true
		close(s.done)
	}
}
func (m *Session) retire(s *Stream) {
	terminal := m.closed || m.client && !s.openSent && s.endWanted || s.endAcked && s.peerEnded
	if terminal {
		m.markDone(s)
	}
	if !terminal || !s.released || m.streams[s.id] != s {
		return
	}
	s.cancel()
	if s.link != nil {
		s.link.Close(s.err)
	}
	delete(m.streams, s.id)
	for i, p := range m.order {
		if p == s {
			copy(m.order[i:], m.order[i+1:])
			m.order[len(m.order)-1] = nil
			m.order = m.order[:len(m.order)-1]
			break
		}
	}
	if m.client {
		if !s.openSent {
			m.remoteUsed--
		}
	} else {
		m.retired++
	}
	m.signal()
}
func (s *Stream) Status() StreamStatus {
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	v := StreamStatus{ID: s.id, Request: s.request, Result: s.result, ResultKnown: s.resultKnown, OpenSent: s.openSent, ResultSent: s.resultSent, EndSent: s.endSent, EndAcked: s.endAcked, PeerEnded: s.peerEnded, Released: s.released, EndCode: s.endCode, PeerEndCode: s.peerEndCode}
	if s.link != nil {
		v.Link = s.link.Status()
	}
	if s.err != nil {
		v.Error = s.err.Error()
	}
	return v
}
func (m *Session) receive(f Frame) error {
	if f.Kind == SettingsKind {
		if m.settingsReceived {
			return errors.New("duplicate mux settings")
		}
		p, e := DecodeSettings(f.Body)
		if e != nil {
			return e
		}
		m.limits, e = m.local.Intersect(p)
		if e != nil {
			return e
		}
		m.settingsReceived = true
		if m.client && m.nextID >= m.limits.Opened {
			m.draining = true
		}
		m.signal()
		return nil
	}
	if f.Kind == EarlyOpenKind {
		return m.receiveEarlyOpen(f)
	}
	if !m.ready() {
		return errors.New("mux frame before settings exchange")
	}
	switch f.Kind {
	case DrainKind:
		if m.peerDrain {
			return errors.New("duplicate mux drain")
		}
		m.peerDrain = true
		m.draining = true
		m.signal()
		return nil
	case GrantKind:
		if !m.client {
			return errors.New("client sent mux grant")
		}
		n := binary.BigEndian.Uint32(f.Body)
		if n <= m.remoteGranted || n > m.openSent || int(n-m.remoteGranted) > m.remoteUsed {
			return errors.New("mux grant beyond emitted opens")
		}
		m.remoteUsed -= int(n - m.remoteGranted)
		m.remoteGranted = n
		m.signal()
		return nil
	case OpenKind:
		if m.client || m.peerDrain || f.ID <= m.lastPeerID || f.ID > m.limits.Opened || len(m.order) >= m.limits.Streams {
			return errors.New("mux OPEN direction, ID or admission")
		}
		r, e := so.DecodeRequest(f.Body)
		if e != nil {
			return e
		}
		if r.Limits.Window > m.limits.Window || r.Limits.MaxBytes > m.limits.StreamBytes {
			return errors.New("mux OPEN expands settings")
		}
		m.lastPeerID = f.ID
		s := m.newStream(f.ID, r)
		s.openSent = true
		select {
		case m.accept <- s:
		default:
			return errors.New("mux accept queue bound")
		}
		return nil
	}
	s := m.streams[f.ID]
	if s == nil || !s.openSent || s.peerEnded {
		return errors.New("mux unknown or terminal stream")
	}
	switch f.Kind {
	case ResultKind:
		if !m.client || s.resultKnown {
			return errors.New("mux RESULT direction or phase")
		}
		r, e := so.DecodeResult(f.Body)
		if e != nil {
			return e
		}
		if r.Code == so.OK && (r.Limits.Window > s.request.Limits.Window || r.Limits.MaxBytes > s.request.Limits.MaxBytes || r.Limits.Window > m.limits.Window || r.Limits.MaxBytes > m.limits.StreamBytes) {
			return errors.New("mux RESULT expands OPEN")
		}
		s.result = r
		s.resultKnown = true
		if r.Code == so.OK && !s.endWanted {
			l, e := sl.NewWithPending(s.ctx, sl.Config{Window: r.Limits.Window, MaxBytes: r.Limits.MaxBytes}, m.leases.Limit())
			if e != nil {
				return e
			}
			s.link = l
		}
	case DataKind:
		if s.endWanted && s.endCode != EndNormal {
			return nil
		}
		if !s.resultKnown || s.result.Code != so.OK || !m.client && !s.resultSent || s.link == nil {
			return errors.New("mux DATA before successful RESULT")
		}
		if e := s.link.Feed(f.Body); e != nil {
			m.abort(s, EndInternal, e)
		}
	case EndKind:
		code := f.Body[0]
		if code == EndNormal && !(s.endWanted && s.endCode != EndNormal) {
			if !s.resultKnown || !m.client && !s.resultSent {
				return errors.New("mux normal END before RESULT")
			}
			if s.result.Code == so.OK {
				if s.link == nil {
					return errors.New("mux END without link")
				}
				if e := s.link.PeerDone(); e != nil {
					return e
				}
			}
		} else if code != EndNormal {
			m.abort(s, code, errors.New("peer reset inner stream"))
		}
		s.peerEnded = true
		s.peerEndCode = code
		m.retire(s)
	default:
		return errors.New("mux stream frame kind")
	}
	m.signal()
	return nil
}
func (m *Session) Feed(p []byte) error {
	m.feedMu.Lock()
	defer m.feedMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.live(); e != nil {
		return e
	}
	if m.peerEOF {
		if len(p) == 0 {
			return nil
		}
		return m.fail(errors.New("mux bytes after outer EOF"))
	}
	if uint64(len(p)) > m.limits.ConnectionBytes-m.received {
		return m.fail(errors.New("mux receive byte budget"))
	}
	m.received += uint64(len(p))
	if e := m.decoder.Feed(p, m.receive); e != nil {
		return m.fail(e)
	}
	if m.received > m.limits.ConnectionBytes {
		return m.fail(errors.New("mux negotiated receive byte budget"))
	}
	return nil
}
func (m *Session) eof() bool {
	return m.ready() && m.drainAcked && m.peerDrain && len(m.order) == 0 && (m.client || m.grantSent == m.retired)
}
func (m *Session) PeerDone() error {
	m.feedMu.Lock()
	defer m.feedMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.live(); e != nil {
		return e
	}
	if m.peerEOF {
		return nil
	}
	if !m.peerDrain {
		return m.fail(errors.New("mux outer EOF before peer drain"))
	}
	// Peer EOF ends its byte direction. Local application cleanup can still be
	// pending and must retain admission until Release and the local END receipt.
	for _, s := range m.order {
		if !s.peerEnded && !(m.client && !s.openSent && s.endWanted) {
			return m.fail(errors.New("mux outer EOF before stream END"))
		}
	}
	if e := m.decoder.Finish(); e != nil {
		return m.fail(e)
	}
	m.peerEOF = true
	m.signal()
	return nil
}
func (m *Session) Lease(capacity int) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, p, eof, e := m.leaseLocked(capacity, true)
	return p, eof, e
}

// LeaseNext preserves byte issue order while allowing bounded overlapping outer
// transactions. Their receiver must restore this order before calling Feed.
func (m *Session) LeaseNext(capacity int) (uint64, []byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leaseLocked(capacity, false)
}
func (m *Session) leaseLocked(capacity int, legacy bool) (uint64, []byte, bool, error) {
	if e := m.live(); e != nil {
		return 0, nil, false, e
	}
	front, held := m.leases.Front()
	if legacy && m.pending || held && front.Value.legacy || capacity < MinLease || capacity > sl.MaxLease {
		return 0, nil, false, m.fail(errors.New("mux lease bound or phase"))
	}
	if m.leases.Full() {
		return 0, nil, false, ErrLeaseBusy
	}
	out := make([]byte, 0, capacity)
	lease := pendingLease{}
	var emitErr error
	emit := func(f Frame) bool {
		if Header+len(f.Body) > capacity-len(out) {
			return false
		}
		p, e := Encode(f)
		if e != nil {
			emitErr = e
			return false
		}
		out = append(out, p...)
		return true
	}
	if !m.settingsSent {
		p, _ := EncodeSettings(m.local)
		emit(Frame{Kind: SettingsKind, Body: p})
		m.settingsSent = true
		lease.settings = true
	}
	// A single provisional OPEN may follow the first SETTINGS without waiting
	// for peer SETTINGS. All application DATA still requires successful RESULT.
	if s := m.pendingEarlyOpen(); s != nil {
		p, _ := so.EncodeRequest(s.request)
		if emit(Frame{Kind: EarlyOpenKind, ID: s.id, Body: p}) {
			s.openSent = true
			m.openSent++
			m.earlyOpenSent = true
		}
	}
	if m.ready() {
		if !m.client && m.retired > m.grantSent {
			p := make([]byte, 4)
			binary.BigEndian.PutUint32(p, m.retired)
			if emit(Frame{Kind: GrantKind, Body: p}) {
				m.grantSent = m.retired
			}
		}
		// OPENs are emitted in increasing ID order before data scheduling.
		for _, s := range m.order {
			if s.endWanted && m.client && !s.openSent {
				continue
			}
			if m.client && !s.openSent {
				p, _ := so.EncodeRequest(s.request)
				if !emit(Frame{Kind: OpenKind, ID: s.id, Body: p}) {
					break
				}
				s.openSent = true
				m.openSent++
			}
			if !m.client && s.resultKnown && !s.resultSent && !s.endWanted {
				p, _ := so.EncodeResult(s.result)
				if emit(Frame{Kind: ResultKind, ID: s.id, Body: p}) {
					s.resultSent = true
				}
			}
		}
		readyCount := 0
		for _, s := range m.order {
			if s.endWanted || s.endSent {
				continue
			}
			if s.link != nil {
				st := s.link.Status()
				if st.Closed {
					m.abort(s, EndInternal, errors.New("inner link closed"))
				} else if st.SourceEOF {
					s.endWanted = true
					s.endCode = EndNormal
				} else if st.AvailableBytes > 0 {
					readyCount++
				}
			}
			if s.resultKnown && s.result.Code != so.OK && (m.client || s.resultSent) {
				s.endWanted = true
				s.endCode = EndNormal
			}
		}
		for _, s := range m.order {
			if s.endWanted && !s.endSent && s.openSent {
				if emit(Frame{Kind: EndKind, ID: s.id, Body: []byte{s.endCode}}) {
					s.endSent = true
					lease.ends = append(lease.ends, s)
				}
			}
		}
		start := 0
		for i, s := range m.order {
			if s.id > m.lastServed {
				start = i
				break
			}
		}
		for i := 0; i < len(m.order); i++ {
			s := m.order[(start+i)%len(m.order)]
			if s.link == nil || s.endWanted || s.endSent || !s.resultKnown || !m.client && !s.resultSent {
				continue
			}
			st := s.link.Status()
			if st.AvailableBytes == 0 {
				continue
			}
			left := capacity - len(out)
			budget := min(Quantum, left-Header)
			if readyCount == 1 {
				budget = left - ((left+Quantum-1)/Quantum)*Header
			}
			if budget < sl.Header+1 {
				continue
			}
			linkID, p, _, e := s.link.LeaseNext(min(budget, sl.MaxLease))
			if errors.Is(e, sl.ErrLeaseBusy) {
				continue
			}
			if e != nil {
				m.abort(s, EndInternal, e)
				continue
			}
			if len(p) == 0 {
				if e = s.link.AckLease(linkID); e != nil {
					m.abort(s, EndInternal, e)
				}
				continue
			}
			for len(p) > 0 {
				n := min(len(p), Quantum)
				if !emit(Frame{Kind: DataKind, ID: s.id, Body: p[:n]}) {
					return 0, nil, false, m.fail(errors.New("mux scheduler capacity"))
				}
				p = p[n:]
			}
			lease.links = append(lease.links, linkReceipt{stream: s, id: linkID})
			m.lastServed = s.id
		}
		if m.draining && !m.drainSent {
			unsent := false
			if m.client {
				for _, s := range m.order {
					unsent = unsent || !s.openSent && !s.endWanted
				}
			}
			if !unsent && emit(Frame{Kind: DrainKind}) {
				m.drainSent = true
				lease.drain = true
			}
		}
	}
	if emitErr != nil {
		return 0, nil, false, m.fail(emitErr)
	}
	if uint64(len(out)) > m.limits.ConnectionBytes-m.sent {
		return 0, nil, false, m.fail(errors.New("mux send byte budget"))
	}
	m.sent += uint64(len(out))
	lease.bytes, lease.legacy = len(out), legacy
	id, e := m.leases.Push(lease)
	if e != nil {
		return 0, nil, false, m.fail(e)
	}
	m.pending = true
	m.pendingBytes += len(out)
	m.signal()
	return id, out, m.eof(), nil
}
func (m *Session) Commit() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.live(); e != nil {
		return e
	}
	front, held := m.leases.Front()
	if !held || m.leases.Count() != 1 || !front.Value.legacy {
		return m.fail(errors.New("mux receipt without legacy lease"))
	}
	return m.ackLocked(front.ID)
}
func (m *Session) AckLease(id uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.live(); e != nil {
		return e
	}
	if front, held := m.leases.Front(); held && front.Value.legacy {
		return m.fail(errors.New("numbered receipt for legacy mux lease"))
	}
	return m.ackLocked(id)
}
func (m *Session) ackLocked(id uint64) error {
	ready, e := m.leases.Ack(id)
	if e != nil {
		return m.fail(e)
	}
	for _, entry := range ready {
		lease := entry.Value
		for _, receipt := range lease.links {
			s := receipt.stream
			if s.link != nil && !s.link.Status().Closed {
				if e := s.link.AckLease(receipt.id); e != nil {
					m.abort(s, EndInternal, e)
				}
			}
		}
		for _, s := range lease.ends {
			s.endAcked = true
			m.retire(s)
		}
		if lease.drain {
			m.drainAcked = true
		}
		m.pendingBytes -= lease.bytes
	}
	m.pending = m.leases.Count() > 0
	m.signal()
	return nil
}
func (m *Session) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := Status{Client: m.client, Ready: m.ready(), Draining: m.draining, PeerDraining: m.peerDrain, Pending: m.pending, SourceEOF: m.eof(), PeerEOF: m.peerEOF, Closed: m.closed, Active: len(m.order), MaximumActive: m.maximum, ReservedWindow: len(m.order) * m.limits.Window, Opened: max(m.nextID, m.lastPeerID), OpenSent: m.openSent, Retired: m.retired, RemoteGranted: m.remoteGranted, SentBytes: m.sent, ReceivedBytes: m.received, Limits: m.limits}
	v.PendingLeases, v.LeaseIssued, v.LeaseCommitted = m.leases.Count(), m.leases.Issued(), m.leases.Committed()
	v.EarlyOpenEnabled, v.EarlyOpenSent, v.EarlyOpenReceived = m.earlyOpen, m.earlyOpenSent, m.earlyOpenReceived
	v.Ready = v.Ready && !m.closed && m.ctx.Err() == nil
	if m.err != nil {
		v.Error = m.err.Error()
	}
	return v
}
func (m *Session) Wait(ctx context.Context, limit time.Duration) error {
	return m.wait(ctx, limit, 0, false)
}

// WaitCoalesced lets a standalone nonterminal CREDIT briefly collect adjacent
// application output. For an opted-in first OPEN, initial server SETTINGS may
// also collect RESULT within the same overall budget. Other DATA, SETTINGS,
// OPEN, RESULT, END, GRANT, DRAIN and FIN remain immediately ready. The original
// overall deadline is never extended.
func (m *Session) WaitCoalesced(ctx context.Context, limit, creditDelay time.Duration) error {
	return m.wait(ctx, limit, creditDelay, false)
}

// WaitCoalescedOutput also collects adjacent nonterminal DATA within the same
// bounded timer as CREDIT. Lifecycle controls and FIN remain immediately ready.
func (m *Session) WaitCoalescedOutput(ctx context.Context, limit, delay time.Duration) error {
	return m.wait(ctx, limit, delay, true)
}

func (m *Session) wait(ctx context.Context, limit, creditDelay time.Duration, coalesceData bool) error {
	if ctx == nil || limit < 0 || creditDelay < 0 {
		return errors.New("mux wait arguments")
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	var creditTimer *time.Timer
	creditExpired := false
	defer func() {
		if creditTimer != nil {
			creditTimer.Stop()
		}
	}()
	for {
		m.mu.Lock()
		if e := m.live(); e != nil {
			m.mu.Unlock()
			return e
		}
		var creditChannel <-chan time.Time
		if creditTimer != nil && !creditExpired {
			creditChannel = creditTimer.C
		}
		cases := []reflect.SelectCase{{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())}, {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(m.ctx.Done())}, {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timer.C)}, {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(m.changed)}, {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(creditChannel)}}
		ready := !m.settingsSent && !m.awaitEarlyResult() || m.eof() || m.pendingEarlyOpen() != nil
		creditReady := false
		if m.ready() {
			ready = ready || m.draining && !m.drainSent || !m.client && m.grantSent < m.retired
			for _, s := range m.order {
				ready = ready || m.client && !s.openSent && !s.endWanted || !m.client && s.resultKnown && !s.resultSent && !s.endWanted || s.endWanted && !s.endSent && s.openSent
				if s.endSent {
					continue
				}
				if s.resultKnown && s.result.Code != so.OK {
					ready = true
				}
				if s.link != nil {
					ch := s.link.Changes()
					st := s.link.Status()
					coalescible := st.AvailableBytes > 0 && (st.Queued == 0 || coalesceData) && !st.RemoteDelivered && !(st.WriteEOF && !st.FinSent) && !st.SourceEOF && !st.Closed
					creditReady = creditReady || coalescible
					ready = ready || st.AvailableBytes > 0 && !coalescible || st.SourceEOF || st.Closed
					cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
				}
			}
		}
		m.mu.Unlock()
		if creditReady {
			ready = ready || creditDelay == 0 || creditExpired
			if !ready && creditTimer == nil {
				creditTimer = time.NewTimer(min(limit, creditDelay))
				cases[4].Chan = reflect.ValueOf(creditTimer.C)
			}
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if ready {
			return nil
		}
		n, _, _ := reflect.Select(cases)
		switch n {
		case 0:
			return ctx.Err()
		case 1:
			return m.ctx.Err()
		case 2:
			return nil
		case 4:
			creditExpired = true
		}
	}
}
