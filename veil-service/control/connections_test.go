package control

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"veil/service"
)

func freeAddress(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func namedProfile(t *testing.T, id string) Profile {
	var cfg service.Config
	if err := json.Unmarshal(profile(t, ""), &cfg); err != nil {
		t.Fatal(err)
	}
	return Profile{ID: id, Name: id, Kind: "connection", Config: cfg, Inlets: []service.Inlet{{Protocol: "mixed", Listen: freeAddress(t)}}}
}
func storeProfile(t *testing.T, m *Manager, p Profile, rev string, apply bool) Response {
	t.Helper()
	r := m.Handle(Request{Version: Version, Action: "connection_save", Profile: &p, ExpectedRevision: &rev, Apply: apply})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	return r
}
func TestConnectionIsolationPersistenceAndRedaction(t *testing.T) {
	m, dir := manager(t)
	a, b := namedProfile(t, "la"), namedProfile(t, "tokyo")
	a.Enabled = true
	b.Enabled = true
	ar := storeProfile(t, m, a, "", true)
	storeProfile(t, m, b, "", true)
	original := m.catalog.entries[b.ID].run
	own := m.catalog.entries[a.ID].run
	snapshot := m.Handle(Request{Version: Version, Action: "connections"})
	data, _ := json.Marshal(snapshot)
	if strings.Contains(string(data), a.Config.Secret) || strings.Contains(string(data), "\"config\"") {
		t.Fatal("status leaked credentials")
	}
	a.Name = "Renamed"
	storeProfile(t, m, a, ar.Revision, true)
	if m.catalog.entries[a.ID].run != own {
		t.Fatal("rename restarted the connection")
	}
	if m.catalog.entries[b.ID].run != original {
		t.Fatal("other connection restarted")
	}
	if r := m.Handle(Request{Version: Version, Action: "connection_save", Profile: &a, ExpectedRevision: &ar.Revision}); r.Error == nil || r.Error.Code != "conflict" {
		t.Fatal("stale update accepted")
	}
	list := m.Handle(Request{Version: Version, Action: "connections"})
	var rev string
	for _, s := range list.Connections {
		if s.ID == a.ID {
			rev = s.Revision
		}
	}
	if r := m.Handle(Request{Version: Version, Action: "connection_stop", ID: a.ID, ExpectedRevision: &rev}); r.Error != nil {
		t.Fatal(r.Error)
	}
	if m.catalog.entries[a.ID].run != nil || m.catalog.entries[b.ID].run != original {
		t.Fatal("stop affected wrong connection")
	}
	m.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.StartConnections()
	if reopened.catalog.entries[b.ID].run == nil || reopened.catalog.profiles[a.ID].Enabled {
		t.Fatal("enabled state did not survive restart")
	}
	checkConfigPrivate(t, filepath.Join(dir, "connections.json"))
}
func TestConnectionProfileOwnership(t *testing.T) {
	m, _ := manager(t)
	p := namedProfile(t, "owned")
	address := p.Inlets[0].Listen
	storeProfile(t, m, p, "", false)
	p.Inlets[0].Listen = "127.0.0.1:1"
	got := m.Handle(Request{Version: Version, Action: "connection_get", ID: p.ID})
	if got.Profile.Inlets[0].Listen != address {
		t.Fatal("save retained caller-owned listeners")
	}
	got.Profile.Inlets[0].Listen = "127.0.0.1:2"
	list := m.Handle(Request{Version: Version, Action: "connections"})
	if list.Connections[0].Inlets[0].Listen != address {
		t.Fatal("get exposed stored listeners")
	}
	list.Connections[0].Inlets[0].Listen = "127.0.0.1:3"
	if m.catalog.profiles[p.ID].Inlets[0].Listen != address {
		t.Fatal("status exposed stored listeners")
	}
}
func TestConnectionPortConflictsAndRelayReferences(t *testing.T) {
	m, _ := manager(t)
	a := namedProfile(t, "a")
	storeProfile(t, m, a, "", false)
	b := namedProfile(t, "b")
	b.Inlets = a.Inlets
	empty := ""
	r := m.Handle(Request{Version: Version, Action: "connection_save", Profile: &b, ExpectedRevision: &empty})
	if r.Error == nil {
		t.Fatal("duplicate port allowed")
	}
	b.Inlets = []service.Inlet{{Protocol: "mixed", Listen: "127.0.0.1:0"}}
	if m.Handle(Request{Version: Version, Action: "connection_save", Profile: &b, ExpectedRevision: &empty}).Error == nil {
		t.Fatal("ephemeral user listener accepted")
	}
	relay := namedProfile(t, "relay")
	relay.Kind = "relay"
	relay.Inlets = nil
	rr := storeProfile(t, m, relay, "", false)
	b.Inlets = nil
	b.RelayID = relay.ID
	storeProfile(t, m, b, "", false)
	if r := m.Handle(Request{Version: Version, Action: "connection_delete", ID: relay.ID, ExpectedRevision: &rr.Revision}); r.Error == nil || r.Error.Code != "in_use" {
		t.Fatal("in-use relay removed")
	}
	b.RelayID = "missing"
	if err := m.catalog.validate(&b); err == nil {
		t.Fatal("missing relay accepted")
	}
}
func TestLegacyConnectionMigration(t *testing.T) {
	m, _ := manager(t)
	perform(t, m, "save", profile(t, ""))
	if err := m.MigrateConnection(); err != nil {
		t.Fatal(err)
	}
	if len(m.catalog.profiles) != 1 || m.catalog.profiles["default"].Config.Secret != m.cfg.Secret {
		t.Fatal("migration lost configuration")
	}
	if err := m.MigrateConnection(); err != nil {
		t.Fatal(err)
	}
}

func TestSlowProbeDoesNotBlockStatus(t *testing.T) {
	m, _ := manager(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	p := namedProfile(t, "slow")
	p.Config.Server = ln.Addr().String()
	p.Inlets = nil
	storeProfile(t, m, p, "", false)
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := ln.Accept()
		if e == nil {
			accepted <- c
		}
	}()
	done := make(chan Response, 1)
	go func() { done <- m.Handle(Request{Version: Version, Action: "connection_test", ID: p.ID}) }()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not use configured tunnel")
	}
	defer conn.Close()
	status := make(chan Response, 1)
	go func() { status <- m.Handle(Request{Version: Version, Action: "connections"}) }()
	select {
	case <-status:
	case <-time.After(time.Second):
		conn.Close()
		t.Fatal("probe blocked status")
	}
	conn.Close()
	select {
	case r := <-done:
		if r.Probe == nil || r.Probe.OK {
			t.Fatal("failed TLS reported successful probe")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failed probe did not finish")
	}
}
