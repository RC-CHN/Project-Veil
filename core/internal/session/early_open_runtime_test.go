package session

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

func TestEarlyOpenRuntimeConfigBounds(t *testing.T) {
	c := ClientConfig{Version: 4, Listen: "127.0.0.1:0", ServerURL: "https://owned.test", Bucket: "owned-test"}
	if e := c.defaults(); e != nil {
		t.Fatal(e)
	}
	if c.Window != 65536 || c.Mux.Streams != 8 || c.MaxCarriers != 2 {
		t.Fatal("early runtime changed resource defaults")
	}
	bundle, e := GenerateEarlyOpenFlightBundle(4)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "model.json")
	if e = WriteFlightBundle(path, bundle); e != nil {
		t.Fatal(e)
	}
	s := ServerConfig{Version: 4, Listen: "127.0.0.1:0", ModelFile: path, Bucket: "owned-test", ClientCA: "ca", ClientFingerprints: []string{"fingerprint"}, BackendMode: "local-object-v1", MaxSessions: 9}
	if e = s.defaults(); e != nil {
		t.Fatal(e)
	}
	if _, e = NewServer(s, nil); e == nil || !strings.Contains(e.Error(), "object capacity") {
		t.Fatal("early runtime omitted object capacity bound", e)
	}
	s.BackendMode, s.BackendAccess, s.BackendSecret = "remote-s3", "access", "secret"
	if e = s.defaults(); e == nil {
		t.Fatal("early runtime accepted remote backend")
	}
}

func TestEarlyOpenRuntimeModelBinding(t *testing.T) {
	clientTLS, serverTLS, _ := flightTLSStates(t)
	wantIDs := []string{
		"f62f69e3464224deff5dce4d5ccdc8831088d27cff74be0ab6bbd8cbeee695ee",
		"c175aee17f923aff6462ecc7f60a2df5cf04852e137eb651ebd0228223a2451f",
		"3d67dc31d1c4f883d0a317c14f46adf1929db793e82a88e85233854cd49f5ef9",
		"c1615217cfa157df02af497cde54a1463c3a0ad5eca16f6a1c0d12d9455cf6ee",
	}
	for n := 1; n <= 4; n++ {
		bundle, e := GenerateEarlyOpenFlightBundle(n)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(t.TempDir(), "model.json")
		if e = WriteFlightBundle(path, bundle); e != nil {
			t.Fatal(e)
		}
		_, _, p, e := loadRuntimeModel(path, 4)
		if e != nil || !p.earlyOpen() || p.objectOffset() != 1 || runtimeInner(4) != EarlyOpenFlightInnerProtocol {
			t.Fatal("early runtime binding", e)
		}
		if p.id != wantIDs[n-1] {
			t.Fatal("early composition ID changed", p.id)
		}
		old, e := compileFlight(preparedFlightModel(n))
		if e != nil {
			t.Fatal(e)
		}
		material, e := p.material(&clientTLS)
		peerMaterial, peerErr := p.material(&serverTLS)
		oldMaterial, oldErr := old.material(&clientTLS)
		if e != nil || peerErr != nil || oldErr != nil || material != peerMaterial || material == oldMaterial || p.id == old.id || p.child.ID() != old.child.ID() {
			t.Fatal("early inner semantics not bound to exporter")
		}
		if _, _, _, e = loadRuntimeModel(path, 3); e == nil {
			t.Fatal("early bundle accepted by old runtime config")
		}
		for _, change := range []func(*FlightBundle){
			func(b *FlightBundle) { b.Version = 3 },
			func(b *FlightBundle) { b.InnerProtocol = FlightInnerProtocol },
			func(b *FlightBundle) { b.Model.Version = 2 },
		} {
			bad := bundle
			change(&bad)
			path := filepath.Join(t.TempDir(), "bad.json")
			if e = WriteFlightBundle(path, bad); e != nil {
				t.Fatal(e)
			}
			if _, e = CheckBundle(path); e == nil {
				t.Fatal("inconsistent early model bundle accepted")
			}
		}
		t.Logf("instances=%d model=%s child=%s", n, p.id, p.child.ID())
	}
}

type earlyNoNetworkTransport struct{}

func (earlyNoNetworkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected transport after capability mismatch")
}

func TestEarlyOpenDriverRejectsSourceCapabilityMismatch(t *testing.T) {
	clientTLS, _, _ := flightTLSStates(t)
	for _, early := range []bool{false, true} {
		model := preparedFlightModel(1)
		if early {
			model = earlyOpenFlightModel(1)
		}
		program, e := compileFlight(model)
		if e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithCancel(context.Background())
		source, e := newFlightMuxSource(ctx, sm.Config{EarlyOpen: !early, Client: true, Limits: sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}})
		if e != nil {
			cancel()
			t.Fatal(e)
		}
		_, e = driveFlight(ctx, program, [32]byte{}, &clientTLS, &http.Client{Transport: earlyNoNetworkTransport{}}, "https://owned.test", "owned-test", source, nil)
		st := source.status()
		source.finish(context.Canceled)
		cancel()
		if e == nil || !strings.Contains(e.Error(), "fresh matching client source") || st.Mux.LeaseIssued != 0 || st.Delivered != 0 {
			t.Fatal("early source capability mismatch reached transport", e)
		}
	}
}

func earlyPoolFixture(t *testing.T) (*clientMuxPool, *clientMuxCarrier, *sm.Stream) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := newClientMuxPool(&Client{cfg: ClientConfig{MaxCarriers: 1}}, ctx)
	t.Cleanup(p.close)
	m, e := sm.New(ctx, sm.Config{EarlyOpen: true, Client: true, Limits: sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close(context.Canceled) })
	first, e := m.OpenFirst(so.Request{Network: so.NetworkUDP, Limits: so.Limits{Window: 16384, MaxBytes: 1 << 20}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(first.Release)
	c := &clientMuxCarrier{pool: p, ctx: ctx, cancel: cancel, source: &muxSource{mux: m}, readyDone: make(chan struct{}), done: make(chan struct{})}
	c.workers.Add(1)
	p.entries = []*clientMuxCarrier{c}
	return p, c, first
}

func TestEarlyOpenPoolCancellationAndPublication(t *testing.T) {
	for _, name := range []string{"cancel-unsent", "cancel-emitted", "pool-close", "failed", "handoff", "cancel-already-ready"} {
		t.Run(name, func(t *testing.T) {
			p, c, first := earlyPoolFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "cancel-emitted":
				if _, _, _, e := c.source.mux.LeaseNext(65536); e != nil {
					t.Fatal(e)
				}
				cancel()
			case "cancel-unsent":
				cancel()
			case "pool-close":
				p.close()
			case "failed":
				c.markReady(errors.New("controlled startup failure"))
			case "handoff", "cancel-already-ready":
				c.prefix = "published-prefix"
				c.material[0] = 7
				c.markReady(nil)
				if name == "cancel-already-ready" {
					cancel()
				}
			}
			got, stream, e := c.awaitFirst(ctx, first)
			if name == "handoff" {
				if e != nil || got != c || stream != first || first.Status().Released || got.prefix != "published-prefix" || got.material[0] != 7 {
					t.Fatal("first stream publication or ownership", e)
				}
				first.Release()
				c.workers.Done()
			} else {
				if e == nil || got != nil || stream != nil || !first.Status().Released {
					t.Fatal("failed first OPEN leaked ownership", e, first.Status())
				}
				if name == "cancel-emitted" && c.source.mux.Status().Active != 1 {
					t.Fatal("emitted OPEN released protocol ownership early")
				}
				if name != "cancel-emitted" && c.source.mux.Status().Active != 0 {
					t.Fatal("unsent OPEN retained admission")
				}
			}
			joined := make(chan struct{})
			go func() { c.workers.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Fatal("provisional worker was not released")
			}
			if c.source.mux.Status().Opened != 1 {
				t.Fatal("first request retried")
			}
		})
	}
}

func TestEarlyOpenPoolConcurrentWaitDoesNotIssueAnotherOpen(t *testing.T) {
	p, c, first := earlyPoolFixture(t)
	defer c.workers.Done()
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, stream, e := p.acquire(ctx, first.Request())
	if !errors.Is(e, context.DeadlineExceeded) || stream != nil || c.source.mux.Status().Opened != 1 || c.source.mux.Status().Active != 1 {
		t.Fatal("waiting request changed initial ownership", e)
	}
}

func TestEarlyOpenPoolStartupFailureJoinsWithoutRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	program, e := compileFlight(earlyOpenFlightModel(4))
	if e != nil {
		t.Fatal(e)
	}
	events := make(chan SessionEvent, 1)
	client := &Client{flight: program, program: program.child, dialAddress: "invalid-address-without-port", cfg: ClientConfig{Version: 4, MaxCarriers: 1, Window: 16384, MaxBytes: 1 << 20, Mux: MuxConfig{Streams: 4, Opened: 8, ConnectionBytes: 4 << 20}}, observer: func(e SessionEvent) { events <- e }}
	p := newClientMuxPool(client, ctx)
	defer p.close()
	_, stream, e := p.acquire(ctx, so.Request{Network: so.NetworkUDP, Limits: so.Limits{Window: 16384, MaxBytes: 1 << 20}})
	if e == nil || stream != nil {
		t.Fatal("invalid dial unexpectedly succeeded")
	}
	joined := make(chan struct{})
	go func() { p.workers.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("failed first OPEN prevented carrier cleanup")
	}
	select {
	case event := <-events:
		if event.Outcome != "transport_error" || event.Mux == nil || event.Mux.Active != 0 || event.Mux.Opened != 1 || event.Mux.OpenSent != 0 || !event.Mux.EarlyOpenEnabled {
			t.Fatal("startup failure state", event)
		}
	default:
		t.Fatal("missing startup failure event")
	}
	if len(p.entries) != 0 || client.stats.sessions.Load() != 0 || client.stats.started.Load() != 1 {
		t.Fatal("failed first OPEN leaked or retried carrier")
	}
}
