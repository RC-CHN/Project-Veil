package session

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	fw "veil.local/core/internal/flightwindow"
	sm "veil.local/core/internal/streammux"
)

const flightHeader = 16

func encodeFlightFrame(frame fw.Frame) ([]byte, error) {
	if frame.ID == 0 || len(frame.Body) > maxObject-flightHeader {
		return nil, errors.New("flight frame bound")
	}
	p := make([]byte, flightHeader+len(frame.Body))
	p[0] = 1
	if frame.EOF {
		p[1] = 1
	}
	binary.BigEndian.PutUint64(p[8:], frame.ID)
	copy(p[flightHeader:], frame.Body)
	return p, nil
}

func decodeFlightFrame(p []byte, eof bool) (fw.Frame, error) {
	if len(p) == 0 {
		return fw.Frame{}, nil // Model-checked control object; no global data ID.
	}
	if len(p) < flightHeader || len(p) > maxObject || p[0] != 1 || p[1] > 1 || (p[1] == 1) != eof {
		return fw.Frame{}, errors.New("flight frame schema")
	}
	for _, v := range p[2:8] {
		if v != 0 {
			return fw.Frame{}, errors.New("flight reserved field")
		}
	}
	id := binary.BigEndian.Uint64(p[8:])
	if id == 0 {
		return fw.Frame{}, errors.New("zero flight data ID")
	}
	return fw.Frame{ID: id, Body: p[flightHeader:], EOF: eof}, nil
}

// Each lane is serial at the model layer, while the shared source owns global
// data order. A lane may perform receipt-only exchanges with no global lease.
type flightLane struct {
	coordination                  flightWaitTrace
	mu                            sync.Mutex
	ctx                           context.Context
	source                        *flightMuxSource
	pending, pendingEOF, ackedEOF bool
	id                            uint64
	leased                        int
}

func (l *flightLane) lease(capacity int) ([]byte, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending || capacity < flightHeader+sm.MinLease || capacity > maxObject {
		return nil, false, errors.New("flight lane lease phase or capacity")
	}
	frame, eof, e := l.source.nextFlight(capacity - flightHeader)
	if e != nil {
		return nil, false, e
	}
	var body []byte
	if frame.ID != 0 {
		body, e = encodeFlightFrame(frame)
		if e != nil {
			return nil, false, e
		}
	}
	l.pending, l.pendingEOF, l.id, l.leased = true, eof, frame.ID, len(body)
	return body, eof, nil
}

func (l *flightLane) commit() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.pending {
		return errors.New("flight lane receipt phase")
	}
	if l.id != 0 {
		if e := l.source.ack(l.id); e != nil {
			return e
		}
	}
	l.ackedEOF = l.ackedEOF || l.pendingEOF
	l.pending, l.pendingEOF, l.id, l.leased = false, false, 0, 0
	return nil
}

func (l *flightLane) deliver(body []byte, eof bool) error {
	return l.deliverContext(l.ctx, body, eof)
}

func (l *flightLane) deliverContext(ctx context.Context, body []byte, eof bool) error {
	frame, e := decodeFlightFrame(body, eof)
	if e != nil {
		return e
	}
	if frame.ID == 0 {
		return ctx.Err()
	}
	if _, e := l.source.receive(frame); e != nil {
		return e
	}
	start := time.Now()
	e = l.source.waitFlightState(ctx, frame.ID, false)
	l.coordination.add(start, false, e)
	return e
}

func (l *flightLane) wait(ctx context.Context, limit time.Duration) error {
	if l.source.mux.Status().EarlyOpenEnabled {
		return l.source.mux.WaitCoalescedOutput(ctx, limit, time.Millisecond)
	}
	return l.source.mux.WaitCoalesced(ctx, limit, time.Millisecond)
}

func (l *flightLane) waitClosing(ctx context.Context) error {
	start := time.Now()
	e := l.source.waitFlightState(ctx, 0, true)
	l.coordination.add(start, true, e)
	return e
}

func (l *flightLane) readyClosing() bool {
	l.source.mu.Lock()
	defer l.source.mu.Unlock()
	return l.source.closingReadyLocked()
}

func (l *flightLane) status() streamQueueStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.source.status()
	out := l.source.mux.Output()
	used := out.AvailableBytes + out.PendingBytes
	ready := st.AckedEOF && st.PeerEOF && !st.Mux.Pending && st.Receive.Pending == 0
	return streamQueueStatus{Used: used, Leased: l.leased, Pending: l.pending, InputEOF: st.Mux.SourceEOF, AckedEOF: l.ackedEOF && ready, Closed: st.Closed}
}

func (l *flightLane) close() { l.source.finish(errors.New("flight lane closed")) }
