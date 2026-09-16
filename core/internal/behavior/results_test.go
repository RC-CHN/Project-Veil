package behavior

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
)

type resultFixture struct {
	Model Model  `json:"model"`
	ID    string `json:"model_id"`
	Steps []struct {
		Sequence uint64  `json:"sequence"`
		Action   string  `json:"action"`
		Request  []Value `json:"request"`
		Response []Value `json:"response"`
		State    int     `json:"state"`
		Head     string  `json:"head_hex"`
		Sample   int     `json:"sample_1000"`
	} `json:"steps"`
}

func loadResultFixture(t testing.TB) resultFixture {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/behavior-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var f resultFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}
func TestIndependentResultVectors(t *testing.T) {
	f := loadResultFixture(t)
	checkResultVectors(t, f)
}
func TestIndependentBootstrapVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/behavior-v2-bootstrap.json")
	if err != nil {
		t.Fatal(err)
	}
	var f resultFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	checkResultVectors(t, f)
}
func checkResultVectors(t *testing.T, f resultFixture) {
	t.Helper()
	p, err := Compile(f.Model)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID() != f.ID {
		t.Fatal("v2 model ID mismatch", p.ID())
	}
	a, b := session(t, f.Model), session(t, f.Model)
	for _, step := range f.Steps {
		for _, s := range []*Session{a, b} {
			if got := s.choose('a', 0, 1000); got != step.Sample {
				t.Fatal("v2 independent selection domain", got, step.Sample)
			}
			o, err := s.Plan()
			if err != nil || o.Action != step.Action || o.Sequence != step.Sequence {
				t.Fatal("offer", o, err)
			}
			if err = s.Request(o.Sequence, step.Request); err != nil {
				t.Fatal(err)
			}
			if err = s.Response(o.Sequence, step.Response); err != nil {
				t.Fatal(err)
			}
			status := s.Status()
			if status.State != step.State || status.Completed != step.Sequence+1 || status.Closed || status.PendingBytes != 0 || hex.EncodeToString(status.Registers[0].Bytes) != step.Head {
				t.Fatal("result transition", status)
			}
		}
		if !reflect.DeepEqual(a.Status(), b.Status()) {
			t.Fatal("paired result state diverged")
		}
	}
}

func TestResultOwnershipAndRealEdges(t *testing.T) {
	f := loadResultFixture(t)
	s := session(t, f.Model)
	f.Model.Actions[0].Results.Field = 1
	f.Model.Actions[0].Results.Cases[0].Code = 99
	f.Model.Actions[0].Results.Cases[0].Checks[0].Left.Index = 99
	o, err := s.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Request(o.Sequence, f.Steps[0].Request); err != nil {
		t.Fatal(err)
	}
	if err = s.Response(o.Sequence, f.Steps[0].Response); err != nil {
		t.Fatal("caller modified compiled result schema", err)
	}
	m := loadResultFixture(t).Model
	for i := 0; i < 2; i++ {
		for j := range m.Actions[i].Results.Cases {
			m.Actions[i].Results.Cases[j].To = 1
		}
	}
	if _, err := Compile(m); err == nil {
		t.Fatal("unreachable refresh state accepted")
	}
}
func TestResultRejectionAndAtomicity(t *testing.T) {
	for _, kind := range []string{"unknown", "false_conflict", "wrong_updated_head", "overflow", "unchosen_error"} {
		t.Run(kind, func(t *testing.T) {
			f := loadResultFixture(t)
			if kind == "overflow" || kind == "unchosen_error" {
				f.Model.Registers = append(f.Model.Registers, Register{Name: "counter", Kind: "u64"})
				i := 0
				if kind == "unchosen_error" {
					i = 1
				}
				f.Model.Actions[0].Results.Cases[i].Assign = append(f.Model.Actions[0].Results.Cases[i].Assign, Assign{Register: 1, Value: Expr{Scope: "constant", Number: math.MaxUint64, Op: "inc"}})
			}
			s := session(t, f.Model)
			o, _ := s.Plan()
			q, r := f.Steps[0].Request, f.Steps[0].Response
			before := s.Status().Registers
			switch kind {
			case "unknown":
				r[0].Number = 99
			case "false_conflict":
				r[0].Number = 1
				r[1].Bytes = bytes.Clone(q[0].Bytes)
			case "wrong_updated_head":
				r[1].Bytes[0] ^= 1
			}
			if err := s.Request(o.Sequence, q); err != nil {
				t.Fatal(err)
			}
			err := s.Response(o.Sequence, r)
			if kind == "unchosen_error" {
				if err != nil {
					t.Fatal("unchosen assignment evaluated", err)
				}
				return
			}
			if err == nil || !s.Status().Closed || s.Status().Completed != 0 || !reflect.DeepEqual(before, s.Status().Registers) || s.Status().PendingBytes != 0 {
				t.Fatal("invalid result committed", err)
			}
		})
	}
}
func TestResultSchemaBoundsAndLegacyRejection(t *testing.T) {
	mutations := []func(*Model){
		func(m *Model) { m.Version = 1 }, func(m *Model) { m.Actions[0].Results.Cases[1].Code = 0 }, func(m *Model) { m.Actions[0].Results.Cases[1].Name = "updated" },
		func(m *Model) { m.Actions[0].Results.Field = 1 }, func(m *Model) { m.Actions[0].Results.Field = -1 }, func(m *Model) { m.Actions[0].Results.Cases = nil },
		func(m *Model) { m.Actions[0].To = 1 }, func(m *Model) { m.Actions[0].Assign = []Assign{{Register: 0, Value: ref("request", 0, "")}} },
		func(m *Model) { m.Actions[0].Results.Cases[0].To = 3 }, func(m *Model) { m.Actions[0].Results.Cases[0].Checks = make([]Equal, 32) },
		func(m *Model) { m.Actions[0].Results.Cases[0].Assign = make([]Assign, 17) }, func(m *Model) { m.Actions[0].Results.Cases = make([]Outcome, 5) },
		func(m *Model) {
			m.Actions[0].Results.Cases[0].Assign = append(m.Actions[0].Results.Cases[0].Assign, m.Actions[0].Results.Cases[0].Assign[0])
		},
	}
	for i, mutate := range mutations {
		m := loadResultFixture(t).Model
		mutate(&m)
		if _, err := Compile(m); err == nil {
			t.Fatal("bad result schema accepted", i)
		}
	}
	m := objectModel()
	m.Actions[0].Checks[0].Not = true
	if _, err := Compile(m); err == nil {
		t.Fatal("v1 accepted inequality")
	}
}
func FuzzConditionalResults(f *testing.F) {
	fixture := loadResultFixture(f)
	raw, _ := json.Marshal(fixture.Model)
	f.Add(raw, uint64(0))
	f.Add(raw, uint64(1))
	f.Add(raw, uint64(99))
	f.Fuzz(func(t *testing.T, raw []byte, code uint64) {
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
		s, err := NewSession(p, [32]byte{1}, [32]byte{2})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
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
		if a.Results != nil {
			r[a.Results.Field].Number = code
		}
		if s.Request(o.Sequence, q) == nil {
			before := s.Status().Registers
			if s.Response(o.Sequence, r) != nil && !reflect.DeepEqual(before, s.Status().Registers) {
				t.Fatal("partial failed commit")
			}
		}
	})
}
