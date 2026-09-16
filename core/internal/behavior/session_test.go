package behavior

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func ref(scope string, index int, op string) Expr { return Expr{Scope: scope, Index: index, Op: op} }
func objectModel() Model {
	a := Action{Name: "create", From: 0, To: 1, Weight: 1,
		Request:  []Field{{Name: "expected", Kind: "digest"}, {Name: "revision", Kind: "u64"}, {Name: "object", Kind: "bytes", Min: 31, Max: 89}},
		Response: []Field{{Name: "stored", Kind: "digest"}, {Name: "revision", Kind: "u64"}, {Name: "independent_object", Kind: "bytes", Min: 17, Max: 53}, {Name: "independent_digest", Kind: "digest"}},
		Checks: []Equal{
			{Left: ref("request", 0, ""), Right: ref("register", 0, "")},
			{Left: ref("request", 1, ""), Right: ref("register", 1, "")},
			{Left: ref("response", 0, ""), Right: ref("request", 2, "sha256")},
			{Left: ref("response", 1, ""), Right: ref("request", 1, "inc")},
			{Left: ref("response", 3, ""), Right: ref("response", 2, "sha256")},
		}, Assign: []Assign{{0, ref("response", 0, "")}, {1, ref("response", 1, "")}, {2, ref("response", 3, "")}}}
	b := a
	b.Name = "replace"
	b.From = 1
	c := a
	c.Name = "alternate"
	c.From = 1
	c.Weight = 2
	c.Request = append([]Field(nil), a.Request...)
	c.Request[2].Min = 90
	c.Request[2].Max = 128
	return Model{Version: 1, States: 2, MaxTransactions: 6, LifetimeMS: 10000, Registers: []Register{{Name: "head", Kind: "digest", Initial: Value{Bytes: make([]byte, 32)}}, {Name: "revision", Kind: "u64"}, {Name: "remote_head", Kind: "digest", Initial: Value{Bytes: make([]byte, 32)}}}, Actions: []Action{a, b, c}}
}
func session(t *testing.T, m Model) *Session {
	t.Helper()
	p, err := Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(p, [32]byte{1}, [32]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func values(t *testing.T, s *Session, o Offer) ([]Value, []Value) {
	t.Helper()
	state := s.Status()
	up := bytes.Repeat([]byte{11}, o.RequestSizes[2])
	down := bytes.Repeat([]byte{29}, o.ResponseSizes[2])
	u := sha256.Sum256(up)
	d := sha256.Sum256(down)
	return []Value{{Bytes: bytes.Clone(state.Registers[0].Bytes)}, {Number: state.Registers[1].Number}, {Bytes: up}}, []Value{{Bytes: u[:]}, {Number: state.Registers[1].Number + 1}, {Bytes: down}, {Bytes: d[:]}}
}

func TestPairedProgramAndAtomicObjects(t *testing.T) {
	a, b := session(t, objectModel()), session(t, objectModel())
	for step := 0; step < 6; step++ {
		oa, err := a.Plan()
		if err != nil {
			t.Fatal(err)
		}
		ob, err := b.Plan()
		if err != nil || !reflect.DeepEqual(oa, ob) {
			t.Fatal("paired offers differ", err)
		}
		q, r := values(t, a, oa)
		// Different caller chunk assembly cannot affect the already chosen
		// action or lengths. This is an API invariant, not a wire experiment.
		qb := cloneValues(q)
		qb[2].Bytes = nil
		for _, v := range q[2].Bytes {
			qb[2].Bytes = append(qb[2].Bytes, v)
		}
		for _, x := range []struct {
			s *Session
			q []Value
		}{{a, q}, {b, qb}} {
			if err = x.s.Request(oa.Sequence, x.q); err != nil {
				t.Fatal(err)
			}
			before := x.s.Status()
			if before.Completed != uint64(step) || before.PendingBytes != 40+len(q[2].Bytes) {
				t.Fatal("premature commit or pending bound")
			}
			if err = x.s.Response(oa.Sequence, r); err != nil {
				t.Fatal(err)
			}
			got := x.s.Status()
			if got.Completed != uint64(step+1) || got.PendingBytes != 0 || !bytes.Equal(got.Registers[0].Bytes, r[0].Bytes) || !bytes.Equal(got.Registers[2].Bytes, r[3].Bytes) {
				t.Fatal("paired object commit")
			}
		}
		if !reflect.DeepEqual(a.Status(), b.Status()) {
			t.Fatal("peer states diverged")
		}
	}
	if !a.Status().Closed {
		t.Fatal("transaction lifetime not enforced")
	}
	if _, err := a.Plan(); err == nil {
		t.Fatal("expired program accepted work")
	}
}

func TestContractFailureLeavesRegistersUnchanged(t *testing.T) {
	for _, kind := range []string{"stale", "request_size", "response_size", "receipt", "response_object", "response_revision", "wrong_sequence", "old_response", "overflow_effect", "overflow_check"} {
		t.Run(kind, func(t *testing.T) {
			m := objectModel()
			if kind == "overflow_effect" {
				m.Actions[0].Assign = append(m.Actions[0].Assign[:1], Assign{1, Expr{Scope: "constant", Number: math.MaxUint64, Op: "inc"}})
			}
			if kind == "overflow_check" {
				m.Registers[1].Initial.Number = math.MaxUint64
			}
			s := session(t, m)
			o, _ := s.Plan()
			q, r := values(t, s, o)
			before := s.Status().Registers
			if kind == "stale" {
				q[0].Bytes[0] = 1
			}
			if kind == "request_size" {
				q[2].Bytes = append(q[2].Bytes, 1)
			}
			err := s.Request(o.Sequence, q)
			if kind == "stale" || kind == "request_size" {
				if err == nil {
					t.Fatal("bad request accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "response_size":
					r[2].Bytes = append(r[2].Bytes, 1)
				case "receipt":
					r[0].Bytes[0] ^= 1
				case "response_object":
					r[2].Bytes[0] ^= 1
				case "response_revision":
					r[1].Number++
				case "wrong_sequence":
					o.Sequence++
				case "old_response":
					if err = s.Response(o.Sequence, r); err != nil {
						t.Fatal(err)
					}
					old := o.Sequence
					o, _ = s.Plan()
					q, r = values(t, s, o)
					before = s.Status().Registers
					if err = s.Request(o.Sequence, q); err != nil {
						t.Fatal(err)
					}
					o.Sequence = old
				}
				if err = s.Response(o.Sequence, r); err == nil {
					t.Fatal("invalid response accepted")
				}
			}
			got := s.Status()
			if !got.Closed || got.PendingBytes != 0 || !reflect.DeepEqual(got.Registers, before) {
				t.Fatal("failure partially committed or retained request")
			}
		})
	}
}

func TestOwnedModelOfferRequestAndRegisters(t *testing.T) {
	m := objectModel()
	s := session(t, m)
	m.Actions[0].Request[2].Max = 1
	m.Registers[0].Initial.Bytes[0] = 99
	o, err := s.Plan()
	if err != nil {
		t.Fatal(err)
	}
	q, r := values(t, s, o)
	o.RequestSizes[2] = 1
	o.Action = "tampered"
	if err = s.Request(o.Sequence, q); err != nil {
		t.Fatal("caller changed compiled model/offer", err)
	}
	q[2].Bytes[0] ^= 1
	if err = s.Response(o.Sequence, r); err != nil {
		t.Fatal("caller mutated accepted request", err)
	}
	r[0].Bytes[0] ^= 1
	status := s.Status()
	original := bytes.Clone(status.Registers[0].Bytes)
	status.Registers[0].Bytes[0] ^= 1
	if !bytes.Equal(s.Status().Registers[0].Bytes, original) {
		t.Fatal("status or response aliases registers")
	}
}

func TestCompileRejectsUnboundedOrIllTypedPrograms(t *testing.T) {
	mutations := []func(*Model){
		func(m *Model) { m.Version = 3 }, func(m *Model) { m.States = 17 }, func(m *Model) { m.States = 3 }, func(m *Model) { m.MaxTransactions = 4097 },
		func(m *Model) { m.Actions[0].Request[2].Max = MaxBody }, func(m *Model) { m.Actions[0].Request[2].Min = -1 }, func(m *Model) { m.Actions[0].Request[2].Kind = "code" },
		func(m *Model) { m.Actions[0].Checks[0].Left.Index = 16 }, func(m *Model) { m.Actions[0].Checks[0].Left.Op = "inc" }, func(m *Model) { m.Actions[0].Checks[0].Left.Op = "execute" },
		func(m *Model) { m.Actions[0].Assign = append(m.Actions[0].Assign, m.Actions[0].Assign[0]) }, func(m *Model) { m.Actions[0].Assign[0].Register = 1 },
		func(m *Model) { m.Registers[0].Initial.Bytes = make([]byte, 33) }, func(m *Model) { m.Actions[0].Weight = 0 }, func(m *Model) { m.Actions[0].From = -1 },
		func(m *Model) { m.Actions = make([]Action, 33) }, func(m *Model) { m.Registers = make([]Register, 17) }, func(m *Model) { m.Actions[0].Checks = make([]Equal, 33) },
		func(m *Model) { m.Actions[0].Assign = make([]Assign, 17) }, func(m *Model) { m.Actions[0].Response = make([]Field, 17) },
	}
	for i, mutate := range mutations {
		m := objectModel()
		mutate(&m)
		if _, err := Compile(m); err == nil {
			t.Fatal("invalid model accepted", i)
		}
	}
}

func TestSelectionMaterialsAndCloseRace(t *testing.T) {
	p, _ := Compile(objectModel())
	seen := map[int]bool{}
	for i := byte(0); i < 32; i++ {
		s, _ := NewSession(p, [32]byte{1}, [32]byte{i})
		defer s.Close()
		o, _ := s.Plan()
		seen[o.RequestSizes[2]] = true
	}
	if len(seen) < 2 {
		t.Fatal("execution material ignored")
	}
	s := session(t, objectModel())
	o, _ := s.Plan()
	q, _ := values(t, s, o)
	if err := s.Request(o.Sequence, q); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { s.Close(); s.Status() })
	}
	wg.Wait()
	if !s.Status().Closed || s.Status().PendingBytes != 0 {
		t.Fatal("cancel retains snapshot")
	}
}

func TestLifetimeAndParentCancellation(t *testing.T) {
	if _, err := NewSession(&Program{}, [32]byte{}, [32]byte{}); err == nil {
		t.Fatal("uncompiled program")
	}
	for _, parentCancel := range []bool{false, true} {
		m := objectModel()
		m.LifetimeMS = 20
		if parentCancel {
			m.LifetimeMS = 600000
		}
		p, err := Compile(m)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		s, err := NewSessionContext(ctx, p, [32]byte{1}, [32]byte{2})
		if err != nil {
			t.Fatal(err)
		}
		o, err := s.Plan()
		if err != nil {
			t.Fatal(err)
		}
		q, r := values(t, s, o)
		if err = s.Request(o.Sequence, q); err != nil {
			t.Fatal(err)
		}
		before := s.Status().Registers
		if parentCancel {
			cancel()
		}
		select {
		case <-s.ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("lifetime not bounded")
		}
		// Inspect under the mutex without Status triggering lazy cleanup, to
		// establish that the idle cancellation callback releases the snapshot.
		until := time.Now().Add(time.Second)
		for {
			s.mu.Lock()
			released := s.phase == 3 && s.request == nil
			s.mu.Unlock()
			if released {
				break
			}
			if time.Now().After(until) {
				t.Fatal("idle callback retained snapshot")
			}
			time.Sleep(time.Millisecond)
		}
		if err = s.Response(o.Sequence, r); err == nil {
			t.Fatal("response committed after expiry")
		}
		if !reflect.DeepEqual(before, s.Status().Registers) {
			t.Fatal("expiry changed registers")
		}
		cancel()
		s.Close()
	}
	for _, ms := range []int{0, -1, 600001} {
		m := objectModel()
		m.LifetimeMS = ms
		if _, err := Compile(m); err == nil {
			t.Fatal("lifetime bounds", ms)
		}
	}
}

func FuzzCompileAndLifecycle(f *testing.F) {
	raw, _ := json.Marshal(objectModel())
	f.Add(raw)
	minimal, _ := json.Marshal(Model{Version: 1, States: 2, MaxTransactions: 1, LifetimeMS: 1000, Actions: []Action{{Name: "finish", From: 0, To: 1, Weight: 1}}})
	f.Add(minimal)
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		var m Model
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		p, err := Compile(m)
		if err != nil {
			return
		}
		s, err := NewSession(p, [32]byte{}, [32]byte{})
		if err != nil {
			t.Fatal(err)
		}
		o, err := s.Plan()
		if err != nil {
			return
		}
		a := p.model.Actions[s.action]
		q := make([]Value, len(a.Request))
		r := make([]Value, len(a.Response))
		for i, field := range a.Request {
			if field.Kind != "u64" {
				q[i].Bytes = make([]byte, o.RequestSizes[i])
			}
		}
		for i, field := range a.Response {
			if field.Kind != "u64" {
				r[i].Bytes = make([]byte, o.ResponseSizes[i])
			}
		}
		if s.Request(o.Sequence, q) == nil {
			s.Response(o.Sequence, r)
		}
		s.Close()
		if s.Status().PendingBytes != 0 {
			t.Fatal("retained request")
		}
	})
}

func TestIndependentPythonVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/behavior-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Model    Model  `json:"model"`
		ID       string `json:"canonical_model_sha256"`
		Seed     string `json:"seed_hex"`
		Material string `json:"material_hex"`
		Steps    []struct {
			Sequence uint64 `json:"sequence"`
			Action   string `json:"action"`
			Request  []int  `json:"request_sizes"`
			Response []int  `json:"response_sizes"`
			Head     string `json:"head"`
			Remote   string `json:"remote_head"`
			Revision uint64 `json:"revision"`
		} `json:"steps"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	p, err := Compile(fixture.Model)
	if err != nil || p.ID() != fixture.ID {
		t.Fatal("model identity", err)
	}
	seed, err := hex.DecodeString(fixture.Seed)
	if err != nil || len(seed) != 32 {
		t.Fatal("seed vector")
	}
	material, err := hex.DecodeString(fixture.Material)
	if err != nil || len(material) != 32 {
		t.Fatal("material vector")
	}
	s, err := NewSession(p, [32]byte(seed), [32]byte(material))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, step := range fixture.Steps {
		o, err := s.Plan()
		if err != nil {
			t.Fatal(err)
		}
		if o.Sequence != step.Sequence || o.Action != step.Action || !reflect.DeepEqual(o.RequestSizes, step.Request) || !reflect.DeepEqual(o.ResponseSizes, step.Response) {
			t.Fatal("independent offer mismatch", o, step)
		}
		q, r := values(t, s, o)
		if err = s.Request(o.Sequence, q); err != nil {
			t.Fatal(err)
		}
		if err = s.Response(o.Sequence, r); err != nil {
			t.Fatal(err)
		}
		v := s.Status().Registers
		if hex.EncodeToString(v[0].Bytes) != step.Head || hex.EncodeToString(v[2].Bytes) != step.Remote || v[1].Number != step.Revision {
			t.Fatal("independent committed state mismatch")
		}
	}
}

func TestPhasesTerminalStateAndMaximumSnapshot(t *testing.T) {
	for _, phase := range []string{"request_without_plan", "response_without_request", "duplicate_plan", "wrong_request_sequence", "duplicate_response"} {
		t.Run(phase, func(t *testing.T) {
			s := session(t, objectModel())
			if phase == "request_without_plan" {
				if s.Request(0, nil) == nil {
					t.Fatal("request accepted without plan")
				}
				return
			}
			o, _ := s.Plan()
			q, r := values(t, s, o)
			if phase == "response_without_request" {
				if s.Response(o.Sequence, r) == nil {
					t.Fatal("early response accepted")
				}
				return
			}
			if phase == "duplicate_plan" {
				if _, err := s.Plan(); err == nil {
					t.Fatal("duplicate plan accepted")
				}
				return
			}
			if phase == "wrong_request_sequence" {
				if s.Request(o.Sequence+1, q) == nil {
					t.Fatal("wrong request sequence")
				}
				return
			}
			if err := s.Request(o.Sequence, q); err != nil {
				t.Fatal(err)
			}
			if err := s.Response(o.Sequence, r); err != nil {
				t.Fatal(err)
			}
			before := s.Status()
			if s.Response(o.Sequence, r) == nil {
				t.Fatal("duplicate response accepted")
			}
			if !reflect.DeepEqual(before.Registers, s.Status().Registers) {
				t.Fatal("duplicate changed state")
			}
		})
	}
	m := objectModel()
	m.Actions = m.Actions[:1]
	m.Actions[0].Request[2].Min = MaxBody - 40
	m.Actions[0].Request[2].Max = MaxBody - 40
	m.Actions[0].Response[2].Min = MaxBody - 72
	m.Actions[0].Response[2].Max = MaxBody - 72
	s := session(t, m)
	o, _ := s.Plan()
	q, r := values(t, s, o)
	if err := s.Request(o.Sequence, q); err != nil {
		t.Fatal(err)
	}
	if s.Status().PendingBytes != MaxBody {
		t.Fatal("maximum snapshot accounting")
	}
	if err := s.Response(o.Sequence, r); err != nil {
		t.Fatal(err)
	}
	if !s.Status().Closed || s.Status().PendingBytes != 0 {
		t.Fatal("terminal state retained work")
	}
}
