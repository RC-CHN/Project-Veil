package session

import (
	"sync"
	"time"

	b "veil.local/core/internal/behavior"
)

// Lane trace prefixes are bounded independently of the complete counters and
// budget hashes. Payload includes the flight envelope, not just application data.
type FlightLaneEvent struct {
	Generation          uint64             `json:",omitempty"`
	Handoff             FlightHandoffTrace `json:",omitzero"`
	Instance            int
	Model               b.BatchStatus
	MaterialFingerprint string
	Transactions        uint64
	MaxActiveWaitUS     int64
	BudgetCount         uint64
	BudgetSHA256        string
	Budgets             []BudgetPoint
	Trace               CarrierTrace
	Coordination        FlightCoordination
}

// Handoff HTTP exchanges have no application payload. Count them separately
// and in the carrier total so renewal cannot disappear from wire accounting.
type FlightHandoffTrace struct {
	Count, Completed, Failed uint64
	TotalUS, MaximumUS       int64
}

func (t *FlightHandoffTrace) add(start time.Time, err error) {
	us := time.Since(start).Microseconds()
	t.Count++
	if err == nil {
		t.Completed++
	} else {
		t.Failed++
	}
	t.TotalUS += us
	t.MaximumUS = max(t.MaximumUS, us)
}

type FlightEvent struct {
	ModelVersion         int
	ObjectSequenceOffset uint64 `json:",omitempty"`
	ChildModelID         string
	FrameHeaderBytes     int
	MaximumHandlers      int
	Source               flightSourceStatus
	Lanes                []FlightLaneEvent
}
type FlightWait struct {
	Count, Failed      uint64
	TotalUS, MaximumUS int64
}
type FlightCoordination struct{ Delivery, Closing FlightWait }
type flightWaitTrace struct {
	mu sync.Mutex
	v  FlightCoordination
}

func (t *flightWaitTrace) add(start time.Time, closing bool, e error) {
	us := time.Since(start).Microseconds()
	t.mu.Lock()
	defer t.mu.Unlock()
	v := &t.v.Delivery
	if closing {
		v = &t.v.Closing
	}
	v.Count++
	if e != nil {
		v.Failed++
	}
	v.TotalUS += us
	v.MaximumUS = max(v.MaximumUS, us)
}
func (t *flightWaitTrace) snapshot() FlightCoordination {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.v
}
func flightLaneEvent(instance int, model b.BatchStatus, material [32]byte, count uint64, wait int64, trace CarrierTrace, budget *budgetTrace, lane *flightLane) FlightLaneEvent {
	n, h, points := budget.snapshot()
	trace.Points = append([]CarrierPoint(nil), trace.Points[:min(len(trace.Points), 24)]...)
	return FlightLaneEvent{Instance: instance, Model: model, MaterialFingerprint: hashHex(material[:]), Transactions: count, MaxActiveWaitUS: wait, BudgetCount: n, BudgetSHA256: h, Budgets: points[:min(len(points), 16)], Trace: trace, Coordination: lane.coordination.snapshot()}
}
func (g *flightFront) snapshot() *FlightEvent {
	g.mu.Lock()
	maximum := g.maximum
	g.mu.Unlock()
	v := &FlightEvent{ModelVersion: g.program.version, ObjectSequenceOffset: g.program.objectOffset(), ChildModelID: g.program.child.ID(), FrameHeaderBytes: flightHeader, MaximumHandlers: maximum, Source: g.source.status()}
	for i, front := range g.fronts {
		front.mu.Lock()
		var model b.BatchStatus
		if front.session != nil {
			model = front.session.Status()
		}
		laneEvent := flightLaneEvent(i, model, front.material, front.eventCount, front.maxWaitUS, front.trace, g.traces[i], g.lanes[i])
		laneEvent.Generation = front.generation
		laneEvent.Handoff = front.handoff
		laneEvent.Transactions += front.handoff.Count
		v.Lanes = append(v.Lanes, laneEvent)
		front.mu.Unlock()
	}
	return v
}
func flightComplete(v *FlightEvent, instances int) bool {
	if v == nil || len(v.Lanes) != instances {
		return false
	}
	for _, lane := range v.Lanes {
		if lane.Model.State != 2 || lane.Model.Closed {
			return false
		}
	}
	return true
}
func (v *FlightEvent) apply(event *SessionEvent) {
	event.Flight = v
	for _, lane := range v.Lanes {
		event.Transactions += lane.Transactions
		event.MaxActiveWaitUS = max(event.MaxActiveWaitUS, lane.MaxActiveWaitUS)
		event.BudgetCount += lane.BudgetCount
	}
}
