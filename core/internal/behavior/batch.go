package behavior

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// BatchModel is a separate prototype format; Model v1/v2 remains unchanged.
type BatchModel struct {
	Version    int          `json:"version"`
	States     int          `json:"states"`
	MaxBatches int          `json:"max_batches"`
	LifetimeMS int          `json:"lifetime_ms"`
	Registers  []Register   `json:"registers"`
	Stages     []BatchStage `json:"stages"`
}
type BatchStage struct {
	Name    string        `json:"name"`
	From    int           `json:"from"`
	To      int           `json:"to"`
	Weight  int           `json:"weight"`
	Actions []BatchAction `json:"actions"`
	Branch  *BatchBranch  `json:"branch,omitempty"`
}
type BatchAction struct {
	Name            string         `json:"name"`
	Request         []Field        `json:"request"`
	Checks          []Equal        `json:"checks"`
	Replies         []BatchReply   `json:"replies"`
	ResponseAfter   []int          `json:"response_after"`
	RequestMinimums []BatchMinimum `json:"request_minimums,omitempty"`
}
type BatchReply struct {
	Code     uint64         `json:"code"`
	Fields   []Field        `json:"fields"`
	Checks   []Equal        `json:"checks"`
	Assign   []Assign       `json:"assign"`
	Minimums []BatchMinimum `json:"minimums,omitempty"`
}
type BatchProgram struct {
	model BatchModel
	id    [32]byte
}

func (p *BatchProgram) ID() string { return hex.EncodeToString(p.id[:]) }

func CompileBatch(input BatchModel) (*BatchProgram, error) {
	if (input.Version != 1 && input.Version != 2) || input.States < 2 || input.States > 16 || input.MaxBatches < 1 || input.MaxBatches > 4096 || input.LifetimeMS < 1 || input.LifetimeMS > 600000 || len(input.Registers) > 16 || len(input.Stages) < 1 || len(input.Stages) > 32 {
		return nil, errors.New("batch model bounds")
	}
	names := map[string]bool{}
	for _, stage := range input.Stages {
		if !safeName.MatchString(stage.Name) || names[stage.Name] || stage.From < 0 || stage.From >= input.States || stage.To < 0 || stage.To >= input.States || stage.Weight < 1 || stage.Weight > 256 || len(stage.Actions) < 1 || len(stage.Actions) > 2 {
			return nil, errors.New("batch stage schema")
		}
		names[stage.Name] = true
		if e := validateBatchBranch(input, stage); e != nil {
			return nil, e
		}
		actionNames := map[string]bool{}
		written := map[int]int{}
		for i, a := range stage.Actions {
			if !safeName.MatchString(a.Name) || actionNames[a.Name] || !fieldsValid(a.Request) || len(a.Checks) > 32 || len(a.Replies) < 1 || len(a.Replies) > 4 || len(a.ResponseAfter) > 1 {
				return nil, errors.New("batch action schema")
			}
			actionNames[a.Name] = true
			if e := validateBatchMinimums(input.Version, a.Request, a.RequestMinimums); e != nil {
				return nil, e
			}
			for _, dep := range a.ResponseAfter {
				if dep < 0 || dep >= len(stage.Actions) || dep == i {
					return nil, errors.New("response dependency")
				}
			}
			for _, check := range a.Checks {
				if check.Left.Scope == "response" || check.Right.Scope == "response" {
					return nil, errors.New("request guard references response")
				}
			}
			codes := map[uint64]bool{}
			checks := len(a.Checks)
			for _, reply := range a.Replies {
				if codes[reply.Code] || !fieldsValid(reply.Fields) || len(reply.Checks) > 32 || len(reply.Assign) > 16 {
					return nil, errors.New("batch reply schema")
				}
				codes[reply.Code] = true
				if e := validateBatchMinimums(input.Version, reply.Fields, reply.Minimums); e != nil {
					return nil, e
				}
				checks += len(reply.Checks)
				if checks > 32 {
					return nil, errors.New("batch combined checks")
				}
				// Reuse the established expression/register/type validator. This
				// temporary program is never executed and its ID is not a wire ID.
				model := Model{Version: 2, States: 2, MaxTransactions: 1, LifetimeMS: input.LifetimeMS, Registers: input.Registers, Actions: []Action{{Name: a.Name, From: 0, To: 1, Weight: 1, Request: a.Request, Response: reply.Fields, Checks: append(append([]Equal(nil), a.Checks...), reply.Checks...), Assign: reply.Assign}}}
				if _, e := Compile(model); e != nil {
					return nil, e
				}
				for _, set := range reply.Assign {
					if other, ok := written[set.Register]; ok && other != i {
						return nil, errors.New("overlapping batch write sets")
					}
					written[set.Register] = i
				}
			}
		}
		if len(stage.Actions) == 2 && len(stage.Actions[0].ResponseAfter) != 0 && len(stage.Actions[1].ResponseAfter) != 0 {
			return nil, errors.New("response dependency cycle")
		}
	}
	reached := make([]bool, input.States)
	reached[0] = true
	for range input.States {
		for _, s := range input.Stages {
			if reached[s.From] {
				if s.Branch == nil {
					reached[s.To] = true
				} else {
					for _, c := range s.Branch.Cases {
						reached[c.To] = true
					}
				}
			}
		}
	}
	for _, ok := range reached {
		if !ok {
			return nil, errors.New("unreachable batch state")
		}
	}
	raw, e := json.Marshal(input)
	if e != nil {
		return nil, e
	}
	var own BatchModel
	if e = json.Unmarshal(raw, &own); e != nil {
		return nil, e
	}
	return &BatchProgram{model: own, id: sha256.Sum256(raw)}, nil
}

type BatchReplyOffer struct {
	Code  uint64
	Sizes []int
}
type BatchMemberOffer struct {
	Name         string
	RequestSizes []int
	Replies      []BatchReplyOffer
}
type BatchOffer struct {
	Sequence uint64
	Stage    string
	Members  []BatchMemberOffer
}
type BatchStatus struct {
	State               int
	Completed           uint64
	Closed              bool
	PendingRequestBytes int
	PendingWrites       int
	Registers           []Value
	MemberPhases        []int
}
type batchMember struct {
	phase      int
	request    []Value
	writes     []batchWrite
	done       chan struct{}
	ownedBytes int
}
type batchWrite struct {
	index int
	value Value
}
type BatchSession struct {
	mu                  sync.Mutex
	p                   *BatchProgram
	seed, material      [32]byte
	state, stage, phase int // idle, active, closed
	nextState           int
	sequence            uint64
	registers           []Value
	offer               BatchOffer
	members             []batchMember
	ctx                 context.Context
	cancel              context.CancelFunc
	stop                func() bool
}

var ErrResponseNotReady = errors.New("batch response dependency not ready")

func NewBatchSession(parent context.Context, p *BatchProgram, seed, material [32]byte) (*BatchSession, error) {
	if parent == nil || p == nil || (p.model.Version != 1 && p.model.Version != 2) {
		return nil, errors.New("nil context or uncompiled batch")
	}
	if e := parent.Err(); e != nil {
		return nil, e
	}
	s := &BatchSession{p: p, seed: seed, material: material}
	for _, r := range p.model.Registers {
		s.registers = append(s.registers, cloneValues([]Value{r.Initial})[0])
	}
	s.ctx, s.cancel = context.WithTimeout(parent, time.Duration(p.model.LifetimeMS)*time.Millisecond)
	s.mu.Lock()
	s.stop = context.AfterFunc(s.ctx, func() { s.mu.Lock(); defer s.mu.Unlock(); s.failLocked(s.ctx.Err()) })
	s.mu.Unlock()
	return s, nil
}
func (s *BatchSession) failLocked(e error) error {
	s.phase = 2
	s.members = nil
	s.offer = BatchOffer{}
	if s.stop != nil {
		s.stop()
	}
	s.cancel()
	return e
}
func (s *BatchSession) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.failLocked(errors.New("closed")) }
func (s *BatchSession) choose(domain byte, stage, member, reply, field, bound int) int {
	h := hmac.New(sha256.New, s.seed[:])
	if s.p.model.Version == 1 {
		h.Write([]byte("veil-batch-1\x00"))
	} else {
		h.Write([]byte("veil-batch-2\x00"))
	}
	h.Write(s.p.id[:])
	h.Write(s.material[:])
	var key [17]byte
	binary.BigEndian.PutUint64(key[:8], s.sequence)
	binary.BigEndian.PutUint16(key[8:10], uint16(s.state))
	binary.BigEndian.PutUint16(key[10:12], uint16(stage))
	key[12] = byte(member)
	key[13] = byte(reply)
	binary.BigEndian.PutUint16(key[14:16], uint16(field))
	key[16] = domain
	h.Write(key[:])
	return int(binary.BigEndian.Uint64(h.Sum(nil)[:8]) % uint64(bound))
}
func (s *BatchSession) sizes(fields []Field, domain byte, member, reply int) []int {
	values := make([]int, len(fields))
	for i, f := range fields {
		switch f.Kind {
		case "u64":
			values[i] = 8
		case "digest":
			values[i] = 32
		case "bytes":
			values[i] = f.Min + s.choose(domain, s.stage, member, reply, i, f.Max-f.Min+1)
		}
	}
	return values
}
func copyBatchOffer(o BatchOffer) BatchOffer {
	v := BatchOffer{Sequence: o.Sequence, Stage: o.Stage}
	for _, a := range o.Members {
		n := BatchMemberOffer{Name: a.Name, RequestSizes: append([]int(nil), a.RequestSizes...)}
		for _, r := range a.Replies {
			n.Replies = append(n.Replies, BatchReplyOffer{r.Code, append([]int(nil), r.Sizes...)})
		}
		v.Members = append(v.Members, n)
	}
	return v
}
func (s *BatchSession) Plan() (BatchOffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ctx.Err(); e != nil {
		return BatchOffer{}, s.failLocked(e)
	}
	if s.phase != 0 {
		return BatchOffer{}, s.failLocked(errors.New("batch plan phase"))
	}
	weight := 0
	for _, stage := range s.p.model.Stages {
		if stage.From == s.state {
			weight += stage.Weight
		}
	}
	if weight == 0 || s.sequence >= uint64(s.p.model.MaxBatches) {
		return BatchOffer{}, s.failLocked(errors.New("batch program complete"))
	}
	choice := s.choose('s', 65535, 255, 255, 65535, weight)
	for i, stage := range s.p.model.Stages {
		if stage.From == s.state {
			if choice < stage.Weight {
				s.stage = i
				break
			}
			choice -= stage.Weight
		}
	}
	stage := s.p.model.Stages[s.stage]
	s.nextState = stage.To
	s.offer = BatchOffer{Sequence: s.sequence, Stage: stage.Name}
	s.members = make([]batchMember, len(stage.Actions))
	for i, a := range stage.Actions {
		o := BatchMemberOffer{Name: a.Name, RequestSizes: s.sizes(a.Request, 'q', i, 255)}
		for j, r := range a.Replies {
			o.Replies = append(o.Replies, BatchReplyOffer{r.Code, s.sizes(r.Fields, 'r', i, j)})
		}
		s.offer.Members = append(s.offer.Members, o)
		s.members[i].done = make(chan struct{})
	}
	s.phase = 1
	if e := s.ctx.Err(); e != nil {
		return BatchOffer{}, s.failLocked(e)
	}
	return copyBatchOffer(s.offer), nil
}
func (s *BatchSession) memberLocked(seq uint64, index, phase int) (*batchMember, error) {
	if e := s.ctx.Err(); e != nil {
		return nil, s.failLocked(e)
	}
	if s.phase != 1 || seq != s.sequence || index < 0 || index >= len(s.members) || s.members[index].phase != phase {
		return nil, s.failLocked(errors.New("batch member phase"))
	}
	return &s.members[index], nil
}
func (s *BatchSession) Request(seq uint64, index int, values []Value) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, e := s.memberLocked(seq, index, 0)
	if e != nil {
		return e
	}
	a := s.p.model.Stages[s.stage].Actions[index]
	if e = validateBatchValues(a.Request, s.offer.Members[index].RequestSizes, a.RequestMinimums, values); e != nil {
		return s.failLocked(e)
	}
	q := cloneValues(values)
	frame := Session{registers: s.registers, request: q}
	if e = frame.checks(a.Checks, nil, false); e != nil {
		return s.failLocked(e)
	}
	if branch := s.p.model.Stages[s.stage].Branch; branch != nil && branch.Action == index {
		matched := false
		for _, c := range branch.Cases {
			if q[branch.Field].Number == c.Code {
				if e = frame.checks(c.Checks, nil, false); e != nil {
					return s.failLocked(e)
				}
				s.nextState, matched = c.To, true
				break
			}
		}
		if !matched {
			return s.failLocked(errors.New("unknown batch request branch"))
		}
	}
	if e = s.ctx.Err(); e != nil {
		return s.failLocked(e)
	}
	m.request = q
	for i, f := range a.Request {
		if f.Kind == "bytes" {
			m.ownedBytes += len(q[i].Bytes)
		} else {
			m.ownedBytes += s.offer.Members[index].RequestSizes[i]
		}
	}
	m.phase = 1
	return nil
}

// WaitResponse waits only on declared sibling completion; it does not buffer
// response bodies. A caller cancellation terminates the whole batch session.
func (s *BatchSession) WaitResponse(ctx context.Context, seq uint64, index int) error {
	s.mu.Lock()
	_, e := s.memberLocked(seq, index, 1)
	if e != nil {
		s.mu.Unlock()
		return e
	}
	if ctx == nil {
		e = s.failLocked(errors.New("nil wait context"))
		s.mu.Unlock()
		return e
	}
	var waiting []<-chan struct{}
	for _, dep := range s.p.model.Stages[s.stage].Actions[index].ResponseAfter {
		waiting = append(waiting, s.members[dep].done)
	}
	s.mu.Unlock()
	for _, done := range waiting {
		select {
		case <-done:
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-ctx.Done():
			s.Close()
			return ctx.Err()
		}
	}
	if e = ctx.Err(); e != nil {
		s.Close()
		return e
	}
	return s.ctx.Err()
}
func (s *BatchSession) Response(seq uint64, index int, code uint64, values []Value) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, e := s.memberLocked(seq, index, 1)
	if e != nil {
		return e
	}
	a := s.p.model.Stages[s.stage].Actions[index]
	reply := -1
	for i, r := range a.Replies {
		if r.Code == code {
			reply = i
			break
		}
	}
	if reply < 0 {
		return s.failLocked(errors.New("unknown batch response code"))
	}
	r := a.Replies[reply]
	if e = validateBatchValues(r.Fields, s.offer.Members[index].Replies[reply].Sizes, r.Minimums, values); e != nil {
		return s.failLocked(e)
	}
	for _, dep := range a.ResponseAfter {
		if s.members[dep].phase != 2 {
			return ErrResponseNotReady
		}
	}
	frame := Session{registers: s.registers, request: m.request}
	if e = frame.checks(r.Checks, values, false); e != nil {
		return s.failLocked(e)
	}
	var writes []batchWrite
	for _, set := range r.Assign {
		v, err := frame.eval(set.Value, values)
		if err != nil {
			return s.failLocked(err)
		}
		writes = append(writes, batchWrite{set.Register, cloneValues([]Value{v})[0]})
	}
	if e = s.ctx.Err(); e != nil {
		return s.failLocked(e)
	}
	m.writes = writes
	m.request = nil
	m.ownedBytes = 0
	m.phase = 2
	close(m.done)
	for _, other := range s.members {
		if other.phase != 2 {
			return nil
		}
	}
	next := cloneValues(s.registers)
	for _, other := range s.members {
		for _, set := range other.writes {
			next[set.index] = set.value
		}
	}
	if e = s.ctx.Err(); e != nil {
		return s.failLocked(e)
	}
	s.registers = next
	s.state = s.nextState
	s.sequence++
	s.phase = 0
	s.members = nil
	s.offer = BatchOffer{}
	return nil
}
func (s *BatchSession) Status() BatchStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ctx.Err(); e != nil {
		s.failLocked(e)
	}
	r := BatchStatus{State: s.state, Completed: s.sequence, Closed: s.phase == 2, Registers: cloneValues(s.registers)}
	for _, m := range s.members {
		r.MemberPhases = append(r.MemberPhases, m.phase)
		r.PendingWrites += len(m.writes)
		r.PendingRequestBytes += m.ownedBytes
	}
	return r
}
