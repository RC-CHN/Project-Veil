package mux

import (
	"errors"
	"math/rand/v2"
)

// Range is inclusive. A profile describes permitted behavior, not a script or
// a packet number -> length table. Each sender chooses its own connection state.
type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type Profile struct {
	Version        int   `json:"version"`
	Quantum        Range `json:"quantum_bytes"`
	StartupSize    Range `json:"startup_bytes"`
	StartupWrites  Range `json:"startup_writes"`
	PaddingLimit   Range `json:"padding_limit"`
	PaddingBudget  Range `json:"padding_budget"`
	CreditBlocks   Range `json:"credit_blocks"`
	ControlPadding Range `json:"control_padding_limit"`
}

func DefaultProfile() Profile {
	return Profile{Version: 1, Quantum: Range{8192, 32768}, StartupSize: Range{512, 2048}, StartupWrites: Range{2, 6}, PaddingLimit: Range{64, 256}, PaddingBudget: Range{512, 1024}, CreditBlocks: Range{16, 64}, ControlPadding: Range{16, 96}}
}

func (p Profile) Validate() error {
	if p.Version != 1 {
		return errors.New("unsupported traffic profile version")
	}
	for _, v := range []struct {
		r         Range
		low, high int
	}{
		{p.Quantum, 4096, blockSize}, {p.StartupSize, 128, 8192},
		{p.StartupWrites, 0, 12}, {p.PaddingLimit, 0, 512}, {p.PaddingBudget, 0, 4096},
		{p.CreditBlocks, 16, 64}, {p.ControlPadding, 0, 128},
	} {
		if v.r.Min < v.low || v.r.Max > v.high || v.r.Min > v.r.Max {
			return errors.New("traffic profile outside allowed budget")
		}
	}
	return nil
}

func (r Range) choose() int { return r.Min + rand.IntN(r.Max-r.Min+1) }

type shape struct {
	quantum, credit, startup, writes, limit, budget, control Range
	remaining                                                int
}

func (r Range) narrowed() Range {
	center := r.choose()
	return Range{max(r.Min, center-center/4), min(r.Max, center+center/4)}
}

func newShape(p Profile) shape {
	return shape{quantum: p.Quantum.narrowed(), credit: p.CreditBlocks.narrowed(), startup: p.StartupSize.narrowed(), writes: p.StartupWrites.narrowed(), limit: p.PaddingLimit.narrowed(), budget: p.PaddingBudget.narrowed(), control: p.ControlPadding.narrowed()}
}

// Keep a connection's range stable, varying the share within that range. The
// scheduler visits every ready stream before allowing a second turn.
func (s *shape) share() int {
	return s.quantum.choose()
}
