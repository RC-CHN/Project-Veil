package flightwindow

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func TestReceivePrefixOwnershipAndBound(t *testing.T) {
	r, _ := NewReceiver(4)
	body := bytes.Repeat([]byte{7}, MaxBody)
	for id := uint64(4); id > 1; id-- {
		if e := r.Put(id, body, false); e != nil {
			t.Fatal(e)
		}
		if _, ok := r.Pop(); ok {
			t.Fatal("released a frame across missing first frame")
		}
	}
	if e := r.Put(1, body, false); e != nil {
		t.Fatal(e)
	}
	if st := r.Status(); st.Pending != 4 || st.Bytes != 4*MaxBody || st.Released != 0 {
		t.Fatal(st)
	}
	body[0] = 9
	for id := uint64(1); id <= 4; id++ {
		frame, ok := r.Pop()
		if !ok || frame.ID != id || frame.Body[0] != 7 || len(frame.Body) != MaxBody {
			t.Fatal("order or input alias", id)
		}
		frame.Body[0] = 3
	}
	if st := r.Status(); st.Pending != 0 || st.Bytes != 0 || st.Released != 4 || st.Closed {
		t.Fatal(st)
	}
	for _, slot := range r.slots {
		if slot.ID != 0 || slot.Body != nil {
			t.Fatal("released reference retained")
		}
	}
}

func TestReceiveViolationsCloseAndClear(t *testing.T) {
	for _, name := range []string{"zero", "duplicate", "future", "large", "past", "late-data", "late-non-eof", "early-eof", "close"} {
		t.Run(name, func(t *testing.T) {
			r, _ := NewReceiver(4)
			if e := r.Put(2, []byte{2}, false); e != nil {
				t.Fatal(e)
			}
			var e error
			switch name {
			case "zero":
				e = r.Put(0, nil, false)
			case "duplicate":
				e = r.Put(2, nil, false)
			case "future":
				e = r.Put(5, nil, false)
			case "large":
				e = r.Put(1, make([]byte, MaxBody+1), false)
			case "past":
				r.Put(1, nil, false)
				r.Pop()
				e = r.Put(1, nil, false)
			case "late-data", "late-non-eof":
				r.Put(3, nil, true)
				if name == "late-data" {
					e = r.Put(4, []byte{4}, true)
				} else {
					e = r.Put(4, nil, false)
				}
			case "early-eof":
				e = r.Put(1, nil, true)
			case "close":
				r.Close()
				e = r.Put(1, nil, false)
			}
			if !errors.Is(e, ErrReceive) || !r.Status().Closed || r.Status().Pending != 0 || r.Status().Bytes != 0 {
				t.Fatal("violation retained receive state", e, r.Status())
			}
			if _, ok := r.Pop(); ok {
				t.Fatal("closed receiver produced data")
			}
			for _, slot := range r.slots {
				if slot.Body != nil {
					t.Fatal("failure retained body")
				}
			}
		})
	}
}

func TestReceiveEOFAndSequenceEnd(t *testing.T) {
	for _, limit := range []int{-1, 0, 5} {
		if _, e := NewReceiver(limit); e == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	r, _ := NewReceiver(4)
	for _, frame := range []Frame{{ID: 4, EOF: true}, {ID: 3, EOF: true}, {ID: 2, Body: []byte{2}, EOF: true}, {ID: 1, Body: []byte{1}}} {
		if e := r.Put(frame.ID, frame.Body, frame.EOF); e != nil {
			t.Fatal(e)
		}
	}
	for id := uint64(1); id <= 4; id++ {
		if frame, ok := r.Pop(); !ok || frame.ID != id || frame.EOF != (id >= 2) {
			t.Fatal("EOF prefix")
		}
	}
	if r.Status().FirstEOF != 2 {
		t.Fatal("first EOF changed")
	}
	r, _ = NewReceiver(2)
	r.released = math.MaxUint64 - 2
	if e := r.Put(math.MaxUint64, nil, true); e != nil {
		t.Fatal(e)
	}
	if _, ok := r.Pop(); ok {
		t.Fatal("final sequence skipped prefix")
	}
	if e := r.Put(math.MaxUint64-1, nil, false); e != nil {
		t.Fatal(e)
	}
	r.Pop()
	r.Pop()
	if r.Status().Released != math.MaxUint64 {
		t.Fatal("final sequence did not release")
	}
	if e := r.Put(1, nil, true); e == nil || r.Status().Released != math.MaxUint64 {
		t.Fatal("sequence wrapped or reset")
	}
}

func FuzzReceiveWindow(f *testing.F) {
	f.Add([]byte{40, 24, 32, 16, 5, 5, 5, 5})
	f.Add([]byte{48, 56, 0, 5, 8, 5, 5, 5, 7})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 256 {
			ops = ops[:256]
		}
		limit := 4
		r, _ := NewReceiver(limit)
		pending := map[uint64]Frame{}
		var released, firstEOF uint64
		closed := false
		for step, op := range ops {
			if op&7 == 7 {
				limit = 1 + int(op>>3)%4
				r, _ = NewReceiver(limit)
				pending, released, firstEOF, closed = map[uint64]Frame{}, 0, 0, false
			} else if op&7 == 6 {
				r.Close()
				pending, closed = map[uint64]Frame{}, true
			} else if op&7 == 5 {
				want, exists := pending[released+1]
				got, ok := r.Pop()
				if ok != exists || ok && (got.ID != want.ID || got.EOF != want.EOF || !bytes.Equal(got.Body, want.Body)) {
					t.Fatal("reference pop differs")
				}
				if ok {
					delete(pending, got.ID)
					released = got.ID
				}
			} else {
				id := uint64(max(0, int(released)+int(op>>3)%7-1))
				body, eof := bytes.Repeat([]byte{byte(step)}, int(op>>6)), op&4 != 0
				_, duplicate := pending[id]
				invalid := closed || id <= released || id > released+uint64(limit) || duplicate
				if firstEOF > 0 && id > firstEOF && (len(body) > 0 || !eof) {
					invalid = true
				}
				if eof {
					for later, frame := range pending {
						if later > id && (len(frame.Body) > 0 || !frame.EOF) {
							invalid = true
						}
					}
				}
				e := r.Put(id, body, eof)
				if (e != nil) != invalid {
					t.Fatal("reference acceptance differs", op, id, e)
				}
				if invalid {
					pending, closed = map[uint64]Frame{}, true
				} else {
					pending[id] = Frame{id, body, eof}
					if eof && (firstEOF == 0 || id < firstEOF) {
						firstEOF = id
					}
				}
			}
			var held int
			for _, frame := range pending {
				held += len(frame.Body)
			}
			st := r.Status()
			if st.Limit != limit || st.Pending != len(pending) || st.Bytes != held || st.Released != released || st.FirstEOF != firstEOF || st.Closed != closed {
				t.Fatal("reference state differs", st)
			}
		}
	})
}
