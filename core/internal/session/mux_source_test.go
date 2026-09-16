package session

import (
	"context"
	"errors"
	"testing"
	"time"

	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

func TestMuxSourceReceiptsCleanupAndEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	limits := sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}
	c, err := sm.New(ctx, sm.Config{Client: true, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sm.New(ctx, sm.Config{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	a, b := &muxSource{mux: c}, &muxSource{mux: s}
	t.Cleanup(func() { a.close(); b.close() })
	transfer := func(from, to *muxSource) {
		t.Helper()
		p, eof, e := from.lease(65536)
		if e != nil {
			t.Fatal(e)
		}
		st := from.status()
		if !st.Pending || st.Leased != len(p) || st.Used < len(p) {
			t.Fatal("lost leased control bytes", st, len(p))
		}
		if _, _, e = from.lease(65536); e == nil {
			t.Fatal("duplicate lease accepted")
		}
		if e = to.deliver(p, eof); e != nil {
			t.Fatal(e)
		}
		if e = from.commit(); e != nil {
			t.Fatal(e)
		}
		if e = from.commit(); e == nil {
			t.Fatal("duplicate receipt accepted")
		}
	}
	if a.status().Used != sm.Header+sm.SettingsSize {
		t.Fatal("missing SETTINGS")
	}
	transfer(a, b)
	transfer(b, a)
	r := so.Request{Network: so.NetworkUDP, Limits: so.Limits{Window: 16384, MaxBytes: 1 << 20}}
	left, err := c.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	defer left.Release()
	if a.status().Used == 0 {
		t.Fatal("lazy OPEN invisible")
	}
	transfer(a, b)
	right, err := s.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer right.Release()
	if err = right.Respond(so.Result{Code: so.Busy}); err != nil {
		t.Fatal(err)
	}
	if b.status().Used == 0 {
		t.Fatal("lazy RESULT invisible")
	}
	transfer(b, a)
	transfer(a, b)
	if err = waitMuxTerminal(left); err != nil {
		t.Fatal(err)
	}
	if err = waitMuxTerminal(right); err != nil {
		t.Fatal(err)
	}
	left.Release()
	if _, err = c.Open(r); !errors.Is(err, sm.ErrBusy) {
		t.Fatal("slot reused before peer cleanup", err)
	}
	right.Release()
	if b.status().Used != sm.Header+4 {
		t.Fatal("cleanup GRANT invisible", b.status())
	}
	if _, err = c.Open(r); !errors.Is(err, sm.ErrBusy) {
		t.Fatal("slot reused before GRANT receipt", err)
	}
	transfer(b, a)
	c.Drain()
	s.Drain()
	for i := 0; i < 8 && !(a.status().AckedEOF && b.status().AckedEOF); i++ {
		transfer(a, b)
		transfer(b, a)
	}
	for _, source := range []*muxSource{a, b} {
		st := source.status()
		if !st.AckedEOF || !st.InputEOF || st.Used != 0 || st.Pending {
			t.Fatal("outer terminal incomplete", st)
		}
		source.close()
		if st := source.mux.Status(); !st.Closed || st.Error != "" || st.Active != 0 || st.ReservedWindow != 0 {
			t.Fatal(st)
		}
	}
	aup, adown := a.hashes()
	bdown, bup := b.hashes()
	if aup == "" || aup != bup || adown != bdown {
		t.Fatal("source byte reconciliation")
	}
}
