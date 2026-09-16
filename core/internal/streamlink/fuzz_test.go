package streamlink

import (
	"context"
	"reflect"
	"testing"
)

// Compare whole-buffer and arbitrary-fragment decoding, including terminal
// rejection and bounded cleanup. No consumer runs, so state is deterministic.
func FuzzFragmentedFrames(f *testing.F) {
	f.Add(frame(Data, 0, 0, []byte("abc")), uint8(1))
	f.Add(append(frame(Data, 0, 0, []byte("abc")), frame(Fin, 0, 3, nil)...), uint8(7))
	f.Add(frame(Credit, 1, 96, nil), uint8(16))
	f.Add([]byte{1, 0, 1, 0, 255, 255, 255, 255}, uint8(255))
	f.Fuzz(func(t *testing.T, p []byte, step uint8) {
		if len(p) > 2*(Header+MaxData) {
			t.Skip()
		}
		links := make([]*Link, 2)
		for i := range links {
			l, e := New(context.Background(), Config{128, 1024})
			if e != nil {
				t.Fatal(e)
			}
			links[i] = l
			defer l.Close(nil)
			if _, e = l.Write(context.Background(), make([]byte, 96)); e != nil {
				t.Fatal(e)
			}
			if e = l.Finish(); e != nil {
				t.Fatal(e)
			}
			if _, _, e = l.Lease(256); e != nil {
				t.Fatal(e)
			}
			if e = l.Commit(); e != nil {
				t.Fatal(e)
			}
		}
		whole := links[0].Feed(p)
		var split error
		for at := 0; at < len(p); {
			n := min(int(step)+1, len(p)-at)
			split = links[1].Feed(p[at : at+n])
			at += n
			if split != nil {
				break
			}
		}
		if (whole == nil) != (split == nil) || !reflect.DeepEqual(links[0].Status(), links[1].Status()) {
			t.Fatalf("fragmented decode differs: %v %v", whole, split)
		}
		for _, l := range links {
			s := l.Status()
			if s.Received > 128 || s.Consumed != 0 || s.Acked > s.Sent || s.MaxSendOutstanding > 128 || s.MaxReceiveOutstanding > 128 || l.parsed > Header+MaxData {
				t.Fatal(s)
			}
			_ = l.PeerDone()
			l.Close(nil)
			s = l.Status()
			if !s.Closed || s.Queued != 0 || s.ReceiveQueued != 0 || s.Pending {
				t.Fatal(s)
			}
		}
	})
}
