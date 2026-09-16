//go:build linux

package linux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateReaderLimitsPermissionsAndLinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	if e := os.WriteFile(p, []byte("private"), 0600); e != nil {
		t.Fatal(e)
	}
	r := Reader{}
	if raw, e := r.Read(p, 64, true); e != nil || string(raw) != "private" {
		t.Fatal(e)
	}
	if _, e := r.Read(p, 2, true); e == nil {
		t.Fatal("size bound")
	}
	if e := os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Read(p, 64, true); e == nil {
		t.Fatal("public private file")
	}
	os.Chmod(p, 0600)
	alias := filepath.Join(dir, "alias")
	if e := os.Symlink(p, alias); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Read(alias, 64, true); e == nil {
		t.Fatal("symlink")
	}
	link := filepath.Join(dir, "hardlink")
	if e := os.Link(p, link); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Read(p, 64, true); e == nil {
		t.Fatal("hardlink")
	}
}
