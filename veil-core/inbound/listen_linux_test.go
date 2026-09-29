package inbound

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Compare real accepted sockets against this Go toolchain's defaults. This
// catches missing Linux inheritance as well as future Go keepalive changes.
func TestListenSocketOptionsMatchStandard(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(address, func(t *testing.T) {
			standard, err := net.Listen("tcp", address)
			if err != nil {
				if address == "[::1]:0" {
					t.Skipf("IPv6 unavailable: %v", err)
				}
				t.Fatal(err)
			}
			defer standard.Close()
			optimized, err := Listen(address)
			if err != nil {
				t.Fatal(err)
			}
			defer optimized.Close()
			options := func(ln net.Listener) [5]int {
				t.Helper()
				client, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				ln.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
				peer, err := ln.Accept()
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
				raw, err := peer.(*net.TCPConn).SyscallConn()
				if err != nil {
					t.Fatal(err)
				}
				var values [5]int
				var socketErr error
				err = raw.Control(func(fd uintptr) {
					for i, option := range [][2]int{
						{unix.SOL_SOCKET, unix.SO_KEEPALIVE},
						{unix.IPPROTO_TCP, unix.TCP_KEEPIDLE},
						{unix.IPPROTO_TCP, unix.TCP_KEEPINTVL},
						{unix.IPPROTO_TCP, unix.TCP_KEEPCNT},
						{unix.IPPROTO_TCP, unix.TCP_NODELAY},
					} {
						if values[i], socketErr = unix.GetsockoptInt(int(fd), option[0], option[1]); socketErr != nil {
							return
						}
					}
				})
				if err != nil || socketErr != nil {
					t.Fatalf("socket options: %v %v", err, socketErr)
				}
				return values
			}
			want, got := options(standard), options(optimized)
			if got != want {
				t.Fatalf("accepted socket policy changed: got %v, standard %v", got, want)
			}
		})
	}
}
