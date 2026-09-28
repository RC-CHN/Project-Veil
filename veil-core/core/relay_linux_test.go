package core

import (
	"context"
	"io"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRelayPeerClosesBeforeFinalFIN(t *testing.T) {
	local, backend := tcpPair(t)
	remote, peer := tcpPair(t)
	done := make(chan error, 1)
	go func() { done <- relay(context.Background(), local, remote, 2*time.Second) }()
	// A TLS server can close TCP after the response, before the client sends
	// its final close_notify. Both reads finish cleanly, but the last shutdown
	// encounters a socket already in TCP_CLOSE after that late notification.
	if _, err := backend.Write([]byte("complete response")); err != nil {
		t.Fatal(err)
	}
	backend.Close()
	body, err := io.ReadAll(peer)
	if err != nil || string(body) != "complete response" {
		t.Fatalf("response: %q %v", body, err)
	}
	if _, err := peer.Write([]byte("late close notification")); err != nil {
		t.Fatal(err)
	}
	raw, err := local.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		var info *unix.TCPInfo
		var socketErr error
		if err := raw.Control(func(fd uintptr) {
			info, socketErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		}); err != nil {
			t.Fatal(err)
		}
		if socketErr != nil {
			t.Fatal(socketErr)
		}
		if info.State == 7 { // Linux TCP_CLOSE
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late notification did not reach the closed peer")
		}
		time.Sleep(time.Millisecond)
	}
	peer.CloseWrite()
	if err := <-done; err != nil {
		t.Fatal("completed transfer reported as failed:", err)
	}
}
