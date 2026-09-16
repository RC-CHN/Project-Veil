package session

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	b "veil.local/core/internal/behavior"
	sm "veil.local/core/internal/streammux"
)

func TestPreparedModelInitialStateAndVersionIsolation(t *testing.T) {
	up, down := preparedETags()
	wantIDs := []string{
		"7988ff431873888f834b6d22c5690afe0c402359d5f1f86484da1be91ac0d1e4",
		"bf085cc423ee6d6ea1d68f47c826ba5ce63307d564002f2f5d8168f7a2702a78",
		"01420cf388d4f61c19eedcee96a6f1a7c0471a57b942ae1d6b5382f0f0c20206",
		"f473e5b8fe0a6fbb7d9d654c874315b15e8e992a645e808318831e4dbfb043e4",
	}
	clientTLS, serverTLS, _ := flightTLSStates(t)
	initial := []b.Value{dig([]byte(up)), dig([]byte(down)), dig(streamEncode(0, nil, false))}
	for n := 1; n <= 4; n++ {
		bundle, e := GenerateFlightBundleProfile(n, ProfileBulk192Prepared)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(t.TempDir(), "model.json")
		if e = WriteFlightBundle(path, bundle); e != nil {
			t.Fatal(e)
		}
		_, p, e := loadFlightBundle(path)
		if e != nil {
			t.Fatal(e)
		}
		old, e := compileFlight(flightModel(n))
		if e != nil || p.id == old.id || p.child.ID() == old.child.ID() {
			t.Fatal("prepared identity isolation", e)
		}
		if p.id != wantIDs[n-1] || p.child.ID() != "43f294c61a43b14b0aa2d2375acc0a14a170cacf1366732d7bfe30f5f901242f" {
			t.Fatal("frozen prepared ID changed", p.id, p.child.ID())
		}
		clientMaterial, err := p.material(&clientTLS)
		serverMaterial, peerErr := p.material(&serverTLS)
		oldMaterial, oldErr := old.material(&clientTLS)
		if err != nil || peerErr != nil || oldErr != nil || clientMaterial != serverMaterial || clientMaterial == oldMaterial {
			t.Fatal("prepared exporter version isolation")
		}
		if id, e := CheckBundle(path); e != nil || id != p.id {
			t.Fatal(id, e)
		}
		for i, want := range initial {
			if !bytes.Equal(bundle.Model.Child.Registers[i].Initial.Bytes, want.Bytes) {
				t.Fatal("prepared initial register", i)
			}
		}
		if bundle.Model.Child.Registers[5].Initial.Number != 1 || p.objectOffset() != 1 {
			t.Fatal("first object sequence")
		}
		m := bundle.Model
		m.Version = 1
		if _, e = compileFlight(m); e == nil {
			t.Fatal("new child accepted as old model")
		}
		m = bundle.Model
		m.Child = bulk192Model()
		if _, e = compileFlight(m); e == nil {
			t.Fatal("old child accepted as prepared model")
		}
		m = bundle.Model
		m.Child.Registers[0].Initial.Bytes[0] ^= 1
		if _, e = compileFlight(m); e == nil {
			t.Fatal("unknown initial state accepted")
		}
		t.Logf("instances=%d composition=%s child=%s", n, p.id, p.child.ID())
	}
	if _, e := GenerateFlightBundleProfile(4, "unknown"); e == nil {
		t.Fatal("unknown prepared profile")
	}
}

func preparedRequest(t *testing.T, p *flightProgram, server *tls.ConnectionState, seed [32]byte) *http.Request {
	t.Helper()
	master, e := p.material(server)
	if e != nil {
		t.Fatal(e)
	}
	material, e := p.laneMaterial(master, 0)
	if e != nil {
		t.Fatal(e)
	}
	body := streamEncode(1, nil, false)
	r, e := http.NewRequest("PUT", "https://owned.test/"+sessionPrefix("owned-test", seed, material)+"/upload", bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.TLS = server
	up, _ := preparedETags()
	r.Header.Set("If-Match", up)
	receipt := dig(streamEncode(0, nil, false))
	r.Header.Set("X-Amz-Meta-Receipt", hex.EncodeToString(receipt.Bytes))
	r.Header.Set("X-Amz-Meta-Mode", "0")
	return r
}

func TestPreparedFirstRequestGuards(t *testing.T) {
	_, server, principal := flightTLSStates(t)
	p, e := compileFlight(preparedFlightModel(1))
	if e != nil {
		t.Fatal(e)
	}
	var seed [32]byte
	for _, name := range []string{"condition", "receipt", "sequence", "initial-store", "duplicate", "discover"} {
		t.Run(name, func(t *testing.T) {
			store, e := newLocalBackend("owned-test", 2)
			if e != nil {
				t.Fatal(e)
			}
			defer store.close()
			if e = ensureBucket(context.Background(), store, "owned-test"); e != nil {
				t.Fatal(e)
			}
			r := preparedRequest(t, p, &server, seed)
			if name == "discover" {
				r.Method = "HEAD"
			}
			g, e := newFlightFront(context.Background(), r, p, seed, principal, "owned-test", store, sm.Config{Limits: sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}})
			if name == "discover" {
				if e == nil || store.local.Status().Objects != 0 {
					t.Fatal("legacy discovery accepted", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			defer g.finish(errors.New("test cleanup"))
			switch name {
			case "condition":
				r.Header.Set("If-Match", "\"00000000000000000000000000000000\"")
			case "receipt":
				r.Header.Set("X-Amz-Meta-Receipt", hex.EncodeToString(make([]byte, 32)))
			case "sequence":
				wrong := streamEncode(0, nil, false)
				r.Body = http.NoBody
				r2, err := http.NewRequest("PUT", r.URL.String(), bytes.NewReader(wrong))
				if err != nil {
					t.Fatal(err)
				}
				r2.Header = r.Header.Clone()
				r2.TLS = r.TLS
				r = r2
			case "initial-store":
				if _, e = store.must(context.Background(), "test_corrupt_initial", "PUT", r.URL.Path, []byte("different"), nil, 200); e != nil {
					t.Fatal(e)
				}
			}
			before := store.local.Status().Writes
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if name == "duplicate" {
				if w.Code != 200 {
					t.Fatal("valid initial control rejected", w.Code, w.Body.String())
				}
				again := preparedRequest(t, p, &server, seed)
				w = httptest.NewRecorder()
				g.ServeHTTP(w, again)
			} else if store.local.Status().Writes != before {
				t.Fatal("invalid first request changed store")
			}
			if w.Code == 200 {
				t.Fatal("invalid prepared request accepted", name)
			}
			if g.source.status().Mux.LeaseCommitted != 0 {
				t.Fatal("invalid first request acknowledged numbered data")
			}
		})
	}
}

// Direct HTTP dispatch makes the first receipt boundary observable without
// waiting for a later application stream to time out after a broken commit.
type preparedRoundTripper struct {
	front *flightFront
	state *tls.ConnectionState
}

func (rt preparedRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.TLS = rt.state
	w := httptest.NewRecorder()
	rt.front.ServeHTTP(w, req)
	return w.Result(), nil
}

func TestPreparedFirstTransferCommitsBeforeNextPlan(t *testing.T) {
	clientTLS, serverTLS, principal := flightTLSStates(t)
	p, e := compileFlight(preparedFlightModel(1))
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seed [32]byte
	store, e := newLocalBackend("owned-test", 2)
	if e != nil {
		t.Fatal(e)
	}
	defer store.close()
	if e = ensureBucket(ctx, store, "owned-test"); e != nil {
		t.Fatal(e)
	}
	cfg := sm.Config{Limits: sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}}
	first := preparedRequest(t, p, &serverTLS, seed)
	group, e := newFlightFront(ctx, first, p, seed, principal, "owned-test", store, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer group.finish(context.Canceled)
	cfg.Client = true
	source, e := newFlightMuxSource(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer source.finish(context.Canceled)
	master, e := p.material(&clientTLS)
	if e != nil {
		t.Fatal(e)
	}
	material, e := p.laneMaterial(master, 0)
	if e != nil {
		t.Fatal(e)
	}
	session, e := b.NewBatchSession(ctx, p.child, seed, material)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	lane := &flightLane{ctx: ctx, source: source}
	observed := false
	client := &http.Client{Transport: preparedRoundTripper{group, &serverTLS}}
	_, e = driveAdaptive(ctx, session, client, "https://owned.test", sessionPrefix("owned-test", seed, material), lane, adaptiveHooks{Prepared: true, Deliver: lane.deliver, AfterBatch: func(o b.BatchOffer, _ uint64, _ int64, _ time.Duration, _ b.BatchStatus) {
		if o.Sequence != 0 {
			t.Error("first transfer callback skipped")
		}
		observed = true
		st := source.status()
		if st.Mux.LeaseIssued != 1 || st.Mux.LeaseCommitted != 1 || lane.status().Pending {
			t.Error("first transfer was not committed")
		}
		cancel()
	}})
	if !observed || e == nil {
		t.Fatal("did not observe and stop at first receipt", e)
	}
}
