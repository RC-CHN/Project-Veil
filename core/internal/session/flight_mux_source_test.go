package session

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fw "veil.local/core/internal/flightwindow"
	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

func flightSources(t *testing.T) (*flightMuxSource, *flightMuxSource) {
	t.Helper()
	limits := sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}
	a, e := newFlightMuxSource(context.Background(), sm.Config{Client: true, Limits: limits, MaxPending: 4})
	if e != nil {
		t.Fatal(e)
	}
	b, e := newFlightMuxSource(context.Background(), sm.Config{Limits: limits, MaxPending: 4})
	if e != nil {
		a.finish(e)
		t.Fatal(e)
	}
	t.Cleanup(func() { a.finish(errors.New("test cleanup")); b.finish(errors.New("test cleanup")) })
	return a, b
}

func flightTransfer(t *testing.T, from, to *flightMuxSource) {
	t.Helper()
	frame, e := from.lease(65536)
	if e != nil {
		t.Fatal(e)
	}
	if delivered, e := to.receive(frame); e != nil || delivered != frame.ID {
		t.Fatal("ordered source transfer", delivered, frame.ID, e)
	}
	if e := from.ack(frame.ID); e != nil {
		t.Fatal(e)
	}
}

func TestFlightSourceConcurrentCompletionsAndReceipts(t *testing.T) {
	a, b := flightSources(t)
	var frames [4]fw.Frame
	for i := range frames {
		var e error
		frames[i], e = a.lease(512)
		if e != nil {
			t.Fatal(e)
		}
	}
	before := a.status()
	if _, e := a.lease(512); !errors.Is(e, sm.ErrLeaseBusy) || a.status() != before {
		t.Fatal("full source window changed state", e)
	}
	start := make(chan struct{})
	results := make(chan error, 3)
	var workers sync.WaitGroup
	for _, frame := range frames[1:] {
		workers.Add(1)
		go func(frame fw.Frame) {
			defer workers.Done()
			<-start
			prefix, e := b.receive(frame)
			if e == nil && prefix != 0 {
				e = errors.New("completion crossed initial gap")
			}
			results <- e
		}(frame)
	}
	close(start)
	workers.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	if b.status().Receive.Pending != 3 || b.status().Delivered != 0 || b.status().Mux.ReceivedBytes != 0 {
		t.Fatal("received completions advanced delivery")
	}
	if prefix, e := b.receive(frames[0]); e != nil || prefix != 4 {
		t.Fatal("prefix did not release", prefix, e)
	}
	// HTTP receipt processing can itself complete out of order after delivery.
	for _, frame := range frames[1:] {
		workers.Add(1)
		go func(id uint64) {
			defer workers.Done()
			if e := a.ack(id); e != nil {
				t.Error(e)
			}
		}(frame.ID)
	}
	workers.Wait()
	if a.status().Mux.PendingLeases != 4 || a.status().Mux.LeaseCommitted != 0 {
		t.Fatal("receipt crossed initial gap")
	}
	if e := a.ack(1); e != nil {
		t.Fatal(e)
	}
	flightTransfer(t, b, a)
	aup, adown := a.hashes()
	bdown, bup := b.hashes()
	if aup != bup || adown != bdown || !a.status().Mux.Ready || !b.status().Mux.Ready {
		t.Fatal("source reconciliation")
	}
}

func TestFlightSourceFeedFailureDropsLaterCompletions(t *testing.T) {
	a, b := flightSources(t)
	if _, e := b.receive(fw.Frame{ID: 2, Body: []byte{1}}); e != nil {
		t.Fatal(e)
	}
	_, before := b.hashes()
	bad := make([]byte, sm.Header)
	bad[0] = 255
	if prefix, e := b.receive(fw.Frame{ID: 1, Body: bad}); e == nil || prefix != 0 {
		t.Fatal("invalid mux frame acknowledged", prefix, e)
	}
	st := b.status()
	_, after := b.hashes()
	if !st.Closed || !st.Mux.Closed || st.Error == "" || st.Receive.Pending != 0 || st.Receive.Bytes != 0 || st.Delivered != 0 || before != after {
		t.Fatal("failed feed retained or committed later completion", st)
	}
	if e := a.ack(1); e == nil || !a.status().Closed {
		t.Fatal("unknown receipt accepted")
	}
}

func TestFlightSourceStreamDataAndConsumerCredit(t *testing.T) {
	a, b := flightSources(t)
	flightTransfer(t, a, b)
	flightTransfer(t, b, a)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	limits := so.Limits{Window: 16384, MaxBytes: 1 << 20}
	left, e := a.mux.Open(so.Request{Network: so.NetworkUDP, Limits: limits})
	if e != nil {
		t.Fatal(e)
	}
	defer left.Release()
	flightTransfer(t, a, b)
	right, e := b.mux.Accept(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer right.Release()
	if e := right.Respond(so.Result{Code: so.OK, Limits: limits}); e != nil {
		t.Fatal(e)
	}
	flightTransfer(t, b, a)
	payload := make([]byte, 1536)
	for i := range payload {
		payload[i] = byte(i*131 + 17)
	}
	if _, e := left.Link().Write(ctx, payload); e != nil {
		t.Fatal(e)
	}
	left.Link().Finish()
	var frames [4]fw.Frame
	for i := range frames {
		frames[i], e = a.lease(512)
		if e != nil {
			t.Fatal(e)
		}
	}
	before := b.status().Delivered
	for i := len(frames) - 1; i >= 0; i-- {
		prefix, e := b.receive(frames[i])
		want := before
		if i == 0 {
			want = frames[3].ID
		}
		if e != nil || prefix != want {
			t.Fatal("source delivery watermark", prefix, want, e)
		}
	}
	for i := len(frames) - 1; i >= 0; i-- {
		if e := a.ack(frames[i].ID); e != nil {
			t.Fatal(e)
		}
	}
	if left.Link().Status().Acked != 0 || a.status().Mux.Pending {
		t.Fatal("source receipt changed consumer credit")
	}
	var output bytes.Buffer
	if e := right.Link().Consume(ctx, &output, func() error { return nil }); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(output.Bytes(), payload) {
		t.Fatal("source reordered byte stream")
	}
	flightTransfer(t, b, a)
	if left.Link().Status().Acked != uint64(len(payload)) {
		t.Fatal("independent consumer credit was not delivered")
	}
	up, down := a.hashes()
	peerDown, peerUp := b.hashes()
	if up != peerUp || down != peerDown {
		t.Fatal("source stream hash mismatch")
	}
}

func TestFlightSourceCloseRequiresDeliveredAndAckedEOF(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		name := "pending"
		if acknowledged {
			name = "acknowledged"
		}
		t.Run(name, func(t *testing.T) {
			a, b := flightSources(t)
			flightTransfer(t, a, b)
			flightTransfer(t, b, a)
			a.mux.Drain()
			b.mux.Drain()
			for i := 0; i < 8 && !(a.status().AckedEOF && b.status().AckedEOF); i++ {
				flightTransfer(t, a, b)
				flightTransfer(t, b, a)
			}
			if !a.status().AckedEOF || !b.status().AckedEOF || !a.status().PeerEOF || !b.status().PeerEOF {
				t.Fatal("EOF exchange failed")
			}
			last, e := a.lease(512)
			if e != nil || !last.EOF || len(last.Body) != 0 {
				t.Fatal("last lease", e)
			}
			if _, e := b.receive(last); e != nil {
				t.Fatal(e)
			}
			if acknowledged {
				if e := a.ack(last.ID); e != nil {
					t.Fatal(e)
				}
			}
			a.finish(nil)
			if st := a.status(); !st.Closed || (st.Error == "") != acknowledged || st.Receive.Bytes != 0 || st.Mux.Pending {
				t.Fatal("normal close receipt barrier", st)
			}
			b.finish(nil)
			if st := b.status(); st.Error != "" || !st.Closed {
				t.Fatal("peer normal close", st)
			}
		})
	}
}

func TestFlightSourceParentCancellationClearsReceiveBodies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limits := sm.Settings{Streams: 1, Window: 16384, StreamBytes: 1 << 20, ConnectionBytes: 4 << 20, Opened: 8}
	s, e := newFlightMuxSource(ctx, sm.Config{Limits: limits, MaxPending: 4})
	if e != nil {
		t.Fatal(e)
	}
	defer s.finish(errors.New("test cleanup"))
	if _, e := s.receive(fw.Frame{ID: 2, Body: make([]byte, fw.MaxBody)}); e != nil {
		t.Fatal(e)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		// Inspect fields directly: status() must not be the operation that
		// performs cleanup in this cancellation callback test.
		s.mu.Lock()
		closed, held := s.closed, s.receiver.Status().Bytes
		s.mu.Unlock()
		if closed && held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("parent cancellation retained body")
		}
		time.Sleep(time.Millisecond)
	}
	if !s.mux.Status().Closed {
		t.Fatal("parent cancellation retained mux")
	}
}
