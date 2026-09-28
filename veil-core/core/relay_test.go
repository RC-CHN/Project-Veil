package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, e := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	a, e := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close() })
	b, e := ln.AcceptTCP()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { b.Close() })
	a.SetDeadline(time.Now().Add(3 * time.Second))
	b.SetDeadline(time.Now().Add(3 * time.Second))
	return a, b
}
func TestRelayStreamsPartialData(t *testing.T) {
	local, backend := tcpPair(t)
	remote, peer := tcpPair(t)
	done := make(chan error, 1)
	go func() { done <- relay(context.Background(), local, remote, 200*time.Millisecond) }()
	payload := bytes.Repeat([]byte{19}, 128<<10)
	if _, e := peer.Write(payload[:32]); e != nil {
		t.Fatal(e)
	}
	first := make([]byte, 32)
	backend.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, e := io.ReadFull(backend, first); e != nil {
		t.Fatal("buffered a partial read", e)
	}
	backend.SetReadDeadline(time.Now().Add(3 * time.Second))
	go func() { peer.Write(payload[32:]); peer.CloseWrite() }()
	rest, e := io.ReadAll(backend)
	if e != nil || !bytes.Equal(append(first, rest...), payload) {
		t.Fatal("payload or FIN corrupted", e)
	}
	backend.CloseWrite()
	if _, e = io.Copy(io.Discard, peer); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
func TestRelayResetAndIdleDiagnostics(t *testing.T) {
	for _, rst := range []bool{false, true} {
		t.Run(map[bool]string{false: "blackhole", true: "reset"}[rst], func(t *testing.T) {
			local, backend := tcpPair(t)
			remote, peer := tcpPair(t)
			done := make(chan error, 1)
			go func() { done <- relay(context.Background(), local, remote, 150*time.Millisecond) }()
			if rst {
				peer.SetLinger(0)
				peer.Close()
			}
			e := <-done
			var op *OpError
			if !errors.As(e, &op) {
				t.Fatal("missing operation", e)
			}
			if rst {
				if op.Op != "tunnel read" {
					t.Fatal(e)
				}
			} else if !errors.Is(e, ErrIdleTimeout) || op.Send != "local read" || op.Receive != "tunnel read" {
				t.Fatal(e)
			}
			if _, e = io.ReadAll(backend); e != nil {
				t.Fatal("local connection left waiting", e)
			}
		})
	}
}
func TestRelayFINOrdering(t *testing.T) {
	for _, peerFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "peer first", false: "local first"}[peerFirst], func(t *testing.T) {
			local, backend := tcpPair(t)
			remote, peer := tcpPair(t)
			done := make(chan error, 1)
			go func() { done <- relay(context.Background(), local, remote, time.Second) }()
			if peerFirst {
				peer.CloseWrite()
				if _, e := io.Copy(io.Discard, backend); e != nil {
					t.Fatal(e)
				}
			}
			backend.CloseWrite()
			if _, e := io.Copy(io.Discard, peer); e != nil {
				t.Fatal(e)
			}
			if !peerFirst {
				peer.CloseWrite()
			}
			if e := <-done; e != nil {
				t.Fatal(e)
			}
		})
	}
}
