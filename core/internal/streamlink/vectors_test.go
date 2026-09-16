package streamlink

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestIndependentPythonVectors(t *testing.T) {
	p, e := os.ReadFile("../../testdata/streamlink-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var rows []struct {
		Name          string
		Kind, Flags   byte
		Offset        uint64
		Payload, Wire string
	}
	if e = json.Unmarshal(p, &rows); e != nil {
		t.Fatal(e)
	}
	if len(rows) != 4 {
		t.Fatal(len(rows))
	}
	for _, r := range rows {
		t.Run(r.Name, func(t *testing.T) {
			payload, e := hex.DecodeString(r.Payload)
			if e != nil {
				t.Fatal(e)
			}
			if got := hex.EncodeToString(frame(r.Kind, r.Flags, r.Offset, payload)); got != r.Wire {
				t.Fatal(got, r.Wire)
			}
		})
	}
}
