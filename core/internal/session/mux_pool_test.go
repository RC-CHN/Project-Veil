package session

import (
	"context"
	"errors"
	"testing"
	"time"

	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

func TestMuxPoolWaitsForDrainingCarrier(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newClientMuxPool(&Client{cfg: ClientConfig{MaxCarriers: 1}}, parent)
	defer p.close()
	limits := sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 1}
	client, err := sm.New(parent, sm.Config{Client: true, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	server, err := sm.New(parent, sm.Config{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(nil)
	defer server.Close(nil)
	for _, pair := range [][2]*sm.Session{{client, server}, {server, client}} {
		data, _, err := pair[0].Lease(65536)
		if err != nil {
			t.Fatal(err)
		}
		if err = pair[1].Feed(data); err != nil {
			t.Fatal(err)
		}
		if err = pair[0].Commit(); err != nil {
			t.Fatal(err)
		}
	}
	r := so.Request{Network: so.NetworkUDP, Limits: so.Limits{Window: 16384, MaxBytes: 1 << 20}}
	last, err := client.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	defer last.Release()
	entry := &clientMuxCarrier{pool: p, ctx: parent, source: &muxSource{mux: client}, ready: true, done: make(chan struct{})}
	p.entries = []*clientMuxCarrier{entry}
	ctx, stop := context.WithTimeout(parent, 20*time.Millisecond)
	defer stop()
	_, stream, err := p.acquire(ctx, r)
	if !errors.Is(err, context.DeadlineExceeded) || stream != nil {
		t.Fatal("draining carrier must wait for owned cleanup, not reject the next OPEN", err)
	}
	if len(p.entries) != 1 || entry.retiring || client.Status().Opened != 1 {
		t.Fatal("waiting changed ownership or issued another OPEN")
	}
}
