package mux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// rawSession leaves the peer under test control, including incomplete frames.
func rawSession(t *testing.T) (*Session, net.Conn, *Stream) {
	t.Helper()
	a, b := net.Pipe()
	s, e := New(a, Options{Profile: DefaultProfile(), WriteTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	b.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { s.Close(); b.Close(); s.Wait() })
	done := make(chan *Stream, 1)
	go func() {
		st, e := s.Open(context.Background(), []byte("target"))
		if e != nil {
			t.Error(e)
		}
		done <- st
	}()
	var h [headerSize]byte
	typ, id, n, e := readHeader(b, &h)
	if e != nil || typ != open {
		t.Fatal(typ, e)
	}
	if _, e = io.CopyN(io.Discard, b, int64(n)); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Write(appendFrame(nil, opened, id, nil)); e != nil {
		t.Fatal(e)
	}
	st := <-done
	if st == nil {
		t.Fatal("open failed")
	}
	t.Cleanup(func() { st.Close() })
	return s, b, st
}

func TestPartialDataAndTruncatedFrame(t *testing.T) {
	s, peer, st := rawSession(t)
	payload := appendFrame(nil, data, st.id, make([]byte, blockSize))
	copy(payload[headerSize:], "part")
	if _, e := peer.Write(payload[:headerSize+4]); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		p := make([]byte, 4)
		_, e := io.ReadFull(st, p)
		if e == nil && string(p) != "part" {
			e = ErrProtocol
		}
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("partial frame withheld until full frame")
	}
	peer.Close()
	var p [1]byte
	if _, e := st.Read(p[:]); !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatal("truncation became graceful EOF:", e)
	}
	s.Wait()
}

func TestCancelledPartialDataReleasesReader(t *testing.T) {
	s, peer, st := rawSession(t)
	payload := appendFrame(nil, data, st.id, make([]byte, blockSize))
	if _, e := peer.Write(payload[:headerSize+4]); e != nil {
		t.Fatal(e)
	}
	if e := st.Close(); e != nil {
		t.Fatal(e)
	}
	// Consume the reset while the decoder finishes discarding the incoming frame.
	resetDone := make(chan error, 1)
	go func() {
		var h [headerSize]byte
		typ, _, _, e := readHeader(peer, &h)
		if e == nil && typ != reset {
			e = ErrProtocol
		}
		resetDone <- e
	}()
	if _, e := peer.Write(payload[headerSize+4:]); e != nil {
		t.Fatal(e)
	}
	if e := <-resetDone; e != nil {
		t.Fatal(e)
	}
	if s.Err() != nil {
		t.Fatal("stream cancellation killed session:", s.Err())
	}
	s.mu.Lock()
	active := len(s.streams)
	s.mu.Unlock()
	if active != 0 {
		t.Fatal("cancelled slot retained")
	}
}

func TestMalformedFramesAndCredit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		typ       byte
		id        uint32
		payload   []byte
		oversized bool
	}{
		{"unknown stream", data, 3, []byte{1}, false},
		{"even identifier", data, 2, []byte{1}, false},
		{"credit without debt", credit, 1, []byte{0, 1}, false},
		{"zero credit", credit, 1, []byte{0, 0}, false},
		{"premature done", finished, 1, nil, false},
		{"duplicate opened", opened, 1, nil, false},
		{"oversized data", data, 1, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, peer, _ := rawSession(t)
			p := appendFrame(nil, tc.typ, tc.id, tc.payload)
			if tc.oversized {
				p[5] = 1
			}
			// Invalid headers may be rejected without reading their body.
			go peer.Write(p)
			select {
			case <-s.done:
			case <-time.After(time.Second):
				t.Fatal("malformed frame accepted")
			}
			if !errors.Is(s.Err(), ErrProtocol) {
				t.Fatal(s.Err())
			}
		})
	}
}

func TestReceiveWindowCannotBeExceeded(t *testing.T) {
	s, peer, st := rawSession(t)
	for range windowBlocks {
		if _, e := peer.Write(appendFrame(nil, data, st.id, []byte{42})); e != nil {
			t.Fatal(e)
		}
	}
	s.mu.Lock()
	queued := len(st.queue) - st.head
	creditLeft := st.recvCredit
	s.mu.Unlock()
	if queued != windowBlocks || creditLeft != 0 {
		t.Fatal(queued, creditLeft)
	}
	go peer.Write(appendFrame(nil, data, st.id, []byte{42}))
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("receive queue exceeded budget")
	}
	if !errors.Is(s.Err(), ErrProtocol) {
		t.Fatal(s.Err())
	}
}

func TestPartialHeaderActivity(t *testing.T) {
	s, peer, st := rawSession(t)
	previous := st.LastActivity()
	time.Sleep(time.Millisecond)
	h := appendFrame(nil, fin, st.id, nil)
	if _, e := peer.Write(h[:3]); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for !st.LastActivity().After(previous) {
		if time.Now().After(deadline) {
			t.Fatal("partial header activity lost")
		}
		time.Sleep(time.Millisecond)
	}
	if _, e := peer.Write(h[3:]); e != nil {
		t.Fatal(e)
	}
	var p [1]byte
	if _, e := st.Read(p[:]); e != io.EOF {
		t.Fatal(e)
	}
	s.Close()
}

type prefixConn struct {
	net.Conn
	first chan []byte
}

func (c prefixConn) Write(p []byte) (int, error) {
	select {
	case c.first <- bytes.Clone(p):
	default:
	}
	return c.Conn.Write(p)
}
func TestAuthenticationAndFirstOpenShareWrite(t *testing.T) {
	a, b := net.Pipe()
	prefix := bytes.Repeat([]byte{0xa7}, 53)
	first := make(chan []byte, 1)
	c, e := New(prefixConn{a, first}, Options{Profile: DefaultProfile(), Prefix: prefix, WriteTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(); b.Close(); c.Wait() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, e := c.Open(ctx, []byte("target")); result <- e }()
	select {
	case p := <-first:
		if !bytes.Equal(p[:len(prefix)], prefix) {
			t.Fatal("prefix lost")
		}
		var h [headerSize]byte
		typ, id, n, e := readHeader(bytes.NewReader(p[len(prefix):]), &h)
		if e != nil || typ != open || id != 1 || n != 6 || len(p) != len(prefix)+headerSize+6 {
			t.Fatal("auth and open split", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no first write")
	}
	cancel()
	if e := <-result; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func FuzzFrameHeader(f *testing.F) {
	f.Add(appendFrame(nil, data, 1, []byte{42}))
	f.Fuzz(func(t *testing.T, p []byte) {
		var h [headerSize]byte
		_, id, n, e := readHeader(bytes.NewReader(p), &h)
		if e == nil && (id == 0 || id&1 == 0 || n > blockSize) {
			t.Fatal("invalid header passed")
		}
	})
}

func TestCreditGrantDoesNotWrap(t *testing.T) {
	s, peer, st := rawSession(t)
	var creditPayload [2]byte
	binary.BigEndian.PutUint16(creditPayload[:], 65535)
	go peer.Write(appendFrame(nil, credit, st.id, creditPayload[:]))
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("overflow accepted")
	}
	if !errors.Is(s.Err(), ErrProtocol) {
		t.Fatal(s.Err())
	}
}

func TestFullRelayBufferFitsOneBatch(t *testing.T) {
	a, b := net.Pipe()
	writes := make(chan []byte, 8)
	p := DefaultProfile()
	p.StartupWrites = Range{0, 0}
	c, e := New(prefixConn{a, writes}, Options{Profile: p, WriteTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(b, Options{Server: true, Profile: p, WriteTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close(); s.Close(); c.Wait(); s.Wait() })
	st, _ := openPair(t, c, s)
	<-writes // OPEN
	payload := bytes.Repeat([]byte{77}, PayloadSize)
	if _, e := st.Write(payload); e != nil {
		t.Fatal(e)
	}
	got := <-writes
	if len(got) != batchSize {
		t.Fatalf("full relay read left a tail: batch=%d", len(got))
	}
	select {
	case extra := <-writes:
		t.Fatalf("extra small write: %d", len(extra))
	default:
	}
	reader := bytes.NewReader(got)
	var received []byte
	for reader.Len() > 0 {
		var h [headerSize]byte
		typ, id, n, e := readHeader(reader, &h)
		if e != nil || typ != data || id != st.id {
			t.Fatal("bad data batch", e)
		}
		p := make([]byte, n)
		if _, e := io.ReadFull(reader, p); e != nil {
			t.Fatal(e)
		}
		received = append(received, p...)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("batch corrupted bytes")
	}
}
