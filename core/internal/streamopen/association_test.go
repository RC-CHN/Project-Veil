package streamopen

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	d "veil.local/core/internal/destination"
)

func TestUDPAssociationPrefix(t *testing.T) {
	request := Request{Network: NetworkUDP, Limits: Limits{Window: 65536, MaxBytes: 8 << 20}}
	raw, e := EncodeRequest(request)
	if e != nil {
		t.Fatal(e)
	}
	vector, e := os.ReadFile("../../testdata/datagram-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var v struct{ AssociationHex string }
	if e = json.Unmarshal(vector, &v); e != nil {
		t.Fatal(e)
	}
	expected, e := hex.DecodeString(v.AssociationHex)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(raw, expected) {
		t.Fatal(hex.EncodeToString(raw))
	}
	client, e := NewClient(request)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close(nil)
	server, _ := NewServer(request.Limits)
	defer server.Close(nil)
	p, _ := client.TakePrefix(4096)
	for _, v := range p {
		if _, e = server.Feed([]byte{v}); e != nil {
			t.Fatal(e)
		}
	}
	parsed, e := server.WaitRequest(context.Background())
	if e != nil || parsed != request {
		t.Fatal(parsed, e)
	}
	if e = server.Respond(Result{OK, request.Limits}); e != nil {
		t.Fatal(e)
	}
	p, _ = server.TakePrefix(4096)
	rest, e := client.Feed(append(p, 1, 2))
	if e != nil || !bytes.Equal(rest, []byte{1, 2}) {
		t.Fatal(e, rest)
	}
	request.Address = d.Address{Host: "127.0.0.2", Port: 53}
	if _, e = EncodeRequest(request); e == nil {
		t.Fatal("association with target")
	}
	request.Address = d.Address{}
	request.Network = 3
	if _, e = EncodeRequest(request); e == nil {
		t.Fatal("unknown network")
	}
}
