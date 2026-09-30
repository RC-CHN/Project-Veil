// Package inbound adapts local TCP connections to a shared Veil client.
package inbound

import (
	"context"
	"net"
	"time"
	"veil/core"
	"veil/internal/socks"
	"veil/internal/wire"
)

// Handler owns conn and must close it on cancellation. Callers bound concurrent
// invocations and cancel ctx on stop. The provided handlers honor this contract.
type Handler func(ctx context.Context, conn net.Conn) error

// SOCKS5 implements unauthenticated CONNECT. Success is sent only after the
// destination has connected. timeout bounds local negotiation and replies.
func SOCKS5(client *core.Client, timeout time.Duration) Handler {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return func(ctx context.Context, local net.Conn) error {
		defer local.Close()
		stop := context.AfterFunc(ctx, func() { local.Close() })
		defer stop()
		local.SetDeadline(time.Now().Add(timeout))
		address, err := socks.ReadConnect(local)
		if err != nil {
			return err
		}
		stream, err := client.Open(ctx, address)
		// Remote dialing has its own budget; replies need a fresh local one.
		local.SetWriteDeadline(time.Now().Add(timeout))
		if err != nil {
			socks.Reply(local, 1)
			return err
		}
		defer stream.Close()
		if err = socks.Reply(local, 0); err != nil {
			return err
		}
		local.SetDeadline(time.Time{})
		return stream.Relay(local)
	}
}

// Forward sends each accepted connection through Veil to one fixed target.
// It is an encrypted outbound adapter, not a bare TCP relay to a Veil server.
func Forward(client *core.Client, target string) (Handler, error) {
	if _, err := wire.EncodeAddress(target); err != nil {
		return nil, err
	}
	return func(ctx context.Context, local net.Conn) error {
		defer local.Close()
		stream, err := client.Open(ctx, target)
		if err != nil {
			return err
		}
		defer stream.Close()
		return stream.Relay(local)
	}, nil
}
