package streamopen

import (
	"bytes"
	"reflect"
	"testing"
)

func FuzzHandshakeFragments(f *testing.F) {
	request, _ := EncodeRequest(example())
	result, _ := EncodeResult(Result{OK, example().Limits})
	reject, _ := EncodeResult(Result{Code: Denied})
	association, _ := EncodeRequest(Request{Network: NetworkUDP, Limits: example().Limits})
	f.Add(association, uint8(3), false)
	f.Add(request, uint8(1), false)
	f.Add(result, uint8(7), true)
	f.Add(append(result, 1, 2, 3), uint8(3), true)
	f.Add(reject, uint8(24), true)
	f.Add([]byte{1, 1, 0, 0, 255, 255, 255, 255}, uint8(0), false)
	f.Fuzz(func(t *testing.T, p []byte, step uint8, client bool) {
		if len(p) > 256<<10 {
			t.Skip()
		}
		hs := make([]*Handshake, 2)
		for i := range hs {
			if client {
				hs[i], _ = NewClient(example())
				hs[i].TakePrefix(4096)
			} else {
				hs[i], _ = NewServer(example().Limits)
			}
			defer hs[i].Close(nil)
		}
		whole, e1 := hs[0].Feed(p)
		var split []byte
		var e2 error
		for at := 0; at < len(p); {
			n := min(int(step)+1, len(p)-at)
			rest, e := hs[1].Feed(p[at : at+n])
			split = append(split, rest...)
			at += n
			if e != nil {
				e2 = e
				break
			}
		}
		if (e1 == nil) != (e2 == nil) || !bytes.Equal(whole, split) || !reflect.DeepEqual(hs[0].Status(), hs[1].Status()) {
			t.Fatalf("fragment mismatch %v %v", e1, e2)
		}
		for _, h := range hs {
			st := h.Status()
			if st.ParserBytes > Header+MaxBody || st.QueuedBytes > Header+MaxBody {
				t.Fatal(st)
			}
			h.Close(nil)
			st = h.Status()
			if !st.Closed || st.ParserBytes != 0 || st.QueuedBytes != 0 {
				t.Fatal(st)
			}
		}
	})
}
