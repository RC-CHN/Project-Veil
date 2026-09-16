package session

import (
	"context"
	"net/http"
	"testing"
)

func TestLocalBackendUsesRealConditionsAndCleanup(t *testing.T) {
	b, err := newLocalBackend("owned-test", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	ctx := context.Background()
	if err = ensureBucket(ctx, b, "owned-test"); err != nil {
		t.Fatal(err)
	}
	var created [2]bool
	if err = prepareObjects(ctx, b, "owned-test/session", &created); err != nil {
		t.Fatal(err)
	}
	r, err := b.must(ctx, "inspect", "GET", "/owned-test/session/download", nil, nil, 200)
	if err != nil || string(r.body) != string(streamEncode(0, nil, false)) {
		t.Fatal(r, err)
	}
	old := r.header.Get("ETag")
	if _, err = b.must(ctx, "conditional", "PUT", "/owned-test/session/download", []byte("changed"), http.Header{"If-Match": []string{old}}, 200); err != nil {
		t.Fatal(err)
	}
	r, err = b.do(ctx, "stale", "PUT", "/owned-test/session/download", []byte("wrong"), http.Header{"If-Match": []string{old}})
	if err != nil || r.status != 412 {
		t.Fatal(r, err)
	}
	if !deleteObjects(b, "owned-test/session", created) {
		t.Fatal("cleanup failed")
	}
	for _, key := range []string{"upload", "download"} {
		if _, err = b.must(ctx, "inspect_deleted", "HEAD", "/owned-test/session/"+key, nil, nil, 404); err != nil {
			t.Fatal(err)
		}
	}
	st := b.local.Status()
	if st.Objects != 0 || st.Bytes != 0 || !st.BucketExists || st.Closed {
		t.Fatal("cleanup masked by store close", st)
	}
	if wire := b.wire.snapshot(); wire.Read != 0 || wire.Written != 0 || wire.Connections != 0 {
		t.Fatal("local storage dialed network", wire)
	}
	for _, tx := range b.log {
		if tx.Protocol != "local-object-v1" || tx.TLSVersion != 0 {
			t.Fatal(tx)
		}
	}
}
func TestLocalBackendConfigIsExplicit(t *testing.T) {
	base := ServerConfig{Version: 2, BackendMode: "local-object-v1", Listen: "127.0.0.1:0", Bucket: "owned-test", ClientCA: "ca", ClientFingerprints: []string{"fingerprint"}}
	valid := base
	if err := valid.defaults(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ServerConfig){
		"legacy version":        func(c *ServerConfig) { c.Version = 1 },
		"remote url":            func(c *ServerConfig) { c.BackendURL = "https://minio" },
		"remote ca":             func(c *ServerConfig) { c.BackendCA = "ca" },
		"remote access":         func(c *ServerConfig) { c.BackendAccess = "key" },
		"remote secret":         func(c *ServerConfig) { c.BackendSecret = "secret" },
		"unknown":               func(c *ServerConfig) { c.BackendMode = "local-object-v2" },
		"no automatic fallback": func(c *ServerConfig) { c.BackendMode = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			change(&c)
			if c.defaults() == nil {
				t.Fatal("accepted ambiguous backend configuration")
			}
		})
	}
}

func TestServerStartupFailureClosesLocalStore(t *testing.T) {
	b, err := newLocalBackend("owned-test", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	ctx := context.Background()
	if err = ensureBucket(ctx, b, "owned-test"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.must(ctx, "fixture", "PUT", "/owned-test/key", []byte("retained"), nil, 200); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: ServerConfig{Bucket: "owned-test", Listen: "invalid-address-without-port"}, store: b}
	if err = s.Run(ctx); err == nil {
		t.Fatal("expected listen failure")
	}
	if st := b.local.Status(); !st.Closed || st.Objects != 0 || st.Bytes != 0 {
		t.Fatal("startup failure retained local objects", st)
	}
}
