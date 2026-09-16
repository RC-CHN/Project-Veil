package streamopen

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	d "veil.local/core/internal/destination"
)

func TestIndependentPythonVectors(t *testing.T) {
	p, e := os.ReadFile("../../testdata/streamopen-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var rows []struct {
		Kind, Host, Wire string
		Port             uint16
		Window           int
		MaxBytes         uint64 `json:"max_bytes"`
		Code             byte
	}
	if e = json.Unmarshal(p, &rows); e != nil {
		t.Fatal(e)
	}
	if len(rows) != 9 {
		t.Fatal(len(rows))
	}
	for _, r := range rows {
		var p []byte
		var e error
		if r.Kind == "request" {
			p, e = EncodeRequest(Request{Address: d.Address{Host: r.Host, Port: r.Port}, Limits: Limits{r.Window, r.MaxBytes}})
		} else {
			p, e = EncodeResult(Result{r.Code, Limits{r.Window, r.MaxBytes}})
		}
		if e != nil || hex.EncodeToString(p) != r.Wire {
			t.Fatal(r, e, hex.EncodeToString(p))
		}
	}
}
