package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	so "veil.local/core/internal/streamopen"
)

func TestCancelledDatagramReadNeverEOF(t *testing.T) {
	u := baseDatagramSocket(context.Background())
	defer u.Close()
	u.queue.Close(false)
	u.cancel()
	if _, e := u.Read(make([]byte, 1)); !errors.Is(e, context.Canceled) {
		t.Fatal("cancelled association returned", e)
	}
}
func TestDatagramFINRejectsPartialRecord(t *testing.T) {
	u := baseDatagramSocket(context.Background())
	defer u.Close()
	if _, e := u.Write([]byte{1, 1}); e != nil {
		t.Fatal(e)
	}
	if e := u.CloseWrite(); e == nil {
		t.Fatal("partial UDP record accepted at FIN")
	}
}

type peerMemorySocket struct{ *memorySocket }

func (*peerMemorySocket) RemoteAddr() net.Addr {
	return net.TCPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.2:5000"))
}
func TestSOCKSAssociationSourceBinding(t *testing.T) {
	for _, v := range []struct {
		input []byte
		valid bool
		port  uint16
	}{
		{[]byte{5, 1, 0, 5, 3, 0, 1, 0, 0, 0, 0, 0, 0}, true, 0},
		{[]byte{5, 1, 0, 5, 3, 0, 1, 127, 0, 0, 2, 1, 187}, true, 443},
		{[]byte{5, 1, 0, 5, 3, 0, 1, 127, 0, 0, 1, 0, 0}, false, 0},
		{[]byte{5, 1, 0, 5, 3, 0, 3, 1, 'a', 0, 0}, false, 0},
	} {
		conn := &peerMemorySocket{&memorySocket{input: bytes.NewReader(v.input), fragment: 1}}
		r, e := readSOCKS(conn, true)
		if (e == nil) != v.valid {
			t.Fatal(r, e)
		}
		if v.valid && (r.network != so.NetworkUDP || r.source.Addr() != netip.MustParseAddr("127.0.0.2") || r.source.Port() != v.port || !bytes.Equal(conn.output.Bytes(), []byte{5, 0})) {
			t.Fatal(r, conn.output.Bytes())
		}
	}
}
func FuzzSOCKSAssociation(f *testing.F) {
	f.Add([]byte{5, 1, 0, 5, 3, 0, 1, 0, 0, 0, 0, 0, 0}, uint8(1))
	f.Add([]byte{5, 1, 0, 5, 3, 0, 1, 127, 0, 0, 2, 1, 187}, uint8(7))
	f.Add([]byte{}, uint8(2))
	f.Fuzz(func(t *testing.T, p []byte, step uint8) {
		if len(p) > 4096 {
			t.Skip()
		}
		conn := &peerMemorySocket{&memorySocket{input: bytes.NewReader(p), fragment: int(step)%16 + 1}}
		r, e := readSOCKS(conn, true)
		if len(p)-conn.input.Len() > 522 || conn.output.Len() > 12 {
			t.Fatal("SOCKS bound")
		}
		if e == nil {
			if !bytes.Equal(conn.output.Bytes(), []byte{5, 0}) {
				t.Fatal("premature association success")
			}
			if r.network == so.NetworkUDP {
				if r.source.Addr() != netip.MustParseAddr("127.0.0.2") {
					t.Fatal("wrong association source")
				}
			} else if !r.address.Valid() {
				t.Fatal("invalid target")
			}
		}
		_, _ = io.Copy(io.Discard, conn.input)
	})
}
