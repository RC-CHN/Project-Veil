package inbound

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Listen creates a TCP listener with the standard Go keepalive policy.
func Listen(address string) (net.Listener, error) {
	config := net.ListenConfig{
		// Linux inherits these socket options on accept. Suppress Go's four
		// per-connection setters; keepalive itself stays enabled below.
		KeepAlive: -1,
		Control: func(_, _ string, raw syscall.RawConn) error {
			var socketErr error
			err := raw.Control(func(fd uintptr) {
				// Match net's defaults; the socket-options test checks against
				// the installed Go version rather than duplicating these values.
				for _, option := range [][3]int{
					{unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1},
					{unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, 15},
					{unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, 15},
					{unix.IPPROTO_TCP, unix.TCP_KEEPCNT, 9},
				} {
					if socketErr = unix.SetsockoptInt(int(fd), option[0], option[1], option[2]); socketErr != nil {
						return
					}
				}
			})
			if err != nil {
				return err
			}
			return socketErr
		},
	}
	return config.Listen(context.Background(), "tcp", address)
}
