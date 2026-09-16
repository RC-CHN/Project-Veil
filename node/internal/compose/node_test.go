package compose

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
	"veil.local/core/contracttest"
	"veil.local/core/model"
	"veil.local/node/internal/config"
)

func TestNodeSOCKSTCPAndUDP(t *testing.T) {
	serverID, clientID, roots := contracttest.Identities(t)
	m, e := model.Generate(4)
	if e != nil {
		t.Fatal(e)
	}
	server, e := New(config.Loaded{Config: config.Config{Role: "server", Listen: "127.0.0.1:0", Bucket: "owned-test", ClientFingerprints: []string{clientID.Fingerprint()}, AllowCIDRs: []string{"127.0.0.0/8"}, MaxConnections: 8}, Model: m, Identity: serverID, Roots: roots})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := make(chan error, 2)
	go func() { done <- server.Run(ctx) }()
	if e = server.WaitReady(ctx); e != nil {
		cancel()
		t.Fatal(e)
	}
	client, e := New(config.Loaded{Config: config.Config{Role: "client", Listen: "127.0.0.1:0", Bucket: "owned-test", ServerURL: "https://owned.test", DialAddress: server.Snapshot().Listen, MaxConnections: 8}, Model: m, Identity: clientID, Roots: roots})
	if e != nil {
		cancel()
		t.Fatal(e)
	}
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
				t.Error("node failed to join")
			}
		}
	})
	if e = client.WaitReady(ctx); e != nil {
		t.Fatal(e)
	}
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
			_, e = c.Write(append([]byte("echo:"), p...))
		}
		targetDone <- e
	}()
	dial := func(command byte, address netip.AddrPort) (*net.TCPConn, netip.AddrPort) {
		t.Helper()
		raw, e := net.Dial("tcp", client.Snapshot().Listen)
		if e != nil {
			t.Fatal(e)
		}
		c := raw.(*net.TCPConn)
		c.SetDeadline(time.Now().Add(5 * time.Second))
		t.Cleanup(func() { c.Close() })
		c.Write([]byte{5, 1, 0})
		var hello [2]byte
		if _, e = io.ReadFull(c, hello[:]); e != nil || hello != [2]byte{5, 0} {
			t.Fatal("greeting", e, hello)
		}
		req := []byte{5, command, 0, 1}
		req = append(req, address.Addr().AsSlice()...)
		req = binary.BigEndian.AppendUint16(req, address.Port())
		c.Write(req)
		var response [10]byte
		if _, e = io.ReadFull(c, response[:]); e != nil || response[1] != 0 || response[3] != 1 {
			t.Fatal("SOCKS response", e, response)
		}
		return c, netip.AddrPortFrom(netip.AddrFrom4([4]byte(response[4:8])), binary.BigEndian.Uint16(response[8:]))
	}
	addr, _ := netip.ParseAddrPort(target.Addr().String())
	c, _ := dial(1, addr)
	c.Write([]byte("through-node"))
	c.CloseWrite()
	p, e := io.ReadAll(c)
	if e != nil || string(p) != "echo:through-node" {
		t.Fatal("TCP relay or half-close", string(p), e)
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
		p := make([]byte, 65535)
		for {
			n, from, e := udp.ReadFromUDP(p)
			if e != nil {
				return
			}
			udp.WriteToUDP(p[:n], from)
		}
	}()
	defer func() { udp.Close(); <-udpDone }()
	control, relay := dial(3, netip.MustParseAddrPort("0.0.0.0:0"))
	defer control.Close()
	u, e := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(relay))
	if e != nil {
		t.Fatal(e)
	}
	defer u.Close()
	u.SetDeadline(time.Now().Add(5 * time.Second))
	dest, _ := netip.ParseAddrPort(udp.LocalAddr().String())
	for _, payload := range [][]byte{nil, []byte("UDP-through-node")} {
		raw := []byte{0, 0, 0, 1}
		raw = append(raw, dest.Addr().AsSlice()...)
		raw = binary.BigEndian.AppendUint16(raw, dest.Port())
		raw = append(raw, payload...)
		if _, e = u.Write(raw); e != nil {
			t.Fatal(e)
		}
		buf := make([]byte, 1024)
		n, e := u.Read(buf)
		if e != nil || n != len(raw) || string(buf[:n]) != string(raw) {
			t.Fatal("UDP relay boundary", n, e)
		}
	}
}
