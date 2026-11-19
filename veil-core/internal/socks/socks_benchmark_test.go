package socks

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"
)

type countingConn struct {
	net.Conn
	reads int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	c.reads++
	return c.Conn.Read(p)
}

// Isolate method negotiation and an IPv4 CONNECT over real loopback TCP.
// The peer waits for each reply. Reuse the socket to exclude TCP setup.
func BenchmarkSOCKSNegotiation(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		b.Fatal(err)
	}
	defer server.Close()
	deadline := time.Now().Add(time.Minute)
	server.SetDeadline(deadline)
	client.SetDeadline(deadline)
	conn := &countingConn{Conn: server}
	reader := bufio.NewReader(conn)
	done := make(chan error, 1)
	go func() {
		greeting := []byte{5, 1, 0}
		request := []byte{5, 1, 0, 1, 127, 0, 0, 1, 0, 80}
		var reply [10]byte
		for range b.N {
			if _, err := client.Write(greeting); err != nil {
				done <- err
				return
			}
			if _, err := io.ReadFull(client, reply[:2]); err != nil {
				done <- err
				return
			}
			if _, err := client.Write(request); err != nil {
				done <- err
				return
			}
			if _, err := io.ReadFull(client, reply[:]); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	b.ResetTimer()
	for range b.N {
		address, err := ReadConnect(reader, conn)
		if err != nil || address != "127.0.0.1:80" {
			b.Fatalf("CONNECT: %q, %v", address, err)
		}
		if err := Reply(conn, 0); err != nil {
			b.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	b.ReportMetric(float64(conn.reads)/float64(b.N), "socket-reads/op")
}
