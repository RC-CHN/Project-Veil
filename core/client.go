package core

import (
	"context"
	"veil.local/core/endpoint"
	"veil.local/core/identity"
	"veil.local/core/internal/runstate"
	"veil.local/core/internal/session"
	"veil.local/core/telemetry"
	"veil.local/core/transport"
)

type Client struct {
	engine *session.Client
	state  *runstate.State
	events *observer
}

var _ transport.Dialer = (*Client)(nil)

func NewClient(o ClientOptions) (*Client, error) {
	i, e := inputs(o.Model, o.Identity, identity.Client, o.Roots)
	if e != nil {
		return nil, e
	}
	events := newObserver(o.Events)
	cfg := session.ClientConfig{Version: 5, Listen: "127.0.0.1:0", ServerURL: o.ServerURL, DialAddress: o.DialAddress, Bucket: o.Bucket, MaxConnections: o.MaxConnections, MaxCarriers: o.MaxCarriers, Window: o.Window, MaxBytes: o.MaxBytes, IdleTimeoutMS: o.IdleTimeoutMS, Mux: o.Mux.internal()}
	c, e := session.NewMemoryClient(cfg, i, events.emit)
	if e != nil {
		return nil, e
	}
	return &Client{c, runstate.New(), events}, nil
}
func (c *Client) Run(ctx context.Context) (err error) {
	if err = c.state.Begin(); err != nil {
		return err
	}
	defer func() { c.state.Finish(err) }()
	return c.engine.RunDirect(ctx, c.state.Ready)
}
func (c *Client) WaitReady(ctx context.Context) error { return c.state.WaitReady(ctx) }
func (c *Client) DialStream(ctx context.Context, e endpoint.Endpoint) (transport.Stream, error) {
	return c.engine.DialStream(ctx, e)
}
func (c *Client) OpenAssociation(ctx context.Context) (transport.Association, error) {
	return c.engine.OpenAssociation(ctx)
}
func (c *Client) Snapshot() telemetry.Snapshot {
	return snapshot(c.engine.Snapshot(), c.state.Current(), c.events)
}
