package flightwindow

import (
	"bytes"
	"errors"
)

const MaxBody = 256 << 10

var ErrReceive = errors.New("receive window closed, sequence, body or EOF violation")

// Frame owns its Body after Pop. Released is an ownership counter, not a
// delivery or receipt acknowledgement. The owner must Feed before any receipt.
type Frame struct {
	ID   uint64
	Body []byte
	EOF  bool
}

// Receiver is serialized by its owner, including Pop and subsequent delivery.
// It has no callbacks, I/O or internal concurrency. Put copies input bodies.
type Receiver struct {
	slots              [Maximum]Frame
	limit, count, held int
	released, firstEOF uint64
	closed             bool
}

type ReceiveStatus struct {
	Pending, Bytes, Limit int
	Released, FirstEOF    uint64
	Closed                bool
}

func NewReceiver(limit int) (*Receiver, error) {
	if limit < 1 || limit > Maximum {
		return nil, errors.New("receive window limit")
	}
	return &Receiver{limit: limit}, nil
}

func (r *Receiver) Status() ReceiveStatus {
	return ReceiveStatus{r.count, r.held, r.limit, r.released, r.firstEOF, r.closed}
}

func (r *Receiver) Close() {
	clear(r.slots[:])
	r.count, r.held, r.closed = 0, 0, true
}

func (r *Receiver) fail() error {
	r.Close()
	return ErrReceive
}

func (r *Receiver) Put(id uint64, body []byte, eof bool) error {
	// Compare before subtracting, so neither the lower nor upper bound wraps.
	if r.closed || id <= r.released || id-r.released > uint64(r.limit) || len(body) > MaxBody {
		return r.fail()
	}
	index := (id - 1) % Maximum
	if r.slots[index].ID != 0 || r.firstEOF != 0 && id > r.firstEOF && (len(body) != 0 || !eof) {
		return r.fail()
	}
	if eof && (r.firstEOF == 0 || id < r.firstEOF) {
		for _, other := range r.slots {
			if other.ID > id && (len(other.Body) != 0 || !other.EOF) {
				return r.fail()
			}
		}
		r.firstEOF = id
	}
	r.slots[index] = Frame{ID: id, Body: bytes.Clone(body), EOF: eof}
	r.count++
	r.held += len(body)
	return nil
}

// Pop transfers only the earliest contiguous frame. The owner must deliver or
// terminate before continuing; popping itself does not acknowledge anything.
func (r *Receiver) Pop() (Frame, bool) {
	if r.closed || r.count == 0 {
		return Frame{}, false
	}
	index := r.released % Maximum
	frame := r.slots[index]
	if frame.ID == 0 {
		return Frame{}, false
	}
	r.slots[index] = Frame{}
	r.count--
	r.held -= len(frame.Body)
	r.released = frame.ID
	return frame, true
}
