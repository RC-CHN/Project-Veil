package session

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	b "veil.local/core/internal/behavior"
)

const flightGenerationBatches = 2048
const flightGenerationAge = 240 * time.Second

// The gate joins the complete old HTTP handlers, including response writes and
// deferred observations, before any model, material or object is replaced.
type flightGeneration struct {
	gate   sync.RWMutex
	number uint64 // Protected by flightFront.mu, including routing snapshots.
}

func renewingFlightModel(instances int) FlightModel {
	m := earlyOpenFlightModel(instances)
	m.Version = 4
	return m
}

func (p *flightProgram) renewable() bool { return p != nil && p.version == 4 }

func (p *flightProgram) generationMaterial(master [32]byte, lane int, generation uint64) ([32]byte, error) {
	var out [32]byte
	if !p.renewable() || lane < 0 || lane >= p.instances {
		return out, errors.New("flight generation model or lane")
	}
	h := hmac.New(sha256.New, master[:])
	h.Write([]byte("veil-flight-generation-1\x00"))
	var number [12]byte
	binary.BigEndian.PutUint32(number[:4], uint32(lane))
	binary.BigEndian.PutUint64(number[4:], generation)
	h.Write(number[:])
	copy(out[:], h.Sum(nil))
	return out, nil
}

type flightHandoff struct {
	generation, batches       uint64
	upload, download, receipt string
}

func (h flightHandoff) headers() http.Header {
	return http.Header{
		"X-Amz-Meta-Generation": {strconv.FormatUint(h.generation, 10)},
		"X-Amz-Meta-Batches":    {strconv.FormatUint(h.batches, 10)},
		"If-Match":              {h.upload}, "If-None-Match": {h.download},
		"X-Amz-Meta-Receipt": {h.receipt},
	}
}

func checkFlightHandoff(r *http.Request, next uint64, st b.BatchStatus) (flightHandoff, error) {
	var h flightHandoff
	bad := errors.New("flight generation handoff state or receipt")
	if r == nil || r.URL == nil || r.Method != "HEAD" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawQuery != "" || r.URL.RawPath != "" || r.URL.ForceQuery || r.Header.Get("Range") != "" || st.Closed || st.State != 1 || st.Completed == 0 || st.Completed > flightGenerationBatches || len(st.MemberPhases) != 0 || st.PendingRequestBytes != 0 || st.PendingWrites != 0 || len(st.Registers) != 6 || next == 0 {
		return h, bad
	}
	for _, key := range []string{"X-Amz-Meta-Generation", "X-Amz-Meta-Batches", "If-Match", "If-None-Match", "X-Amz-Meta-Receipt"} {
		if len(r.Header.Values(key)) != 1 {
			return h, bad
		}
	}
	h = flightHandoff{generation: next, batches: st.Completed, upload: r.Header.Get("If-Match"), download: r.Header.Get("If-None-Match"), receipt: r.Header.Get("X-Amz-Meta-Receipt")}
	if r.Header.Get("X-Amz-Meta-Generation") != strconv.FormatUint(next, 10) || r.Header.Get("X-Amz-Meta-Batches") != strconv.FormatUint(st.Completed, 10) || !objectETag.MatchString(h.upload) || !objectETag.MatchString(h.download) || !bytes.Equal(dig([]byte(h.upload)).Bytes, st.Registers[0].Bytes) || !bytes.Equal(dig([]byte(h.download)).Bytes, st.Registers[1].Bytes) || h.receipt != hex.EncodeToString(st.Registers[2].Bytes) || len(st.Registers[2].Bytes) != sha256.Size {
		return flightHandoff{}, bad
	}
	return h, nil
}

// renewLane is called with this lane's gate held exclusively. No old response
// writer or normal request can still access its VM or native object pair.
func (g *flightFront) renewLane(ctx context.Context, r *http.Request, i int, generation uint64, prefix string) (flightHandoff, error) {
	var empty flightHandoff
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	f := g.fronts[i]
	f.mu.Lock()
	defer f.mu.Unlock()
	master, err := g.program.material(r.TLS)
	if err != nil || master != g.master || generation == math.MaxUint64 || f.session == nil {
		return empty, errors.New("flight generation TLS or phase")
	}
	h, err := checkFlightHandoff(r, generation+1, f.session.Status())
	if err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 {
			return empty, errors.New("flight generation handoff body")
		}
	}
	// The checked receipt settles only the last outer download lease. The
	// consumer's CREDIT is exclusively carried by the unchanged inner Mux.
	if f.output.status().Pending {
		if err := f.output.commit(); err != nil {
			return empty, err
		}
	}
	if !deleteObjects(g.store, f.bucket, g.created[i]) {
		return empty, errors.New("flight generation old object cleanup")
	}
	// Publish cleanup ownership before attempting either new write. A failed
	// or uncertain creation is cleaned by the carrier owner after handler join.
	g.mu.Lock()
	g.prefixes[i], g.created[i] = prefix, [2]bool{}
	g.mu.Unlock()
	if err := prepareObjects(ctx, g.store, prefix, &g.created[i]); err != nil {
		return empty, err
	}
	material, err := g.program.generationMaterial(master, i, generation+1)
	if err != nil {
		return empty, err
	}
	f.session.Close()
	f.session = nil
	f.offer, f.claimed = b.BatchOffer{}, nil
	f.bucket, f.material, f.mode = prefix, material, 0
	f.generation = generation + 1
	f.materialFor = func(cs *tls.ConnectionState) ([32]byte, error) {
		m, err := g.program.material(cs)
		if err != nil || m != master {
			return [32]byte{}, errors.New("flight generation belongs to different TLS connection")
		}
		return material, nil
	}
	g.mu.Lock()
	g.generations[i].number++
	g.mu.Unlock()
	return h, nil
}

func writeFlightHandoff(w http.ResponseWriter, h flightHandoff) {
	for _, key := range []string{"X-Amz-Meta-Generation", "X-Amz-Meta-Batches", "X-Amz-Meta-Receipt"} {
		w.Header().Set(key, h.headers().Get(key))
	}
	w.WriteHeader(http.StatusNoContent)
}

func requestFlightHandoff(ctx context.Context, program *flightProgram, seed, master [32]byte, lane int, generation uint64, st b.BatchStatus, value adaptiveDriveResult, client *http.Client, endpoint, bucket string) ([32]byte, error) {
	var empty [32]byte
	if generation == math.MaxUint64 || len(st.Registers) != 6 || !value.paused {
		return empty, errors.New("flight generation handoff phase or overflow")
	}
	material, err := program.generationMaterial(master, lane, generation+1)
	if err != nil {
		return empty, err
	}
	h := flightHandoff{generation: generation + 1, batches: st.Completed, upload: value.upETag, download: value.downETag, receipt: hex.EncodeToString(st.Registers[2].Bytes)}
	r, err := http.NewRequestWithContext(ctx, "HEAD", endpoint+"/"+sessionPrefix(bucket, seed, material)+"/upload", nil)
	if err != nil {
		return empty, err
	}
	r.Header = h.headers()
	if _, err := checkFlightHandoff(r, generation+1, st); err != nil {
		return empty, err
	}
	response, err := client.Do(r)
	if err != nil {
		return empty, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.ContentLength > 0 || len(response.TransferEncoding) != 0 {
		return empty, errors.New("flight generation handoff response")
	}
	for _, key := range []string{"X-Amz-Meta-Generation", "X-Amz-Meta-Batches", "X-Amz-Meta-Receipt"} {
		if len(response.Header.Values(key)) != 1 || response.Header.Get(key) != r.Header.Get(key) {
			return empty, errors.New("flight generation handoff response binding")
		}
	}
	return material, nil
}

func (g *flightFront) serveGeneration(w http.ResponseWriter, r *http.Request) {
	index, next := -1, false
	var generation uint64
	var prefix string
	g.mu.Lock()
	if r.URL != nil {
		for i, current := range g.prefixes {
			n := g.generations[i].number
			if r.URL.Path == "/"+current+"/upload" || r.URL.Path == "/"+current+"/download" {
				index, generation, prefix = i, n, current
				break
			}
			if n != math.MaxUint64 {
				material, _ := g.program.generationMaterial(g.master, i, n+1)
				future := sessionPrefix(g.bucket, g.seed, material)
				if r.Method == "HEAD" && r.URL.Path == "/"+future+"/upload" {
					index, generation, prefix, next = i, n, future, true
					break
				}
			}
		}
	}
	g.mu.Unlock()
	fail := func() { g.cancel(); http.Error(w, "generation request rejected", http.StatusBadRequest) }
	if index < 0 {
		fail()
		return
	}
	started := time.Now()
	gate := &g.generations[index].gate
	if next {
		gate.Lock()
		defer gate.Unlock()
	} else {
		gate.RLock()
		defer gate.RUnlock()
	}
	g.mu.Lock()
	valid := !g.closing && g.ctx.Err() == nil && generation == g.generations[index].number
	g.mu.Unlock()
	if !valid {
		fail()
		return
	}
	if !next {
		g.fronts[index].ServeHTTP(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(g.ctx, cancel)
	defer cancel()
	defer stop()
	h, err := g.renewLane(ctx, r, index, generation, prefix)
	g.fronts[index].mu.Lock()
	g.fronts[index].handoff.add(started, err)
	g.fronts[index].mu.Unlock()
	if err != nil {
		fail()
		return
	}
	writeFlightHandoff(w, h)
}
