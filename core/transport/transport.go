// Package transport is the platform-independent application transport contract.
package transport

import (
	"context"
	"errors"
	"net"
	"veil.local/core/endpoint"
)

// Stream preserves TCP order and independent sending and receiving halves.
// Write acknowledges local admission, not remote application consumption.
// Close interrupts I/O; the runtime retains admission until its workers exit.
type Stream interface {
	net.Conn
	CloseWrite() error
}
type StreamDialer interface {
	DialStream(context.Context, endpoint.Endpoint) (Stream, error)
}

// Association carries whole datagrams. Send owns a copy after admission;
// Receive consumes one packet, discarding it with ErrShortBuffer if dst is small.
// A zero-length packet is data, not EOF. Close interrupts pending operations.
type Association interface {
	Send(context.Context, endpoint.Endpoint, []byte) error
	Receive(context.Context, []byte) (int, endpoint.Endpoint, error)
	Close() error
}
type DatagramDialer interface {
	OpenAssociation(context.Context) (Association, error)
}
type Dialer interface {
	StreamDialer
	DatagramDialer
}

var (
	ErrShortBuffer = errors.New("datagram buffer too small")
	ErrCapacity    = errors.New("transport capacity exhausted")
	ErrUnsupported = errors.New("transport capability unsupported")
	ErrNotReady    = errors.New("transport not ready")
)

const MaxDatagram = 65507
