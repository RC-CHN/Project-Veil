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

type relayProgressWriter struct {
	net.Conn
	last atomic.Int64
}

func (c *relayProgressWriter) Write(p []byte) (int, error) {
	for i := range p {
		if _, err := c.Conn.Write(p[i : i+1]); err != nil {
			return i, err
		}
		c.last.Store(time.Now().UnixNano())
	}
	return len(p), nil
}
func (c *relayProgressWriter) LastWriteActivity() time.Time { return time.Unix(0, c.last.Load()) }
func (c *relayProgressWriter) LastActivity() time.Time      { return c.LastWriteActivity() }

// A mux Write can span many batches. Progress within that Write must keep its
// deadline alive even when the full caller buffer has not yet been accepted.
func TestRelayPendingWriteProgress(t *testing.T) {
	local, app := net.Pipe()
	remote, peer := net.Pipe()
	defer app.Close()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay(ctx, local, &relayProgressWriter{Conn: remote}, 200*time.Millisecond) }()
	defer func() { cancel(); <-done }()
	if _, err := app.Write(make([]byte, 12)); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		time.Sleep(50 * time.Millisecond)
		var b [1]byte
		if _, err := io.ReadFull(peer, b[:]); err != nil {
			t.Fatal("forward progress was mistaken for a stalled write:", err)
		}
	}
}

// A peer that stops reading must not retain its socket and two relay pumps
// indefinitely just because it continues sending in the reverse direction.
func TestRelayBlockedWriteIgnoresReverseActivity(t *testing.T) {
	for _, outbound := range []bool{true, false} {
		name := "local write"
		if outbound {
			name = "tunnel write"
		}
		t.Run(name, func(t *testing.T) {
			local, app := net.Pipe()
			remote, peer := net.Pipe()
			defer app.Close()
			defer peer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const idle = 150 * time.Millisecond
			done := make(chan error, 1)
			go func() { done <- relay(ctx, local, remote, idle) }()
			blocked, reverse := app, peer
			if !outbound {
				blocked, reverse = peer, app
			}
			start := time.Now()
			if _, err := blocked.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			activity := make(chan struct{})
			go func() {
				defer close(activity)
				var b [1]byte
				for {
					if _, err := reverse.Write([]byte{2}); err != nil {
						return
					}
					if _, err := io.ReadFull(blocked, b[:]); err != nil {
						return
					}
					time.Sleep(idle / 5)
				}
			}()
			select {
			case err := <-done:
				var op *OpError
				if !errors.Is(err, ErrIdleTimeout) || !errors.As(err, &op) {
					t.Fatalf("wrong blocked-write result: %v", err)
				}
				if outbound && op.Send != name || !outbound && op.Receive != name {
					t.Fatalf("wrong blocked direction: %+v", op)
				}
				if time.Since(start) < idle {
					t.Fatal("closed before the write budget elapsed")
				}
			case <-time.After(4 * idle):
				cancel()
				<-done
				t.Error("reverse traffic kept a blocked write and its resources alive")
			}
			<-activity
		})
	}
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
