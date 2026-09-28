package mux

import "testing"

func TestProfileRangesAndBounds(t *testing.T) {
	p := DefaultProfile()
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	for range 1000 {
		s := newShape(p)
		for _, v := range []struct {
			got    int
			bounds Range
		}{{s.share(), p.Quantum}, {s.credit.choose(), p.CreditBlocks}, {s.startup.choose(), p.StartupSize}, {s.writes.choose(), p.StartupWrites}, {s.limit.choose(), p.PaddingLimit}, {s.budget.choose(), p.PaddingBudget}, {s.control.choose(), p.ControlPadding}} {
			if v.got < v.bounds.Min || v.got > v.bounds.Max {
				t.Fatal("escaped configured interval", v)
			}
		}
	}
	for _, change := range []func(*Profile){
		func(p *Profile) { p.Version = 2 }, func(p *Profile) { p.Quantum.Max = blockSize + 1 },
		func(p *Profile) { p.StartupSize.Min = 0 }, func(p *Profile) { p.PaddingLimit.Max = 513 },
		func(p *Profile) { p.PaddingBudget.Max = 4097 }, func(p *Profile) { p.StartupWrites.Max = 13 },
		func(p *Profile) { p.CreditBlocks.Min = 0 }, func(p *Profile) { p.ControlPadding.Max = 129 },
		func(p *Profile) { p.Quantum.Min = p.Quantum.Max + 1 },
	} {
		bad := p
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("unsafe profile accepted", bad)
		}
	}
}
