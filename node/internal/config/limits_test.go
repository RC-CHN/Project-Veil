package config

import "testing"

func TestLimitsRemainBoundedAndDefaultsStable(t *testing.T) {
	if got := (Limits{}).Effective(); got.StreamBytes != 8<<20 || got.CarrierBytes != 64<<20 {
		t.Fatal("baseline budgets changed", got)
	}
	for _, limits := range []Limits{{StreamBytes: 1<<40 + 1}, {CarrierBytes: 4095}, {CarrierBytes: 1<<40 + 1}} {
		if limits.Validate() == nil {
			t.Fatal("unbounded or invalid limits accepted", limits)
		}
	}
	if err := (Limits{StreamBytes: 16 << 20, CarrierBytes: 128 << 20}).Validate(); err != nil {
		t.Fatal(err)
	}
}
