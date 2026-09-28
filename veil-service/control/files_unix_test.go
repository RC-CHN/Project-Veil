//go:build !windows

package control

import (
	"os"
	"testing"
)

func checkConfigPrivate(t *testing.T, path string) {
	t.Helper()
	s, err := os.Stat(path)
	if err != nil || s.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v %v", s, err)
	}
}

func TestSharedDirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("shared directory accepted")
	}
}
