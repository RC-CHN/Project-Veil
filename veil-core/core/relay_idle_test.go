package core

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type relayActivityConn struct {
	*net.TCPConn
	last atomic.Int64
}

func TestLongIdleDoesNotExtendBlockedWrite(t *testing.T) {
	if os.Getenv("VEIL_WRITE_STALL_TEST") != "1" {
		t.Skip("set VEIL_WRITE_STALL_TEST=1 for the two-minute write-stall test")
	}
	local, app := net.Pipe()
	remote, peer := net.Pipe()
	defer app.Close()
	defer peer.Close()
	done := make(chan error, 1)
	go func() { done <- relay(context.Background(), local, remote, DefaultIdleTimeout) }()
	start := time.Now()
	// The first read progresses, then forwarding blocks because peer never reads.
	if _, err := app.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var op *OpError
		if !errors.Is(err, ErrWriteStall) || !errors.As(err, &op) || op.Send != "tunnel write" || time.Since(start) < 2*time.Minute {
			t.Fatal("wrong stall deadline or diagnostic:", err)
		}
	case <-time.After(125 * time.Second):
		local.Close()
		remote.Close()
		<-done
		t.Fatal("blocked forwarding inherited the quiet-application timeout")
	}
}

func (c *relayActivityConn) LastActivity() time.Time {
	return time.Unix(0, c.last.Load())
}

func relayByte(t *testing.T, src, dst net.Conn) {
	t.Helper()
	if _, err := src.Write([]byte{71}); err != nil {
		t.Fatal(err)
	}
	var p [1]byte
	if _, err := io.ReadFull(dst, p[:]); err != nil || p[0] != 71 {
		t.Fatalf("relay byte %x: %v", p, err)
	}
}

func TestRelayIdleDeadlineFollowsActivity(t *testing.T) {
	for _, direction := range []string{"local", "remote", "alternating", "partial frame"} {
		t.Run(direction, func(t *testing.T) {
			local, backend := tcpPair(t)
			remote, peer := tcpPair(t)
			tracked := &relayActivityConn{TCPConn: remote}
			const idle = 240 * time.Millisecond
			done := make(chan error, 1)
			go func() { done <- relay(context.Background(), local, tracked, idle) }()
			var last time.Time
			for i := range 4 {
				time.Sleep(160 * time.Millisecond)
				switch {
				case direction == "partial frame":
					// A mux frame may make progress before an application Read
					// completes. This activity must also defer idle expiry.
					tracked.last.Store(time.Now().UnixNano())
				case direction == "local" || direction == "alternating" && i%2 == 0:
					relayByte(t, backend, peer)
				default:
					relayByte(t, peer, backend)
				}
				last = time.Now()
				select {
				case err := <-done:
					t.Fatal("active relay expired", err)
				default:
				}
			}
			select {
			case err := <-done:
				var op *OpError
				if !errors.Is(err, ErrIdleTimeout) || !errors.As(err, &op) || op.Send != "local read" || op.Receive != "tunnel read" {
					t.Fatal("idle diagnostics", err)
				}
				if time.Since(last) < idle-40*time.Millisecond {
					t.Fatal("expired before the latest activity's deadline")
				}
			case <-time.After(time.Second):
				t.Fatal("inactive relay failed to expire")
			}
		})
	}
}

func TestRelayTimerReuseAfterExit(t *testing.T) {
	for _, exit := range []string{"cancel", "idle", "close"} {
		t.Run(exit, func(t *testing.T) {
			local, backend := tcpPair(t)
			remote, peer := tcpPair(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- relay(ctx, local, remote, 20*time.Millisecond) }()
			switch exit {
			case "cancel":
				cancel()
			case "close":
				backend.CloseWrite()
				peer.CloseWrite()
			}
			select {
			case err := <-done:
				if exit == "cancel" && !errors.Is(err, context.Canceled) || exit == "idle" && !errors.Is(err, ErrIdleTimeout) || exit == "close" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("relay failed to exit")
			}
			local, backend = tcpPair(t)
			remote, peer = tcpPair(t)
			go func() { done <- relay(context.Background(), local, remote, 240*time.Millisecond) }()
			time.Sleep(80 * time.Millisecond)
			relayByte(t, backend, peer)
			relayByte(t, peer, backend)
			backend.CloseWrite()
			peer.CloseWrite()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("previous timer affected a new relay", err)
				}
			case <-time.After(time.Second):
				t.Fatal("new relay failed to close")
			}
		})
	}
}
