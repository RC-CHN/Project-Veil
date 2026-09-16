package streamlink

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"
)

func TestFramePublicEncoding(t *testing.T) {
	got := hex.EncodeToString(frame(Data, 0, 7, []byte("abc")))
	if got != "01000100000000030000000000000007616263" {
		t.Fatal(got)
	}
	if hex.EncodeToString(frame(Credit, 1, 0, nil)) != "02010100000000000000000000000000" {
		t.Fatal("credit encoding")
	}
}

func TestPairedStreamCreditAndEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, _ := New(ctx, Config{4096, 1 << 20})
	b, _ := New(ctx, Config{4096, 1 << 20})
	defer a.Close(nil)
	defer b.Close(nil)
	up, down := bytes.Repeat([]byte("up!"), 10000), bytes.Repeat([]byte("down!"), 9000)
	var receivedUp, receivedDown bytes.Buffer
	consumer := make(chan error, 2)
	go func() { consumer <- b.Consume(ctx, &receivedUp, func() error { return nil }) }()
	go func() { consumer <- a.Consume(ctx, &receivedDown, func() error { return nil }) }()
	producer := make(chan error, 2)
	go func() {
		_, e := a.Write(ctx, up)
		if e == nil {
			e = a.Finish()
		}
		producer <- e
	}()
	go func() {
		_, e := b.Write(ctx, down)
		if e == nil {
			e = b.Finish()
		}
		producer <- e
	}()
	for !(a.Status().OuterAckedEOF && b.Status().OuterAckedEOF) {
		for _, pair := range [][2]*Link{{a, b}, {b, a}} {
			src, dst := pair[0], pair[1]
			if e := src.Wait(ctx, time.Millisecond); e != nil {
				t.Fatal(e)
			}
			p, eof, e := src.Lease(127)
			if e != nil {
				t.Fatal(e)
			}
			for len(p) > 0 {
				n := min(3, len(p))
				if e = dst.Feed(p[:n]); e != nil {
					t.Fatal(e)
				}
				p = p[n:]
			}
			if eof {
				if e = dst.PeerDone(); e != nil {
					t.Fatal(e)
				}
			}
			if e = src.Commit(); e != nil {
				t.Fatal(e)
			}
		}
	}
	for i := 0; i < 2; i++ {
		if e := <-producer; e != nil {
			t.Fatal(e)
		}
		if e := <-consumer; e != nil {
			t.Fatal(e)
		}
	}
	if !bytes.Equal(receivedUp.Bytes(), up) || !bytes.Equal(receivedDown.Bytes(), down) {
		t.Fatal("ordered streams differ")
	}
	for _, l := range []*Link{a, b} {
		st := l.Status()
		if st.Sent != st.Acked || st.Received != st.Consumed || !st.FinAcked || !st.RemoteDelivered || st.MaxSendOutstanding > 4096 || st.MaxReceiveOutstanding > 4096 {
			t.Fatal(st)
		}
	}
}

func TestOuterReceiptDoesNotReleaseConsumerCredit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, _ := New(ctx, Config{32, 1024})
	defer l.Close(nil)
	if _, e := l.Write(ctx, make([]byte, 32)); e != nil {
		t.Fatal(e)
	}
	p, _, e := l.Lease(128)
	if e != nil || len(p) != 48 {
		t.Fatal(e)
	}
	if e = l.Commit(); e != nil {
		t.Fatal(e)
	}
	short, stop := context.WithTimeout(ctx, 10*time.Millisecond)
	defer stop()
	if n, e := l.Write(short, []byte{1}); n != 0 || e == nil {
		t.Fatal("outer ack released inner window", n, e)
	}
	if e = l.Feed(frame(Credit, 0, 32, nil)); e != nil {
		t.Fatal(e)
	}
	if n, e := l.Write(ctx, []byte{1}); n != 1 || e != nil {
		t.Fatal(n, e)
	}
}

func TestMalformedAndUnadvertisedFrames(t *testing.T) {
	for _, name := range []string{"version", "reserved", "zero_data", "offset", "over_credit", "credit_unsent", "fin_boundary", "duplicate_fin", "after_fin", "partial_eof"} {
		t.Run(name, func(t *testing.T) {
			l, _ := New(context.Background(), Config{32, 1024})
			defer l.Close(nil)
			p := frame(Data, 0, 0, []byte{1})
			switch name {
			case "version":
				p[2] = 9
			case "reserved":
				p[3] = 1
			case "zero_data":
				p = frame(Data, 0, 0, nil)
			case "offset":
				p = frame(Data, 0, 1, []byte{1})
			case "over_credit":
				p = frame(Data, 0, 0, make([]byte, 33))
			case "credit_unsent":
				p = frame(Credit, 0, 1, nil)
			case "fin_boundary":
				p = frame(Fin, 0, 1, nil)
			case "duplicate_fin", "after_fin":
				if e := l.Feed(frame(Fin, 0, 0, nil)); e != nil {
					t.Fatal(e)
				}
				if name == "duplicate_fin" {
					p = frame(Fin, 0, 0, nil)
				}
			case "partial_eof":
				if e := l.Feed(p[:5]); e != nil {
					t.Fatal(e)
				}
				if e := l.PeerDone(); e == nil {
					t.Fatal("partial EOF accepted")
				}
				return
			}
			if e := l.Feed(p); e == nil || !l.Status().Closed {
				t.Fatal("invalid frame accepted", name, e)
			}
		})
	}
	// Even after a sink consumes bytes, peer cannot use credit not yet issued.
	l, _ := New(context.Background(), Config{4, 1024})
	defer l.Close(nil)
	if e := l.Feed(frame(Data, 0, 0, []byte("abcd"))); e != nil {
		t.Fatal(e)
	}
	l.mu.Lock()
	l.rx.drop(4)
	l.consumed = 4
	l.mu.Unlock()
	if e := l.Feed(frame(Data, 0, 4, []byte("x"))); e == nil {
		t.Fatal("unadvertised credit accepted")
	}
}

type blockedWriter struct {
	ctx     context.Context
	entered chan struct{}
}

func (w blockedWriter) Write([]byte) (int, error) {
	close(w.entered)
	<-w.ctx.Done()
	return 0, w.ctx.Err()
}
func TestCancellationAndByteLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l, _ := New(ctx, Config{32, 32})
	if e := l.Feed(frame(Data, 0, 0, []byte("blocked"))); e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- l.Consume(ctx, blockedWriter{ctx, entered}, func() error { return nil }) }()
	<-entered
	cancel()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("cancel hidden")
		}
	case <-time.After(time.Second):
		t.Fatal("consumer stranded")
	}
	l.Close(context.Canceled)
	st := l.Status()
	if !st.Closed || st.Queued != 0 || st.ReceiveQueued != 0 || st.Pending {
		t.Fatal(st)
	}
	l, _ = New(context.Background(), Config{32, 3})
	defer l.Close(nil)
	if n, e := l.Write(context.Background(), []byte("four")); n != 3 || e == nil || !l.Status().Closed {
		t.Fatal(n, e)
	}
}

type partialSink struct {
	received bytes.Buffer
	fail     bool
}

func (w *partialSink) Write(p []byte) (int, error) {
	n := min(3, len(p))
	w.received.Write(p[:n])
	if w.fail {
		return n, context.Canceled
	}
	return n, nil
}
func TestPartialConsumerCreditAndFIN(t *testing.T) {
	for _, fail := range []bool{false, true} {
		l, _ := New(context.Background(), Config{32, 1024})
		sink := &partialSink{fail: fail}
		fin := 0
		if e := l.Feed(append(frame(Data, 0, 0, []byte("abcdefg")), frame(Fin, 0, 7, nil)...)); e != nil {
			t.Fatal(e)
		}
		e := l.Consume(context.Background(), sink, func() error { fin++; return nil })
		s := l.Status()
		if fail {
			if e == nil || s.Consumed != 3 || !s.Closed || fin != 0 {
				t.Fatal(e, s, fin)
			}
		} else {
			if e != nil || s.Consumed != 7 || sink.received.String() != "abcdefg" || fin != 1 || !s.RemoteDelivered {
				t.Fatal(e, s, fin)
			}
			p, _, e := l.Lease(128)
			if e != nil || !bytes.Equal(p, frame(Credit, 1, 7, nil)) {
				t.Fatal(e, p)
			}
			if e = l.Consume(context.Background(), sink, func() error { return nil }); e == nil {
				t.Fatal("second consumer accepted")
			}
		}
		l.Close(nil)
	}
}
