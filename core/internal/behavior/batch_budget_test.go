package behavior

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func budgetFixture(t *testing.T) batchVector {
	t.Helper()
	p, e := os.ReadFile("../../testdata/batch-v2-budget.json")
	if e != nil {
		t.Fatal(e)
	}
	var v batchVector
	if e = json.Unmarshal(p, &v); e != nil {
		t.Fatal(e)
	}
	return v
}

func TestBatchBudgetPublicLoopAndAtomicExit(t *testing.T) {
	v := budgetFixture(t)
	p, e := CompileBatch(v.Model)
	if e != nil || p.ID() != v.ModelID {
		t.Fatal(p, e)
	}
	s, e := NewBatchSession(context.Background(), p, [32]byte{1}, [32]byte{2})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for seq := 0; seq < 3; seq++ {
		o, e := s.Plan()
		if e != nil || !reflect.DeepEqual(o, v.Offers[seq]) {
			t.Fatal(o, e)
		}
		n := seq
		if seq == 2 {
			n = o.Members[0].RequestSizes[1]
		}
		data := bytes.Repeat([]byte{byte(seq)}, n)
		mode := uint64(0)
		if seq == 2 {
			mode = 1
		}
		if e = s.Request(o.Sequence, 0, []Value{{Number: mode}, {Bytes: data}}); e != nil {
			t.Fatal(e)
		}
		if e = s.Request(o.Sequence, 1, []Value{{Number: uint64(seq)}}); e != nil {
			t.Fatal(e)
		}
		if st := s.Status(); st.PendingRequestBytes != 16+n || st.State != 0 || st.Completed != uint64(seq) {
			t.Fatal(st)
		}
		h := sha256.Sum256(data)
		if e = s.Response(o.Sequence, 0, 200, []Value{{Bytes: h[:]}}); e != nil {
			t.Fatal(e)
		}
		if st := s.Status(); st.State != 0 || st.Completed != uint64(seq) || st.PendingRequestBytes != 8 {
			t.Fatal("partial branch commit", st)
		}
		response := bytes.Repeat([]byte{byte(seq + 9)}, seq)
		if e = s.Response(o.Sequence, 1, 200, []Value{{Bytes: response}}); e != nil {
			t.Fatal(e)
		}
		wantState := 0
		if seq == 2 {
			wantState = 1
		}
		h = sha256.Sum256(response)
		if st := s.Status(); st.State != wantState || st.Completed != uint64(seq+1) || !bytes.Equal(st.Registers[1].Bytes, h[:]) || st.PendingRequestBytes != 0 {
			t.Fatal(st)
		}
	}
	if _, e = s.Plan(); e == nil || !s.Status().Closed {
		t.Fatal("terminal state accepted")
	}
}

func TestBatchBudgetRejectsBadLengthsAndBranches(t *testing.T) {
	for _, name := range []string{"oversize", "unknown", "premature_exit", "wrong_count", "sibling_error", "cancel"} {
		t.Run(name, func(t *testing.T) {
			p, e := CompileBatch(budgetFixture(t).Model)
			if e != nil {
				t.Fatal(e)
			}
			s, e := NewBatchSession(context.Background(), p, [32]byte{1}, [32]byte{2})
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			o, e := s.Plan()
			if e != nil {
				t.Fatal(e)
			}
			values := []Value{{}, {Bytes: []byte{1}}}
			switch name {
			case "oversize":
				values[1].Bytes = make([]byte, o.Members[0].RequestSizes[1]+1)
			case "unknown":
				values[0].Number = 99
			case "premature_exit":
				values[0].Number = 1
			case "wrong_count":
				values = values[:1]
			}
			e = s.Request(0, 0, values)
			if name == "sibling_error" || name == "cancel" {
				if e != nil {
					t.Fatal(e)
				}
				h := sha256.Sum256(values[1].Bytes)
				if e = s.Response(0, 0, 200, []Value{{Bytes: h[:]}}); e != nil {
					t.Fatal(e)
				}
				if name == "cancel" {
					s.Close()
				} else {
					e = s.Request(0, 1, []Value{{Number: 10}})
					if e == nil {
						t.Fatal("bad sibling accepted")
					}
				}
			} else if e == nil {
				t.Fatal("invalid request accepted")
			}
			st := s.Status()
			if !st.Closed || st.State != 0 || st.Completed != 0 || st.Registers[0].Number != 0 || st.PendingRequestBytes != 0 || st.PendingWrites != 0 {
				t.Fatal(st)
			}
		})
	}
}

func TestBatchBudgetCompilerBounds(t *testing.T) {
	cases := []func(*BatchModel){
		func(m *BatchModel) { m.Version = 1 },
		func(m *BatchModel) { m.Stages[0].To = 1 },
		func(m *BatchModel) { m.Stages[0].Branch.Action = 2 },
		func(m *BatchModel) { m.Stages[0].Branch.Field = 1 },
		func(m *BatchModel) { m.Stages[0].Branch.Cases[1].Code = 0 },
		func(m *BatchModel) { m.Stages[0].Branch.Cases[1].To = 9 },
		func(m *BatchModel) { m.Stages[0].Branch.Cases[1].Checks[0].Left.Scope = "response" },
		func(m *BatchModel) { m.Stages[0].Actions[0].RequestMinimums[0].Min = 33 },
		func(m *BatchModel) { m.Stages[0].Actions[0].RequestMinimums[0].Min = -1 },
		func(m *BatchModel) { m.Stages[0].Actions[0].RequestMinimums[0].Field = 0 },
		func(m *BatchModel) {
			m.Stages[0].Actions[0].RequestMinimums = append(m.Stages[0].Actions[0].RequestMinimums, BatchMinimum{1, 0})
		},
		func(m *BatchModel) { m.Stages[0].Actions[1].Replies[0].Minimums[0].Field = 3 },
		func(m *BatchModel) { m.Stages[0].Branch.Cases[1].To = 0 },
	}
	for i, mutate := range cases {
		m := budgetFixture(t).Model
		mutate(&m)
		if _, e := CompileBatch(m); e == nil {
			t.Fatalf("invalid model %d accepted", i)
		}
	}
	// Unmarked fields keep their exact-size requirement, even in version 2.
	m := budgetFixture(t).Model
	m.Stages[0].Actions[0].RequestMinimums = nil
	p, e := CompileBatch(m)
	if e != nil {
		t.Fatal(e)
	}
	s, e := NewBatchSession(context.Background(), p, [32]byte{}, [32]byte{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Plan(); e != nil {
		t.Fatal(e)
	}
	if e = s.Request(0, 0, []Value{{}, {Bytes: []byte{1}}}); e == nil {
		t.Fatal("short unmarked field accepted")
	}
}

func FuzzBatchBudgetLifecycle(f *testing.F) {
	raw, e := os.ReadFile("../../testdata/batch-v2-budget.json")
	if e != nil {
		f.Fatal(e)
	}
	var v batchVector
	if e = json.Unmarshal(raw, &v); e != nil {
		f.Fatal(e)
	}
	p, e := CompileBatch(v.Model)
	if e != nil {
		f.Fatal(e)
	}
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{7, 1, 3, 2})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{1, 2, 3, 255})
	f.Fuzz(func(t *testing.T, ops []byte) {
		s, e := NewBatchSession(context.Background(), p, [32]byte{1}, [32]byte{2})
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		o, e := s.Plan()
		if e != nil {
			t.Fatal(e)
		}
		var data []byte
		for _, op := range ops[:min(len(ops), 64)] {
			st := s.Status()
			switch op % 7 {
			case 0:
				data = bytes.Repeat([]byte{op}, int(op)%40)
				s.Request(o.Sequence, 0, []Value{{Number: uint64(op/7) % 3}, {Bytes: data}})
			case 1:
				s.Request(o.Sequence, 1, []Value{{Number: st.Completed}})
			case 2:
				h := sha256.Sum256(data)
				s.Response(o.Sequence, 0, 200, []Value{{Bytes: h[:]}})
			case 3:
				s.Response(o.Sequence, 1, 200, []Value{{Bytes: bytes.Repeat([]byte{op}, int(op)%20)}})
			case 4:
				next, err := s.Plan()
				if err == nil {
					o = next
				}
			case 5:
				s.Close()
			case 6:
				s.Response(o.Sequence, 0, 999, nil)
			}
			next := s.Status()
			if next.Completed < st.Completed || next.Completed > 4 || next.Registers[0].Number != next.Completed || next.PendingRequestBytes > 48 || next.State < 0 || next.State > 1 {
				t.Fatal(next)
			}
			if next.Closed && (next.PendingRequestBytes != 0 || next.PendingWrites != 0) {
				t.Fatal(next)
			}
		}
	})
}
