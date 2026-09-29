package control

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExportReplacesPublicFilePrivately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateFile(path, []byte("new credentials")); err != nil {
		t.Fatal(err)
	}
	checkConfigPrivate(t, path)
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "new credentials" {
		t.Fatal("export not replaced", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("temporary export retained", err)
	}
}
