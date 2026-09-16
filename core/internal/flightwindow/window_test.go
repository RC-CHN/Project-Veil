package flightwindow

import (
	"errors"
	"math"
	"testing"
)

func TestReceiptsCannotSkipAnEarlierLease(t *testing.T) {
	w, _ := New[int](4)
	for i := 1; i <= 4; i++ {
		id, e := w.Push(i * 10)
		if e != nil || id != uint64(i) {
			t.Fatal(id, e)
		}
	}
	before := w
	if _, e := w.Push(50); !errors.Is(e, ErrFull) || w != before {
		t.Fatal("full window changed state", e)
	}
	for _, id := range []uint64{4, 2, 3} {
		ready, e := w.Ack(id)
		if e != nil || len(ready) != 0 || w.Count() != 4 || w.Committed() != 0 {
			t.Fatal("receipt crossed gap", id, e)
		}
	}
	before = w
	if _, e := w.Ack(2); !errors.Is(e, ErrReceipt) || w != before {
		t.Fatal("duplicate receipt mutated window", e)
	}
	ready, e := w.Ack(1)
	if e != nil || len(ready) != 4 || w.Count() != 0 || w.Committed() != 4 {
		t.Fatal(ready, e)
	}
	for i, entry := range ready {
		if entry.ID != uint64(i+1) || entry.Value != (i+1)*10 {
			t.Fatal("commit order", ready)
		}
	}
	for _, slot := range w.entries {
		if slot.ID != 0 || slot.Value != 0 {
			t.Fatal("retained retired metadata")
		}
	}
	if _, e := w.Ack(1); !errors.Is(e, ErrReceipt) {
		t.Fatal("expired receipt accepted", e)
	}
}

func TestSequenceDoesNotWrapOrResetOnClear(t *testing.T) {
	for _, limit := range []int{0, -1, 5} {
		if _, e := New[int](limit); e == nil {
			t.Fatal("limit", limit)
		}
	}
	w, _ := New[*int](4)
	x := 1
	w.Push(&x)
	w.Clear()
	if w.Count() != 0 || w.Issued() != 1 || w.Committed() != 0 {
		t.Fatal("clear invented receipt or reused ID")
	}
	for _, slot := range w.entries {
		if slot.Value != nil {
			t.Fatal("clear retained reference")
		}
	}
	w.issued = math.MaxUint64 - 1
	id, e := w.Push(&x)
	if e != nil || id != math.MaxUint64 {
		t.Fatal(id, e)
	}
	w.Ack(id)
	if _, e = w.Push(&x); !errors.Is(e, ErrSequence) || w.Count() != 0 || w.Issued() != math.MaxUint64 {
		t.Fatal("sequence wrapped", e)
	}
}

func FuzzReceiptWindow(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 13, 9, 5, 1, 0, 3})
	f.Add([]byte{0, 1, 1, 2, 3, 0, 0, 5, 1})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 256 {
			ops = ops[:256]
		}
		w, _ := New[uint64](4)
		type item struct {
			id, value uint64
			ack       bool
		}
		var queue []item
		var issued, committed uint64
		for index, op := range ops {
			switch op % 4 {
			case 0:
				id, e := w.Push(uint64(index))
				if len(queue) == 4 {
					if !errors.Is(e, ErrFull) {
						t.Fatal("missing backpressure", e)
					}
				} else {
					issued++
					if e != nil || id != issued {
						t.Fatal("issue order", id, e)
					}
					queue = append(queue, item{id: id, value: uint64(index)})
				}
			case 1:
				id := uint64(0)
				at := 0
				if len(queue) > 0 {
					at = int(op>>2) % len(queue)
					id = queue[at].id
				}
				ready, e := w.Ack(id)
				if len(queue) == 0 || queue[at].ack {
					if !errors.Is(e, ErrReceipt) {
						t.Fatal("invalid receipt accepted")
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					queue[at].ack = true
					var expected []item
					for len(queue) > 0 && queue[0].ack {
						expected = append(expected, queue[0])
						committed = queue[0].id
						queue = queue[1:]
					}
					if len(expected) != len(ready) {
						t.Fatal("commit prefix length")
					}
					for i, p := range expected {
						if ready[i].ID != p.id || ready[i].Value != p.value {
							t.Fatal("commit order")
						}
					}
				}
			case 2:
				before := w
				if _, e := w.Ack(issued + 1 + uint64(op)); !errors.Is(e, ErrReceipt) || before != w {
					t.Fatal("unknown receipt changed state")
				}
			case 3:
				w.Clear()
				queue = nil
			}
			if w.Count() != len(queue) || w.Issued() != issued || w.Committed() != committed {
				t.Fatal("window accounting diverged")
			}
		}
	})
}
