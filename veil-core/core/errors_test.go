package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
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
		{fmt.Errorf("dial: %w", syscall.ECONNREFUSED), TargetRefused},
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
