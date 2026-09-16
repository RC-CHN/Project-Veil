package datagram

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	d "veil.local/core/internal/destination"
)

func TestIndependentDatagramVectors(t *testing.T) {
	raw, e := os.ReadFile("../../testdata/datagram-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var v struct {
		Cases []struct {
			Host, RecordSHA256, RecordHex, SOCKSSHA256 string
			Port                                       uint16
			PayloadBytes, RecordBytes, SOCKSBytes      int
			SOCKSAllowed                               bool
		}
	}
	if e = json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	for _, c := range v.Cases {
		payload := make([]byte, c.PayloadBytes)
		for i := range payload {
			payload[i] = byte(i*131 + 17)
		}
		packet := Packet{Address: d.Address{Host: c.Host, Port: c.Port}, Payload: payload}
		record, e := Encode(packet)
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(record)
		if len(record) != c.RecordBytes || hex.EncodeToString(sum[:]) != c.RecordSHA256 {
			t.Fatal("independent record", c.Host, c.PayloadBytes)
		}
		decoded, e := Decode(record)
		if e != nil || decoded.Address != packet.Address || !bytes.Equal(decoded.Payload, payload) {
			t.Fatal(e)
		}
		if c.RecordHex != "" && hex.EncodeToString(record) != c.RecordHex {
			t.Fatal("wire bytes")
		}
		socks, e := SOCKS(packet)
		if (e == nil) != c.SOCKSAllowed {
			t.Fatal("SOCKS envelope cap")
		}
		if e == nil {
			sum = sha256.Sum256(socks)
			if len(socks) != c.SOCKSBytes || hex.EncodeToString(sum[:]) != c.SOCKSSHA256 {
				t.Fatal("independent SOCKS")
			}
		}
	}
}
