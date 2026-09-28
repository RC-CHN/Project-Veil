//go:build !windows

package core

import "syscall"

const (
	errConnectionRefused = syscall.ECONNREFUSED
	errNotConnected      = syscall.ENOTCONN
)
