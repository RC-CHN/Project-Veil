package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"veil/internal/wire"
)

var ErrIdleTimeout = errors.New("veil: stream idle timeout")

type TargetError = wire.OpenFailure

const (
	TargetFailed  = wire.OpenFailed
	TargetDNS     = wire.OpenDNS
	TargetRefused = wire.OpenRefused
	TargetTimeout = wire.OpenTimeout
)

func targetFailure(err error) TargetError {
	var n net.Error
	var dns *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &n) && n.Timeout()):
		return TargetTimeout
	case errors.As(err, &dns):
		return TargetDNS
	case errors.Is(err, errConnectionRefused):
		return TargetRefused
	default:
		return TargetFailed
	}
}

// OpError describes the failing endpoint/operation without recording payloads.
// Underlying network errors can include addresses. Local means the accepted
// socket on clients and the target socket on servers.
// An idle error additionally reports which operations the two pumps await.
type OpError struct {
	Op, Send, Receive string
	Err               error
}

func (e *OpError) Error() string {
	if e.Send != "" {
		return fmt.Sprintf("veil: %s (send=%s receive=%s): %v", e.Op, e.Send, e.Receive, e.Err)
	}
	return fmt.Sprintf("veil: %s: %v", e.Op, e.Err)
}

func (e *OpError) Unwrap() error { return e.Err }

func opError(op string, err error) error {
	if err == nil {
		return nil
	}
	return &OpError{Op: op, Err: err}
}
