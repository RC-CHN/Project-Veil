//go:build !linux

package inbound

import "net"

// Listen creates a TCP listener with the standard Go keepalive policy.
func Listen(address string) (net.Listener, error) {
	return net.Listen("tcp", address)
}
