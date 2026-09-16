package session

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sync"
	"sync/atomic"
	"time"
	b "veil.local/core/internal/behavior"
	os "veil.local/core/internal/objectstore"
	sl "veil.local/core/internal/streamlink"
	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

type BudgetPoint struct {
	Sequence         uint64
	Upload, Download int
}
type budgetTrace struct {
	mu     sync.Mutex
	h      hash.Hash
	count  uint64
	points []BudgetPoint
}

func (t *budgetTrace) add(o b.BatchOffer) {
	point := BudgetPoint{Sequence: o.Sequence}
	for _, a := range o.Members {
		if a.Name == "upload" {
			point.Upload = a.RequestSizes[3]
		}
		if a.Name == "download" {
			for _, r := range a.Replies {
				if r.Code == 200 {
					point.Download = r.Sizes[1]
				}
			}
		}
	}
	var p [24]byte
	binary.BigEndian.PutUint64(p[:8], point.Sequence)
	binary.BigEndian.PutUint64(p[8:16], uint64(point.Upload))
	binary.BigEndian.PutUint64(p[16:], uint64(point.Download))
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.h == nil {
		t.h = sha256.New()
	}
	t.h.Write(p[:])
	t.count++
	if len(t.points) < 64 {
		t.points = append(t.points, point)
	}
}
func (t *budgetTrace) snapshot() (uint64, string, []BudgetPoint) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sum := ""
	if t.h != nil {
		sum = hex.EncodeToString(t.h.Sum(nil))
	}
	return t.count, sum, append([]BudgetPoint(nil), t.points...)
}

type SessionEvent struct {
	Flight                                                *FlightEvent     `json:",omitempty"`
	CarrierTrace                                          *CarrierTrace    `json:",omitempty"`
	ModelMaterialFingerprint                              string           `json:",omitempty"`
	LocalAddress, RemoteAddress                           string           `json:",omitempty"`
	MuxOutputSHA256, MuxInputSHA256                       string           `json:",omitempty"`
	EventType                                             string           `json:",omitempty"`
	Mux                                                   *sm.Status       `json:",omitempty"`
	Stream                                                *sm.StreamStatus `json:",omitempty"`
	UDP                                                   *UDPStatus       `json:",omitempty"`
	Role, Prefix, ModelID, MaterialFingerprint, Principal string
	StartedNS, EndedNS, OpenResultNS                      int64
	Outcome, Error                                        string
	Model                                                 b.BatchStatus
	Open                                                  so.Status
	Link                                                  sl.Status
	Pump                                                  socketPumpResult
	TLS                                                   wireSnapshot
	BudgetCount                                           uint64
	BudgetSHA256                                          string
	Budgets                                               []BudgetPoint
	Transactions                                          uint64
	MaxActiveWaitUS                                       int64
	ObjectsDeleted                                        bool
}
type counters struct {
	streams, maxStreams, streamsStarted, streamsCompleted, streamsRejected, streamsFailed                   atomic.Int64
	maxConnections, connectionRejected                                                                      atomic.Int64
	connections, sessions, started, completed, rejected, failed, authDenied, cleanupFailures, eventsDropped atomic.Int64
}
type Snapshot struct {
	BackendMode                                                                                                                    string        `json:",omitempty"`
	LocalObjects                                                                                                                   *os.Status    `json:",omitempty"`
	BackendTLS                                                                                                                     *wireSnapshot `json:",omitempty"`
	InnerProtocol                                                                                                                  string        `json:",omitempty"`
	ActiveStreams, MaximumStreams, StartedStreams, CompletedStreams, RejectedStreams, FailedStreams                                int64
	StreamLimit, CarrierLimit, AdmittedStreams, MaximumAdmittedStreams, StreamAdmissionLimit                                       int
	MaximumConnections, RejectedConnections                                                                                        int64
	ConnectionLimit                                                                                                                int
	Ready                                                                                                                          bool
	Role, Listen, ModelID                                                                                                          string
	UpdatedNS                                                                                                                      int64
	ActiveConnections, ActiveSessions, Started, CompletedModels, RejectedOpens, Failed, AuthDenied, CleanupFailures, EventsDropped int64
	BackendRequests, BackendFailures                                                                                               uint64
}

func (c *counters) baseSnapshot(role, listen, id string, ready bool) Snapshot {
	return Snapshot{MaximumConnections: c.maxConnections.Load(), RejectedConnections: c.connectionRejected.Load(), Ready: ready, Role: role, Listen: listen, ModelID: id, UpdatedNS: time.Now().UnixNano(), ActiveConnections: c.connections.Load(), ActiveSessions: c.sessions.Load(), Started: c.started.Load(), CompletedModels: c.completed.Load(), RejectedOpens: c.rejected.Load(), Failed: c.failed.Load(), AuthDenied: c.authDenied.Load(), CleanupFailures: c.cleanupFailures.Load(), EventsDropped: c.eventsDropped.Load()}
}

type Observer func(SessionEvent)

func (c *counters) snapshot(role, listen, id string, ready bool) Snapshot {
	v := c.baseSnapshot(role, listen, id, ready)
	v.ActiveStreams = c.streams.Load()
	v.MaximumStreams = c.maxStreams.Load()
	v.StartedStreams = c.streamsStarted.Load()
	v.CompletedStreams = c.streamsCompleted.Load()
	v.RejectedStreams = c.streamsRejected.Load()
	v.FailedStreams = c.streamsFailed.Load()
	return v
}
func (c *counters) openStream() {
	n := c.streams.Add(1)
	c.streamsStarted.Add(1)
	for {
		old := c.maxStreams.Load()
		if n <= old || c.maxStreams.CompareAndSwap(old, n) {
			return
		}
	}
}

func (c *counters) openConnection() {
	n := c.connections.Add(1)
	for {
		old := c.maxConnections.Load()
		if n <= old || c.maxConnections.CompareAndSwap(old, n) {
			return
		}
	}
}
