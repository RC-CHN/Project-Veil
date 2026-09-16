package session

import (
	"context"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	b "veil.local/core/internal/behavior"
	sm "veil.local/core/internal/streammux"
)

func TestFlightGenerationMaterialIsolation(t *testing.T) {
	client, server, _ := flightTLSStates(t)
	other, _, _ := flightTLSStates(t)
	p, err := compileFlight(renewingFlightModel(4))
	if err != nil {
		t.Fatal(err)
	}
	if p.id != "22f9ec6fb93a8c06110d35ab76f92db9712eed66b19cce054e531976204e6bf2" || p.generationBatches != 2048 || p.generationAge != 240*time.Second {
		t.Fatal("renewal contract identity or production trigger changed")
	}
	left, _ := p.material(&client)
	right, _ := p.material(&server)
	foreign, _ := p.material(&other)
	seen := map[[32]byte]bool{}
	paths := map[string]bool{}
	for lane := 0; lane < 4; lane++ {
		for _, generation := range []uint64{0, 1, 2, 255, 256, 1 << 32, math.MaxUint64} {
			m, err := p.generationMaterial(left, lane, generation)
			peer, _ := p.generationMaterial(right, lane, generation)
			wrong, _ := p.generationMaterial(foreign, lane, generation)
			path := sessionPrefix("owned-test", [32]byte{19}, m)
			if err != nil || m != peer || m == wrong || seen[m] || paths[path] {
				t.Fatal("generation isolation", lane, generation, err)
			}
			seen[m], paths[path] = true, true
			if generation == 0 {
				initial, _ := p.laneMaterial(left, lane)
				if initial != m {
					t.Fatal("bootstrap generation differs")
				}
			}
		}
	}
	old, _ := compileFlight(earlyOpenFlightModel(4))
	if _, err := old.generationMaterial(left, 0, 0); err == nil {
		t.Fatal("legacy model accepted renewal")
	}
	for _, lane := range []int{-1, 4} {
		if _, err := p.generationMaterial(left, lane, 0); err == nil {
			t.Fatal("invalid lane")
		}
	}
	t.Logf("renewing model=%s child=%s", p.id, p.child.ID())
}

func TestFlightGenerationCannotMasqueradeAsLegacyBundle(t *testing.T) {
	bundle, err := GenerateEarlyOpenFlightBundle(4)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Model = renewingFlightModel(4)
	path := filepath.Join(t.TempDir(), "model.json")
	if err := WriteFlightBundle(path, bundle); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := loadRuntimeModel(path, 4); err == nil {
		t.Fatal("new lifecycle silently enabled by legacy configuration")
	}
}

func TestFlightHandoffRejectsNoncanonicalOrIncompleteState(t *testing.T) {
	upload, download := preparedETags()
	receipt := dig(streamEncode(0, nil, false))
	base := b.BatchStatus{State: 1, Completed: 2, Registers: []b.Value{dig([]byte(upload)), dig([]byte(download)), receipt, {}, {}, {Number: 3}}}
	cases := map[string]func(*http.Request, *b.BatchStatus){
		"method":             func(r *http.Request, _ *b.BatchStatus) { r.Method = "GET" },
		"body":               func(r *http.Request, _ *b.BatchStatus) { r.ContentLength = 1 },
		"unknown-length":     func(r *http.Request, _ *b.BatchStatus) { r.ContentLength = -1 },
		"chunked":            func(r *http.Request, _ *b.BatchStatus) { r.TransferEncoding = []string{"chunked"} },
		"query":              func(r *http.Request, _ *b.BatchStatus) { r.URL.RawQuery = "x=1" },
		"empty-query":        func(r *http.Request, _ *b.BatchStatus) { r.URL.ForceQuery = true },
		"raw-path":           func(r *http.Request, _ *b.BatchStatus) { r.URL.RawPath = r.URL.Path },
		"range":              func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("Range", "bytes=0-1") },
		"future":             func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("X-Amz-Meta-Generation", "2") },
		"old":                func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("X-Amz-Meta-Generation", "0") },
		"leading-zero":       func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("X-Amz-Meta-Generation", "01") },
		"wrong-count":        func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("X-Amz-Meta-Batches", "1") },
		"count-leading-zero": func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("X-Amz-Meta-Batches", "02") },
		"upload-etag":        func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("If-Match", download) },
		"download-etag":      func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("If-None-Match", upload) },
		"receipt":            func(r *http.Request, _ *b.BatchStatus) { r.Header.Set("X-Amz-Meta-Receipt", strings.Repeat("0", 64)) },
		"receipt-uppercase": func(r *http.Request, _ *b.BatchStatus) {
			r.Header.Set("X-Amz-Meta-Receipt", strings.ToUpper(r.Header.Get("X-Amz-Meta-Receipt")))
		},
		"closed":          func(_ *http.Request, s *b.BatchStatus) { s.Closed = true },
		"initial":         func(_ *http.Request, s *b.BatchStatus) { s.State = 0 },
		"terminal":        func(_ *http.Request, s *b.BatchStatus) { s.State = 2 },
		"no-completed":    func(_ *http.Request, s *b.BatchStatus) { s.Completed = 0 },
		"exhausted":       func(_ *http.Request, s *b.BatchStatus) { s.Completed = 4096 },
		"active-member":   func(_ *http.Request, s *b.BatchStatus) { s.MemberPhases = []int{2, 1} },
		"pending-request": func(_ *http.Request, s *b.BatchStatus) { s.PendingRequestBytes = 1 },
		"pending-writes":  func(_ *http.Request, s *b.BatchStatus) { s.PendingWrites = 1 },
		"register-schema": func(_ *http.Request, s *b.BatchStatus) { s.Registers = nil },
	}
	for _, key := range []string{"X-Amz-Meta-Generation", "X-Amz-Meta-Batches", "If-Match", "If-None-Match", "X-Amz-Meta-Receipt"} {
		cases["missing-"+key] = func(r *http.Request, _ *b.BatchStatus) { r.Header.Del(key) }
		cases["duplicate-"+key] = func(r *http.Request, _ *b.BatchStatus) { r.Header.Add(key, r.Header.Get(key)) }
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := http.NewRequest("HEAD", "https://owned.test/next/upload", nil)
			r.Header = (flightHandoff{1, 2, upload, download, hex.EncodeToString(receipt.Bytes)}).headers()
			if _, err := checkFlightHandoff(r, 1, base); err != nil {
				t.Fatal("valid control", err)
			}
			st := base
			change(r, &st)
			if _, err := checkFlightHandoff(r, 1, st); err == nil {
				t.Fatal("invalid handoff accepted")
			}
		})
	}
}

// This fixture executes one complete native-object batch through the real
// frontend, then stops the client exactly at its validated receipt boundary.
func generationBoundary(t *testing.T) (*flightFront, *http.Request, *b.BatchSession) {
	t.Helper()
	clientTLS, serverTLS, principal := flightTLSStates(t)
	p, err := compileFlight(renewingFlightModel(1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	store, err := newLocalBackend("owned-test", 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.close)
	if err := ensureBucket(ctx, store, "owned-test"); err != nil {
		t.Fatal(err)
	}
	cfg := sm.Config{EarlyOpen: true, Limits: sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}}
	seed := [32]byte{19}
	first := preparedRequest(t, p, &serverTLS, seed)
	g, err := newFlightFront(ctx, first, p, seed, principal, "owned-test", store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !g.closing && !g.finish(context.Canceled) {
			t.Error("fixture object cleanup")
		}
	})
	// No application OPEN is needed to verify an outer pending receipt. Early
	// mode also permits a normal HELLO, so both sources can establish here.
	cfg.Client = true
	source, err := newFlightMuxSource(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.finish(context.Canceled) })
	master, _ := p.material(&clientTLS)
	material, _ := p.laneMaterial(master, 0)
	session, err := b.NewBatchSession(ctx, p.child, seed, material)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	lane := &flightLane{ctx: ctx, source: source}
	value, err := driveAdaptive(ctx, session, &http.Client{Transport: preparedRoundTripper{g, &serverTLS}}, "https://owned.test", sessionPrefix("owned-test", seed, material), lane, adaptiveHooks{Prepared: true, Deliver: lane.deliver, Pause: func(b.BatchStatus) bool { return true }})
	if err != nil || !value.paused || lane.status().Pending || !g.lanes[0].status().Pending {
		t.Fatal("missing complete client / pending server boundary", err)
	}
	st := session.Status()
	next, _ := p.generationMaterial(master, 0, 1)
	r, _ := http.NewRequestWithContext(ctx, "HEAD", "https://owned.test/"+sessionPrefix("owned-test", seed, next)+"/upload", nil)
	r.TLS = &serverTLS
	r.Header = (flightHandoff{1, st.Completed, value.upETag, value.downETag, hex.EncodeToString(st.Registers[2].Bytes)}).headers()
	return g, r, session
}

func TestFlightGenerationFrontendFailureDoesNotAcknowledge(t *testing.T) {
	for _, name := range []string{"receipt", "generation", "skip-path", "old-path", "tls", "principal", "active-batch", "cancelled", "undeclared-body"} {
		t.Run(name, func(t *testing.T) {
			g, r, _ := generationBoundary(t)
			before, objects := g.source.status(), g.store.local.Status()
			switch name {
			case "receipt":
				r.Header.Set("X-Amz-Meta-Receipt", strings.Repeat("0", 64))
			case "generation":
				r.Header.Set("X-Amz-Meta-Generation", "2")
			case "skip-path":
				m, _ := g.program.generationMaterial(g.master, 0, 2)
				r.URL.Path = "/" + sessionPrefix(g.bucket, g.seed, m) + "/upload"
			case "old-path":
				r.URL.Path = "/" + g.prefixes[0] + "/upload"
			case "tls":
				_, foreign, _ := flightTLSStates(t)
				// Retain the authorized identity only to isolate exporter failure.
				foreign.PeerCertificates, foreign.VerifiedChains = r.TLS.PeerCertificates, r.TLS.VerifiedChains
				r.TLS = &foreign
			case "principal":
				g.principal = strings.Repeat("0", 64)
			case "active-batch":
				if _, err := g.fronts[0].session.Plan(); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			case "undeclared-body":
				r.Body = io.NopCloser(strings.NewReader("x"))
			}
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			after := g.source.status()
			if w.Code != 400 || g.ctx.Err() == nil || after.Mux.LeaseCommitted != before.Mux.LeaseCommitted || g.store.local.Status().Writes != objects.Writes || !g.lanes[0].status().Pending {
				t.Fatal("bad handoff mutated receipt or objects", w.Code, before, after)
			}
		})
	}
}

func TestFlightGenerationCreationConflictKeepsForeignObject(t *testing.T) {
	for _, object := range []string{"upload", "download"} {
		t.Run(object, func(t *testing.T) {
			g, r, _ := generationBoundary(t)
			ctx := context.Background()
			// The old pair already fills the configured two-object cap. Remove
			// one old object before planting an independent collision fixture.
			if _, err := g.store.must(ctx, "fixture_remove", "DELETE", "/"+g.prefixes[0]+"/upload", nil, nil, 204); err != nil {
				t.Fatal(err)
			}
			path := strings.TrimSuffix(r.URL.Path, "upload") + object
			body := []byte("foreign generation collision")
			if _, err := g.store.must(ctx, "fixture_collision", "PUT", path, body, nil, 200); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != 400 || g.ctx.Err() == nil || g.generations[0].number != 0 {
				t.Fatal("creation failure activated generation", w.Code)
			}
			if !g.finish(context.Canceled) {
				t.Fatal("partial generation ownership cleanup")
			}
			foreign, err := g.store.do(ctx, "fixture_verify", "GET", path, nil, nil)
			if err != nil || foreign.status != 200 || string(foreign.body) != string(body) || g.store.local.Status().Objects != 1 {
				t.Fatal("foreign object removed or owned object leaked", err, g.store.local.Status())
			}
			if _, err := g.store.must(ctx, "fixture_cleanup", "DELETE", path, nil, nil, 204); err != nil {
				t.Fatal(err)
			}
			if g.store.local.Status().Objects != 0 || g.store.local.Status().Bytes != 0 {
				t.Fatal("fixture resource leak")
			}
		})
	}
}

func TestFlightGenerationHandoffJoinsOldHandlersAndRejectsReplay(t *testing.T) {
	g, r, _ := generationBoundary(t)
	oldSession := g.fronts[0].session
	oldPrefix := g.prefixes[0]
	before := g.source.status()
	g.generations[0].gate.RLock() // Model the old response writer's retained ownership.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); g.ServeHTTP(w, r); done <- w }()
	select {
	case <-done:
		g.generations[0].gate.RUnlock()
		t.Fatal("handoff overtook old response writer")
	case <-time.After(20 * time.Millisecond):
	}
	if g.source.status().Mux.LeaseCommitted != before.Mux.LeaseCommitted {
		t.Error("early receipt commit")
	}
	g.generations[0].gate.RUnlock()
	w := <-done
	if w.Code != 204 || !oldSession.Status().Closed || g.fronts[0].session != nil || g.generations[0].number != 1 || g.lanes[0].status().Pending || g.source.status().Mux.LeaseCommitted != before.Mux.LeaseIssued {
		t.Fatal("handoff did not retire old VM and receipt", w.Code, w.Body.String())
	}
	if g.store.local.Status().Objects != 2 || g.prefixes[0] == oldPrefix {
		t.Fatal("object pair was not replaced in place")
	}
	old, err := g.store.do(context.Background(), "test_old_object", "HEAD", "/"+oldPrefix+"/upload", nil, nil)
	if err != nil || old.status != 404 {
		t.Fatal("old generation object remains")
	}
	after := g.source.status()
	// Handoff does not grant application receive credit or reset OPEN counts.
	if before.Mux.SentBytes != after.Mux.SentBytes || before.Mux.ReceivedBytes != after.Mux.ReceivedBytes || before.Mux.Opened != after.Mux.Opened || before.Mux.ReservedWindow != after.Mux.ReservedWindow || before.Delivered != after.Delivered {
		t.Fatal("handoff changed inner state")
	}
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r.Clone(r.Context()))
	if w.Code != 400 || g.generations[0].number != 1 || g.source.status().Mux.LeaseCommitted != after.Mux.LeaseCommitted {
		t.Fatal("replayed handoff accepted")
	}
}

type generationReplyTransport struct {
	header http.Header
	status int
	calls  int
}

func (rt *generationReplyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls++
	return &http.Response{StatusCode: rt.status, Header: rt.header, Body: http.NoBody}, nil
}

func TestFlightGenerationClientRejectsResponseWithoutRetry(t *testing.T) {
	for _, name := range []string{"status", "generation", "batches", "receipt", "duplicate", "overflow"} {
		t.Run(name, func(t *testing.T) {
			g, r, session := generationBoundary(t)
			st := session.Status()
			value := adaptiveDriveResult{paused: true, upETag: r.Header.Get("If-Match"), downETag: r.Header.Get("If-None-Match")}
			rt := &generationReplyTransport{header: r.Header.Clone(), status: 204}
			var generation uint64
			switch name {
			case "status":
				rt.status = 200
			case "generation":
				rt.header.Set("X-Amz-Meta-Generation", "2")
			case "batches":
				rt.header.Set("X-Amz-Meta-Batches", "0")
			case "receipt":
				rt.header.Set("X-Amz-Meta-Receipt", strings.Repeat("0", 64))
			case "duplicate":
				rt.header.Add("X-Amz-Meta-Receipt", rt.header.Get("X-Amz-Meta-Receipt"))
			case "overflow":
				generation = math.MaxUint64
			}
			_, err := requestFlightHandoff(r.Context(), g.program, g.seed, g.master, 0, generation, st, value, &http.Client{Transport: rt}, "https://owned.test", g.bucket)
			wantCalls := 1
			if name == "overflow" {
				wantCalls = 0
			}
			if err == nil || rt.calls != wantCalls || !reflect.DeepEqual(session.Status(), st) {
				t.Fatal("response accepted, retried or advanced local VM", err, rt.calls)
			}
		})
	}
}

// Compile-time checks keep these helpers tied to the actual HTTP interfaces.
var _ http.RoundTripper = (*generationReplyTransport)(nil)
