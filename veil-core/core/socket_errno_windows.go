package core

import "golang.org/x/sys/windows"

const (
	errConnectionRefused = windows.WSAECONNREFUSED
	errNotConnected      = windows.WSAENOTCONN
)
