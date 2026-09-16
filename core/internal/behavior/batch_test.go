package behavior

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

type batchVector struct {
	Model   BatchModel   `json:"model"`
	ModelID string       `json:"model_id"`
	Offers  []BatchOffer `json:"offers"`
}

func batchFixture(t *testing.T) batchVector {
	t.Helper()
	p, e := os.ReadFile("../../testdata/batch-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var v batchVector
	if e = json.Unmarshal(p, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func TestBatchPublicVectorsAndResponseOrder(t *testing.T) {
	v := batchFixture(t)
	p, e := CompileBatch(v.Model)
	if e != nil {
		t.Fatal(e)
	}
	if p.ID() != v.ModelID {
		t.Fatal(p.ID(), v.ModelID)
	}
	for _, reverse := range []bool{false, true} {
		s, e := NewBatchSession(context.Background(), p, [32]byte{1}, [32]byte{2})
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		o, e := s.Plan()
		if e != nil || !reflect.DeepEqual(o, v.Offers[0]) {
			t.Fatalf("offer: %#v %v", o, e)
		}
		payload := []byte(nil)
		publish := -1
		for i, a := range o.Members {
			var values []Value
			if a.Name == "publish" {
				publish = i
				payload = bytes.Repeat([]byte{9}, a.RequestSizes[0])
				values = []Value{{Bytes: payload}}
			} else {
				values = []Value{{Number: 3}}
			}
			if e = s.Request(o.Sequence, i, values); e != nil {
				t.Fatal(e)
			}
		}
		h := sha256.Sum256(payload)
		payload[0] ^= 1 // Request is owned by the session.
		order := []int{0, 1}
		if reverse {
			order = []int{1, 0}
		}
		for n, i := range order {
			values := []Value{{Number: 7}}
			if i == publish {
				values = []Value{{Bytes: h[:]}}
			}
			if e = s.Response(o.Sequence, i, 200, values); e != nil {
				t.Fatal(e)
			}
			if n == 0 {
				st := s.Status()
				if st.Completed != 0 || st.Registers[0].Number != 3 || st.Registers[1].Number != 7 {
					t.Fatal("partial commit", st)
				}
			}
		}
		st := s.Status()
		if st.Completed != 1 || st.Registers[0].Number != 4 || st.Registers[1].Number != 8 || !bytes.Equal(st.Registers[2].Bytes, h[:]) {
			t.Fatal(st)
		}
		o, e = s.Plan()
		if e != nil || !reflect.DeepEqual(o, v.Offers[1]) {
			t.Fatal(o, e)
		}
		if e = s.Request(o.Sequence, 0, []Value{{Bytes: h[:]}}); e != nil {
			t.Fatal(e)
		}
		if e = s.Response(o.Sequence, 0, 204, nil); e != nil {
			t.Fatal(e)
		}
		if s.Status().Completed != 2 {
			t.Fatal(s.Status())
		}
	}
}
func simpleBatch(t *testing.T, dependency bool) *BatchProgram {
	t.Helper()
	v := batchFixture(t)
	v.Model.Stages = v.Model.Stages[:1]
	v.Model.States = 2
	if dependency {
		v.Model.Stages[0].Actions[1].ResponseAfter = []int{0}
	}
	p, e := CompileBatch(v.Model)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func requestedBatch(t *testing.T, p *BatchProgram) (*BatchSession, BatchOffer, []byte) {
	t.Helper()
	s, e := NewBatchSession(context.Background(), p, [32]byte{1}, [32]byte{2})
	if e != nil {
		t.Fatal(e)
	}
	o, e := s.Plan()
	if e != nil {
		t.Fatal(e)
	}
	data := bytes.Repeat([]byte{9}, o.Members[0].RequestSizes[0])
	h := sha256.Sum256(data)
	if e = s.Request(0, 0, []Value{{Bytes: data}}); e != nil {
		t.Fatal(e)
	}
	if e = s.Request(0, 1, []Value{{Number: 3}}); e != nil {
		t.Fatal(e)
	}
	return s, o, h[:]
}
func TestBatchDependencyCancellationAndVariants(t *testing.T) {
	s, _, h := requestedBatch(t, simpleBatch(t, true))
	defer s.Close()
	if e := s.Response(0, 1, 304, nil); !errors.Is(e, ErrResponseNotReady) {
		t.Fatal(e)
	}
	ready := make(chan error, 1)
	go func() { ready <- s.WaitResponse(context.Background(), 0, 1) }()
	select {
	case <-ready:
		t.Fatal("dependency bypass")
	case <-time.After(time.Millisecond):
	}
	if e := s.Response(0, 0, 200, []Value{{Bytes: h}}); e != nil {
		t.Fatal(e)
	}
	if e := <-ready; e != nil {
		t.Fatal(e)
	}
	if e := s.Response(0, 1, 304, nil); e != nil {
		t.Fatal(e)
	}
	if s.Status().Registers[1].Number != 7 || s.Status().Completed != 1 {
		t.Fatal("304 assignment", s.Status())
	}
	s2, _, _ := requestedBatch(t, simpleBatch(t, true))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := s2.WaitResponse(ctx, 0, 1); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if st := s2.Status(); !st.Closed || st.PendingRequestBytes != 0 || st.PendingWrites != 0 || st.Completed != 0 {
		t.Fatal(st)
	}
}
func TestBatchFailureNeverPartiallyCommits(t *testing.T) {
	for _, failure := range []string{"wrong_reply", "unknown", "duplicate", "close"} {
		t.Run(failure, func(t *testing.T) {
			s, _, h := requestedBatch(t, simpleBatch(t, false))
			defer s.Close()
			if e := s.Response(0, 0, 200, []Value{{Bytes: h}}); e != nil {
				t.Fatal(e)
			}
			var e error
			switch failure {
			case "wrong_reply":
				e = s.Response(0, 1, 200, []Value{{Number: 99}})
			case "unknown":
				e = s.Response(0, 1, 503, nil)
			case "duplicate":
				e = s.Response(0, 0, 200, []Value{{Bytes: h}})
			case "close":
				s.Close()
				e = s.Response(0, 1, 304, nil)
			}
			if e == nil {
				t.Fatal("accepted failure")
			}
			st := s.Status()
			if !st.Closed || st.Completed != 0 || st.PendingWrites != 0 || st.PendingRequestBytes != 0 || st.Registers[0].Number != 3 {
				t.Fatal(st)
			}
		})
	}
}
func TestBatchCompileRejectsHazardsAndOwnsModel(t *testing.T) {
	v := batchFixture(t)
	for _, kind := range []string{"write_conflict", "cycle", "bad_dependency", "duplicate_code", "request_response_guard", "large_fields"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(v.Model)
			var m BatchModel
			json.Unmarshal(raw, &m)
			a := &m.Stages[0].Actions[0]
			other := &m.Stages[0].Actions[1]
			switch kind {
			case "write_conflict":
				other.Replies[0].Assign[0].Register = 0
			case "cycle":
				a.ResponseAfter = []int{1}
				other.ResponseAfter = []int{0}
			case "bad_dependency":
				a.ResponseAfter = []int{2}
			case "duplicate_code":
				other.Replies[1].Code = 200
			case "request_response_guard":
				a.Checks = []Equal{{Left: Expr{Scope: "response"}, Right: Expr{Scope: "response"}}}
			case "large_fields":
				a.Request[0].Max = MaxBody + 1
			}
			if _, e := CompileBatch(m); e == nil {
				t.Fatal("hazard accepted")
			}
		})
	}
	p, e := CompileBatch(v.Model)
	if e != nil {
		t.Fatal(e)
	}
	v.Model.Registers[0].Initial.Number = 123
	v.Model.Stages[0].Actions[0].Request[0].Max = 999
	s, e := NewBatchSession(context.Background(), p, [32]byte{1}, [32]byte{2})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	o, _ := s.Plan()
	if !reflect.DeepEqual(o, v.Offers[0]) || s.Status().Registers[0].Number != 3 {
		t.Fatal("model alias")
	}
	o.Members[0].RequestSizes[0] = 999
	if reflect.DeepEqual(o, v.Offers[0]) {
		t.Fatal("test mutation failed")
	}
}
func TestBatchConcurrentResponseAndLifetime(t *testing.T) {
	p := simpleBatch(t, false)
	for range 50 {
		s, _, h := requestedBatch(t, p)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); errs <- s.Response(0, 0, 200, []Value{{Bytes: h}}) }()
		go func() { defer wg.Done(); errs <- s.Response(0, 1, 304, nil) }()
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
		if s.Status().Completed != 1 {
			t.Fatal(s.Status())
		}
		s.Close()
	}
	v := batchFixture(t)
	v.Model.LifetimeMS = 10
	p, e := CompileBatch(v.Model)
	if e != nil {
		t.Fatal(e)
	}
	s, e := NewBatchSession(context.Background(), p, [32]byte{1}, [32]byte{2})
	if e != nil {
		t.Fatal(e)
	}
	o, _ := s.Plan()
	for i, a := range o.Members {
		if a.Name == "publish" {
			s.Request(0, i, []Value{{Bytes: make([]byte, a.RequestSizes[0])}})
		}
	}
	time.Sleep(20 * time.Millisecond)
	if st := s.Status(); !st.Closed || st.PendingRequestBytes != 0 {
		t.Fatal(st)
	}
}

func TestBatchMaximumPendingAndEmptyFieldAccounting(t *testing.T) {
	a := BatchAction{Name: "a", Request: []Field{{Name: "data", Kind: "bytes", Min: MaxBody, Max: MaxBody}}, Replies: []BatchReply{{Code: 204}}}
	c := a
	c.Name = "b"
	m := BatchModel{Version: 1, States: 2, MaxBatches: 1, LifetimeMS: 30000, Stages: []BatchStage{{Name: "pair", From: 0, To: 1, Weight: 1, Actions: []BatchAction{a, c}}}}
	p, e := CompileBatch(m)
	if e != nil {
		t.Fatal(e)
	}
	s, e := NewBatchSession(context.Background(), p, [32]byte{}, [32]byte{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.Plan()
	data := make([]byte, MaxBody)
	for i := range 2 {
		if e = s.Request(0, i, []Value{{Bytes: data}}); e != nil {
			t.Fatal(e)
		}
	}
	if s.Status().PendingRequestBytes != 2*MaxBody {
		t.Fatal(s.Status())
	}
	s.Close()
	if s.Status().PendingRequestBytes != 0 {
		t.Fatal("retained maximum requests")
	}
	m.Stages[0].Actions = m.Stages[0].Actions[:1]
	m.Stages[0].Actions[0].Request[0].Min = 0
	m.Stages[0].Actions[0].Request[0].Max = 0
	p, e = CompileBatch(m)
	if e != nil {
		t.Fatal(e)
	}
	s, e = NewBatchSession(context.Background(), p, [32]byte{}, [32]byte{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.Plan()
	if e = s.Request(0, 0, []Value{{}}); e != nil {
		t.Fatal(e)
	}
	if s.Status().PendingRequestBytes != 0 {
		t.Fatal("empty bytes counted as u64")
	}
}

func FuzzBatchLifecycle(f *testing.F) {
	raw, e := os.ReadFile("../../testdata/batch-v1.json")
	if e != nil {
		f.Fatal(e)
	}
	var v batchVector
	if e = json.Unmarshal(raw, &v); e != nil {
		f.Fatal(e)
	}
	v.Model.Stages = v.Model.Stages[:1]
	v.Model.States = 2
	v.Model.Stages[0].Actions[1].ResponseAfter = []int{0}
	p, e := CompileBatch(v.Model)
	if e != nil {
		f.Fatal(e)
	}
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{0, 1, 3, 2, 4})
	f.Add([]byte{0, 2, 1, 5})
	f.Add([]byte{7, 0, 1, 2})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 64 {
			return
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
		data := make([]byte, o.Members[0].RequestSizes[0])
		h := sha256.Sum256(data)
		var completed uint64
		for _, op := range operations {
			switch op % 8 {
			case 0:
				s.Request(0, 0, []Value{{Bytes: data}})
			case 1:
				s.Request(0, 1, []Value{{Number: 3}})
			case 2:
				s.Response(0, 0, 200, []Value{{Bytes: h[:]}})
			case 3:
				s.Response(0, 1, 304, nil)
			case 4:
				s.Response(0, 1, 200, []Value{{Number: 7}})
			case 5:
				s.Response(0, 1, 999, nil)
			case 6:
				s.Plan()
			case 7:
				s.Close()
			}
			st := s.Status()
			if st.Completed > 1 || st.Completed < completed || st.PendingRequestBytes > MaxBody+8 || (st.Closed && (st.PendingRequestBytes != 0 || st.PendingWrites != 0)) {
				t.Fatal(st)
			}
			if st.Completed == 0 && (st.Registers[0].Number != 3 || st.Registers[1].Number != 7) {
				t.Fatal("partial commit", st)
			}
			if st.Completed == 1 && (st.Registers[0].Number != 4 || (st.Registers[1].Number != 7 && st.Registers[1].Number != 8)) {
				t.Fatal(st)
			}
			completed = st.Completed
		}
	})
}
