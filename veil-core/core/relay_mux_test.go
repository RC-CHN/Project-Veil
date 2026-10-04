package core

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
	"veil/internal/mux"
)

// Observing CloseWrite proves the receive pump has already consumed wire FIN.
type relayHalfCloseNotice struct {
	*net.TCPConn
	halfClosed chan struct{}
}

func (c *relayHalfCloseNotice) CloseWrite() error {
	err := c.TCPConn.CloseWrite()
	close(c.halfClosed)
	return err
}

func relayMuxPair(t *testing.T) (*mux.Session, *mux.Session, *mux.Stream, *mux.Stream) {
	t.Helper()
	a, b := net.Pipe()
	client, err := mux.New(a, mux.Options{Profile: mux.DefaultProfile(), WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server, err := mux.New(b, mux.Options{Server: true, Profile: mux.DefaultProfile(), WriteTimeout: time.Second})
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close(); client.Wait(); server.Wait() })
	stream, peer := relayMuxOpen(t, client, server)
	return client, server, stream, peer
}

func relayMuxOpen(t *testing.T, client, server *mux.Session) (*mux.Stream, *mux.Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		stream *mux.Stream
		err    error
	}
	opened := make(chan result, 1)
	go func() { st, err := client.Open(ctx, []byte("test target")); opened <- result{st, err} }()
	peer, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Respond(0); err != nil {
		t.Fatal(err)
	}
	r := <-opened
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { r.stream.Close(); peer.Close() })
	return r.stream, peer
}

func TestRelayMuxFailureAfterFIN(t *testing.T) {
	for _, failure := range []string{"reset", "transport EOF"} {
		t.Run(failure, func(t *testing.T) {
			client, server, stream, peer := relayMuxPair(t)
			local, app := tcpPair(t)
			noticed := &relayHalfCloseNotice{TCPConn: local, halfClosed: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- relay(ctx, noticed, stream, time.Hour) }()
			if err := peer.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-noticed.halfClosed:
			case <-time.After(time.Second):
				t.Fatal("remote FIN did not reach local socket")
			}
			select {
			case err := <-done:
				t.Fatal("FIN prematurely ended the reverse direction", err)
			default:
			}
			want := error(mux.ErrReset)
			if failure == "reset" {
				peer.Close()
			} else {
				server.Close()
				want = io.ErrUnexpectedEOF
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("lost terminal cause: got %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("terminal mux failure left local Read blocked after FIN")
			}
			if failure == "reset" {
				// Reset must retire only the failed stream, not its shared tunnel.
				next, target := relayMuxOpen(t, client, server)
				written := make(chan error, 1)
				go func() { _, err := next.Write([]byte("ping")); written <- err }()
				var p [4]byte
				if _, err := io.ReadFull(target, p[:]); err != nil || string(p[:]) != "ping" {
					t.Fatalf("adjacent stream: %q, %v", p, err)
				}
				if err := <-written; err != nil {
					t.Fatal(err)
				}
			}
			app.Close()
		})
	}
}

func TestRelayMuxFINPreservesReverseDirection(t *testing.T) {
	_, _, stream, peer := relayMuxPair(t)
	local, app := tcpPair(t)
	noticed := &relayHalfCloseNotice{TCPConn: local, halfClosed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relay(ctx, noticed, stream, time.Hour) }()
	if err := peer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-noticed.halfClosed:
	case <-time.After(time.Second):
		t.Fatal("remote FIN did not reach local socket")
	}
	if _, err := app.Write([]byte("after FIN")); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	p, err := io.ReadAll(peer)
	if err != nil || string(p) != "after FIN" {
		t.Fatalf("reverse direction lost: %q, %v", p, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("normal half-close did not finish")
	}
}
