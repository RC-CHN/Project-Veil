package behavior

import "errors"

// BatchMinimum changes an explicitly marked bytes field from an exact planned
// size to an actual length in [Min, planned capacity]. Only BatchModel v2 allows it.
type BatchMinimum struct {
	Field int `json:"field"`
	Min   int `json:"min"`
}

// A request branch chooses a tentative destination. All sibling responses must
// still validate before it can become the next committed state.
type BatchBranch struct {
	Action int         `json:"action"`
	Field  int         `json:"field"`
	Cases  []BatchCase `json:"cases"`
}
type BatchCase struct {
	Code   uint64  `json:"code"`
	To     int     `json:"to"`
	Checks []Equal `json:"checks"`
}

func validateBatchMinimums(version int, fields []Field, mins []BatchMinimum) error {
	if len(mins) > len(fields) || (version != 2 && len(mins) != 0) {
		return errors.New("batch minimum schema")
	}
	seen := map[int]bool{}
	for _, m := range mins {
		if m.Field < 0 || m.Field >= len(fields) || seen[m.Field] || fields[m.Field].Kind != "bytes" || m.Min < 0 || m.Min > fields[m.Field].Min {
			return errors.New("batch minimum field")
		}
		seen[m.Field] = true
	}
	return nil
}

func validateBatchValues(fields []Field, sizes []int, mins []BatchMinimum, values []Value) error {
	if len(values) != len(fields) {
		return errors.New("batch field count")
	}
	if len(mins) == 0 {
		return validateValues(fields, sizes, values)
	}
	actual := append([]int(nil), sizes...)
	for _, m := range mins {
		n := len(values[m.Field].Bytes)
		if n < m.Min || n > sizes[m.Field] {
			return errors.New("batch field outside planned capacity")
		}
		actual[m.Field] = n
	}
	return validateValues(fields, actual, values)
}

func validateBatchBranch(model BatchModel, stage BatchStage) error {
	b := stage.Branch
	if b == nil {
		return nil
	}
	if model.Version != 2 || stage.To != 0 || b.Action < 0 || b.Action >= len(stage.Actions) || len(b.Cases) < 1 || len(b.Cases) > 4 {
		return errors.New("batch branch schema")
	}
	a := stage.Actions[b.Action]
	if b.Field < 0 || b.Field >= len(a.Request) || a.Request[b.Field].Kind != "u64" {
		return errors.New("batch branch field")
	}
	seen := map[uint64]bool{}
	checks := 0
	for _, c := range b.Cases {
		checks += len(c.Checks)
		if seen[c.Code] || c.To < 0 || c.To >= model.States || checks > 32 {
			return errors.New("batch branch case")
		}
		seen[c.Code] = true
		for _, check := range c.Checks {
			if check.Left.Scope == "response" || check.Right.Scope == "response" {
				return errors.New("batch branch references response")
			}
		}
		m := Model{Version: 2, States: 2, MaxTransactions: 1, LifetimeMS: model.LifetimeMS, Registers: model.Registers,
			Actions: []Action{{Name: a.Name, From: 0, To: 1, Weight: 1, Request: a.Request, Checks: c.Checks}}}
		if _, e := Compile(m); e != nil {
			return e
		}
	}
	return nil
}
