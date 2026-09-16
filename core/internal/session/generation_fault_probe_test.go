package session

// Copied into an extracted, frozen session18 test tree by the probe builder.
// Fault hooks exist only in this authenticated owned-peer fixture.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dg "veil.local/core/internal/datagram"
	d "veil.local/core/internal/destination"
	sl "veil.local/core/internal/streamlink"
	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

type generationFaultTransport struct {
	base           *http.Transport
	outer          net.Conn
	mode           string
	program        *flightProgram
	seed, master   [32]byte
	bucket         string
	armed, tripped atomic.Bool
	mu             sync.Mutex
	evidence       map[string]any
}

func (f *generationFaultTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != "HEAD" || !f.armed.Load() || !f.tripped.CompareAndSwap(false, true) {
		return f.base.RoundTrip(r)
	}
	generation, err := strconv.ParseUint(r.Header.Get("X-Amz-Meta-Generation"), 10, 64)
	if err != nil {
		return nil, err
	}
	lane, nextFingerprint := -1, ""
	for i := 0; i < f.program.instances; i++ {
		material, e := f.program.generationMaterial(f.master, i, generation)
		if e != nil {
			return nil, e
		}
		if r.URL.Path == "/"+sessionPrefix(f.bucket, f.seed, material)+"/upload" {
			lane, nextFingerprint = i, hashHex(material[:])
		}
	}
	if lane < 0 {
		return nil, errors.New("fault request does not identify one model lane")
	}
	f.mu.Lock()
	f.evidence = map[string]any{"mode": f.mode, "started_ns": time.Now().UnixNano(), "path": r.URL.Path,
		"lane": lane, "next_material_fingerprint": nextFingerprint,
		"generation": r.Header.Get("X-Amz-Meta-Generation"), "batches": r.Header.Get("X-Amz-Meta-Batches"),
		"request_receipt": r.Header.Get("X-Amz-Meta-Receipt"), "forwarded": false}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.evidence["ended_ns"] = time.Now().UnixNano(); f.mu.Unlock() }()
	if f.mode == "drop-before-request" {
		f.outer.Close()
		return nil, errors.New("owned fixture disconnected before handoff request")
	}
	if f.mode == "wrong-request-receipt" {
		r = r.Clone(r.Context())
		r.Header.Set("X-Amz-Meta-Receipt", strings.Repeat("0", 64))
	}
	f.mu.Lock()
	f.evidence["forwarded"] = true
	f.mu.Unlock()
	response, err := f.base.RoundTrip(r)
	f.mu.Lock()
	if err != nil {
		f.evidence["transport_error"] = err.Error()
	}
	if response != nil {
		f.evidence["response_status"] = response.StatusCode
		f.evidence["response_receipt"] = response.Header.Get("X-Amz-Meta-Receipt")
	}
	f.mu.Unlock()
	if err != nil {
		return response, err
	}
	if f.mode == "lost-response" {
		response.Body.Close()
		f.outer.Close()
		return nil, errors.New("owned fixture lost completed handoff response")
	}
	if f.mode == "wrong-response-receipt" {
		response.Header = response.Header.Clone()
		response.Header.Set("X-Amz-Meta-Receipt", strings.Repeat("0", 64))
	}
	return response, nil
}

func (f *generationFaultTransport) snapshot() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make(map[string]any, len(f.evidence))
	for k, v := range f.evidence {
		result[k] = v
	}
	return result
}

type generationEchoWriter struct {
	mu      sync.Mutex
	want    []byte
	used    int
	done    chan struct{}
	endedNS int64
}

func (w *generationEchoWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > len(w.want)-w.used || !bytes.Equal(p, w.want[w.used:w.used+len(p)]) {
		return 0, errors.New("unexpected or duplicated application response")
	}
	w.used += len(p)
	if w.used == len(w.want) && w.endedNS == 0 {
		w.endedNS = time.Now().UnixNano()
		close(w.done)
	}
	return len(p), nil
}

type generationProbeApplication struct {
	stream   *sm.Stream
	link     *sl.Link
	writer   *generationEchoWriter
	consumed chan error
	evidence map[string]any
}

func TestGenerationRuntimeHandoffFault(t *testing.T) {
	path, target, mode := os.Getenv("VEIL_GENERATION_FAULT_CONFIG"), os.Getenv("VEIL_GENERATION_FAULT_TARGET"), os.Getenv("VEIL_GENERATION_FAULT_MODE")
	if path == "" {
		t.Skip("requires the owned installed-runtime fixture")
	}
	if ip := net.ParseIP(target); ip == nil || ip.To4() == nil || !ip.IsPrivate() {
		t.Fatal("owned private IPv4 target required")
	}
	switch mode {
	case "healthy", "wrong-request-receipt", "drop-before-request", "lost-response", "wrong-response-receipt":
	default:
		t.Fatal("unsupported fault mode")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var cfg ClientConfig
	if err := ReadPrivateJSON(path, &cfg, 65536); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !client.flight.renewable() || cfg.Window != 65536 || cfg.MaxBytes != 8<<20 || client.flight.instances != 4 {
		t.Fatal("unexpected production limits or model")
	}
	// Accelerate only this instrumented peer's boundary; the installed server
	// and production client binary retain their actual 2048/240 limits.
	client.flight.generationBatches = 2
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", client.dialAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	outer := tls.Client(raw, client.tls.Clone())
	if err := outer.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	state := outer.ConnectionState()
	if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != "h2" {
		t.Fatal("TLS1.3/H2 required")
	}
	master, err := client.flight.material(&state)
	if err != nil {
		t.Fatal(err)
	}
	var dialed atomic.Bool
	var dialAttempts atomic.Uint64
	transport := &http.Transport{ForceAttemptHTTP2: true, MaxConnsPerHost: 1, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: client.tls.Clone(),
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			dialAttempts.Add(1)
			if !dialed.CompareAndSwap(false, true) {
				return nil, errors.New("replacement fixture TLS connection prohibited")
			}
			return outer, nil
		}}
	defer transport.CloseIdleConnections()
	fault := &generationFaultTransport{base: transport, outer: outer, mode: mode, program: client.flight, seed: client.seed, master: master, bucket: cfg.Bucket}
	httpClient := &http.Client{Transport: fault, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect prohibited") }}
	source, err := newFlightMuxSource(ctx, sm.Config{Client: true, EarlyOpen: true, MaxPending: 4, Limits: cfg.Mux.settings(cfg.Window, cfg.MaxBytes)})
	if err != nil {
		t.Fatal(err)
	}
	defer source.finish(errors.New("probe final cleanup"))
	request := so.Request{Address: d.Address{Host: target, Port: 9000}, Limits: so.Limits{Window: cfg.Window, MaxBytes: cfg.MaxBytes}}
	tcp, err := source.mux.OpenFirst(request)
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Release()
	type driveResult struct {
		value flightDriveResult
		err   error
	}
	driven := make(chan driveResult, 1)
	ready := make(chan struct{})
	var readyOnce sync.Once
	go func() {
		value, e := driveFlight(ctx, client.flight, client.seed, &state, httpClient, client.endpoint, cfg.Bucket, source, func() { readyOnce.Do(func() { close(ready) }) })
		driven <- driveResult{value, e}
	}()
	select {
	case <-ready:
	case value := <-driven:
		t.Fatal("driver failed before ready", value.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	udp, err := source.mux.Open(so.Request{Network: so.NetworkUDP, Limits: request.Limits})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Release()
	applications := make([]generationProbeApplication, 0, 2)
	for i, stream := range []*sm.Stream{tcp, udp} {
		result, link, e := stream.WaitResult(ctx)
		if e != nil || result.Code != so.OK {
			t.Fatal("OPEN failed", result, e)
		}
		size, kind := 512, "tcp"
		if i == 1 {
			size, kind = 1200, "udp"
		}
		payload := make([]byte, size)
		if _, e := rand.Read(payload[:16]); e != nil {
			t.Fatal(e)
		}
		for j := 24; j < len(payload); j++ {
			payload[j] = byte((j-24)*131 + 17)
		}
		var wire []byte
		if i == 0 {
			wire = make([]byte, 4+len(payload))
			binary.BigEndian.PutUint32(wire, uint32(len(payload)))
			copy(wire[4:], payload)
		} else {
			wire, e = dg.Encode(dg.Packet{Address: d.Address{Host: target, Port: 9002}, Payload: payload})
			if e != nil {
				t.Fatal(e)
			}
		}
		writer := &generationEchoWriter{want: wire, done: make(chan struct{})}
		consumed := make(chan error, 1)
		go func() { consumed <- link.Consume(ctx, writer, func() error { return nil }) }()
		sum := sha256.Sum256(payload)
		record := map[string]any{"kind": kind, "size": size, "index": 0, "prefix": hex.EncodeToString(payload[:16]), "sha256": hex.EncodeToString(sum[:]), "started_ns": time.Now().UnixNano(), "stream_id": stream.ID()}
		if n, e := link.Write(ctx, wire); e != nil || n != len(wire) {
			t.Fatal("application write", n, e)
		}
		select {
		case <-writer.done:
		case e := <-consumed:
			t.Fatal("application ended before echo", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		writer.mu.Lock()
		record["ended_ns"] = writer.endedNS
		writer.mu.Unlock()
		record["echo_verified"] = true
		applications = append(applications, generationProbeApplication{stream, link, writer, consumed, record})
	}
	armedNS := time.Now().UnixNano()
	if mode == "healthy" {
		for _, app := range applications {
			if e := app.link.Finish(); e != nil {
				t.Fatal(e)
			}
		}
		for _, app := range applications {
			select {
			case <-app.stream.Done():
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if e := <-app.consumed; e != nil {
				t.Fatal("healthy consumer", e)
			}
			app.stream.Release()
		}
		source.mux.Drain()
	} else {
		fault.armed.Store(true)
	}
	var result driveResult
	select {
	case result = <-driven:
	case <-ctx.Done():
		t.Fatal("driver exceeded fixture timeout", ctx.Err())
	}
	if ctx.Err() != nil {
		t.Fatal("fixture parent deadline fired", ctx.Err())
	}
	if mode == "healthy" && result.err != nil {
		t.Fatal("healthy driver", result.err)
	}
	if mode != "healthy" && (result.err == nil || !fault.tripped.Load()) {
		t.Fatal("fault was not reached or rejected")
	}
	source.finish(result.err)
	if mode != "healthy" {
		for _, app := range applications {
			if e := <-app.consumed; e == nil {
				t.Fatal("failed stream ended as success")
			}
			if n, e := app.link.Write(ctx, []byte("must-not-replay")); e == nil || n != 0 {
				t.Fatal("failed stream accepted later bytes", n, e)
			}
			app.stream.Release()
		}
	}
	appEvidence := make([]map[string]any, 0, len(applications))
	for _, app := range applications {
		app.evidence["stream"] = app.stream.Status()
		appEvidence = append(appEvidence, app.evidence)
	}
	after := source.status()
	if !after.Closed || after.Mux.Active != 0 || after.Mux.ReservedWindow != 0 || after.Receive.Bytes != 0 || after.Receive.Pending != 0 {
		t.Fatal("probe resources not released", after)
	}
	if mode != "healthy" {
		var failed uint64
		for _, lane := range result.value.Event.Lanes {
			failed += lane.Handoff.Failed
		}
		if failed == 0 {
			t.Fatal("missing failed handoff accounting")
		}
	}
	if !dialed.Load() {
		t.Fatal("no outer connection")
	}
	record := map[string]any{"kind": "generation-fault-result", "passed": true, "mode": mode, "started_ns": started.UnixNano(), "armed_ns": armedNS,
		"ended_ns": time.Now().UnixNano(), "tls_version": state.Version, "alpn": state.NegotiatedProtocol, "successful_tls_connections": 1,
		"transport_dial_attempts": dialAttempts.Load(), "model_id": client.flight.id, "prefix": sessionPrefix(cfg.Bucket, client.seed, master),
		"probe_generation_batches": 2, "production_runtime_changed": false, "applications": appEvidence, "fault": fault.snapshot(),
		"driver_error": errorText(result.err), "flight": result.value.Event, "source_after": after, "application_retries": 0}
	if e := json.NewEncoder(os.Stdout).Encode(record); e != nil && !errors.Is(e, io.ErrClosedPipe) {
		t.Fatal(e)
	}
}
