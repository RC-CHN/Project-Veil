package socks5

import (
	"testing"
	"veil.local/core/endpoint"
)

func TestDatagramDomainAndFragmentRejection(t *testing.T) {
	target, e := endpoint.Parse("EXAMPLE.test", 53)
	if e != nil {
		t.Fatal(e)
	}
	for _, payload := range [][]byte{nil, []byte("payload")} {
		raw, e := encodeUDP(target, payload)
		if e != nil {
			t.Fatal(e)
		}
		got, p, e := decodeUDP(raw)
		if e != nil || got != target || string(p) != string(payload) {
			t.Fatal("datagram round trip", e)
		}
		raw[2] = 1
		if _, _, e = decodeUDP(raw); e == nil {
			t.Fatal("fragment accepted")
		}
	}
}
