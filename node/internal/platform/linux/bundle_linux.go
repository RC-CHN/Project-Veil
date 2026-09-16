//go:build linux

package linux

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

type BundleWriter struct{}

// WriteNew uses descriptor-relative operations and publishes with NOREPLACE.
// An interrupted preparation leaves only a private staging directory; it is not
// an installation/upgrade transaction. Existing outputs are never modified.
func (BundleWriter) WriteNew(path string, files map[string][]byte) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	base := filepath.Base(path)
	if base == "/" || base == "." || base == ".." {
		return errors.New("invalid output directory")
	}
	var existing unix.Stat_t
	if err = unix.Fstatat(parent, base, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return errors.New("output already exists")
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	names := make([]string, 0, len(files))
	for name, data := range files {
		parts := strings.Split(name, "/")
		if len(parts) > 2 || len(data) > 1<<20 {
			return errors.New("bundle path or size bound")
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
				return errors.New("invalid bundle path")
			}
		}
		names = append(names, name)
	}
	if len(names) == 0 || len(names) > 32 {
		return errors.New("bundle file count")
	}
	sort.Strings(names)
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	stage := ".veil-prepare-" + hex.EncodeToString(nonce[:])
	if err = unix.Mkdirat(parent, stage, 0700); err != nil {
		return err
	}
	root, err := unix.Openat(parent, stage, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	dirs := map[string]int{"": root}
	defer func() {
		for name, fd := range dirs {
			if name != "" {
				unix.Close(fd)
			}
		}
	}()
	fail := func(e error) error {
		return fmt.Errorf("preparation failed; private staging directory %s retained: %w", filepath.Join(filepath.Dir(path), stage), e)
	}
	for _, name := range names {
		parts := strings.Split(name, "/")
		dir, leaf := "", parts[0]
		if len(parts) == 2 {
			dir, leaf = parts[0], parts[1]
		}
		fd, ok := dirs[dir]
		if !ok {
			if err = unix.Mkdirat(root, dir, 0700); err != nil {
				return fail(err)
			}
			fd, err = unix.Openat(root, dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return fail(err)
			}
			dirs[dir] = fd
		}
		fileFD, e := unix.Openat(fd, leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e != nil {
			return fail(e)
		}
		file := os.NewFile(uintptr(fileFD), name)
		_, e = file.Write(files[name])
		if e == nil {
			e = file.Sync()
		}
		e = errors.Join(e, file.Close())
		if e != nil {
			return fail(e)
		}
	}
	for name, fd := range dirs {
		if name != "" {
			if err = unix.Fsync(fd); err != nil {
				return fail(err)
			}
		}
	}
	if err = unix.Fsync(root); err != nil {
		return fail(err)
	}
	if err = unix.Renameat2(parent, stage, parent, base, unix.RENAME_NOREPLACE); err != nil {
		return fail(err)
	}
	if err = unix.Fsync(parent); err != nil {
		return fmt.Errorf("output published but parent sync failed; inspect output before retrying: %w", err)
	}
	return nil
}

func openDirectory(path string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
	}
	return fd, nil
}
