package proxy

import (
	"bytes"
	"context"
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
