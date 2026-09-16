package streamlink

import "time"

// CreditFlowStatus uses constant space and contains no payload or peer metadata.
// Ready samples measure the oldest consumption advance in a coalesced CREDIT,
// ending when Lease serializes it, not when the peer receives or consumes it.
// Wait slices classify a full producer window at the start of each sleep. A
// slice with queued data can also include network work; neither class is RTT.
type CreditFlowStatus struct {
	ReadyLeased, ReadyAbandoned                 uint64
	ReadyLeaseNS, ReadyLeaseMaxNS, AbandonedNS  int64
	SentFrames, ReceivedFrames, AdvancingFrames uint64
	WaitSlices, QueuedWaitSlices                uint64
	QueuedWaitNS, AllLeasedWaitNS               int64
}

type creditObservation struct {
	status CreditFlowStatus
	ready  time.Time
}

func (o *creditObservation) consumed(now time.Time) {
	if o.ready.IsZero() {
		o.ready = now
	}
}

func (o *creditObservation) leased(now time.Time) {
	o.status.SentFrames++
	if !o.ready.IsZero() {
		n := now.Sub(o.ready).Nanoseconds()
		o.status.ReadyLeased++
		o.status.ReadyLeaseNS += n
		o.status.ReadyLeaseMaxNS = max(o.status.ReadyLeaseMaxNS, n)
		o.ready = time.Time{}
	}
}

func (o *creditObservation) closed(now time.Time) {
	if !o.ready.IsZero() {
		o.status.ReadyAbandoned++
		o.status.AbandonedNS += now.Sub(o.ready).Nanoseconds()
		o.ready = time.Time{}
	}
}

func (o *creditObservation) waited(n int64, queued bool) {
	o.status.WaitSlices++
	if queued {
		o.status.QueuedWaitSlices++
		o.status.QueuedWaitNS += n
	} else {
		o.status.AllLeasedWaitNS += n
	}
}
