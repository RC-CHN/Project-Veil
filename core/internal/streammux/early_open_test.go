package streammux

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	so "veil.local/core/internal/streamopen"
)

func earlyPair(t *testing.T, clientLimits, serverLimits Settings) (*Session, *Session) {
	t.Helper()
	c, e := New(context.Background(), Config{EarlyOpen: true, Client: true, MaxPending: 4, Limits: clientLimits})
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(context.Background(), Config{EarlyOpen: true, MaxPending: 4, Limits: serverLimits})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		for _, m := range []*Session{c, s} {
			m.Close(context.Canceled)
			m.mu.Lock()
			entries := append([]*Stream(nil), m.order...)
			m.mu.Unlock()
			for _, stream := range entries {
				stream.Release()
			}
			if m.Status().Active != 0 {
				t.Error("early pair leaked application ownership")
			}
		}
	})
	return c, s
}

func earlyAccept(t *testing.T, m *Session) *Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, e := m.Accept(ctx)
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestEarlyOpenSingleExchangeAndNegotiatedLimits(t *testing.T) {
	clientLimits, serverLimits := settings(), settings()
	serverLimits.Streams, serverLimits.Window, serverLimits.StreamBytes, serverLimits.Opened = 1, 16384, 8192, 1
	c, s := earlyPair(t, clientLimits, serverLimits)
	a, e := c.OpenFirst(request())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.OpenFirst(request()); e == nil {
		t.Fatal("multiple provisional opens")
	}
	if _, e = c.Open(request()); !errors.Is(e, ErrNotReady) {
		t.Fatal("ordinary OPEN before peer settings", e)
	}
	if c.Status().Ready || a.Link() != nil || a.Status().ResultKnown || c.Status().Active != 1 {
		t.Fatal("provisional OPEN claimed success")
	}
	wantRequest, _ := so.EncodeRequest(a.Request())
	if c.Output().AvailableBytes != 2*Header+SettingsSize+len(wantRequest) {
		t.Fatal("initial SETTINGS and OPEN availability")
	}
	xfer(t, c, s)
	b := earlyAccept(t, s)
	if s.Status().Ready || b.Link() != nil || !s.Status().EarlyOpenReceived || !c.Status().EarlyOpenSent {
		t.Fatal("first request was not accepted before server SETTINGS")
	}
	want := so.Limits{Window: 16384, MaxBytes: 8192}
	if b.Request().Limits != want || s.Status().ReservedWindow != 16384 {
		t.Fatal("early OPEN did not intersect limits", b.Request(), s.Status())
	}
	if e = b.Respond(so.Result{Code: so.OK, Limits: request().Limits}); e == nil {
		t.Fatal("expanded early RESULT accepted")
	}
	if e = b.Respond(so.Result{Code: so.OK, Limits: want}); e != nil {
		t.Fatal(e)
	}
	xfer(t, s, c)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, link, e := a.WaitResult(ctx)
	if e != nil || r.Limits != want || link == nil || !c.Status().Ready || !s.Status().Ready {
		t.Fatal("single exchange did not establish bounded stream", r, e)
	}
	if !c.Status().Draining {
		t.Fatal("negotiated first-ID budget did not drain")
	}
	if _, e = c.Open(request()); !errors.Is(e, ErrDraining) {
		t.Fatal("negotiated opened limit", e)
	}
}

func TestEarlyOpenPayloadHalfCloseAndCleanup(t *testing.T) {
	c, s := earlyPair(t, settings(), settings())
	a, e := c.OpenFirst(request())
	if e != nil {
		t.Fatal(e)
	}
	xfer(t, c, s)
	b := earlyAccept(t, s)
	if e = b.Respond(so.Result{Code: so.OK, Limits: b.Request().Limits}); e != nil {
		t.Fatal(e)
	}
	// Server banner may follow RESULT in the same ordered response. Client
	// output remains unavailable before that successful result is received.
	down := []byte("server banner before client half-close")
	if _, e = b.Link().Write(context.Background(), down); e != nil {
		t.Fatal(e)
	}
	if a.Link() != nil {
		t.Fatal("client DATA enabled before RESULT")
	}
	xfer(t, s, c)
	up := bytes.Repeat([]byte{0xa7}, 32768)
	if _, e = a.Link().Write(context.Background(), up); e != nil {
		t.Fatal(e)
	}
	var gotUp, gotDown bytes.Buffer
	finished := make(chan error, 2)
	for _, v := range []struct {
		stream *Stream
		output io.Writer
	}{{a, &gotDown}, {b, &gotUp}} {
		go func(stream *Stream, w io.Writer) {
			finished <- stream.Link().Consume(stream.Context(), w, func() error { return nil })
		}(v.stream, v.output)
	}
	defer func() { c.Close(context.Canceled); s.Close(context.Canceled); <-finished; <-finished }()
	if e = a.Link().Finish(); e != nil {
		t.Fatal(e)
	}
	if e = b.Link().Finish(); e != nil {
		t.Fatal(e)
	}
	c.Drain()
	s.Drain()
	deadline := time.Now().Add(2 * time.Second)
	for !a.Status().PeerEnded || !b.Status().PeerEnded {
		if time.Now().After(deadline) {
			t.Fatal("early stream completion stalled")
		}
		xfer(t, c, s)
		xfer(t, s, c)
		runtime.Gosched()
	}
	// Consume writes precede remoteDelivered/END under Link's mutex, so END
	// observation synchronizes these completed buffers.
	if !bytes.Equal(gotUp.Bytes(), up) || !bytes.Equal(gotDown.Bytes(), down) {
		t.Fatal("early stream payload mismatch")
	}
	a.Release()
	b.Release()
	for !c.Status().PeerEOF || !s.Status().PeerEOF {
		if time.Now().After(deadline) {
			t.Fatal("early carrier cleanup stalled")
		}
		xfer(t, c, s)
		xfer(t, s, c)
	}
	if c.Status().Active != 0 || s.Status().Active != 0 || c.Status().Pending || s.Status().Pending {
		t.Fatal("early stream retained resources")
	}
}

func TestEarlyOpenRejectsInvalidPhases(t *testing.T) {
	for _, name := range []string{"disabled", "before-settings", "wrong-direction", "wrong-id", "duplicate", "normal-first", "peer-drained", "data-before-result", "expanded-result"} {
		t.Run(name, func(t *testing.T) {
			c, s := earlyPair(t, settings(), settings())
			body, _ := so.EncodeRequest(request())
			f := Frame{Kind: EarlyOpenKind, ID: 1, Body: body}
			target := s
			var accepted *Stream
			switch name {
			case "before-settings":
			case "disabled":
				s.earlyOpen = false
				xfer(t, c, s)
			case "wrong-direction":
				target = c
				xfer(t, s, c)
			case "wrong-id":
				xfer(t, c, s)
				f.ID = 2
			case "peer-drained":
				xfer(t, c, s)
				xfer(t, s, c)
				c.Drain()
				xfer(t, c, s)
			case "normal-first":
				xfer(t, c, s)
				xfer(t, s, c)
				if _, e := c.Open(request()); e != nil {
					t.Fatal(e)
				}
				xfer(t, c, s)
				accepted = earlyAccept(t, s)
			default:
				if _, e := c.OpenFirst(request()); e != nil {
					t.Fatal(e)
				}
				xfer(t, c, s)
				accepted = earlyAccept(t, s)
				if name == "data-before-result" {
					xfer(t, s, c)
					f = Frame{Kind: DataKind, ID: 1, Body: []byte{1}}
				}
				if name == "expanded-result" {
					// The request ceiling is larger than peer SETTINGS. A forged
					// success within the request but above negotiation must fail.
					lower := settings()
					lower.Window = 16384
					p, _ := EncodeSettings(lower)
					wire, _ := Encode(Frame{Kind: SettingsKind, Body: p})
					if e := c.Feed(wire); e != nil {
						t.Fatal(e)
					}
					p, _ = so.EncodeResult(so.Result{Code: so.OK, Limits: request().Limits})
					f, target = Frame{Kind: ResultKind, ID: 1, Body: p}, c
				}
			}
			p, e := Encode(f)
			if e != nil {
				t.Fatal(e)
			}
			before := target.Status().Active
			if e = target.Feed(p); e == nil || !target.Status().Closed {
				t.Fatal("invalid early OPEN phase accepted", name)
			}
			if target.Status().Active != before {
				t.Fatal("failed early carrier released accepted work")
			}
			if accepted != nil {
				accepted.Release()
			}
		})
	}
}

func TestEarlyOpenCancellationAndDenialKeepAdmission(t *testing.T) {
	for _, emitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel-unsent", true: "denied-emitted"}[emitted], func(t *testing.T) {
			limits := settings()
			limits.Streams = 1
			c, s := earlyPair(t, limits, limits)
			a, e := c.OpenFirst(request())
			if e != nil {
				t.Fatal(e)
			}
			if !emitted {
				if e = a.Reset(EndCancelled); e != nil {
					t.Fatal(e)
				}
				a.Release()
				xfer(t, c, s)
				xfer(t, s, c)
				if s.Status().EarlyOpenReceived || c.Status().Active != 0 {
					t.Fatal("cancelled provisional OPEN emitted")
				}
			} else {
				xfer(t, c, s)
				b := earlyAccept(t, s)
				if e = b.Respond(so.Result{Code: so.Denied}); e != nil {
					t.Fatal(e)
				}
				xfer(t, s, c)
				xfer(t, c, s)
				if a.Link() != nil || !a.Status().ResultKnown || a.Status().Result.Code != so.Denied {
					t.Fatal("denial became success")
				}
				a.Release()
				if _, e = c.Open(request()); !errors.Is(e, ErrBusy) {
					t.Fatal("early OPEN reused remote cleanup slot", e)
				}
				b.Release()
				if _, e = c.Open(request()); !errors.Is(e, ErrBusy) {
					t.Fatal("early OPEN reused slot before GRANT", e)
				}
				xfer(t, s, c)
			}
			if _, e = c.OpenFirst(request()); e == nil {
				t.Fatal("early request replayed")
			}
			next, e := c.Open(request())
			if e != nil || next.ID() != 2 {
				t.Fatal("early ID reused or admission leaked", e)
			}
		})
	}
}

func TestEarlyOpenIndependentWireAndFragmentation(t *testing.T) {
	// Independently encoded: mux v1 kind 8, ID 1, 24-byte UDP OPEN;
	// window 65536 and byte ceiling 8388608. No application data is included.
	wire, e := hex.DecodeString("010800000000000100000018010100000000001002000000000100000000000000800000")
	if e != nil {
		t.Fatal(e)
	}
	for split := 0; split <= len(wire); split++ {
		var d Decoder
		calls := 0
		accept := func(f Frame) error {
			calls++
			if f.Kind != EarlyOpenKind || f.ID != 1 {
				t.Fatal("early vector identity")
			}
			r, err := so.DecodeRequest(f.Body)
			if err != nil || r != request() {
				t.Fatal("early vector request", r, err)
			}
			encoded, err := Encode(f)
			if err != nil || !bytes.Equal(encoded, wire) {
				t.Fatal("early vector encoding", err)
			}
			return nil
		}
		if d.Feed(wire[:split], accept) != nil || d.Feed(wire[split:], accept) != nil || d.Finish() != nil || calls != 1 {
			t.Fatal("early vector fragmentation", split)
		}
	}
}

func TestEarlyOpenSplitLeasesAndOrderedReceipts(t *testing.T) {
	c, s := earlyPair(t, settings(), settings())
	r := request()
	r.Network = 0
	r.Address.Host = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	r.Address.Port = 443
	first, e := c.OpenFirst(r)
	if e != nil {
		t.Fatal(e)
	}
	id1, p1, _, e := c.LeaseNext(MinLease)
	if e != nil || len(p1) != Header+SettingsSize || first.Status().OpenSent {
		t.Fatal("oversize first pair did not split at a record boundary", e, len(p1))
	}
	if c.Output().AvailableBytes != MinLease {
		t.Fatal("pending early request omitted from output")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = c.Wait(ctx, time.Second); e != nil {
		t.Fatal(e)
	}
	id2, p2, _, e := c.LeaseNext(MinLease)
	if e != nil || len(p2) != MinLease || !first.Status().OpenSent {
		t.Fatal("second lease omitted early OPEN", e)
	}
	for _, chunk := range [][]byte{p1, p2} {
		for _, value := range chunk {
			if e = s.Feed([]byte{value}); e != nil {
				t.Fatal(e)
			}
		}
	}
	if earlyAccept(t, s).Request() != first.Request() {
		t.Fatal("fragmented request changed")
	}
	if e = c.AckLease(id2); e != nil || c.Status().LeaseCommitted != 0 {
		t.Fatal("out-of-order early receipt crossed prefix", e)
	}
	if e = c.AckLease(id1); e != nil || c.Status().LeaseCommitted != id2 || c.Status().Pending {
		t.Fatal("early receipt prefix did not commit", e)
	}
}

func TestEarlyOpenUnavailableInLegacySession(t *testing.T) {
	m, e := New(context.Background(), Config{Client: true, Limits: settings()})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close(context.Canceled)
	if _, e = m.OpenFirst(request()); e == nil || m.Status().Active != 0 || m.Status().Opened != 0 {
		t.Fatal("legacy session allocated provisional OPEN")
	}
	if m.Status().EarlyOpenEnabled || m.Status().EarlyOpenSent || m.Status().EarlyOpenReceived {
		t.Fatal("legacy extension status")
	}
}

func TestEarlyOpenFirstResponseCoalescingIsBounded(t *testing.T) {
	c, s := earlyPair(t, settings(), settings())
	if _, e := c.OpenFirst(request()); e != nil {
		t.Fatal(e)
	}
	xfer(t, c, s)
	b := earlyAccept(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if e := s.WaitCoalesced(ctx, time.Second, time.Millisecond); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("initial SETTINGS escaped before RESULT or wait budget", e)
	}
	// Exhausting the original queue budget does not require a RESULT. This
	// prevents a slow or failed target from deadlocking the settings exchange.
	if e := s.Wait(context.Background(), 0); e != nil {
		t.Fatal(e)
	}
	xfer(t, s, c)
	if !c.Status().Ready || b.Status().ResultKnown {
		t.Fatal("budget expiry did not allow SETTINGS-only output")
	}
	if e := b.Respond(so.Result{Code: so.Denied}); e != nil {
		t.Fatal(e)
	}
	if e := s.Wait(ctx, time.Second); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("expired context ignored")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if e := s.Wait(ctx2, time.Second); e != nil {
		t.Fatal(e)
	}
}

func FuzzEarlyOpenPrefixes(f *testing.F) {
	settingsBody, _ := EncodeSettings(settings())
	hello, _ := Encode(Frame{Kind: SettingsKind, Body: settingsBody})
	requestBody, _ := so.EncodeRequest(request())
	early, _ := Encode(Frame{Kind: EarlyOpenKind, ID: 1, Body: requestBody})
	resultBody, _ := so.EncodeResult(so.Result{Code: so.OK, Limits: request().Limits})
	result, _ := Encode(Frame{Kind: ResultKind, ID: 1, Body: resultBody})
	for _, v := range []struct {
		p      []byte
		client bool
	}{
		{append(bytes.Clone(hello), early...), false},
		{append(append(bytes.Clone(hello), early...), early...), false},
		{early, false},
		{append(bytes.Clone(hello), result...), true},
		{append(bytes.Clone(hello), early...), true},
		{nil, false},
	} {
		f.Add(v.p, uint16(1), v.client)
	}
	f.Fuzz(func(t *testing.T, wire []byte, stride uint16, client bool) {
		if len(wire) > 4096 {
			return
		}
		m, e := New(context.Background(), Config{EarlyOpen: true, Client: client, MaxPending: 4, Limits: settings()})
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			m.Close(context.Canceled)
			m.mu.Lock()
			streams := append([]*Stream(nil), m.order...)
			m.mu.Unlock()
			for _, stream := range streams {
				stream.Release()
			}
			if m.Status().Active != 0 {
				t.Fatal("fuzz early cleanup")
			}
		}()
		if client {
			if _, e = m.OpenFirst(request()); e != nil {
				t.Fatal(e)
			}
			if _, _, _, e = m.LeaseNext(65536); e != nil {
				t.Fatal(e)
			}
		}
		step := 1 + int(stride)%512
		for at := 0; at < len(wire); at += step {
			e = m.Feed(wire[at:min(at+step, len(wire))])
			st := m.Status()
			if st.Active > st.Limits.Streams || st.ReservedWindow > MaxReservedWindow || st.PendingLeases > 4 {
				t.Fatal("early admission or receipt bound")
			}
			m.mu.Lock()
			streams := append([]*Stream(nil), m.order...)
			m.mu.Unlock()
			for _, stream := range streams {
				s := stream.Status()
				if stream.Link() != nil && (!s.ResultKnown || s.Result.Code != so.OK || s.Result.Limits.Window > st.Limits.Window || s.Result.Limits.MaxBytes > st.Limits.StreamBytes) {
					t.Fatal("early link allocated before negotiated success")
				}
			}
			if e != nil {
				if !st.Closed {
					t.Fatal("early protocol failure left session live")
				}
				break
			}
		}
	})
}
