// Package flightwindow tracks a fixed maximum of in-flight receipts. The owner
// serializes calls; it performs no I/O, callbacks, locking or body buffering.
package flightwindow

import (
	"errors"
	"math"
)

const Maximum = 4

var (
	ErrFull     = errors.New("in-flight lease window full")
	ErrReceipt  = errors.New("duplicate, expired or unknown lease receipt")
	ErrSequence = errors.New("lease sequence exhausted")
)

type Entry[T any] struct {
	ID    uint64
	Value T
}

type slot[T any] struct {
	Entry[T]
	acked bool
}

type Window[T any] struct {
	entries            [Maximum]slot[T]
	head, count, limit int
	issued, committed  uint64
}

func New[T any](limit int) (Window[T], error) {
	if limit < 1 || limit > Maximum {
		return Window[T]{}, errors.New("in-flight lease limit")
	}
	return Window[T]{limit: limit}, nil
}

func (w *Window[T]) Count() int        { return w.count }
func (w *Window[T]) Limit() int        { return w.limit }
func (w *Window[T]) Issued() uint64    { return w.issued }
func (w *Window[T]) Committed() uint64 { return w.committed }
func (w *Window[T]) Full() bool        { return w.count >= w.limit }

func (w *Window[T]) Front() (Entry[T], bool) {
	if w.count == 0 {
		return Entry[T]{}, false
	}
	return w.entries[w.head].Entry, true
}

func (w *Window[T]) Push(value T) (uint64, error) {
	if w.Full() {
		return 0, ErrFull
	}
	if w.issued == math.MaxUint64 {
		return 0, ErrSequence
	}
	w.issued++
	w.entries[(w.head+w.count)%Maximum] = slot[T]{Entry: Entry[T]{ID: w.issued, Value: value}}
	w.count++
	return w.issued, nil
}

// Ack returns only the contiguous acknowledged prefix, in issue order. Slots
// remain occupied behind a missing receipt, including already-acked slots.
func (w *Window[T]) Ack(id uint64) ([]Entry[T], error) {
	found := false
	for i := 0; i < w.count; i++ {
		entry := &w.entries[(w.head+i)%Maximum]
		if entry.ID == id {
			if entry.acked {
				return nil, ErrReceipt
			}
			entry.acked = true
			found = true
			break
		}
	}
	if !found {
		return nil, ErrReceipt
	}
	var ready []Entry[T]
	for w.count > 0 && w.entries[w.head].acked {
		entry := w.entries[w.head].Entry
		ready = append(ready, entry)
		w.committed = entry.ID
		w.entries[w.head] = slot[T]{}
		w.head = (w.head + 1) % Maximum
		w.count--
	}
	return ready, nil
}

// Clear drops references after owner shutdown without manufacturing receipts
// or resetting the issue counter to permit accidental token reuse.
func (w *Window[T]) Clear() {
	clear(w.entries[:])
	w.head, w.count = 0, 0
}
