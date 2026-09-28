package systemproxy

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type fakeBackend struct {
	current Snapshot
	writes  int
	fail    bool
}

func (*fakeBackend) Name() string              { return "test" }
func (b *fakeBackend) Read() (Snapshot, error) { return clone(b.current), nil }
func (b *fakeBackend) Write(s Snapshot) error {
	b.writes++
	if b.fail {
		b.fail = false
		return errors.New("denied")
	}
	b.current = clone(s)
	return nil
}
func (*fakeBackend) Manual(s Snapshot, endpoint string) Snapshot {
	s["mode"] = "manual"
	s["server"] = endpoint
	return s
}
func (*fakeBackend) Direct(s Snapshot) Snapshot { s["mode"] = "direct"; return s }
func setup(t *testing.T) (*Controller, *fakeBackend, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxy.json")
	b := &fakeBackend{current: Snapshot{"mode": "pac", "server": "original"}}
	c, err := Open(path, b)
	if err != nil {
		t.Fatal(err)
	}
	return c, b, path
}
func TestDefaultDoesNotWrite(t *testing.T) {
	c, b, _ := setup(t)
	if err := c.Apply("127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(); err != nil {
		t.Fatal(err)
	}
	if b.writes != 0 || c.Status().Mode != "keep" {
		t.Fatal("default touched system settings")
	}
}
func TestRecoverAndRestoreAcrossPortChanges(t *testing.T) {
	c, b, path := setup(t)
	original := clone(b.current)
	if err := c.SetMode("auto", "127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply("127.0.0.1:1081"); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(path, b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.current, original) || recovered.Status().Managed {
		t.Fatal("restart did not recover original proxy")
	}
	if recovered.Status().Mode != "auto" {
		t.Fatal("lost mode preference")
	}
}
func TestExternalChangesArePreserved(t *testing.T) {
	c, b, _ := setup(t)
	if err := c.SetMode("auto", "127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	b.current = Snapshot{"mode": "manual", "server": "another app"}
	if err := c.Restore(); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	if b.current["server"] != "another app" || c.Status().Managed {
		t.Fatal("overwrote external settings")
	}
}
func TestFailedReapplyRetainsOriginalRecovery(t *testing.T) {
	c, b, _ := setup(t)
	original := clone(b.current)
	if err := c.SetMode("auto", "127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	b.fail = true
	if err := c.Apply("127.0.0.1:1081"); err == nil {
		t.Fatal("missing failure")
	}
	if b.current["server"] != "127.0.0.1:1080" {
		t.Fatal("did not roll back failed update")
	}
	if err := c.Restore(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.current, original) {
		t.Fatal("lost original recovery after failed update")
	}
}
func TestKeepRestoresAndClearIsExplicit(t *testing.T) {
	c, b, _ := setup(t)
	original := clone(b.current)
	if err := c.SetMode("auto", "127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMode("keep", ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.current, original) {
		t.Fatal("keep did not restore original")
	}
	if err := c.Clear(); err != nil {
		t.Fatal(err)
	}
	if b.current["mode"] != "direct" || c.Status().Mode != "keep" {
		t.Fatal("clear did not disable proxy")
	}
	if err := c.Apply("127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	if b.current["mode"] != "direct" {
		t.Fatal("clear was undone by later start")
	}
}

func TestFailedClearKeepsRecoveryRecord(t *testing.T) {
	c, b, _ := setup(t)
	original := clone(b.current)
	if err := c.SetMode("auto", "127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	b.fail = true
	if err := c.Clear(); err == nil {
		t.Fatal("clear failure was hidden")
	}
	if !c.Status().Managed || c.Status().Mode != "auto" || b.current["server"] != "127.0.0.1:1080" {
		t.Fatal("failed clear lost the running proxy or recovery record")
	}
	if err := c.Restore(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.current, original) {
		t.Fatal("could not restore original settings after failed clear")
	}
}

func TestClearJournalFailurePreservesSettings(t *testing.T) {
	c, b, path := setup(t)
	original := clone(b.current)
	if err := c.SetMode("auto", "127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	active := clone(b.current)
	// Make replacing the journal fail without relying on filesystem permissions.
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.Clear(); err == nil {
		t.Fatal("journal failure was hidden")
	}
	if !reflect.DeepEqual(b.current, active) || c.Status().Mode != "auto" || !c.Status().Managed {
		t.Fatal("failed clear changed settings or lost the recovery record")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.current, original) {
		t.Fatal("failed clear prevented restoring the original settings")
	}
}
