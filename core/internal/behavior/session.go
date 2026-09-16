package behavior

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"time"
)

type Offer struct {
	Sequence      uint64
	Action        string
	RequestSizes  []int
	ResponseSizes []int
}
type Status struct {
	State        int
	Completed    uint64
	Closed       bool
	PendingBytes int
	Registers    []Value
}
type Session struct {
	mu                   sync.Mutex
	program              *Program
	seed, material       [32]byte
	state, phase, action int // idle, offered, requested, closed
	sequence             uint64
	registers            []Value
	offer                Offer
	request              []Value
	ctx                  context.Context
	cancel               context.CancelFunc
	stop                 func() bool
}

func cloneValues(vs []Value) []Value {
	copyOf := make([]Value, len(vs))
	for i, v := range vs {
		copyOf[i] = Value{Number: v.Number, Bytes: bytes.Clone(v.Bytes)}
	}
	return copyOf
}
func NewSession(p *Program, seed, material [32]byte) (*Session, error) {
	return NewSessionContext(context.Background(), p, seed, material)
}
func NewSessionContext(parent context.Context, p *Program, seed, material [32]byte) (*Session, error) {
	if p == nil || (p.model.Version != 1 && p.model.Version != 2) {
		return nil, errors.New("nil or uncompiled program")
	}
	if parent == nil {
		return nil, errors.New("nil context")
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	s := &Session{program: p, seed: seed, material: material}
	for _, r := range p.model.Registers {
		s.registers = append(s.registers, Value{Number: r.Initial.Number, Bytes: bytes.Clone(r.Initial.Bytes)})
	}
	s.ctx, s.cancel = context.WithTimeout(parent, time.Duration(p.model.LifetimeMS)*time.Millisecond)
	s.mu.Lock()
	s.stop = context.AfterFunc(s.ctx, func() { s.mu.Lock(); defer s.mu.Unlock(); s.fail(s.ctx.Err()) })
	s.mu.Unlock()
	return s, nil
}
func (s *Session) choose(domain byte, index, bound int) int {
	h := hmac.New(sha256.New, s.seed[:])
	h.Write([]byte("veil-behavior-prototype-"))
	h.Write([]byte{byte('0' + s.program.model.Version), 0})
	h.Write(s.program.id[:])
	h.Write(s.material[:])
	h.Write([]byte{domain, byte(s.state), byte(index)})
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], s.sequence)
	h.Write(seq[:])
	sum := h.Sum(nil)
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(bound))
}
func (s *Session) sizes(fields []Field, domain byte) []int {
	sizes := make([]int, len(fields))
	for i, f := range fields {
		switch f.Kind {
		case "u64":
			sizes[i] = 8
		case "digest":
			sizes[i] = 32
		case "bytes":
			sizes[i] = f.Min + s.choose(domain, i, f.Max-f.Min+1)
		}
	}
	return sizes
}
func (s *Session) fail(err error) error {
	s.phase = 3
	s.request = nil
	s.offer = Offer{}
	if s.stop != nil {
		s.stop()
	}
	s.cancel()
	return err
}
func (s *Session) Plan() (Offer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return Offer{}, s.fail(err)
	}
	if s.phase != 0 {
		return Offer{}, s.fail(errors.New("plan phase"))
	}
	weight := 0
	for _, a := range s.program.model.Actions {
		if a.From == s.state {
			weight += a.Weight
		}
	}
	if weight == 0 || s.sequence >= uint64(s.program.model.MaxTransactions) {
		return Offer{}, s.fail(errors.New("program complete"))
	}
	choice := s.choose('a', 0, weight)
	for i, a := range s.program.model.Actions {
		if a.From == s.state {
			if choice < a.Weight {
				s.action = i
				break
			}
			choice -= a.Weight
		}
	}
	a := s.program.model.Actions[s.action]
	s.offer = Offer{Sequence: s.sequence, Action: a.Name, RequestSizes: s.sizes(a.Request, 'q'), ResponseSizes: s.sizes(a.Response, 'r')}
	s.phase = 1
	if err := s.ctx.Err(); err != nil {
		return Offer{}, s.fail(err)
	}
	return Offer{Sequence: s.offer.Sequence, Action: s.offer.Action, RequestSizes: append([]int(nil), s.offer.RequestSizes...), ResponseSizes: append([]int(nil), s.offer.ResponseSizes...)}, nil
}
func (s *Session) eval(e Expr, response []Value) (Value, error) {
	var v Value
	switch e.Scope {
	case "constant":
		v.Number = e.Number
	case "register":
		v = s.registers[e.Index]
	case "request":
		v = s.request[e.Index]
	case "response":
		v = response[e.Index]
	}
	switch e.Op {
	case "inc":
		if v.Number == math.MaxUint64 {
			return Value{}, errors.New("integer overflow")
		}
		v.Number++
	case "sha256":
		sum := sha256.Sum256(v.Bytes)
		v = Value{Bytes: sum[:]}
	}
	return v, nil
}
func (s *Session) checks(checks []Equal, response []Value, requestOnly bool) error {
	for _, c := range checks {
		if requestOnly && (c.Left.Scope == "response" || c.Right.Scope == "response") {
			continue
		}
		left, err := s.eval(c.Left, response)
		if err != nil {
			return err
		}
		right, err := s.eval(c.Right, response)
		if err != nil {
			return err
		}
		equal := left.Number == right.Number && bytes.Equal(left.Bytes, right.Bytes)
		if equal == c.Not {
			return errors.New("transaction relation")
		}
	}
	return nil
}
func (s *Session) Request(sequence uint64, values []Value) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return s.fail(err)
	}
	if s.phase != 1 || sequence != s.sequence {
		return s.fail(errors.New("request phase"))
	}
	if err := validateValues(s.program.model.Actions[s.action].Request, s.offer.RequestSizes, values); err != nil {
		return s.fail(err)
	}
	s.request = cloneValues(values)
	if err := s.checks(s.program.model.Actions[s.action].Checks, nil, true); err != nil {
		return s.fail(err)
	}
	if err := s.ctx.Err(); err != nil {
		return s.fail(err)
	}
	s.phase = 2
	return nil
}
func (s *Session) Response(sequence uint64, values []Value) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return s.fail(err)
	}
	if s.phase != 2 || sequence != s.sequence {
		return s.fail(errors.New("response phase"))
	}
	a := s.program.model.Actions[s.action]
	if err := validateValues(a.Response, s.offer.ResponseSizes, values); err != nil {
		return s.fail(err)
	}
	if err := s.checks(a.Checks, values, false); err != nil {
		return s.fail(err)
	}
	to, assign := a.To, a.Assign
	if a.Results != nil {
		var selected *Outcome
		for i := range a.Results.Cases {
			if a.Results.Cases[i].Code == values[a.Results.Field].Number {
				selected = &a.Results.Cases[i]
				break
			}
		}
		if selected == nil {
			return s.fail(errors.New("unknown transaction result"))
		}
		if err := s.checks(selected.Checks, values, false); err != nil {
			return s.fail(err)
		}
		to, assign = selected.To, selected.Assign
	}
	// Evaluate against the same pre-commit snapshot. No partial register update
	// is visible if a later expression overflows or another check fails.
	next := cloneValues(s.registers)
	for _, set := range assign {
		v, err := s.eval(set.Value, values)
		if err != nil {
			return s.fail(err)
		}
		next[set.Register] = Value{Number: v.Number, Bytes: bytes.Clone(v.Bytes)}
	}
	if err := s.ctx.Err(); err != nil {
		return s.fail(err)
	}
	s.registers = next
	s.state = to
	s.sequence++
	s.request = nil
	s.offer = Offer{}
	s.phase = 0
	if s.sequence >= uint64(s.program.model.MaxTransactions) {
		s.phase = 3
	}
	nextAction := false
	for _, candidate := range s.program.model.Actions {
		nextAction = nextAction || candidate.From == s.state
	}
	if !nextAction {
		s.phase = 3
	}
	if s.phase == 3 {
		s.fail(errors.New("program complete"))
	}
	return nil
}
func (s *Session) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.fail(errors.New("closed")) }
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		s.fail(err)
	}
	status := Status{State: s.state, Completed: s.sequence, Closed: s.phase == 3, Registers: cloneValues(s.registers)}
	if s.phase == 2 {
		for _, n := range s.offer.RequestSizes {
			status.PendingBytes += n
		}
	}
	return status
}
