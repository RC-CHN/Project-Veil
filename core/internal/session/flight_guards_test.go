package session

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	fw "veil.local/core/internal/flightwindow"
	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

func flightTLSStates(t *testing.T) (tls.ConnectionState, tls.ConnectionState, string) {
	t.Helper()
	serverConfig, clientConfig, principal := flightTLSConfigs(t)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	server, client := tls.Server(a, serverConfig), tls.Client(b, clientConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.HandshakeContext(ctx) }()
	if e := client.HandshakeContext(ctx); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	return client.ConnectionState(), server.ConnectionState(), principal
}

func TestFlightModelAndExporterIsolation(t *testing.T) {
	client, server, _ := flightTLSStates(t)
	otherClient, _, _ := flightTLSStates(t)
	seenIDs := map[string]bool{}
	seenMaterials := map[[32]byte]bool{}
	wantIDs := []string{
		"51549abf8083d68a72afc93def8d83cad91459b695e4e4b9c185570e67db4945",
		"4c7b01cb4eac84e947e0902721bd19c3982051c26bce3f086bb7883f070eb717",
		"682ace614793785a4f666c7819e3ed6b23960b6bd8bf1317381250592774540e",
		"2c2c32d0a8a7a7e1f5242887f000e17783f8747e8753caff28d91848b7c95ab1",
	}
	for instances := 1; instances <= 4; instances++ {
		model := flightModel(instances)
		p, e := compileFlight(model)
		if e != nil || seenIDs[p.id] {
			t.Fatal("composition ID", e)
		}
		seenIDs[p.id] = true
		if p.id != wantIDs[instances-1] {
			t.Fatal("frozen composition ID changed", p.id)
		}
		left, e := p.material(&client)
		right, peerErr := p.material(&server)
		other, otherErr := p.material(&otherClient)
		if e != nil || peerErr != nil || otherErr != nil || left != right || left == other {
			t.Fatal("connection material separation")
		}
		old, _ := runtimeMaterial(&client, p.child.ID(), 2)
		if old == left {
			t.Fatal("legacy exporter domain reused")
		}
		for i := 0; i < instances; i++ {
			m, e := p.laneMaterial(left, i)
			peer, _ := p.laneMaterial(right, i)
			if e != nil || m != peer || m == left || seenMaterials[m] {
				t.Fatal("instance material separation")
			}
			seenMaterials[m] = true
		}
		if _, e := p.laneMaterial(left, instances); e == nil {
			t.Fatal("instance bound")
		}
		id := p.id
		model.Child.Stages[1].Actions[0].Request[3].Max--
		if p.id != id || p.child.ID() != "4a8f1caae580d78ac6c40ea45cde72d7b432665a8272e406d0a1facc84cc6b87" {
			t.Fatal("compiled program aliases input")
		}
		if _, e := compileFlight(model); e == nil {
			t.Fatal("arbitrary child mutation accepted")
		}
		t.Logf("instances=%d composition=%s", instances, id)
	}
	for _, change := range []func(*FlightModel){
		func(m *FlightModel) { m.Version = 2 },
		func(m *FlightModel) { m.Instances = 5 },
		func(m *FlightModel) { m.Window = 3 },
		func(m *FlightModel) { m.FrameVersion = 0 },
	} {
		m := flightModel(4)
		change(&m)
		if _, e := compileFlight(m); e == nil {
			t.Fatal("invalid composition accepted")
		}
	}
}

func TestFlightLaneFullWindowStillCarriesControl(t *testing.T) {
	a, b := flightSources(t)
	flightTransfer(t, a, b)
	flightTransfer(t, b, a)
	var ids [4]uint64
	for i := range ids {
		frame, e := a.lease(512)
		if e != nil {
			t.Fatal(e)
		}
		ids[i] = frame.ID
	}
	stream, e := a.mux.Open(so.Request{Network: so.NetworkUDP, Limits: so.Limits{Window: 16384, MaxBytes: 1 << 20}})
	if e != nil {
		t.Fatal(e)
	}
	defer stream.Release()
	before := a.status()
	lane := &flightLane{ctx: context.Background(), source: a}
	data, eof, e := lane.lease(1024)
	if e != nil || len(data) != 0 || eof || !lane.status().Pending {
		t.Fatal("full window blocked receipt-only control", e)
	}
	if e := lane.commit(); e != nil || a.status() != before {
		t.Fatal("control changed data lease or credit", e)
	}
	if e := a.ack(ids[0]); e != nil {
		t.Fatal(e)
	}
	data, eof, e = lane.lease(1024)
	if e != nil || len(data) <= flightHeader || eof {
		t.Fatal("data did not resume", e)
	}
	frame, e := decodeFlightFrame(data, eof)
	if e != nil || frame.ID != ids[3]+1 {
		t.Fatal("control consumed data sequence", e)
	}
}

func TestFlightDeliveryWaitCancelsWithoutReceipt(t *testing.T) {
	a, b := flightSources(t)
	first, _ := a.lease(512)
	second, _ := a.lease(512)
	body, _ := encodeFlightFrame(second)
	ctx, cancel := context.WithCancel(context.Background())
	lane := &flightLane{ctx: ctx, source: b}
	done := make(chan error, 1)
	go func() { done <- lane.deliverContext(ctx, body, false) }()
	deadline := time.After(time.Second)
	for b.status().Receive.Pending == 0 {
		select {
		case e := <-done:
			t.Fatal("delivery skipped prefix", e)
		case <-deadline:
			t.Fatal("delivery did not buffer")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if b.status().Delivered != 0 || a.status().Mux.LeaseCommitted >= first.ID {
		t.Fatal("cancelled delivery acknowledged data")
	}
	b.finish(context.Canceled)
	if b.status().Receive.Bytes != 0 || b.status().Receive.Pending != 0 {
		t.Fatal("cancelled owner retained completions")
	}
}

func TestFlightFrameBoundsAndEOF(t *testing.T) {
	frame := fw.Frame{ID: 1, Body: make([]byte, maxObject-flightHeader), EOF: true}
	p, e := encodeFlightFrame(frame)
	if e != nil || len(p) != maxObject {
		t.Fatal(e)
	}
	if got, e := decodeFlightFrame(p, true); e != nil || got.ID != frame.ID || !bytes.Equal(got.Body, frame.Body) {
		t.Fatal(e)
	}
	for _, change := range []func([]byte){func(p []byte) { p[0]++ }, func(p []byte) { p[1] = 2 }, func(p []byte) { p[2] = 1 }, func(p []byte) { clear(p[8:16]) }} {
		bad := bytes.Clone(p)
		change(bad)
		if _, e := decodeFlightFrame(bad, true); e == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	if _, e := decodeFlightFrame(p, false); e == nil {
		t.Fatal("EOF disagrees with object")
	}
	if _, e := decodeFlightFrame(append(p, 0), true); e == nil {
		t.Fatal("oversize envelope accepted")
	}
	if _, e := encodeFlightFrame(fw.Frame{ID: 1, Body: make([]byte, maxObject)}); e == nil {
		t.Fatal("envelope overhead omitted from capacity")
	}
}

func FuzzFlightFrame(f *testing.F) {
	f.Add([]byte{}, false)
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 7}, false)
	f.Fuzz(func(t *testing.T, p []byte, eof bool) {
		frame, e := decodeFlightFrame(p, eof)
		if e != nil {
			return
		}
		if len(p) == 0 {
			if frame.ID != 0 || len(frame.Body) != 0 || frame.EOF {
				t.Fatal("control acquired data identity")
			}
			return
		}
		encoded, e := encodeFlightFrame(frame)
		if e != nil || !bytes.Equal(encoded, p) || len(encoded) > maxObject || frame.ID == 0 || frame.EOF != eof {
			t.Fatal("noncanonical flight envelope")
		}
	})
}

func TestFlightFrontendBindingBeforeObjectsAndRollback(t *testing.T) {
	client, server, principal := flightTLSStates(t)
	_, otherTLS, otherPrincipal := flightTLSStates(t)
	p, _ := compileFlight(flightModel(4))
	otherModel, _ := compileFlight(flightModel(2))
	master, _ := p.material(&client)
	var seed [32]byte
	material, _ := p.laneMaterial(master, 0)
	first, _ := http.NewRequest("HEAD", "https://owned.test/"+sessionPrefix("owned-test", seed, material)+"/upload", nil)
	first.TLS = &server
	limits := sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}
	store, _ := newLocalBackend("owned-test", 8)
	defer store.close()
	ensureBucket(context.Background(), store, "owned-test")
	for _, name := range []string{"identity", "model", "connection", "path"} {
		request := first.Clone(context.Background())
		program, identity := p, principal
		switch name {
		case "identity":
			identity = "unlisted"
		case "model":
			program = otherModel
		case "connection":
			request.TLS, identity = &otherTLS, otherPrincipal
		case "path":
			request.URL.Path += "/unknown"
		}
		if group, e := newFlightFront(context.Background(), request, program, seed, identity, "owned-test", store, sm.Config{Limits: limits}); e == nil || group != nil {
			t.Fatal("unbound request accepted", name)
		}
		if st := store.local.Status(); st.Objects != 0 || st.Bytes != 0 {
			t.Fatal("rejected bootstrap created objects", name, st)
		}
	}
	conflictMaterial, _ := p.laneMaterial(master, 2)
	conflict := "/" + sessionPrefix("owned-test", seed, conflictMaterial) + "/upload"
	if _, e := store.must(context.Background(), "fixture", "PUT", conflict, []byte("owned-before"), nil, 200); e != nil {
		t.Fatal(e)
	}
	if group, e := newFlightFront(context.Background(), first, p, seed, principal, "owned-test", store, sm.Config{Limits: limits}); e == nil || group != nil {
		t.Fatal("conflict accepted")
	}
	if st := store.local.Status(); st.Objects != 1 || st.Bytes != len("owned-before") {
		t.Fatal("partial initialization rollback", st)
	}
	if r, e := store.must(context.Background(), "verify", "GET", conflict, nil, nil, 200); e != nil || string(r.body) != "owned-before" {
		t.Fatal("rollback deleted preexisting object", e)
	}
}

type heldFlightWriter struct {
	*httptest.ResponseRecorder
	started, release chan struct{}
}

func (w *heldFlightWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestFlightCleanupWaitsForHTTPHandler(t *testing.T) {
	client, server, principal := flightTLSStates(t)
	p, _ := compileFlight(flightModel(4))
	master, _ := p.material(&client)
	material, _ := p.laneMaterial(master, 0)
	var seed [32]byte
	request, _ := http.NewRequest("GET", "https://owned.test/"+sessionPrefix("owned-test", seed, material)+"/download", nil)
	request.TLS = &server
	request.Body = http.NoBody // Match the non-nil Body guaranteed by net/http servers.
	store, _ := newLocalBackend("owned-test", 8)
	defer store.close()
	ensureBucket(context.Background(), store, "owned-test")
	limits := sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}
	group, e := newFlightFront(context.Background(), request, p, seed, principal, "owned-test", store, sm.Config{Limits: limits})
	if e != nil {
		t.Fatal(e)
	}
	writer := &heldFlightWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	handlerDone := make(chan struct{})
	go func() { defer close(handlerDone); group.ServeHTTP(writer, request) }()
	<-writer.started
	cleanupDone := make(chan bool, 1)
	go func() { cleanupDone <- group.finish(errors.New("test connection interruption")) }()
	select {
	case <-group.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cleanup did not cancel")
	}
	select {
	case <-cleanupDone:
		t.Fatal("cleanup crossed handler ownership barrier")
	case <-time.After(20 * time.Millisecond):
	}
	if st := store.local.Status(); st.Objects != 8 {
		t.Fatal("objects released before handler returned", st)
	}
	close(writer.release)
	<-handlerDone
	if !<-cleanupDone || store.local.Status().Objects != 0 {
		t.Fatal("handler completion did not release objects")
	}
}

type heldFlightHeader struct {
	*httptest.ResponseRecorder
	started chan<- struct{}
	release <-chan struct{}
}

func (w *heldFlightHeader) WriteHeader(code int) {
	w.started <- struct{}{}
	<-w.release
	w.ResponseRecorder.WriteHeader(code)
}

func TestFlightFrontendRejectsNinthActiveRequest(t *testing.T) {
	client, server, principal := flightTLSStates(t)
	p, _ := compileFlight(flightModel(4))
	master, _ := p.material(&client)
	var seed [32]byte
	material, _ := p.laneMaterial(master, 0)
	first, _ := http.NewRequest("HEAD", "https://owned.test/"+sessionPrefix("owned-test", seed, material)+"/upload", nil)
	first.TLS, first.Body = &server, http.NoBody
	store, _ := newLocalBackend("owned-test", 8)
	defer store.close()
	ensureBucket(context.Background(), store, "owned-test")
	limits := sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}
	group, e := newFlightFront(context.Background(), first, p, seed, principal, "owned-test", store, sm.Config{Limits: limits})
	if e != nil {
		t.Fatal(e)
	}
	started, release := make(chan struct{}, 8), make(chan struct{})
	var workers sync.WaitGroup
	defer func() {
		close(release)
		workers.Wait()
		if !group.finish(errors.New("test request overflow")) || store.local.Status().Objects != 0 {
			t.Error("overflow cleanup failed")
		}
	}()
	for _, prefix := range group.prefixes {
		for _, method := range []string{"HEAD", "GET"} {
			path := "/upload"
			if method == "GET" {
				path = "/download"
			}
			r, _ := http.NewRequest(method, "https://owned.test/"+prefix+path, nil)
			r.TLS, r.Body = &server, http.NoBody
			workers.Add(1)
			go func() {
				defer workers.Done()
				group.ServeHTTP(&heldFlightHeader{httptest.NewRecorder(), started, release}, r)
			}()
		}
	}
	for i := 0; i < 8; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("could not hold all allowed handlers")
		}
	}
	rejected := httptest.NewRecorder()
	group.ServeHTTP(rejected, first)
	group.mu.Lock()
	active, maximum := group.active, group.maximum
	group.mu.Unlock()
	if rejected.Code != http.StatusTooManyRequests || active != 8 || maximum != 8 || group.ctx.Err() == nil {
		t.Fatal("request overflow expanded bound or did not cancel", rejected.Code, active, maximum)
	}
}
