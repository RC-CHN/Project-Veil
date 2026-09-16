package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlightBundleRuntimeVersionIsolation(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 5} {
		path := filepath.Join(t.TempDir(), "model.json")
		var e error
		if version >= 3 {
			bundle, err := GenerateFlightBundle(4)
			if version == 4 {
				bundle, err = GenerateEarlyOpenFlightBundle(4)
			}
			if version == 5 {
				bundle, err = GenerateRenewingFlightBundle(4)
			}
			if err != nil {
				t.Fatal(err)
			}
			e = WriteFlightBundle(path, bundle)
		} else {
			bundle, err := GenerateBundle()
			if version == 2 {
				bundle, err = GenerateMuxBundle()
			}
			if err != nil {
				t.Fatal(err)
			}
			e = WriteBundle(path, bundle)
		}
		if e != nil {
			t.Fatal(e)
		}
		id, e := CheckBundle(path)
		if e != nil {
			t.Fatal(e)
		}
		for _, configVersion := range []int{0, 1, 2, 3, 4, 5, 6} {
			_, p, f, err := loadRuntimeModel(path, configVersion)
			if (err == nil) != (version == configVersion) {
				t.Fatalf("bundle=%d config=%d: %v", version, configVersion, err)
			}
			if err == nil && runtimeModelID(p, f) != id {
				t.Fatal("composition ID lost")
			}
		}
		if version == 3 {
			if id != "2c2c32d0a8a7a7e1f5242887f000e17783f8747e8753caff28d91848b7c95ab1" {
				t.Fatal(id)
			}
			if _, _, _, e = LoadBundle(path); e == nil {
				t.Fatal("legacy loader accepted composition")
			}
		}
	}
}

func TestFlightBundleRejectsMalformedAndOverwrite(t *testing.T) {
	for _, n := range []int{-1, 0, 5} {
		if _, e := GenerateFlightBundle(n); e == nil {
			t.Fatal("invalid instances", n)
		}
	}
	bundle, e := GenerateFlightBundle(4)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "model.json")
	if e = WriteFlightBundle(path, bundle); e != nil {
		t.Fatal(e)
	}
	if e = WriteFlightBundle(path, bundle); e == nil {
		t.Fatal("overwrote model")
	}
	st, e := os.Stat(path)
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, e)
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	for name, change := range map[string]func(map[string]any){
		"seed":     func(v map[string]any) { v["SeedHex"] = "00" },
		"protocol": func(v map[string]any) { v["InnerProtocol"] = "streammux-v1" },
		"unknown":  func(v map[string]any) { v["Unknown"] = 1 },
		"window":   func(v map[string]any) { v["Model"].(map[string]any)["Window"] = 3 },
		"child":    func(v map[string]any) { v["Model"].(map[string]any)["Child"] = adaptiveModel() },
	} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			if e := json.Unmarshal(data, &v); e != nil {
				t.Fatal(e)
			}
			change(v)
			p, e := json.Marshal(v)
			if e != nil {
				t.Fatal(e)
			}
			name := filepath.Join(t.TempDir(), "bad.json")
			if e = os.WriteFile(name, p, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = CheckBundle(name); e == nil {
				t.Fatal("malformed bundle accepted")
			}
		})
	}
}

func TestFlightRuntimeConfigBounds(t *testing.T) {
	c := ClientConfig{Version: 3, Listen: "127.0.0.1:0", ServerURL: "https://owned.test", Bucket: "owned-test"}
	if e := c.defaults(); e != nil {
		t.Fatal(e)
	}
	if c.Window != 65536 || c.Mux.Streams != 8 || c.MaxCarriers != 2 {
		t.Fatal(c)
	}
	bundle, e := GenerateFlightBundle(4)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "model.json")
	if e = WriteFlightBundle(path, bundle); e != nil {
		t.Fatal(e)
	}
	s := ServerConfig{Version: 3, Listen: "127.0.0.1:0", ModelFile: path, Bucket: "owned-test", ClientCA: "ca", ClientFingerprints: []string{"fingerprint"}, BackendMode: "local-object-v1", MaxSessions: 9}
	if e = s.defaults(); e != nil {
		t.Fatal(e)
	}
	if _, e = NewServer(s, nil); e == nil || !strings.Contains(e.Error(), "object capacity") {
		t.Fatal("missing early object bound", e)
	}
	s.BackendMode, s.BackendAccess, s.BackendSecret = "remote-s3", "access", "secret"
	if e = s.defaults(); e == nil {
		t.Fatal("remote flight backend accepted")
	}
}
