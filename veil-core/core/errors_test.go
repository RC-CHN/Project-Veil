package core

import (
	"bytes"
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
		var b bytes.Buffer
		wire.Write(&b, wire.OpenError, []byte{byte(got)})
		if err := wire.ReadOpenResult(&wire.Reader{R: &b}); !errors.Is(err, tc.code) {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	wire.Write(&b, wire.OpenOK, nil)
	if err := wire.ReadOpenResult(&wire.Reader{R: &b}); err != nil {
		t.Fatal(err)
	}
}
