package session

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"net/http"
	"sync"
	"time"

	b "veil.local/core/internal/behavior"
	sm "veil.local/core/internal/streammux"
)

type flightDriveResult struct {
	Event  *FlightEvent
	Models []b.BatchStatus
	Lanes  []adaptiveDriveResult
}

// driveFlight keeps each child VM serial. Only separate child instances run in
// parallel, sharing one authenticated HTTP client and one numbered Mux source.
func driveFlight(ctx context.Context, program *flightProgram, seed [32]byte, cs *tls.ConnectionState, client *http.Client, endpoint, bucket string, source *flightMuxSource, ready func()) (flightDriveResult, error) {
	var result flightDriveResult
	if ctx == nil || client == nil || program == nil || cs == nil || len(cs.VerifiedChains) == 0 || source == nil {
		return result, errors.New("flight client model, source or verified TLS state")
	}
	initial := source.status()
	if initial.Receive.Limit != program.instances || !initial.Mux.Client || initial.Mux.EarlyOpenEnabled != program.earlyOpen() || initial.Mux.LeaseIssued != 0 || initial.Delivered != 0 || initial.Closed {
		return result, errors.New("flight driver requires a fresh matching client source")
	}
	master, e := program.material(cs)
	if e != nil {
		return result, e
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type completion struct {
		lane   int
		model  b.BatchStatus
		result adaptiveDriveResult
		event  FlightLaneEvent
		err    error
	}
	done := make(chan completion, program.instances)
	var workers sync.WaitGroup
	result.Event = &FlightEvent{ModelVersion: program.version, ObjectSequenceOffset: program.objectOffset(), ChildModelID: program.child.ID(), FrameHeaderBytes: flightHeader, Lanes: make([]FlightLaneEvent, program.instances)}
	result.Models = make([]b.BatchStatus, program.instances)
	result.Lanes = make([]adaptiveDriveResult, program.instances)
	for i := 0; i < program.instances; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			material, err := program.laneMaterial(master, i)
			if err != nil {
				done <- completion{lane: i, err: err}
				return
			}
			session, err := b.NewBatchSession(childCtx, program.child, seed, material)
			if err != nil {
				done <- completion{lane: i, err: err}
				return
			}
			defer func() { session.Close() }()
			lane := &flightLane{ctx: childCtx, source: source}
			deliver := func(p []byte, eof bool) error {
				if err := lane.deliver(p, eof); err != nil {
					return err
				}
				if ready != nil && source.mux.Status().Ready {
					ready()
				}
				return nil
			}
			var budget budgetTrace
			var value adaptiveDriveResult
			var generation uint64
			var handoff FlightHandoffTrace
			for {
				started := time.Now()
				hooks := adaptiveHooks{Generation: generation, Prepared: program.version >= 2, Deliver: deliver, AfterPlan: budget.add, Continue: &value}
				if program.renewable() {
					hooks.Pause = func(st b.BatchStatus) bool {
						// Reserve the next checked batch for normal closure when
						// both EOF flags and the global receipt barrier are ready.
						// Renewing here would discard those per-VM flags again.
						if st.Registers[3].Number == 1 && st.Registers[4].Number == 1 && lane.readyClosing() {
							return false
						}
						return st.Completed >= program.generationBatches || time.Since(started) >= program.generationAge
					}
				}
				value, err = driveAdaptive(childCtx, session, client, endpoint, sessionPrefix(bucket, seed, material), lane, hooks)
				if err != nil || !value.paused {
					break
				}
				var next [32]byte
				handoffStarted := time.Now()
				next, err = requestFlightHandoff(childCtx, program, seed, master, i, generation, session.Status(), value, client, endpoint, bucket)
				handoff.add(handoffStarted, err)
				if err != nil {
					break
				}
				session.Close()
				var nextSession *b.BatchSession
				nextSession, err = b.NewBatchSession(childCtx, program.child, seed, next)
				if err != nil {
					break
				}
				session, material = nextSession, next
				generation++
			}
			event := flightLaneEvent(i, session.Status(), material, value.TransactionCount, value.MaxClientWaitUS, value.Trace, &budget, lane)
			event.Generation = generation
			event.Handoff = handoff
			event.Transactions += handoff.Count
			done <- completion{lane: i, model: session.Status(), result: value, event: event, err: err}
		}(i)
	}
	for i := 0; i < program.instances; i++ {
		value := <-done
		result.Event.Lanes[value.lane] = value.event
		result.Models[value.lane], result.Lanes[value.lane] = value.model, value.result
		if value.err != nil && e == nil {
			e = value.err
			cancel()
		}
	}
	workers.Wait()
	result.Event.Source = source.status()
	if e == nil {
		for _, model := range result.Models {
			if model.Closed || model.State != 2 {
				e = errors.New("flight child model did not complete")
			}
		}
		if !(&flightLane{source: source}).readyClosing() {
			e = errors.New("flight driver ended before global receipts and EOF")
		}
	}
	return result, e
}

type flightFront struct {
	mu              sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	closing         bool
	active, maximum int
	handlers        sync.WaitGroup
	program         *flightProgram
	master          [32]byte
	principal       string
	source          *flightMuxSource
	store           *backend
	fronts          []*adaptiveFront
	lanes           []*flightLane
	traces          []*budgetTrace
	prefixes        []string
	created         [][2]bool
	seed            [32]byte
	bucket          string
	generations     []flightGeneration
}

func verifyFlightPeer(cs *tls.ConnectionState, principal string) error {
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 || principal == "" || hashHex(cs.PeerCertificates[0].Raw) != principal {
		return errors.New("flight verified and authorized peer required")
	}
	return nil
}

// newFlightFront is called only for the first request of an already owned TLS
// connection. It validates authentication and the complete bootstrap path
// before creating any per-connection source or object.
func newFlightFront(parent context.Context, first *http.Request, program *flightProgram, seed [32]byte, principal, bucket string, store *backend, cfg sm.Config) (*flightFront, error) {
	if parent == nil || program == nil || first == nil || first.URL == nil || store == nil || store.local == nil || !bucketName.MatchString(bucket) || cfg.Client || cfg.MaxPending != 0 && cfg.MaxPending != program.instances {
		return nil, errors.New("flight frontend configuration")
	}
	if e := verifyFlightPeer(first.TLS, principal); e != nil {
		return nil, e
	}
	master, e := program.material(first.TLS)
	if e != nil {
		return nil, e
	}
	prefixes, e := flightPrefixes(first, program, seed, master, bucket)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(parent)
	cfg.MaxPending = program.instances
	cfg.EarlyOpen = program.earlyOpen()
	source, e := newFlightMuxSource(ctx, cfg)
	if e != nil {
		cancel()
		return nil, e
	}
	g := &flightFront{ctx: ctx, cancel: cancel, program: program, master: master, principal: principal, source: source, store: store, prefixes: prefixes, created: make([][2]bool, program.instances), seed: seed, bucket: bucket, generations: make([]flightGeneration, program.instances)}
	for i, prefix := range prefixes {
		if e = prepareObjects(ctx, store, prefix, &g.created[i]); e != nil {
			g.finish(e)
			return nil, e
		}
		lane := &flightLane{ctx: ctx, source: source}
		materialFor := func(cs *tls.ConnectionState) ([32]byte, error) {
			material, err := program.material(cs)
			if err != nil || material != master {
				return [32]byte{}, errors.New("flight instance belongs to a different TLS connection")
			}
			return program.laneMaterial(material, i)
		}
		trace := &budgetTrace{}
		g.traces = append(g.traces, trace)
		g.lanes = append(g.lanes, lane)
		front := &adaptiveFront{objectOffset: program.objectOffset(), parallelFront: &parallelFront{ctx: ctx, cancel: cancel, materialFor: materialFor, onPlan: trace.add, program: program.child, seed: seed, store: store, bucket: prefix, active: make(chan struct{}, 2)}, output: lane, inputDeliveryContext: lane.deliverContext, inputHash: sha256.New()}
		g.fronts = append(g.fronts, front)
	}
	return g, nil
}

func (g *flightFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if g.closing || g.ctx.Err() != nil {
		g.mu.Unlock()
		http.Error(w, "closed", http.StatusBadRequest)
		return
	}
	if g.active == 2*g.program.instances {
		g.cancel()
		g.mu.Unlock()
		http.Error(w, "request capacity", http.StatusTooManyRequests)
		return
	}
	g.active++
	g.maximum = max(g.maximum, g.active)
	g.handlers.Add(1)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
		g.handlers.Done()
	}()
	if e := verifyFlightPeer(r.TLS, g.principal); e != nil {
		g.cancel()
		http.Error(w, "request rejected", http.StatusBadRequest)
		return
	}
	if g.program.renewable() {
		g.serveGeneration(w, r)
		return
	}
	for i, prefix := range g.prefixes {
		if r.URL.Path == "/"+prefix+"/upload" || r.URL.Path == "/"+prefix+"/download" {
			g.fronts[i].ServeHTTP(w, r)
			return
		}
	}
	g.cancel()
	http.Error(w, "request rejected", http.StatusBadRequest)
}

// The HTTP owner must interrupt the connection on failure before waiting here,
// as with the existing serverConnection HTTPDone/handler cleanup barrier.
func (g *flightFront) finish(cause error) bool {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return false
	}
	g.closing = true
	g.mu.Unlock()
	// Runtime cleanup may already have validated and finished the source before
	// cancelling its parent and joining application workers. Preserve that result.
	if cause == nil && !g.source.status().Closed {
		for _, front := range g.fronts {
			front.mu.Lock()
			complete := front.session != nil && front.session.Status().State == 2 && !front.session.Status().Closed
			front.mu.Unlock()
			if !complete {
				cause = errors.New("flight frontend ended before child completion")
			}
		}
	}
	g.source.finish(cause)
	g.cancel()
	g.handlers.Wait()
	ok := true
	for i, prefix := range g.prefixes {
		ok = deleteObjects(g.store, prefix, g.created[i]) && ok
	}
	return ok
}

func flightPrefixes(first *http.Request, program *flightProgram, seed, master [32]byte, bucket string) ([]string, error) {
	if first == nil || first.URL == nil {
		return nil, errors.New("flight bootstrap request")
	}
	prefixes := make([]string, program.instances)
	matched := false
	for i := range prefixes {
		material, _ := program.laneMaterial(master, i)
		prefixes[i] = sessionPrefix(bucket, seed, material)
		uploadMethod := "HEAD"
		if program.version >= 2 {
			uploadMethod = "PUT"
		}
		matched = matched || first.Method == uploadMethod && first.URL.Path == "/"+prefixes[i]+"/upload" || first.Method == "GET" && first.URL.Path == "/"+prefixes[i]+"/download"
	}
	if !matched || first.URL.RawQuery != "" || first.URL.RawPath != "" {
		return nil, errors.New("flight bootstrap instance or path")
	}
	return prefixes, nil
}
