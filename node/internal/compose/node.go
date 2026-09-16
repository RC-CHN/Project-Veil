package compose

import (
	"context"
	"net"
	core "veil.local/core"
	"veil.local/core/telemetry"
	"veil.local/node/internal/config"
	"veil.local/node/internal/ingress/socks5"
	"veil.local/node/internal/lifecycle"
)

type Node struct {
	component lifecycle.Component
	snapshot  func() telemetry.Snapshot
}

func New(l config.Loaded) (*Node, error) {
	if l.Config.Role == "client" {
		c, e := l.Client()
		if e != nil {
			return nil, e
		}
		s, e := socks5.New(l.Config.Listen, l.Config.MaxConnections, c, c.WaitReady)
		if e != nil {
			return nil, e
		}
		g, e := lifecycle.New(c, s)
		if e != nil {
			return nil, e
		}
		return &Node{g, func() telemetry.Snapshot { v := c.Snapshot(); v.Listen = s.Address(); return v }}, nil
	}
	s, e := l.Server()
	if e != nil {
		return nil, e
	}
	r := &serverRunner{server: s, address: l.Config.Listen, bound: lifecycle.NewGate()}
	return &Node{r, s.Snapshot}, nil
}
func (n *Node) Run(ctx context.Context) error       { return n.component.Run(ctx) }
func (n *Node) WaitReady(ctx context.Context) error { return n.component.WaitReady(ctx) }
func (n *Node) Snapshot() telemetry.Snapshot        { return n.snapshot() }

type serverRunner struct {
	server  *core.Server
	address string
	bound   *lifecycle.Gate
}

func (r *serverRunner) Run(ctx context.Context) error {
	l, e := net.Listen("tcp", r.address)
	r.bound.Finish(e)
	if e != nil {
		return e
	}
	return r.server.Serve(ctx, l)
}
func (r *serverRunner) WaitReady(ctx context.Context) error {
	if e := r.bound.WaitReady(ctx); e != nil {
		return e
	}
	return r.server.WaitReady(ctx)
}
