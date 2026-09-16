package core

import (
	"context"
	"errors"
	"net"
	"veil.local/core/identity"
	"veil.local/core/internal/runstate"
	"veil.local/core/internal/session"
	"veil.local/core/telemetry"
)

type Server struct {
	engine *session.Server
	state  *runstate.State
	events *observer
}

func NewServer(o ServerOptions) (*Server, error) {
	i, e := inputs(o.Model, o.Identity, identity.Server, o.ClientRoots)
	if e != nil {
		return nil, e
	}
	events := newObserver(o.Events)
	cfg := session.ServerConfig{Version: 5, Listen: "127.0.0.1:0", BackendMode: "local-object-v1", Bucket: o.Bucket, ClientFingerprints: append([]string(nil), o.ClientFingerprints...), AllowCIDRs: append([]string(nil), o.AllowCIDRs...), DenyCIDRs: append([]string(nil), o.DenyCIDRs...), DNSAddress: o.DNSAddress, MaxConnections: o.MaxConnections, MaxSessions: o.MaxSessions, MaxSessionsPerIdentity: o.MaxSessionsPerIdentity, MaxActiveStreams: o.MaxActiveStreams, MaxActiveStreamsPerIdentity: o.MaxActiveStreamsPerIdentity, Window: o.Window, MaxBytes: o.MaxBytes, ConnectTimeoutMS: o.ConnectTimeoutMS, IdleTimeoutMS: o.IdleTimeoutMS, UDPMaxTargets: o.UDPMaxTargets, UDPIdleMS: o.UDPIdleMS, Mux: o.Mux.internal()}
	s, e := session.NewMemoryServer(cfg, i, events.emit)
	if e != nil {
		return nil, e
	}
	out := &Server{s, runstate.New(), events}
	s.OnReady = out.state.Ready
	return out, nil
}

// Serve takes ownership of l on every path, including rejected repeated runs.
func (s *Server) Serve(ctx context.Context, l net.Listener) (err error) {
	if l != nil {
		defer l.Close()
	}
	if err = s.state.Begin(); err != nil {
		return err
	}
	defer func() { s.state.Finish(err) }()
	if l == nil {
		return errors.New("nil listener")
	}
	if ctx == nil {
		return errors.New("nil server context")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return s.engine.Serve(ctx, l)
}
func (s *Server) WaitReady(ctx context.Context) error { return s.state.WaitReady(ctx) }
func (s *Server) Snapshot() telemetry.Snapshot {
	return snapshot(s.engine.Snapshot(), s.state.Current(), s.events)
}
