package socks

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"veil/internal/wire"
)

func TestReadConnectFragmented(t *testing.T) {
	for _, dst := range []string{"127.0.0.1:80", "[2001:db8::1]:443", strings.Repeat("a", 255) + ":53"} {
		addr, err := wire.EncodeAddress(dst)
		if err != nil {
			t.Fatal(err)
		}
		// Offer the supported method last in a maximum-size greeting.
		request := append([]byte{5, 255}, bytes.Repeat([]byte{1}, 254)...)
		request = append(request, 0, 5, 1, 0)
		request = append(request, addr...)
		r := bufio.NewReader(iotest.OneByteReader(bytes.NewReader(request)))
		var reply bytes.Buffer
		got, err := ReadConnect(r, &reply)
		if err != nil || got != dst || !bytes.Equal(reply.Bytes(), []byte{5, 0}) {
			t.Fatalf("CONNECT %q: got %q, method %x, err %v", dst, got, reply.Bytes(), err)
		}
		if _, err := r.ReadByte(); err != io.EOF {
			t.Fatalf("trailing data after CONNECT: %v", err)
		}
	}
}
