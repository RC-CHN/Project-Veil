package session

// CarrierTrace contains counters for all claimed exchanges and a bounded prefix
// of detailed points. It never stores application bytes, credentials or headers.
// ExchangeUS includes network transfer and peer processing; it is not pure RTT.
// Client WaitUS is charged only to the upload member (once per transfer batch).
const carrierTraceLimit = 96

type CarrierPoint struct {
	Generation                     uint64 `json:",omitempty"`
	InitialTransfer                bool   `json:",omitempty"`
	ObjectSequence                 uint64 `json:",omitempty"`
	Sequence                       uint64
	Member                         int
	StartedNS, EndedNS             int64
	Capacity, Payload              int
	WaitUS, ExchangeUS             int64
	WriteUS, FirstByteUS           int64 `json:",omitempty"`
	ModelWaitUS, ReadUS, BackendUS int64 `json:",omitempty"`
	Failed                         bool  `json:",omitempty"`
}

type CarrierDirection struct {
	Count, Failed, Empty, NearFull uint64
	Capacity, Payload              uint64
	WaitUS, ExchangeUS             int64
}

type CarrierTrace struct {
	Count    uint64
	Up, Down CarrierDirection
	Points   []CarrierPoint
}

func (t *CarrierTrace) add(p CarrierPoint) {
	t.Count++
	d := &t.Up
	if p.Member == 1 {
		d = &t.Down
	}
	d.Count++
	if p.Failed {
		d.Failed++
	}
	// Bootstrap is not a transfer opportunity. Empty and near-full count only
	// transfer members, including conditional responses with no payload.
	if p.Sequence > 0 || p.InitialTransfer {
		d.Capacity += uint64(p.Capacity)
		d.Payload += uint64(p.Payload)
		if p.Payload == 0 {
			d.Empty++
		}
		if p.Capacity > 0 && p.Payload*100 >= p.Capacity*95 {
			d.NearFull++
		}
	}
	d.WaitUS += p.WaitUS
	d.ExchangeUS += p.ExchangeUS
	if len(t.Points) < carrierTraceLimit {
		t.Points = append(t.Points, p)
	}
}
