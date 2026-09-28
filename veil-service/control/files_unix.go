//go:build !windows

package control

import (
	"errors"
	"os"
	"path/filepath"
)

// PrivateDir refuses a shared or symlinked final directory instead of changing
// permissions on a directory the caller may be using for something else.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	s, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !s.IsDir() || s.Mode().Perm()&0077 != 0 {
		return errors.New("state and socket directories must be private (0700), not symlinks")
	}
	return nil
}

func replaceConfig(from, to string) (bool, error) {
	if err := os.Rename(from, to); err != nil {
		return false, err
	}
	// A sync failure after rename means the new config is visible but durability
	// is uncertain. The manager must still publish the new revision.
	d, err := os.Open(filepath.Dir(to))
	if err != nil {
		return true, err
	}
	defer d.Close()
	return true, d.Sync()
}

func configTemp(dir string) (*os.File, error) {
	return os.CreateTemp(dir, ".config-*")
}
