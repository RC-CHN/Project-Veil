package session

import (
	"path/filepath"
	"testing"
)

func TestMuxBundleVersionIsolation(t *testing.T) {
	for _, version := range []int{1, 2} {
		bundle, err := GenerateBundle()
		if version == 2 {
			bundle, err = GenerateMuxBundle()
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "model.json")
		if err = WriteBundle(path, bundle); err != nil {
			t.Fatal(err)
		}
		loaded, _, program, err := LoadBundle(path)
		if err != nil || program == nil {
			t.Fatal(err)
		}
		for _, configVersion := range []int{0, 1, 2, 3} {
			if (checkInnerBundle(loaded, configVersion) == nil) != (configVersion == version) {
				t.Fatalf("bundle %d accepted configuration %d", version, configVersion)
			}
		}
		for _, inner := range []string{"streammux-v2", "single-v1"} {
			bundle.InnerProtocol = inner
			path = filepath.Join(t.TempDir(), "model.json")
			if err = WriteBundle(path, bundle); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err = LoadBundle(path); err == nil {
				t.Fatal("unknown inner protocol accepted")
			}
		}
	}
}

func TestMuxConfigResourceBounds(t *testing.T) {
	base := ClientConfig{Version: 2, Listen: "127.0.0.1:0", ServerURL: "https://localhost", Bucket: "owned-test"}
	valid := base
	if err := valid.defaults(); err != nil {
		t.Fatal(err)
	}
	if valid.Window != 65536 || valid.Mux.Streams != 8 || valid.MaxCarriers != 2 {
		t.Fatal(valid)
	}
	for name, change := range map[string]func(*ClientConfig){
		"window product":       func(c *ClientConfig) { c.Mux.Streams = 32 },
		"too many streams":     func(c *ClientConfig) { c.Mux.Streams = 33 },
		"opened ids":           func(c *ClientConfig) { c.Mux.Opened = 4097 },
		"connection budget":    func(c *ClientConfig) { c.Mux.ConnectionBytes = 4095 },
		"short idle":           func(c *ClientConfig) { c.Mux.CarrierIdleMS = 99 },
		"long idle":            func(c *ClientConfig) { c.Mux.CarrierIdleMS = 60001 },
		"carrier bound":        func(c *ClientConfig) { c.MaxCarriers = 9 },
		"carrier versus local": func(c *ClientConfig) { c.MaxCarriers = 3; c.MaxConnections = 2 },
		"legacy mux":           func(c *ClientConfig) { c.Version = 1; c.Mux.Streams = 1 },
		"legacy carrier":       func(c *ClientConfig) { c.Version = 1; c.MaxCarriers = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			change(&c)
			if c.defaults() == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	server := ServerConfig{Version: 2, Listen: "127.0.0.1:0", Bucket: "owned-test", ClientCA: "ca", ClientFingerprints: []string{"fingerprint"}, BackendAccess: "access", BackendSecret: "secret"}
	if err := server.defaults(); err != nil {
		t.Fatal(err)
	}
	if server.MaxActiveStreams != 64 || server.MaxActiveStreamsPerIdentity != 16 {
		t.Fatal(server)
	}
	for _, pair := range [][2]int{{257, 1}, {1, 2}, {-1, 1}, {1, -1}} {
		c := server
		c.MaxActiveStreams = pair[0]
		c.MaxActiveStreamsPerIdentity = pair[1]
		if c.defaults() == nil {
			t.Fatal("accepted global admission", pair)
		}
	}
}
