package session

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"

	b "veil.local/core/internal/behavior"
)

func TestBulkProfileExactWhitelistAndLegacyIdentity(t *testing.T) {
	old, e := b.CompileBatch(adaptiveModel())
	if e != nil || old.ID() != "b4619ff0d7b0c1e89009ecdb67bfd1475418c090f6364293823eedb7e7ddc6f2" {
		t.Fatal("legacy model changed", e)
	}
	bulk, e := b.CompileBatch(bulk192Model())
	if e != nil || bulk.ID() != "4a8f1caae580d78ac6c40ea45cde72d7b432665a8272e406d0a1facc84cc6b87" {
		t.Fatal("bulk model not independently identified", e)
	}
	for _, profile := range []string{ProfileAdaptive64, ProfileBulk192} {
		bundle, e := GenerateMuxBundleProfile(profile)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(t.TempDir(), "model.json")
		if e = WriteBundle(path, bundle); e != nil {
			t.Fatal(e)
		}
		_, _, p, e := LoadBundle(path)
		if e != nil || profile == ProfileBulk192 && p.ID() != bulk.ID() || profile == ProfileAdaptive64 && p.ID() != old.ID() {
			t.Fatal("profile selection/load", e)
		}
	}
	if _, e = GenerateMuxBundleProfile("unreviewed"); e == nil {
		t.Fatal("unknown profile accepted")
	}
	for _, mutation := range []string{"capacity", "dependency", "single"} {
		bundle, _ := GenerateMuxBundleProfile(ProfileBulk192)
		switch mutation {
		case "capacity":
			bundle.Model.Stages[1].Actions[0].Request[3].Max++
		case "dependency":
			bundle.Model.Stages[1].Actions[1].ResponseAfter = nil
		case "single":
			bundle.Version, bundle.InnerProtocol = 1, ""
		}
		path := filepath.Join(t.TempDir(), "model.json")
		if e = WriteBundle(path, bundle); e != nil {
			t.Fatal(e)
		}
		if _, _, _, e = LoadBundle(path); e == nil {
			t.Fatal("unreviewed model accepted", mutation)
		}
	}
}

func TestBulkPairedBudgetsAndCodecBounds(t *testing.T) {
	program, e := b.CompileBatch(bulk192Model())
	if e != nil {
		t.Fatal(e)
	}
	var seed [32]byte
	seed[0] = 1
	seen := map[int]bool{}
	for n := byte(1); n <= 16; n++ {
		var material [32]byte
		material[0] = n
		left, _ := b.NewBatchSession(context.Background(), program, seed, material)
		right, _ := b.NewBatchSession(context.Background(), program, seed, material)
		for i := 0; i < 4; i++ {
			a, e := left.Plan()
			z, f := right.Plan()
			if e != nil || f != nil || !reflect.DeepEqual(a, z) {
				t.Fatal("paired bulk choices diverged", e, f)
			}
			if i > 0 {
				up, down := a.Members[0].RequestSizes[3], a.Members[1].Replies[0].Sizes[1]
				if up < 163856 || up > 245776 || down < 122896 || down > 245776 || max(up, down) > maxObject {
					t.Fatal("bulk object escaped bound", up, down)
				}
				seen[up] = true
				for _, budget := range []int{up, down} {
					data := bytes.Repeat([]byte{17}, (budget-16)*4/5)
					encoded := streamEncode(a.Sequence, data, false)
					seq, decoded, eof, e := streamDecode(encoded)
					if e != nil || len(encoded) > budget || seq != a.Sequence || eof || !bytes.Equal(decoded, data) {
						t.Fatal("bulk codec roundtrip/bound", e)
					}
				}
			}
			advanceEmpty(t, left, a)
			advanceEmpty(t, right, z)
		}
		left.Close()
		right.Close()
	}
	if len(seen) < 16 {
		t.Fatal("bulk connection randomness missing")
	}
	if _, _, _, e := streamDecode(make([]byte, maxObject+1)); e == nil {
		t.Fatal("absolute object limit relaxed")
	}
}

func TestModelProfileMismatchRejectedBeforeAdmission(t *testing.T) {
	state, _ := muxTLSStates(t)
	old, _ := b.CompileBatch(adaptiveModel())
	bulk, _ := b.CompileBatch(bulk192Model())
	var seed [32]byte
	seed[0] = 11
	for _, programs := range [][2]*b.BatchProgram{{old, bulk}, {bulk, old}} {
		clientMaterial, e := runtimeMaterial(&state, programs[0].ID(), 2)
		if e != nil {
			t.Fatal(e)
		}
		server := &Server{program: programs[1], seed: seed, cfg: ServerConfig{Version: 2, Bucket: "owned"}}
		conn := &serverConnection{server: server, ctx: context.Background()}
		request := &http.Request{TLS: &state, Method: "HEAD", URL: &url.URL{Path: "/" + sessionPrefix("owned", seed, clientMaterial) + "/upload"}}
		if e = conn.initialize(request); e == nil || e.Error() != "bootstrap path or operation" || conn.release != nil || server.stats.sessions.Load() != 0 {
			t.Fatal("mismatched model reached admission/backend", e)
		}
	}
}
