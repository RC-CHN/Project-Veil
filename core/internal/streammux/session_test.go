package streammux

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	so "veil.local/core/internal/streamopen"
)

func pair(t *testing.T, n int) (*Session, *Session) {
	t.Helper()
	cfg := settings()
	cfg.Streams = n
	return pairWithLimits(t, cfg)
}
func pairWithLimits(t *testing.T, cfg Settings) (*Session, *Session) {
	t.Helper()
	c, e := New(context.Background(), Config{Client: true, Limits: cfg})
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(context.Background(), Config{Limits: cfg})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c.Close(nil)
		s.Close(nil)
		for _, m := range []*Session{c, s} {
			m.mu.Lock()
			entries := append([]*Stream(nil), m.order...)
			m.mu.Unlock()
			for _, v := range entries {
				v.Release()
			}
		}
	})
	xfer(t, c, s)
	xfer(t, s, c)
	if !c.Status().Ready || !s.Status().Ready {
		t.Fatal("settings not ready")
	}
	return c, s
}
func xfer(t *testing.T, from, to *Session) bool {
	t.Helper()
	p, eof, e := from.Lease(65536)
	if e != nil {
		t.Fatal(e)
	}
	if e = to.Feed(p); e != nil {
		t.Fatal(e)
	}
	if eof {
		if e = to.PeerDone(); e != nil {
			t.Fatal(e)
		}
	}
	if e = from.Commit(); e != nil {
		t.Fatal(e)
	}
	return eof
}
func request() so.Request {
	return so.Request{Network: so.NetworkUDP, Limits: so.Limits{Window: 65536, MaxBytes: 8 << 20}}
}
func openPair(t *testing.T, c, s *Session, code byte) (*Stream, *Stream) {
	t.Helper()
	a, e := c.Open(request())
	if e != nil {
		t.Fatal(e)
	}
	xfer(t, c, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, e := s.Accept(ctx)
	if e != nil {
		t.Fatal(e)
	}
	r := so.Result{Code: code}
	if code == so.OK {
		r.Limits = a.Request().Limits
	}
	if e = b.Respond(r); e != nil {
		t.Fatal(e)
	}
	xfer(t, s, c)
	return a, b
}
func rejectedPair(t *testing.T, c, s *Session) (*Stream, *Stream) {
	a, b := openPair(t, c, s, so.Busy)
	xfer(t, c, s)
	for _, v := range []*Stream{a, b} {
		select {
		case <-v.Done():
		default:
			t.Fatal("rejection not terminal", v.Status())
		}
	}
	return a, b
}
func TestGrantWaitsForCleanupAndBothLocalBudgets(t *testing.T) {
	c, s := pair(t, 1)
	a, b := rejectedPair(t, c, s)
	a.Release()
	if _, e := c.Open(request()); !errors.Is(e, ErrBusy) {
		t.Fatal("reused remote cleanup slot", e)
	}
	if s.Status().Active != 1 || s.Status().Retired != 0 {
		t.Fatal("server cleanup admission lost")
	}
	b.Release()
	if s.Status().Active != 0 || s.Status().Retired != 1 {
		t.Fatal("server retirement")
	}
	if _, e := c.Open(request()); !errors.Is(e, ErrBusy) {
		t.Fatal("grant not yet delivered", e)
	}
	xfer(t, s, c)
	a, b = rejectedPair(t, c, s)
	b.Release()
	xfer(t, s, c)
	if _, e := c.Open(request()); !errors.Is(e, ErrBusy) {
		t.Fatal("reused local cleanup slot", e)
	}
	a.Release()
	fresh, e := c.Open(request())
	if e != nil || fresh.ID() != 3 {
		t.Fatal("grant recovery", e)
	}
	if c.Status().MaximumActive != 1 || s.Status().MaximumActive != 1 {
		t.Fatal("work peak exceeded")
	}
}
func TestCancelBeforeOpenLeavesUnusedID(t *testing.T) {
	c, s := pair(t, 1)
	a, e := c.Open(request())
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Reset(EndCancelled); e != nil {
		t.Fatal(e)
	}
	a.Release()
	fresh, e := c.Open(request())
	if e != nil || fresh.ID() != 2 {
		t.Fatal("cancelled ID reused or admission held", e)
	}
	xfer(t, c, s)
	if s.Status().Active != 1 || s.Status().Opened != 2 {
		t.Fatal(s.Status())
	}
}
func TestResetCrossesLeasedDataWithoutKillingSibling(t *testing.T) {
	c, s := pair(t, 2)
	a, b := openPair(t, c, s, so.OK)
	if _, e := a.Link().Write(context.Background(), []byte("already leased")); e != nil {
		t.Fatal(e)
	}
	inflight, _, e := c.Lease(65536)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Reset(EndCancelled); e != nil {
		t.Fatal(e)
	}
	xfer(t, s, c)
	if e = s.Feed(inflight); e != nil {
		t.Fatal("in-flight data after local reset", e)
	}
	if e = c.Commit(); e != nil {
		t.Fatal("reset pending lease", e)
	}
	xfer(t, c, s)
	for _, v := range []*Stream{a, b} {
		select {
		case <-v.Done():
		default:
			t.Fatal("reset not joined")
		}
		v.Release()
	}
	xfer(t, s, c)
	x, y := rejectedPair(t, c, s)
	x.Release()
	y.Release()
	if c.Status().Closed || s.Status().Closed {
		t.Fatal("reset killed carrier")
	}
}
func TestOuterEOFAcceptsCompletedPeerWhileLocalCleanupWaits(t *testing.T) {
	c, s := pair(t, 1)
	a, b := rejectedPair(t, c, s)
	a.Release()
	c.Drain()
	s.Drain()
	xfer(t, c, s)
	xfer(t, s, c)
	if !xfer(t, c, s) {
		t.Fatal("client EOF not emitted")
	}
	if !s.Status().PeerEOF || s.Status().Active != 1 {
		t.Fatal("local cleanup was not retained")
	}
	b.Release()
	if !xfer(t, s, c) {
		t.Fatal("server EOF not emitted after cleanup")
	}
	// Outer models can repeat an already asserted directional EOF until their
	// final paired transaction commits. No additional bytes may follow it.
	if e := s.Feed(nil); e != nil {
		t.Fatal(e)
	}
	if e := s.PeerDone(); e != nil {
		t.Fatal(e)
	}
	if e := s.Feed([]byte{1}); e == nil {
		t.Fatal("accepted bytes after EOF")
	}
}
func TestFairnessAndIndependentConsumptionCredit(t *testing.T) {
	c, s := pair(t, 2)
	a, b := openPair(t, c, s, so.OK)
	x, y := openPair(t, c, s, so.OK)
	if _, e := a.Link().Write(context.Background(), bytes.Repeat([]byte{0x7a}, 65536)); e != nil {
		t.Fatal(e)
	}
	for a.Link().Status().Queued > 0 {
		xfer(t, c, s)
	}
	if b.Link().Status().ReceiveQueued != 65536 {
		t.Fatal("slow consumer did not retain full window")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	n, e := a.Link().Write(ctx, []byte{1})
	cancel()
	if n != 0 || !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("outer receipt released consumer credit", n, e)
	}
	clientBody, serverBody := []byte("small sibling request"), []byte("small sibling response")
	var receivedClient, receivedServer bytes.Buffer
	workers := make(chan error, 2)
	var joined sync.WaitGroup
	joined.Add(2)
	go func() {
		defer joined.Done()
		workers <- x.Link().Consume(x.Context(), &receivedClient, func() error { return nil })
	}()
	go func() {
		defer joined.Done()
		workers <- y.Link().Consume(y.Context(), &receivedServer, func() error { return nil })
	}()
	t.Cleanup(func() { x.Reset(EndCancelled); y.Reset(EndCancelled); joined.Wait() })
	for _, v := range []struct {
		s *Stream
		p []byte
	}{{x, clientBody}, {y, serverBody}} {
		if _, e = v.s.Link().Write(context.Background(), v.p); e != nil {
			t.Fatal(e)
		}
		if e = v.s.Link().Finish(); e != nil {
			t.Fatal(e)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for !x.Status().PeerEnded || !y.Status().PeerEnded {
		if time.Now().After(deadline) {
			t.Fatal("sibling stalled", x.Status(), y.Status())
		}
		xfer(t, c, s)
		xfer(t, s, c)
		runtime.Gosched()
	}
	for i := 0; i < 2; i++ {
		select {
		case e := <-workers:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(time.Second):
			t.Fatal("consumer worker did not finish")
		}
	}
	if !bytes.Equal(receivedClient.Bytes(), serverBody) || !bytes.Equal(receivedServer.Bytes(), clientBody) {
		t.Fatal("sibling data mismatch")
	}
	if a.Link().Status().Acked != 0 || b.Link().Status().Consumed != 0 {
		t.Fatal("slow stream credit was manufactured")
	}
	x.Release()
	y.Release()
	a.Reset(EndCancelled)
	b.Reset(EndCancelled)
}

func TestDrainCrossesQueuedOpenAndIDBudget(t *testing.T) {
	cfg := settings()
	cfg.Streams = 1
	cfg.Opened = 1
	c, s := pairWithLimits(t, cfg)
	a, e := c.Open(request())
	if e != nil {
		t.Fatal(e)
	}
	if !c.Status().Draining {
		t.Fatal("last ID did not drain")
	}
	s.Drain()
	xfer(t, s, c)
	if _, e = c.Open(request()); !errors.Is(e, ErrDraining) {
		t.Fatal(e)
	}
	// The queued OPEN must precede the client DRAIN even though the server's
	// opposite-direction DRAIN reached it first.
	xfer(t, c, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, e := s.Accept(ctx)
	if e != nil || b.ID() != a.ID() {
		t.Fatal(e)
	}
	if !s.Status().PeerDraining {
		t.Fatal("client did not drain")
	}
	if e = b.Respond(so.Result{Code: so.Denied}); e != nil {
		t.Fatal(e)
	}
	xfer(t, s, c)
	xfer(t, c, s)
	a.Release()
	b.Release()
	xfer(t, s, c)
	xfer(t, c, s)
	if !c.Status().SourceEOF || !s.Status().SourceEOF {
		t.Fatal("drain not complete")
	}
}
func TestProtocolViolationsFailClosedWithoutReleasingWork(t *testing.T) {
	for _, name := range []string{"grant-before-open", "grant-wrong-direction", "duplicate-settings", "duplicate-open", "data-before-result", "normal-end-before-result", "expanded-result"} {
		t.Run(name, func(t *testing.T) {
			c, s := pair(t, 2)
			target := c
			var f Frame
			switch name {
			case "grant-before-open":
				f = Frame{Kind: GrantKind, Body: []byte{0, 0, 0, 1}}
			case "grant-wrong-direction":
				target = s
				f = Frame{Kind: GrantKind, Body: []byte{0, 0, 0, 1}}
			case "duplicate-settings":
				p, _ := EncodeSettings(settings())
				f = Frame{Kind: SettingsKind, Body: p}
			default:
				a, e := c.Open(request())
				if e != nil {
					t.Fatal(e)
				}
				xfer(t, c, s)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, acceptedErr := s.Accept(ctx)
				cancel()
				if acceptedErr != nil {
					t.Fatal(acceptedErr)
				}
				switch name {
				case "duplicate-open":
					target = s
					p, _ := so.EncodeRequest(request())
					f = Frame{Kind: OpenKind, ID: a.ID(), Body: p}
				case "data-before-result":
					target = s
					f = Frame{Kind: DataKind, ID: a.ID(), Body: []byte{1}}
				case "normal-end-before-result":
					target = s
					f = Frame{Kind: EndKind, ID: a.ID(), Body: []byte{EndNormal}}
				case "expanded-result":
					p, _ := so.EncodeResult(so.Result{Code: so.OK, Limits: so.Limits{Window: 131072, MaxBytes: 8 << 20}})
					f = Frame{Kind: ResultKind, ID: a.ID(), Body: p}
				}
			}
			p, e := Encode(f)
			if e != nil {
				t.Fatal(e)
			}
			before := target.Status().Active
			if e = target.Feed(p); e == nil || !target.Status().Closed {
				t.Fatal("protocol violation accepted", name, e)
			}
			if target.Status().Active != before {
				t.Fatal("failed carrier released application work early")
			}
		})
	}
}
func TestConnectionByteBudgetIncludesMuxFrames(t *testing.T) {
	for _, send := range []bool{false, true} {
		t.Run(map[bool]string{false: "receive", true: "send"}[send], func(t *testing.T) {
			cfg := settings()
			cfg.Streams = 1
			cfg.ConnectionBytes = 4096
			c, s := pairWithLimits(t, cfg)
			a, _ := openPair(t, c, s, so.OK)
			if send {
				if _, e := a.Link().Write(context.Background(), make([]byte, 4096)); e != nil {
					t.Fatal(e)
				}
				if _, _, e := c.Lease(65536); e == nil || !c.Status().Closed {
					t.Fatal("send connection budget", e)
				}
			} else {
				p, e := Encode(Frame{Kind: DataKind, ID: a.ID(), Body: make([]byte, 4096)})
				if e != nil {
					t.Fatal(e)
				}
				if e = s.Feed(p); e == nil || !s.Status().Closed {
					t.Fatal("receive connection budget", e)
				}
			}
		})
	}
}
func TestRoundRobinServesEveryReadyStream(t *testing.T) {
	c, s := pair(t, 8)
	streams := make([]*Stream, 8)
	for i := range streams {
		streams[i], _ = openPair(t, c, s, so.OK)
	}
	for _, v := range streams {
		if _, e := v.Link().Write(context.Background(), bytes.Repeat([]byte{0x42}, 2048)); e != nil {
			t.Fatal(e)
		}
	}
	seen := make(map[uint32]bool)
	for i := 0; i < 8; i++ {
		p, _, e := c.Lease(1024)
		if e != nil {
			t.Fatal(e)
		}
		var decoder Decoder
		if e = decoder.Feed(p, func(f Frame) error {
			if f.Kind == DataKind {
				seen[f.ID] = true
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
		if e = s.Feed(p); e != nil {
			t.Fatal(e)
		}
		if e = c.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	if len(seen) != 8 {
		t.Fatal("ready stream starved", seen)
	}
	if c.Status().ReservedWindow > MaxReservedWindow || s.Status().ReservedWindow > MaxReservedWindow {
		t.Fatal("aggregate credit expanded")
	}
}

func TestClosingCarrierReclaimsOnlyUnacceptedServerEntries(t *testing.T) {
	c, s := pair(t, 2)
	a, e := c.Open(request())
	if e != nil {
		t.Fatal(e)
	}
	xfer(t, c, s)
	s.Close(errors.New("owned shutdown"))
	if s.Status().Active != 0 || s.Status().Ready {
		t.Fatal("unaccepted server entry retained after shutdown", s.Status())
	}
	if _, e = s.Accept(context.Background()); e == nil {
		t.Fatal("accepted a stream after shutdown")
	}
	if c.Status().Active != 1 {
		t.Fatal("client application ownership lost")
	}
	c.Close(errors.New("owned shutdown"))
	if c.Status().Active != 1 {
		t.Fatal("client cleanup slot released early")
	}
	a.Release()
	if c.Status().Active != 0 {
		t.Fatal("client cleanup slot retained")
	}
}
