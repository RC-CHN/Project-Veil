package core_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
	core "veil.local/core"
	"veil.local/core/contracttest"
	"veil.local/core/endpoint"
	"veil.local/core/identity"
	"veil.local/core/model"
	"veil.local/core/transport"
)

func running(t *testing.T) (*core.Client, *core.Server, context.Context) {
	t.Helper()
	serverID, clientID, roots := contracttest.Identities(t)
	m, e := model.Generate(4)
	if e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server, e := core.NewServer(core.ServerOptions{Bucket: "owned-test", Model: m, Identity: serverID, ClientRoots: roots, ClientFingerprints: []string{clientID.Fingerprint()}, AllowCIDRs: []string{"127.0.0.0/8"}})
	if e != nil {
		l.Close()
		t.Fatal(e)
	}
	client, e := core.NewClient(core.ClientOptions{Bucket: "owned-test", Model: m, Identity: clientID, Roots: roots, ServerURL: "https://owned.test", DialAddress: l.Addr().String()})
	if e != nil {
		l.Close()
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan error, 2)
	go func() { done <- server.Serve(ctx, l) }()
	go func() { done <- client.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for range 2 {
			select {
			case e := <-done:
				if e != nil {
					t.Error(e)
				}
			case <-time.After(5 * time.Second):
				t.Error("runtime failed to join")
			}
		}
		if client.Snapshot().ActiveStreams != 0 || client.Snapshot().ActiveCarriers != 0 || server.Snapshot().ActiveStreams != 0 {
			t.Error("runtime retained streams", client.Snapshot(), server.Snapshot())
		}
	})
	if e = server.WaitReady(ctx); e != nil {
		t.Fatal(e)
	}
	if e = client.WaitReady(ctx); e != nil {
		t.Fatal(e)
	}
	return client, server, ctx
}
func TestDirectTCPHalfCloseAndDatagrams(t *testing.T) {
	client, _, ctx := running(t)
	target, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer target.Close()
	targetDone := make(chan error, 1)
	go func() {
		c, e := target.Accept()
		if e != nil {
			targetDone <- e
			return
		}
		defer c.Close()
		p, e := io.ReadAll(c)
		if e == nil {
			_, e = c.Write(append([]byte("reply:"), p...))
		}
		if e == nil {
			e = c.(*net.TCPConn).CloseWrite()
		}
		targetDone <- e
	}()
	ep, e := endpoint.ParseAddress(target.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, 8*time.Second)
	s, e := client.DialStream(openCtx, ep)
	if e != nil {
		t.Fatal(e)
	}
	cancelOpen()
	defer s.Close()
	payload := bytes.Repeat([]byte("half-close-test"), 9000)
	if _, e = s.Write(payload); e != nil {
		t.Fatal(e)
	}
	if e = s.CloseWrite(); e != nil {
		t.Fatal(e)
	}
	if e = s.CloseWrite(); e != nil {
		t.Fatal("duplicate half close", e)
	}
	reply, e := io.ReadAll(s)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(reply, append([]byte("reply:"), payload...)) {
		t.Fatal("TCP bytes changed")
	}
	if e = <-targetDone; e != nil {
		t.Fatal(e)
	}
	udp, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if e != nil {
		t.Fatal(e)
	}
	defer udp.Close()
	udpDone := make(chan struct{})
	go func() {
		defer close(udpDone)
		buf := make([]byte, 65535)
		for {
			n, p, e := udp.ReadFromUDP(buf)
			if e != nil {
				return
			}
			if _, e = udp.WriteToUDP(buf[:n], p); e != nil {
				return
			}
		}
	}()
	defer func() { udp.Close(); <-udpDone }()
	dest, e := endpoint.ParseAddress(udp.LocalAddr().String())
	if e != nil {
		t.Fatal(e)
	}
	a, e := client.OpenAssociation(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	for _, p := range [][]byte{nil, []byte("one"), bytes.Repeat([]byte{3}, 50000)} {
		if e = a.Send(ctx, dest, p); e != nil {
			t.Fatal(e)
		}
		buf := make([]byte, 65507)
		n, from, e := a.Receive(ctx, buf)
		if e != nil || from != dest || !bytes.Equal(buf[:n], p) {
			t.Fatal("UDP boundary or source", n, from, e)
		}
	}
	if e = a.Send(ctx, dest, []byte("too-large-for-dst")); e != nil {
		t.Fatal(e)
	}
	if n, _, e := a.Receive(ctx, make([]byte, 1)); n != 0 || !errors.Is(e, transport.ErrShortBuffer) {
		t.Fatal("truncated datagram delivered", n, e)
	}
	if e = a.Send(ctx, dest, []byte("after-discard")); e != nil {
		t.Fatal(e)
	}
	buf := make([]byte, 32)
	n, _, e := a.Receive(ctx, buf)
	if e != nil || string(buf[:n]) != "after-discard" {
		t.Fatal("discard damaged next datagram", e)
	}
}
func TestDirectDeadlineUpdateAndCancellation(t *testing.T) {
	client, _, ctx := running(t)
	target, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := target.Accept()
		if e == nil {
			accepted <- c
		}
	}()
	dest, _ := endpoint.ParseAddress(target.Addr().String())
	s, e := client.DialStream(ctx, dest)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	peer := <-accepted
	defer peer.Close()
	result := make(chan error, 1)
	go func() { _, e := s.Read(make([]byte, 1)); result <- e }()
	s.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
	select {
	case e = <-result:
		if !errors.Is(e, os.ErrDeadlineExceeded) {
			t.Fatal("deadline", e)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not interrupt pending read")
	}
	s.SetReadDeadline(time.Time{})
	if _, e = peer.Write([]byte{7}); e != nil {
		t.Fatal(e)
	}
	p := make([]byte, 1)
	if _, e = io.ReadFull(s, p); e != nil || p[0] != 7 {
		t.Fatal("deadline could not be reset", e)
	}
	go func() { _, e := s.Read(p); result <- e }()
	s.Close()
	select {
	case e = <-result:
		if e == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock read")
	}
}
func TestEndpointAndIdentityRejections(t *testing.T) {
	for _, v := range []string{"127.0.0.1:0", "[fe80::1%eth0]:80", "001.2.3.4:80", "bad_label:80"} {
		if _, e := endpoint.ParseAddress(v); e == nil {
			t.Fatal("invalid endpoint accepted", v)
		}
	}
	e, err := endpoint.Parse("EXAMPLE.Test.", 443)
	if err != nil || e.Host() != "example.test" {
		t.Fatal(e, err)
	}
	s, c, _ := contracttest.Identities(t)
	bad := s.Certificate()
	bad.PrivateKey = c.Certificate().PrivateKey
	if _, err = identity.FromTLS(bad, identity.Server); err == nil {
		t.Fatal("mismatched signer accepted")
	}
	if _, err = identity.FromTLS(c.Certificate(), identity.Server); err == nil {
		t.Fatal("wrong certificate usage accepted")
	}
}

func TestServeStartupFailurePublishesReadiness(t *testing.T) {
	serverID, clientID, roots := contracttest.Identities(t)
	m, e := model.Generate(1)
	if e != nil {
		t.Fatal(e)
	}
	server, e := core.NewServer(core.ServerOptions{Bucket: "owned-test", Model: m, Identity: serverID, ClientRoots: roots, ClientFingerprints: []string{clientID.Fingerprint()}})
	if e != nil {
		t.Fatal(e)
	}
	if e = server.Serve(context.Background(), nil); e == nil {
		t.Fatal("nil listener accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = server.WaitReady(ctx); e == nil || errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("startup failure did not publish readiness", e)
	}
}
