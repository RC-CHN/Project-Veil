package main

import (
	"bytes"
	"encoding/base64"
	"net"
	"testing"
	"veil-desktop/internal/systemproxy"
	"veil-service/control"
	"veil/core"
	"veil/service"
)

func catalogInlet(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func catalogProfile(t *testing.T, id string) control.Profile {
	t.Helper()
	return control.Profile{ID: id, Name: id, Kind: "connection", Enabled: true,
		Config: service.Config{Role: "client", Server: "127.0.0.1:9", Secret: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), TLS: core.TLSConfig{Mode: "tls", ServerName: "test.example"}},
		Inlets: []service.Inlet{{Protocol: "mixed", Listen: catalogInlet(t)}}}
}

func TestCatalogSystemProxySelectionAndPendingEdits(t *testing.T) {
	a, backend := proxyApp(t)
	first, second := catalogProfile(t, "first"), catalogProfile(t, "second")
	save := func(p control.Profile, rev string, apply bool) string {
		t.Helper()
		r := a.Request(control.Request{Version: 1, Action: "connection_save", Profile: &p, ExpectedRevision: &rev, Apply: apply})
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		return r.Revision
	}
	revision := save(first, "", true)
	secondRevision := save(second, "", true)
	if err := a.SetSystemProxyConnection(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSystemProxyMode("auto"); err != nil {
		t.Fatal(err)
	}
	if backend.current["server"] != first.Inlets[0].Listen {
		t.Fatal("wrong selected endpoint")
	}
	// Pending SOCKS-only settings cannot redirect the running HTTP system proxy.
	first.Inlets = []service.Inlet{{Protocol: "socks", Listen: catalogInlet(t)}}
	revision = save(first, revision, false)
	if err := a.SetSystemProxyConnection(first.ID); err != nil {
		t.Fatal("running inlet ignored", err)
	}
	if backend.current["server"] == first.Inlets[0].Listen {
		t.Fatal("used saved rather than active inlet")
	}
	if r := a.Request(control.Request{Version: 1, Action: "connection_stop", ID: second.ID, ExpectedRevision: &secondRevision}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if backend.current["server"] == "original" {
		t.Fatal("unrelated stop restored selected proxy")
	}
	if r := a.Request(control.Request{Version: 1, Action: "connection_delete", ID: first.ID, ExpectedRevision: &revision}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if backend.current["server"] != "original" || a.SystemProxy().Connection != "" {
		t.Fatal("deletion left a dangling system proxy")
	}
}

func TestCatalogSelectionPersistsAndExternalChangeSurvives(t *testing.T) {
	a, backend := proxyApp(t)
	p := catalogProfile(t, "first")
	rev := ""
	r := a.Request(control.Request{Version: 1, Action: "connection_save", Profile: &p, ExpectedRevision: &rev, Apply: true})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if err := a.SetSystemProxyConnection(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSystemProxyMode("auto"); err != nil {
		t.Fatal(err)
	}
	backend.current = systemproxy.Snapshot{"server": "external"}
	r = a.Request(control.Request{Version: 1, Action: "connection_stop", ID: p.ID, ExpectedRevision: &r.Revision})
	if r.Error != nil || backend.current["server"] != "external" || a.SystemProxy().Connection != p.ID {
		t.Fatal("stop changed external proxy or lost selection", r.Error)
	}
}
