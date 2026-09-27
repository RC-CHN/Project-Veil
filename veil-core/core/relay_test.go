package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
	"veil/internal/wire"
)

type recordedConn struct {
	net.Conn
	writes [][]byte // read only after the relay and final write have joined
}

func TestRelayStreamsPartialFrame(t *testing.T) {
	local, backend := tcpPair(t)
	a, peer := net.Pipe()
	defer a.Close()
	defer peer.Close()
	peer.SetDeadline(time.Now().Add(3 * time.Second))
	r := wire.Reader{R: a}
	done := make(chan error, 1)
	go func() { _, err := relay(context.Background(), local, a, &r, 200*time.Millisecond, false); done <- err }()
	payload := bytes.Repeat([]byte{19}, wire.MaxData)
	var framed bytes.Buffer
	wire.Write(&framed, wire.Data, payload)
	frame := framed.Bytes()
	// A header may itself be fragmented. Total transfer lasts longer than idle.
	for _, b := range frame[:4] {
		if _, err := peer.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(75 * time.Millisecond)
	}
	if _, err := peer.Write(frame[4:36]); err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 32)
	backend.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := io.ReadFull(backend, first); err != nil {
		t.Fatal("waited for whole DATA frame", err)
	}
	backend.SetReadDeadline(time.Now().Add(3 * time.Second))
	go func() {
		peer.Write(frame[36:])
		// DONE belongs to the caller, not this relay's next stream.
		peer.Write([]byte{wire.Fin, 0, 0, 0, wire.Done, 0, 0, 0})
	}()
	rest, err := io.ReadAll(backend)
	if err != nil || !bytes.Equal(append(first, rest...), payload) {
		t.Fatal("payload or FIN corrupted", err)
	}
	backend.CloseWrite()
	pr := wire.Reader{R: peer}
	if err := wire.Expect(&pr, wire.Fin); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := wire.Expect(&r, wire.Done); err != nil {
		t.Fatal("crossed FIN/DONE boundary", err)
	}
}

func TestRelayTruncationAndIdleDiagnostics(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(map[bool]string{false: "blackhole", true: "truncated frame"}[truncated], func(t *testing.T) {
			local, backend := tcpPair(t)
			a, peer := net.Pipe()
			defer a.Close()
			defer peer.Close()
			done := make(chan error, 1)
			go func() {
				r := wire.Reader{R: a}
				_, err := relay(context.Background(), local, a, &r, 150*time.Millisecond, false)
				done <- err
			}()
			if truncated {
				peer.Write([]byte{wire.Data, 0, 0, 4, 42})
				peer.Close()
			}
			err := <-done
			var op *OpError
			if !errors.As(err, &op) {
				t.Fatal("missing operation", err)
			}
			if truncated {
				if !errors.Is(err, io.ErrUnexpectedEOF) || op.Op != "tunnel payload" {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrIdleTimeout) || op.Send != "local read" || op.Receive != "tunnel read" {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(backend); err != nil {
				t.Fatal("local connection left waiting", err)
			}
		})
	}
}

func (c *recordedConn) Write(p []byte) (int, error) {
	c.writes = append(c.writes, bytes.Clone(p))
	return c.Conn.Write(p)
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	a.SetDeadline(time.Now().Add(3 * time.Second))
	b.SetDeadline(time.Now().Add(3 * time.Second))
	return a, b
}

func TestRelayFINOrdering(t *testing.T) {
	for _, peerFirst := range []bool{true, false} {
		name := "local first sends FIN immediately"
		if peerFirst {
			name = "peer first coalesces FIN after join"
		}
		t.Run(name, func(t *testing.T) {
			local, backend := tcpPair(t)
			a, peer := net.Pipe()
			defer a.Close()
			defer peer.Close()
			peer.SetDeadline(time.Now().Add(3 * time.Second))
			remote := &recordedConn{Conn: a}
			done := make(chan error, 1)
			go func() {
				r := wire.Reader{R: remote}
				pending, err := relay(context.Background(), local, remote, &r, time.Second, true)
				if err == nil {
					err = wire.WriteDone(remote, pending)
				}
				done <- err
			}()
			r := wire.Reader{R: peer}
			defer r.Release()
			if peerFirst {
				if err := wire.Write(peer, wire.Fin, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, backend); err != nil {
					t.Fatal("FIN did not propagate to target", err)
				}
			}
			if err := backend.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			// With local EOF first, FIN must arrive before the peer sends FIN.
			if err := wire.Expect(&r, wire.Fin); err != nil {
				t.Fatal(err)
			}
			if !peerFirst {
				if err := wire.Write(peer, wire.Fin, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := wire.Expect(&r, wire.Done); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			want := 2
			if peerFirst {
				want = 1
			}
			if len(remote.writes) != want {
				t.Fatalf("writes=%x, want %d writes", remote.writes, want)
			}
		})
	}
}
