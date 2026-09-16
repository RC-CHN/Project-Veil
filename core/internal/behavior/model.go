// Package behavior implements bounded paired-transaction contracts. It does
// not implement a network transport or establish a normal-traffic population.
package behavior

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

const MaxBody = 512 << 10

type Value struct {
	Number uint64 `json:"number,omitempty"`
	Bytes  []byte `json:"bytes,omitempty"`
}
type Register struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Initial Value  `json:"initial"`
}
type Field struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Min  int    `json:"min,omitempty"`
	Max  int    `json:"max,omitempty"`
}
type Expr struct {
	Scope  string `json:"scope"` // register, request, response, constant
	Index  int    `json:"index,omitempty"`
	Number uint64 `json:"number,omitempty"`
	Op     string `json:"op,omitempty"` // empty, inc, sha256
}
type Equal struct {
	Left  Expr `json:"left"`
	Right Expr `json:"right"`
	Not   bool `json:"not,omitempty"`
}
type Assign struct {
	Register int  `json:"register"`
	Value    Expr `json:"value"`
}
type Action struct {
	Name     string   `json:"name"`
	From     int      `json:"from"`
	To       int      `json:"to"`
	Weight   int      `json:"weight"`
	Request  []Field  `json:"request"`
	Response []Field  `json:"response"`
	Checks   []Equal  `json:"checks"`
	Assign   []Assign `json:"assign"`
	Results  *Results `json:"results,omitempty"`
}
type Results struct {
	Field int       `json:"field"`
	Cases []Outcome `json:"cases"`
}
type Outcome struct {
	Name   string   `json:"name"`
	Code   uint64   `json:"code"`
	To     int      `json:"to"`
	Checks []Equal  `json:"checks"`
	Assign []Assign `json:"assign"`
}
type Model struct {
	Version         int        `json:"version"`
	States          int        `json:"states"`
	MaxTransactions int        `json:"max_transactions"`
	LifetimeMS      int        `json:"lifetime_ms"`
	Registers       []Register `json:"registers"`
	Actions         []Action   `json:"actions"`
}
type Program struct {
	model Model
	id    [32]byte
}

func (p *Program) ID() string { return hex.EncodeToString(p.id[:]) }

var safeName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func fieldsValid(fs []Field) bool {
	if len(fs) > 16 {
		return false
	}
	seen := map[string]bool{}
	size := 0
	for _, f := range fs {
		if !safeName.MatchString(f.Name) || seen[f.Name] {
			return false
		}
		seen[f.Name] = true
		switch f.Kind {
		case "u64":
			if f.Min != 0 || f.Max != 0 {
				return false
			}
			size += 8
		case "digest":
			if f.Min != 0 || f.Max != 0 {
				return false
			}
			size += 32
		case "bytes":
			if f.Min < 0 || f.Max < f.Min || f.Max > MaxBody {
				return false
			}
			size += f.Max
		default:
			return false
		}
	}
	return size <= MaxBody
}

func exprKind(m Model, a Action, e Expr) (string, error) {
	var kind string
	if e.Index < 0 || (e.Scope != "constant" && e.Number != 0) {
		return "", errors.New("invalid expression")
	}
	switch e.Scope {
	case "constant":
		if e.Index != 0 {
			return "", errors.New("constant index")
		}
		kind = "u64"
	case "register":
		if e.Index >= len(m.Registers) {
			return "", errors.New("register reference")
		}
		kind = m.Registers[e.Index].Kind
	case "request":
		if e.Index >= len(a.Request) {
			return "", errors.New("request reference")
		}
		kind = a.Request[e.Index].Kind
	case "response":
		if e.Index >= len(a.Response) {
			return "", errors.New("response reference")
		}
		kind = a.Response[e.Index].Kind
	default:
		return "", errors.New("expression scope")
	}
	switch e.Op {
	case "":
	case "inc":
		if kind != "u64" {
			return "", errors.New("increment type")
		}
	case "sha256":
		if kind != "bytes" {
			return "", errors.New("hash type")
		}
		kind = "digest"
	default:
		return "", errors.New("expression operation")
	}
	return kind, nil
}

func Compile(input Model) (*Program, error) {
	// Check global counts before copying caller-controlled slices.
	if (input.Version != 1 && input.Version != 2) || input.States < 2 || input.States > 16 || input.MaxTransactions < 1 || input.MaxTransactions > 4096 || input.LifetimeMS < 1 || input.LifetimeMS > 600000 || len(input.Registers) > 16 || len(input.Actions) < 1 || len(input.Actions) > 32 {
		return nil, errors.New("model bounds")
	}
	names := map[string]bool{}
	for _, r := range input.Registers {
		if !safeName.MatchString(r.Name) || names[r.Name] || (r.Kind != "u64" && r.Kind != "digest") {
			return nil, errors.New("register schema")
		}
		names[r.Name] = true
		if (r.Kind == "u64" && len(r.Initial.Bytes) != 0) || (r.Kind == "digest" && (len(r.Initial.Bytes) != 32 || r.Initial.Number != 0)) {
			return nil, errors.New("register initial value")
		}
	}
	names = map[string]bool{}
	for _, a := range input.Actions {
		if !safeName.MatchString(a.Name) || names[a.Name] || a.From < 0 || a.From >= input.States || a.To < 0 || a.To >= input.States || a.Weight < 1 || a.Weight > 256 || len(a.Checks) > 32 || len(a.Assign) > 16 || !fieldsValid(a.Request) || !fieldsValid(a.Response) {
			return nil, errors.New("action schema")
		}
		names[a.Name] = true
		if err := validateRelations(input, a, a.Checks); err != nil {
			return nil, err
		}
		if err := validateAssignments(input, a, a.Assign); err != nil {
			return nil, err
		}
		if a.Results != nil {
			r := a.Results
			if input.Version != 2 || a.To != 0 || len(a.Assign) != 0 || r.Field < 0 || r.Field >= len(a.Response) || a.Response[r.Field].Kind != "u64" || len(r.Cases) < 1 || len(r.Cases) > 4 {
				return nil, errors.New("result schema")
			}
			codes := map[uint64]bool{}
			labels := map[string]bool{}
			checks := len(a.Checks)
			for _, o := range r.Cases {
				if !safeName.MatchString(o.Name) || labels[o.Name] || codes[o.Code] || o.To < 0 || o.To >= input.States || len(o.Checks) > 32 || len(o.Assign) > 16 {
					return nil, errors.New("result case")
				}
				labels[o.Name] = true
				codes[o.Code] = true
				checks += len(o.Checks)
				if checks > 32 {
					return nil, errors.New("combined relation bound")
				}
				if err := validateRelations(input, a, o.Checks); err != nil {
					return nil, err
				}
				if err := validateAssignments(input, a, o.Assign); err != nil {
					return nil, err
				}
			}
		}
	}
	seen := make([]bool, input.States)
	seen[0] = true
	for range input.States {
		for _, a := range input.Actions {
			if seen[a.From] {
				if a.Results == nil {
					seen[a.To] = true
				} else {
					for _, o := range a.Results.Cases {
						seen[o.To] = true
					}
				}
			}
		}
	}
	for _, yes := range seen {
		if !yes {
			return nil, errors.New("unreachable state")
		}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var owned Model
	if err = json.Unmarshal(raw, &owned); err != nil {
		return nil, err
	}
	return &Program{model: owned, id: sha256.Sum256(raw)}, nil
}

func validateRelations(input Model, a Action, checks []Equal) error {
	for _, c := range checks {
		if c.Not && input.Version != 2 {
			return errors.New("inequality requires version 2")
		}
		left, err := exprKind(input, a, c.Left)
		if err != nil {
			return err
		}
		right, err := exprKind(input, a, c.Right)
		if err != nil {
			return err
		}
		if left != right {
			return errors.New("equality type mismatch")
		}
	}
	return nil
}
func validateAssignments(input Model, a Action, assign []Assign) error {
	assigned := map[int]bool{}
	for _, set := range assign {
		if set.Register < 0 || set.Register >= len(input.Registers) || assigned[set.Register] {
			return errors.New("assignment target")
		}
		assigned[set.Register] = true
		kind, err := exprKind(input, a, set.Value)
		if err != nil {
			return err
		}
		if kind != input.Registers[set.Register].Kind {
			return errors.New("assignment type mismatch")
		}
	}
	return nil
}

func validateValues(fields []Field, sizes []int, values []Value) error {
	if len(values) != len(fields) {
		return errors.New("field count")
	}
	for i, f := range fields {
		v := values[i]
		if f.Kind == "u64" {
			if len(v.Bytes) != 0 {
				return fmt.Errorf("field %d integer", i)
			}
		} else if v.Number != 0 || len(v.Bytes) != sizes[i] {
			return fmt.Errorf("field %d exact length", i)
		}
	}
	return nil
}
