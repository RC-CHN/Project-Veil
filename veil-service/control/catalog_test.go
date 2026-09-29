package control

import (
	"encoding/json"
	"testing"
)

func TestCatalogPlatformTransaction(t *testing.T) {
	m, dir := manager(t)
	a, b := namedProfile(t, "a"), namedProfile(t, "b")
	a.Enabled, b.Enabled = true, true
	storeProfile(t, m, a, "", true)
	storeProfile(t, m, b, "", true)
	bRun := m.catalog.entries[b.ID].run
	baseline := m.Handle(Request{Version: Version, Action: "connections_get"})
	var desired catalogFile
	if err := json.Unmarshal(baseline.Config, &desired); err != nil {
		t.Fatal(err)
	}
	desired.Profiles[0].Name = "Updated"
	desired.Profiles[0].Enabled = false
	raw, _ := json.Marshal(desired)
	checked := m.Handle(Request{Version: Version, Action: "connections_validate", Config: raw})
	if checked.Error != nil {
		t.Fatal(checked.Error)
	}
	if m.catalog.profiles[a.ID].Name != "a" || m.catalog.entries[a.ID].run == nil {
		t.Fatal("validation mutated saved or running state")
	}
	wrong := "stale"
	if r := m.Handle(Request{Version: Version, Action: "connections_save", Config: raw, ExpectedRevision: &wrong}); r.Error == nil || r.Error.Code != "conflict" {
		t.Fatal("stale catalog accepted")
	}
	saved := m.Handle(Request{Version: Version, Action: "connections_save", Config: checked.Config, ExpectedRevision: &baseline.Revision})
	if saved.Error != nil {
		t.Fatal(saved.Error)
	}
	if m.catalog.entries[a.ID].run == nil {
		t.Fatal("save stopped a connection")
	}
	applied := m.Handle(Request{Version: Version, Action: "connections_save", Config: checked.Config, ExpectedRevision: &saved.Revision, Apply: true, ID: a.ID})
	if applied.Error != nil {
		t.Fatal(applied.Error)
	}
	if m.catalog.entries[a.ID].run != nil || m.catalog.entries[b.ID].run != bRun {
		t.Fatal("targeted apply affected wrong connection")
	}
	// An invalid replacement is atomic, including deletion of other profiles.
	desired.Profiles = desired.Profiles[:1]
	desired.Profiles[0].RelayID = "missing"
	invalid, _ := json.Marshal(desired)
	if r := m.Handle(Request{Version: Version, Action: "connections_save", Config: invalid, ExpectedRevision: &applied.Revision}); r.Error == nil {
		t.Fatal("missing relay accepted")
	}
	if m.catalog.entries[b.ID].run != bRun {
		t.Fatal("invalid replacement changed runtime")
	}
	m.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.catalog.profiles[a.ID].Name != "Updated" || reopened.catalog.profiles[a.ID].Enabled {
		t.Fatal("catalog did not persist")
	}
}
