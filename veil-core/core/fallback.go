package core

import (
	"context"
	"errors"
	"net"
	"time"
	"veil/internal/wire"
)

// RelayTCP takes ownership of both connections and joins both copy directions.
// EOF half-closes the opposite writer; errors, ctx cancellation, or no progress
// in either direction for idle close both ends. Both must support CloseWrite.
// idle must be positive. It applies no routing, HTTP parsing, authentication,
// or protocol conversion.
func RelayTCP(ctx context.Context, left, right net.Conn, idle time.Duration) error {
	defer left.Close()
	defer right.Close()
	if idle <= 0 {
		return errors.New("veil: relay idle timeout must be positive")
	}
	return relay(ctx, left, right, idle)
}

// authPrefix saves only wire.Reader's bounded authentication reads. It never
// reads ahead, and replays the same bytes once the caller chooses fallback.
type authPrefix struct {
	net.Conn
	bytes        [wire.HeaderSize + wire.AuthSize]byte
	size, offset int
	replay       bool
}

func (p *authPrefix) Read(b []byte) (int, error) {
	if p.replay && p.offset < p.size {
		n := copy(b, p.bytes[p.offset:p.size])
		p.offset += n
		return n, nil
	}
	n, err := p.Conn.Read(b)
	if !p.replay {
		p.size += copy(p.bytes[p.size:], b[:n])
	}
	return n, err
}

func (p *authPrefix) CloseWrite() error {
	return closeWrite(p.Conn)
}
