package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
	"veil/internal/wire"
)

func TestTargetFailureReporting(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code TargetError
	}{
		{context.DeadlineExceeded, TargetTimeout},
		{&net.DNSError{IsNotFound: true}, TargetDNS},
		{&net.DNSError{IsTimeout: true}, TargetTimeout},
		{fmt.Errorf("dial: %w", errConnectionRefused), TargetRefused},
		{errors.New("other"), TargetFailed},
	} {
		got := targetFailure(tc.err)
		if got != tc.code {
			t.Fatal(got, tc.code)
		}
		if err := wire.OpenFailure(byte(got)); !errors.Is(err, tc.code) {
			t.Fatal(err)
		}
	}
}

func TestTargetFailureFromRealSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("closed listener accepted a connection")
	}
	if got := targetFailure(err); got != TargetRefused {
		t.Fatalf("real connection refusal reported as %v: %v", got, err)
	}
}
