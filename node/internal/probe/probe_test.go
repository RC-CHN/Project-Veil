package probe

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestProbeFailureDoesNotReportHealthOrDialTarget(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	socks, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := socks.Accept()
		if err == nil {
			defer c.Close()
			c.Write([]byte{5, 255})
		}
	}()
	report, err := Run(context.Background(), Options{SOCKS: socks.Addr().String(), TCPEcho: target.Addr().String(), Timeout: time.Second})
	socks.Close()
	<-done
	if err == nil || report.EndToEnd != "failed" || len(report.Checks) != 1 || report.Checks[0].Passed {
		t.Fatal("failed proxy reported health", report, err)
	}
	target.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
	if c, err := target.Accept(); err == nil {
		c.Close()
		t.Fatal("probe bypassed proxy")
	}
}

func TestRejectZeroUDPRelayPortAndInvalidAddress(t *testing.T) {
	for _, data := range [][]byte{{1, 127, 0, 0, 1, 0, 0}, {7}, {3, 3, 'a'}} {
		if _, err := readAddress(bytes.NewReader(data), false); err == nil {
			t.Fatal("invalid UDP address accepted", data)
		}
	}
}
