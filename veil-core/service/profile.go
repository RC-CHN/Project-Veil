package service

import (
	"math/rand/v2"
	"veil/core"
)

// GenerateTrafficProfile belongs to the application layer. The core accepts
// and validates the result; it does not distribute profiles or fetch updates.
// All parameters are sampled from intervals, without a catalog of templates.
func GenerateTrafficProfile() core.TrafficProfile {
	rangeWithin := func(low, high, width int) core.TrafficRange {
		min := low + rand.IntN(high-low-width+1)
		max := min + width + rand.IntN(high-min-width+1)
		return core.TrafficRange{Min: min, Max: max}
	}
	return core.TrafficProfile{
		Version:        1,
		Quantum:        rangeWithin(8192, 32768, 4096),
		StartupSize:    rangeWithin(256, 4096, 256),
		StartupWrites:  rangeWithin(2, 8, 2),
		PaddingLimit:   rangeWithin(32, 384, 32),
		PaddingBudget:  rangeWithin(256, 1536, 256),
		CreditBlocks:   rangeWithin(16, 64, 8),
		ControlPadding: rangeWithin(8, 128, 16),
	}
}
