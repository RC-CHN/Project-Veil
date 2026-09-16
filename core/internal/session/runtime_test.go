package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	b "veil.local/core/internal/behavior"
)

func advanceEmpty(t *testing.T, s *b.BatchSession, o b.BatchOffer) {
	t.Helper()
	st := s.Status()
	body := streamEncode(o.Sequence, nil, false)
	for i, a := range o.Members {
		var values []b.Value
		switch a.Name {
		case "upload":
			values = []b.Value{st.Registers[0], st.Registers[2], {Number: 0}, {Bytes: body}, {Number: 0}, {Number: o.Sequence}}
		case "download":
			values = []b.Value{st.Registers[1]}
		}
		if e := s.Request(o.Sequence, i, values); e != nil {
			t.Fatal(e)
		}
	}
	for i, a := range o.Members {
		etag := dig([]byte(fmt.Sprintf("\"%032x\"", o.Sequence+uint64(i)+1)))
		values := []b.Value{etag}
		if a.Name == "download" || a.Name == "discover_download" {
			values = append(values, b.Value{Bytes: body}, b.Value{Number: 0}, b.Value{Number: o.Sequence})
		}
		if e := s.Response(o.Sequence, i, 200, values); e != nil {
			t.Fatal(e)
		}
	}
}
func TestPairedRandomCapacityBeforeQueue(t *testing.T) {
	p, e := b.CompileBatch(adaptiveModel())
	if e != nil {
		t.Fatal(e)
	}
	var seed [32]byte
	seed[0] = 1
	seenUp, seenDown := map[int]bool{}, map[int]bool{}
	for n := byte(1); n <= 16; n++ {
		var material [32]byte
		material[0] = n
		left, e := b.NewBatchSession(context.Background(), p, seed, material)
		if e != nil {
			t.Fatal(e)
		}
		right, _ := b.NewBatchSession(context.Background(), p, seed, material)
		for step := 0; step < 4; step++ {
			a, e := left.Plan()
			if e != nil {
				t.Fatal(e)
			}
			bb, e := right.Plan()
			if e != nil || !reflect.DeepEqual(a, bb) {
				t.Fatal("paired choices differ", e)
			}
			if step > 0 {
				up := a.Members[0].RequestSizes[3]
				down := a.Members[1].Replies[0].Sizes[1]
				if up < 40976 || up > 81936 || down < 20496 || down > 81936 {
					t.Fatal(up, down)
				}
				seenUp[up] = true
				seenDown[down] = true
			}
			advanceEmpty(t, left, a)
			advanceEmpty(t, right, bb)
		}
		left.Close()
		right.Close()
	}
	if len(seenUp) < 8 || len(seenDown) < 8 {
		t.Fatal("capacity variation missing", len(seenUp), len(seenDown))
	}
}
func TestBundleValidationAndPrivateFiles(t *testing.T) {
	bundle, e := GenerateBundle()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "model.json")
	if e = WriteBundle(path, bundle); e != nil {
		t.Fatal(e)
	}
	_, seed, p, e := LoadBundle(path)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.ID()) != 64 || seed == ([32]byte{}) {
		t.Fatal("bundle identity")
	}
	if e = WriteBundle(path, bundle); e == nil {
		t.Fatal("existing bundle overwritten")
	}
	os.Chmod(path, 0644)
	if _, _, _, e = LoadBundle(path); e == nil {
		t.Fatal("public seed file accepted")
	}
	os.Chmod(path, 0600)
	bundle.Model.MaxBatches--
	raw, _ := json.Marshal(bundle)
	os.WriteFile(path, raw, 0600)
	if _, _, _, e = LoadBundle(path); e == nil {
		t.Fatal("unvalidated adapter model accepted")
	}
}
func TestPrefixBindingAndResourceConfiguration(t *testing.T) {
	var seed, material [32]byte
	seed[0] = 1
	material[0] = 2
	a := sessionPrefix("owned", seed, material)
	material[0] = 3
	bb := sessionPrefix("owned", seed, material)
	if len(a) != 38 || a == bb {
		t.Fatal(a, bb)
	}
	c := ClientConfig{Version: 1, Listen: "0.0.0.0:1080", ServerURL: "https://localhost:443", Bucket: "owned"}
	if c.defaults() == nil {
		t.Fatal("public unauthenticated SOCKS accepted")
	}
	c.Listen = "127.0.0.1:0"
	c.DialAddress = "server.example:443"
	if c.defaults() == nil {
		t.Fatal("uncontrolled dial_address DNS")
	}
	c.DialAddress = "127.0.0.1:443"
	if e := c.defaults(); e != nil {
		t.Fatal(e)
	}
}

func TestIndependentSessionVectors(t *testing.T) {
	var vector struct {
		ModelID, SeedHex, MaterialHex, Prefix string
		Offers                                []b.BatchOffer
	}
	raw, e := os.ReadFile("../../testdata/object-session-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &vector); e != nil {
		t.Fatal(e)
	}
	p, e := b.CompileBatch(adaptiveModel())
	if e != nil || p.ID() != vector.ModelID {
		t.Fatal("model", e)
	}
	seed, material := [32]byte{1}, [32]byte{2}
	if sessionPrefix("owned", seed, material) != vector.Prefix {
		t.Fatal("prefix")
	}
	s, e := b.NewBatchSession(context.Background(), p, seed, material)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for _, expected := range vector.Offers {
		actual, e := s.Plan()
		if e != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("offer %d: %v != %v, %v", expected.Sequence, actual, expected, e)
		}
		advanceEmpty(t, s, actual)
	}
}
