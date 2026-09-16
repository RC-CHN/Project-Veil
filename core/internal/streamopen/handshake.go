package streamopen

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

type Rejection struct{ Code byte }

func (e Rejection) Error() string { return fmt.Sprintf("destination OPEN rejected: %d", e.Code) }

type Status struct {
	PrefixSentSHA256, PrefixReceivedSHA256                                          string
	Client, RequestReceived, ResultKnown, PrefixSent, Established, Rejected, Closed bool
	QueuedBytes, ParserBytes                                                        int
	Request                                                                         Request
	Result                                                                          Result
	Error                                                                           string `json:",omitempty"`
}
type Handshake struct {
	sentHash, receivedHash                                   string
	mu                                                       sync.Mutex
	client, requestReceived, resultKnown, prefixSent, closed bool
	cap                                                      Limits
	request                                                  Request
	result                                                   Result
	err                                                      error
	out                                                      []byte
	parser                                                   [Header + MaxBody]byte
	parsed, need                                             int
	changed                                                  chan struct{}
}

func NewClient(r Request) (*Handshake, error) {
	p, e := EncodeRequest(r)
	if e != nil {
		return nil, e
	}
	return &Handshake{client: true, request: r, cap: r.Limits, out: p, need: Header, changed: make(chan struct{})}, nil
}
func NewServer(cap Limits) (*Handshake, error) {
	if !cap.Valid() {
		return nil, errors.New("server OPEN limits")
	}
	return &Handshake{cap: cap, need: Header, changed: make(chan struct{})}, nil
}
func (h *Handshake) signal() { close(h.changed); h.changed = make(chan struct{}) }
func (h *Handshake) fail(e error) error {
	if !h.closed {
		h.closed = true
		h.err = e
		clear(h.out)
		h.out = nil
		clear(h.parser[:])
		h.parsed = 0
		h.signal()
	}
	return e
}
func (h *Handshake) Close(e error) { h.mu.Lock(); defer h.mu.Unlock(); h.fail(e) }
func (h *Handshake) live() error {
	if !h.closed {
		return nil
	}
	if h.err != nil {
		return h.err
	}
	return errors.New("OPEN closed")
}
func (h *Handshake) established() bool {
	return h.resultKnown && h.result.Code == OK && (h.client || h.prefixSent)
}
func (h *Handshake) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := Status{PrefixSentSHA256: h.sentHash, PrefixReceivedSHA256: h.receivedHash, Client: h.client, RequestReceived: h.requestReceived, ResultKnown: h.resultKnown, PrefixSent: h.prefixSent, Established: h.established(), Rejected: h.resultKnown && h.result.Code != OK, Closed: h.closed, QueuedBytes: len(h.out), ParserBytes: h.parsed, Request: h.request, Result: h.result}
	if h.err != nil {
		s.Error = h.err.Error()
	}
	return s
}

// TakePrefix transfers ownership once. The outer carrier owns receipt tracking.
func (h *Handshake) TakePrefix(capacity int) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.live(); e != nil {
		return nil, e
	}
	if len(h.out) == 0 {
		return nil, nil
	}
	if capacity < len(h.out) {
		return nil, h.fail(errors.New("OPEN prefix capacity"))
	}
	p := h.out
	sum := sha256.Sum256(p)
	h.sentHash = hex.EncodeToString(sum[:])
	h.out = nil
	h.prefixSent = true
	h.signal()
	return p, nil
}

// Feed only parses a bounded prefix. Returned bytes belong to streamlink and
// cannot be returned until this endpoint has a successful handshake.
func (h *Handshake) Feed(p []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.live(); e != nil {
		return nil, e
	}
	if h.established() {
		return p, nil
	}
	if h.resultKnown && h.result.Code != OK || !h.client && h.requestReceived {
		if len(p) != 0 {
			return nil, h.fail(errors.New("data before successful RESULT"))
		}
		return nil, nil
	}
	if h.client && !h.prefixSent && len(p) > 0 {
		return nil, h.fail(errors.New("RESULT before OPEN sent"))
	}
	for len(p) > 0 {
		n := copy(h.parser[h.parsed:h.need], p)
		h.parsed += n
		p = p[n:]
		if h.parsed < h.need {
			continue
		}
		if h.need == Header {
			kind := byte(1)
			if h.client {
				kind = 2
			}
			size := binary.BigEndian.Uint32(h.parser[4:8])
			if h.parser[0] != 1 || h.parser[1] != kind || h.parser[2] != 0 || h.parser[3] != 0 || size > MaxBody || !h.client && size < 16 || h.client && size != 16 {
				return nil, h.fail(errors.New("OPEN prefix header"))
			}
			h.need = Header + int(size)
			if h.parsed < h.need {
				continue
			}
		}
		if h.client {
			r, e := decodeResult(h.parser[Header:h.need])
			if e != nil {
				return nil, h.fail(e)
			}
			if r.Code == OK && !r.Limits.within(h.request.Limits) {
				return nil, h.fail(errors.New("RESULT expands requested limits"))
			}
			h.result = r
			h.resultKnown = true
		} else {
			r, e := decodeRequest(h.parser[Header:h.need])
			if e != nil {
				return nil, h.fail(e)
			}
			h.request = r
			h.requestReceived = true
		}
		sum := sha256.Sum256(h.parser[:h.need])
		h.receivedHash = hex.EncodeToString(sum[:])
		clear(h.parser[:])
		h.parsed = 0
		h.need = Header
		h.signal()
		if len(p) > 0 && !h.established() {
			return nil, h.fail(errors.New("data before successful RESULT"))
		}
		return p, nil
	}
	return nil, nil
}
func (h *Handshake) Respond(r Result) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.live(); e != nil {
		return e
	}
	if h.client || !h.requestReceived || h.resultKnown {
		return h.fail(errors.New("RESULT phase"))
	}
	if r.Code == OK && (!r.Limits.within(h.request.Limits) || !r.Limits.within(h.cap)) {
		return h.fail(errors.New("RESULT exceeds limits"))
	}
	p, e := EncodeResult(r)
	if e != nil {
		return h.fail(e)
	}
	h.result = r
	h.resultKnown = true
	h.out = p
	h.signal()
	return nil
}
func (h *Handshake) wait(ctx context.Context, result bool) error {
	if ctx == nil {
		return errors.New("nil OPEN wait context")
	}
	for {
		h.mu.Lock()
		if e := h.live(); e != nil {
			h.mu.Unlock()
			return e
		}
		ready := h.requestReceived
		if result {
			ready = h.resultKnown
		}
		changed := h.changed
		h.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (h *Handshake) WaitRequest(ctx context.Context) (Request, error) {
	if h.client {
		return Request{}, errors.New("client request wait")
	}
	if e := h.wait(ctx, false); e != nil {
		return Request{}, e
	}
	return h.Status().Request, nil
}
func (h *Handshake) WaitResult(ctx context.Context) (Result, error) {
	if !h.client {
		return Result{}, errors.New("server result wait")
	}
	if e := h.wait(ctx, true); e != nil {
		return Result{}, e
	}
	r := h.Status().Result
	if r.Code != OK {
		return r, Rejection{r.Code}
	}
	return r, nil
}
func (h *Handshake) PeerDone() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.live(); e != nil {
		return e
	}
	if h.parsed != 0 || !h.resultKnown || !h.client && !h.prefixSent {
		return h.fail(errors.New("outer EOF before OPEN completion"))
	}
	return nil
}
