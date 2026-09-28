package control

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortableConnectionBundle(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, append(cert, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("must not export")})...), 0600); err != nil {
		t.Fatal(err)
	}
	m, _ := manager(t)
	relay := namedProfile(t, "relay")
	relay.Kind, relay.Inlets = "relay", nil
	relay.Config.TLS.CAFile = ca
	storeProfile(t, m, relay, "", false)
	p := namedProfile(t, "route")
	p.RelayID = relay.ID
	storeProfile(t, m, p, "", false)
	bundle := m.Handle(Request{Version: Version, Action: "connection_export", ID: p.ID})
	if bundle.Error != nil {
		t.Fatal(bundle.Error)
	}
	if bundle.Relay == nil || bundle.Relay.Config.TLS.CAFile != "" || bundle.Relay.Config.TLS.CAPEM != string(cert) {
		t.Fatal("export did not embed only the trust certificate")
	}
	if err := os.Remove(ca); err != nil {
		t.Fatal(err)
	}
	dest, dir := manager(t)
	empty := ""
	imported := dest.Handle(Request{Version: Version, Action: "connection_save", Profile: bundle.Profile, Relay: bundle.Relay, ExpectedRevision: &empty})
	if imported.Error != nil {
		t.Fatal(imported.Error)
	}
	if len(imported.Connections) != 2 {
		t.Fatal("dependency was not imported")
	}
	dest.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.catalog.profiles) != 2 {
		t.Fatal("bundle did not persist")
	}
	if r := reopened.Handle(Request{Version: Version, Action: "connection_test", ID: relay.ID}); r.Error == nil || !strings.Contains(r.Error.Message, "complete connection") {
		t.Fatal("relay accepted an exit test")
	}
}

func TestConnectionBundleFailureIsAtomic(t *testing.T) {
	m, _ := manager(t)
	r := namedProfile(t, "relay")
	r.Kind, r.Inlets = "relay", nil
	p := namedProfile(t, "route")
	p.RelayID = r.ID
	p.Inlets[0].Protocol = "invalid"
	empty := ""
	q := Request{Version: Version, Action: "connection_save", Profile: &p, Relay: &r, ExpectedRevision: &empty}
	if m.Handle(q).Error == nil || len(m.catalog.profiles) != 0 {
		t.Fatal("invalid bundle left a relay behind")
	}
	p.Inlets[0].Protocol = "mixed"
	saved := m.Handle(q)
	if saved.Error != nil {
		t.Fatal(saved.Error)
	}
	r.Name = "overwrite"
	q.ExpectedRevision = &saved.Revision
	if m.Handle(q).Error == nil || m.catalog.profiles[r.ID].Name == r.Name {
		t.Fatal("bundle overwrote an existing relay")
	}
}
