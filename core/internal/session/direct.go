package session

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	ep "veil.local/core/endpoint"
	dg "veil.local/core/internal/datagram"
	d "veil.local/core/internal/destination"
	sl "veil.local/core/internal/streamlink"
	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
	"veil.local/core/transport"
)

// RunDirect runs the carrier pool without creating any application listener.
func (c *Client) RunDirect(ctx context.Context, ready func()) error {
	if ctx == nil {
		return errors.New("nil runtime context")
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	c.mu.Lock()
	if c.directRun {
		c.mu.Unlock()
		return errors.New("client already run")
	}
	c.directRun = true
	c.directSlots = make(chan struct{}, c.cfg.MaxConnections)
	c.pool = newClientMuxPool(c, ctx)
	c.ready = true
	c.mu.Unlock()
	if ready != nil {
		ready()
	}
	<-ctx.Done()
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	c.pool.close()
	c.directWorkers.Wait()
	c.pool.workers.Wait()
	return nil
}
func (c *Client) DialStream(ctx context.Context, target ep.Endpoint) (transport.Stream, error) {
	if !target.Valid() {
		return nil, ep.ErrInvalid
	}
	s, e := c.openDirect(ctx, so.Request{Address: d.Address{Host: target.Host(), Port: target.Port()}, Limits: so.Limits{Window: c.cfg.Window, MaxBytes: c.cfg.MaxBytes}})
	if e != nil {
		return nil, e
	}
	go s.monitor(nil)
	return s, nil
}
func (c *Client) OpenAssociation(ctx context.Context) (transport.Association, error) {
	s, e := c.openDirect(ctx, so.Request{Network: so.NetworkUDP, Limits: so.Limits{Window: c.cfg.Window, MaxBytes: c.cfg.MaxBytes}})
	if e != nil {
		return nil, e
	}
	a := &directAssociation{stream: s, send: dg.NewQueue(), receive: dg.NewQueue()}
	a.workers.Add(2)
	go a.writeLoop()
	go a.readLoop()
	go s.monitor(func() { a.send.Close(true); a.receive.Close(true); a.workers.Wait() })
	return a, nil
}
func (c *Client) openDirect(ctx context.Context, r so.Request) (out *directStream, err error) {
	if ctx == nil {
		return nil, errors.New("nil open context")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	c.mu.Lock()
	if !c.ready || !c.directRun || !time.Now().Before(c.expires) {
		c.mu.Unlock()
		return nil, transport.ErrNotReady
	}
	select {
	case c.directSlots <- struct{}{}:
	default:
		c.mu.Unlock()
		return nil, transport.ErrCapacity
	}
	c.directWorkers.Add(1)
	pool := c.pool
	c.mu.Unlock()
	c.stats.openStream()
	var carrier *clientMuxCarrier
	var stream *sm.Stream
	defer func() {
		if out == nil {
			if stream != nil {
				_ = stream.Reset(sm.EndCancelled)
				stream.Release()
				carrier.workers.Done()
			}
			c.stats.streams.Add(-1)
			c.stats.streamsFailed.Add(1)
			<-c.directSlots
			c.directWorkers.Done()
		}
	}()
	openCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	carrier, stream, err = pool.acquire(openCtx, r)
	if err != nil {
		return nil, err
	}
	_, link, err := stream.WaitResult(openCtx)
	if err != nil {
		return nil, err
	}
	if err = openCtx.Err(); err != nil {
		return nil, err
	}
	local, cancelLocal := context.WithCancel(stream.Context())
	out = &directStream{client: c, carrier: carrier, stream: stream, link: link, ctx: local, cancel: cancelLocal, readGate: make(chan struct{}, 1), writeGate: make(chan struct{}, 1), done: make(chan struct{}), started: time.Now()}
	if r.Network == 0 {
		out.target, _ = ep.Parse(r.Address.Host, r.Address.Port)
	}
	return out, nil
}

type directStream struct {
	client                      *Client
	carrier                     *clientMuxCarrier
	stream                      *sm.Stream
	link                        *sl.Link
	ctx                         context.Context
	cancel                      context.CancelFunc
	readGate, writeGate         chan struct{}
	readDeadline, writeDeadline ioDeadline
	closed, readEOF             atomic.Bool
	writeEOF                    atomic.Bool
	done                        chan struct{}
	target                      ep.Endpoint
	started                     time.Time
}

func (s *directStream) monitor(join func()) {
	stopIdle := watchMuxStreamIdle(s.stream, time.Duration(s.client.cfg.IdleTimeoutMS)*time.Millisecond)
	cause := waitMuxTerminal(s.stream)
	stopIdle()
	if cause == nil && s.link.Status().RemoteDelivered {
		s.readEOF.Store(true)
	}
	s.cancel()
	if join != nil {
		join()
	}
	// Context cancellation releases blocking operations; retain the stream lease
	// until both application I/O critical sections and association pumps join.
	s.readGate <- struct{}{}
	s.writeGate <- struct{}{}
	event := SessionEvent{EventType: "mux_stream", Role: "client", ModelID: s.client.modelID(), Prefix: s.carrier.prefix, StartedNS: s.started.UnixNano()}
	completeMuxStreamEvent(&s.client.stats, &event, s.stream, nil, cause, true)
	if s.client.observer != nil {
		s.client.observer(event)
	}
	s.stream.Release()
	s.carrier.workers.Done()
	s.client.stats.streams.Add(-1)
	<-s.client.directSlots
	s.client.directWorkers.Done()
	<-s.writeGate
	<-s.readGate
	close(s.done)
}
func (s *directStream) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, net.ErrClosed
	}
	if s.readEOF.Load() {
		return 0, io.EOF
	}
	op := s.readDeadline.begin(s.ctx)
	defer op.finish()
	if e := lockIO(op.ctx, s.readGate); e != nil {
		return 0, s.ioError(op.ctx, e)
	}
	defer func() { <-s.readGate }()
	if s.readEOF.Load() {
		return 0, io.EOF
	}
	n, e := s.link.Read(op.ctx, p)
	if e == io.EOF {
		s.readEOF.Store(true)
	}
	return n, s.ioError(op.ctx, e)
}
func (s *directStream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, net.ErrClosed
	}
	op := s.writeDeadline.begin(s.ctx)
	defer op.finish()
	if e := lockIO(op.ctx, s.writeGate); e != nil {
		return 0, s.ioError(op.ctx, e)
	}
	defer func() { <-s.writeGate }()
	if s.writeEOF.Load() {
		return 0, io.ErrClosedPipe
	}
	if e := op.ctx.Err(); e != nil {
		return 0, s.ioError(op.ctx, e)
	}
	n, e := s.link.Write(op.ctx, p)
	return n, s.ioError(op.ctx, e)
}
func (s *directStream) CloseWrite() error {
	if s.writeEOF.Load() {
		return nil
	}
	op := s.writeDeadline.begin(s.ctx)
	defer op.finish()
	if e := lockIO(op.ctx, s.writeGate); e != nil {
		return s.ioError(op.ctx, e)
	}
	defer func() { <-s.writeGate }()
	if s.writeEOF.Load() {
		return nil
	}
	if s.closed.Load() {
		return net.ErrClosed
	}
	if e := op.ctx.Err(); e != nil {
		return s.ioError(op.ctx, e)
	}
	e := s.link.Finish()
	if e == nil {
		s.writeEOF.Store(true)
	}
	return s.ioError(op.ctx, e)
}
func (s *directStream) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		s.cancel()
		_ = s.stream.Reset(sm.EndCancelled)
	}
	return nil
}
func (s *directStream) LocalAddr() net.Addr  { return logicalAddr("local/" + s.carrier.prefix) }
func (s *directStream) RemoteAddr() net.Addr { return s.target }
func (s *directStream) SetDeadline(t time.Time) error {
	s.readDeadline.set(t)
	s.writeDeadline.set(t)
	return nil
}
func (s *directStream) SetReadDeadline(t time.Time) error  { s.readDeadline.set(t); return nil }
func (s *directStream) SetWriteDeadline(t time.Time) error { s.writeDeadline.set(t); return nil }
func (s *directStream) ioError(ctx context.Context, e error) error {
	if e == nil || e == io.EOF {
		return e
	}
	if cause := context.Cause(ctx); cause != nil {
		if errors.Is(cause, os.ErrDeadlineExceeded) {
			return os.ErrDeadlineExceeded
		}
		return net.ErrClosed
	}
	return e
}

type logicalAddr string

func (a logicalAddr) Network() string { return "veil" }
func (a logicalAddr) String() string  { return string(a) }
func lockIO(ctx context.Context, gate chan struct{}) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	select {
	case gate <- struct{}{}:
		if e := ctx.Err(); e != nil {
			<-gate
			return e
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Deadline updates affect operations already waiting, including those waiting
// for the per-direction gate. Timer generations discard stale callbacks.
type ioDeadline struct {
	mu     sync.Mutex
	at     time.Time
	active map[*ioOperation]bool
}
type ioOperation struct {
	owner      *ioDeadline
	ctx        context.Context
	cancel     context.CancelCauseFunc
	timer      *time.Timer
	generation uint64
}

func (d *ioDeadline) begin(parent context.Context) *ioOperation {
	ctx, cancel := context.WithCancelCause(parent)
	op := &ioOperation{owner: d, ctx: ctx, cancel: cancel}
	d.mu.Lock()
	if d.active == nil {
		d.active = make(map[*ioOperation]bool)
	}
	d.active[op] = true
	d.arm(op)
	d.mu.Unlock()
	return op
}
func (d *ioDeadline) arm(op *ioOperation) {
	op.generation++
	gen := op.generation
	if op.timer != nil {
		op.timer.Stop()
		op.timer = nil
	}
	if d.at.IsZero() {
		return
	}
	delay := time.Until(d.at)
	if delay <= 0 {
		op.cancel(os.ErrDeadlineExceeded)
		return
	}
	op.timer = time.AfterFunc(delay, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.active[op] && op.generation == gen {
			op.cancel(os.ErrDeadlineExceeded)
		}
	})
}
func (d *ioDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at = t
	for op := range d.active {
		d.arm(op)
	}
}
func (op *ioOperation) finish() {
	d := op.owner
	d.mu.Lock()
	delete(d.active, op)
	op.generation++
	if op.timer != nil {
		op.timer.Stop()
	}
	d.mu.Unlock()
	op.cancel(context.Canceled)
}

type directAssociation struct {
	stream        *directStream
	send, receive *dg.Queue
	workers       sync.WaitGroup
}

func (a *directAssociation) Send(ctx context.Context, target ep.Endpoint, p []byte) error {
	if ctx == nil {
		return errors.New("nil datagram context")
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if !target.Valid() {
		return ep.ErrInvalid
	}
	if len(p) > transport.MaxDatagram {
		return errors.New("datagram size limit")
	}
	if a.stream.ctx.Err() != nil {
		return net.ErrClosed
	}
	frame, e := dg.Encode(dg.Packet{Address: d.Address{Host: target.Host(), Port: target.Port()}, Payload: p})
	if e != nil {
		return e
	}
	return a.send.PushContext(ctx, frame)
}
func (a *directAssociation) Receive(ctx context.Context, dst []byte) (int, ep.Endpoint, error) {
	if ctx == nil {
		return 0, ep.Endpoint{}, errors.New("nil datagram context")
	}
	if e := ctx.Err(); e != nil {
		return 0, ep.Endpoint{}, e
	}
	if a.stream.ctx.Err() != nil {
		return 0, ep.Endpoint{}, net.ErrClosed
	}
	p, e := a.receive.Pop(ctx)
	if e != nil {
		return 0, ep.Endpoint{}, e
	}
	packet, e := dg.Decode(p)
	if e != nil {
		return 0, ep.Endpoint{}, e
	}
	source, e := ep.Parse(packet.Address.Host, packet.Address.Port)
	if e != nil {
		return 0, ep.Endpoint{}, e
	}
	if len(dst) < len(packet.Payload) {
		return 0, source, transport.ErrShortBuffer
	}
	return copy(dst, packet.Payload), source, nil
}
func (a *directAssociation) Close() error { return a.stream.Close() }
func (a *directAssociation) writeLoop() {
	defer a.workers.Done()
	for {
		p, e := a.send.Pop(a.stream.ctx)
		if e != nil {
			return
		}
		if _, e = a.stream.Write(p); e != nil {
			a.stream.Close()
			return
		}
	}
}
func (a *directAssociation) readLoop() {
	defer a.workers.Done()
	defer a.receive.Close(false)
	var decoder dg.Decoder
	buf := make([]byte, sl.MaxData)
	for {
		n, e := a.stream.Read(buf)
		if n > 0 {
			_, err := decoder.Write(buf[:n], func(p dg.Packet) error {
				frame, err := dg.Encode(p)
				if err == nil {
					a.receive.Push(frame)
				}
				return err
			})
			if err != nil {
				a.stream.Close()
				return
			}
		}
		if e != nil {
			a.stream.Close()
			return
		}
	}
}
