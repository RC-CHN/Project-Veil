//go:build linux || freebsd

// Package local carries the control protocol over a private Unix socket.
package local

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"veil-service/control"

	"golang.org/x/sys/unix"
)

// Lock must be held until the managed runtime has completely stopped. Keep the
// lock file on disk; removing it permits two processes to lock different inodes.
func Lock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another daemon owns this state directory or socket")
	}
	return f, nil
}

type listener struct {
	*net.UnixListener
	lock *os.File
	once sync.Once
	err  error
}

func (l *listener) Close() error {
	l.once.Do(func() {
		l.err = l.UnixListener.Close()
		l.lock.Close()
	})
	return l.err
}

func Listen(path string) (net.Listener, error) {
	if err := control.PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	lock, err := Lock(path + ".lock")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	if s, err := os.Lstat(path); err == nil {
		if s.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("control path is not a socket")
		}
		c, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			c.Close()
			return nil, errors.New("control socket is already listening")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	ok = true
	return &listener{UnixListener: ln, lock: lock}, nil
}
