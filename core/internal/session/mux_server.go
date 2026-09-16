package session

import (
	"context"
	"crypto/sha256"
	"net/http"
	"time"

	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

func (c *serverConnection) initializeMux() error {
	s := c.server
	m, e := sm.New(c.ctx, sm.Config{Limits: s.cfg.Mux.settings(s.cfg.Window, s.cfg.MaxBytes)})
	if e != nil {
		return e
	}
	c.muxSource = &muxSource{mux: m}
	c.front = &adaptiveFront{parallelFront: &parallelFront{innerVersion: 2, ctx: c.ctx, cancel: c.cancel, program: s.program, seed: s.seed, store: s.store, bucket: c.prefix, active: make(chan struct{}, 2), onPlan: c.trace.add}, output: c.muxSource, inputDelivery: c.muxSource.deliver, inputHash: sha256.New()}
	c.startMuxWorkers(m)
	return nil
}
func (c *serverConnection) initializeFlight(r *http.Request) error {
	s := c.server
	g, e := newFlightFront(c.ctx, r, s.flight, s.seed, c.principal, s.cfg.Bucket, s.store, sm.Config{Limits: s.cfg.Mux.settings(s.cfg.Window, s.cfg.MaxBytes)})
	if e != nil {
		return e
	}
	c.flightFront = g
	if s.flight.renewable() {
		// A rejected generation cancels the HTTP owner as well as its source.
		context.AfterFunc(g.ctx, c.cancel)
	}
	c.muxSource = &muxSource{mux: g.source.mux}
	c.startMuxWorkers(g.source.mux)
	return nil
}
func (c *serverConnection) startMuxWorkers(m *sm.Session) {
	s := c.server
	c.stopIdle = watchMuxIdle(c.ctx, m, time.Duration(s.cfg.Mux.CarrierIdleMS)*time.Millisecond)
	c.muxAcceptDone = make(chan struct{})
	go func() {
		defer close(c.muxAcceptDone)
		for {
			stream, e := m.Accept(c.ctx)
			if e != nil {
				return
			}
			c.muxWorkers.Add(1)
			s.stats.openStream()
			go func() { defer c.muxWorkers.Done(); c.serveMuxStream(stream) }()
		}
	}()
}
func (c *serverConnection) serveMuxStream(stream *sm.Stream) {
	s := c.server
	event := SessionEvent{EventType: "mux_stream", Role: "server", Prefix: c.prefix, ModelID: s.modelID(), Principal: c.principal, MaterialFingerprint: hashHex(c.material[:]), StartedNS: time.Now().UnixNano()}
	var pump *socketLink
	var conn streamSocket
	var cause error
	var release func()
	var stopIdle func()
	defer func() {
		if cause != nil {
			_ = stream.Reset(sm.EndInternal)
		}
		if stopIdle != nil {
			stopIdle()
		}
		if pump == nil && conn != nil {
			conn.Close()
			if held, ok := conn.(interface{ Release() }); ok {
				held.Release()
			}
		}
		completeMuxStreamEvent(&s.stats, &event, stream, pump, cause, false)
		if s.observer != nil {
			s.observer(event)
		}
		if release != nil {
			release()
		}
		s.stats.streams.Add(-1)
		stream.Release()
	}()
	release, cause = s.streamAdmission.Acquire(c.principal)
	if cause != nil {
		cause = stream.Respond(so.Result{Code: so.Busy})
		if cause == nil {
			event.OpenResultNS = time.Now().UnixNano()
			cause = waitMuxTerminal(stream)
		}
		return
	}
	request := stream.Request()
	if request.Network == so.NetworkUDP {
		conn = newServerDatagrams(stream.Context(), s.udpConnector, c.principal, s.cfg.UDPMaxTargets, time.Duration(s.cfg.UDPIdleMS)*time.Millisecond)
	} else {
		checked, dialErr := s.connector.Open(stream.Context(), c.principal, request.Address)
		if dialErr != nil {
			code := muxDestinationCode(dialErr)
			cause = stream.Respond(so.Result{Code: code})
			if cause == nil {
				event.OpenResultNS = time.Now().UnixNano()
				cause = waitMuxTerminal(stream)
			}
			return
		}
		conn = checked
	}
	limits := so.Limits{Window: min(request.Limits.Window, s.cfg.Window), MaxBytes: min(request.Limits.MaxBytes, s.cfg.MaxBytes)}
	cause = stream.Respond(so.Result{Code: so.OK, Limits: limits})
	if cause != nil {
		return
	}
	event.OpenResultNS = time.Now().UnixNano()
	ready := make(chan struct{})
	close(ready)
	pump = startSocketLink(stream.Link(), conn, ready)
	stopIdle = watchMuxStreamIdle(stream, time.Duration(s.cfg.IdleTimeoutMS)*time.Millisecond)
	cause = waitMuxTerminal(stream)
}
