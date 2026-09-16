//go:build linux

package linux

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type Reader struct{}

func (Reader) Read(path string, limit int64, private bool) ([]byte, error) {
	path, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/")
	fd, e := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	for i, part := range parts {
		flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		next, err := syscall.Openat(fd, part, flags, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	native, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || native.Nlink != 1 || st.Size() > limit {
		return nil, errors.New("file type, links or size")
	}
	if native.Uid != uint32(os.Geteuid()) && native.Uid != 0 {
		return nil, errors.New("file owner")
	}
	denied := os.FileMode(0022)
	if private {
		denied = 0077
	}
	if st.Mode().Perm()&denied != 0 {
		return nil, errors.New("file access permissions")
	}
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(raw)) > limit {
		return nil, errors.New("file grew beyond limit")
	}
	return raw, e
}
