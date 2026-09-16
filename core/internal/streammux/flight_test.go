package streammux

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	so "veil.local/core/internal/streamopen"
)

func flightPair(t *testing.T) (*Session, *Session) {
	t.Helper()
	limits := settings()
	limits.Streams = 1
	c, e := New(context.Background(), Config{Client: true, Limits: limits, MaxPending: 4})
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(context.Background(), Config{Limits: limits, MaxPending: 4})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		for _, m := range []*Session{c, s} {
			m.Close(nil)
			m.mu.Lock()
			streams := append([]*Stream(nil), m.order...)
			m.mu.Unlock()
			for _, stream := range streams {
				stream.Release()
			}
		}
	})
	xfer(t, c, s)
	xfer(t, s, c)
	return c, s
}

func TestMuxEndReceiptCannotSkipEarlierData(t *testing.T) {
	for _, serverSends := range []bool{false, true} {
		name := "client"
		if serverSends {
			name = "server"
		}
		t.Run(name, func(t *testing.T) {
			c, s := flightPair(t)
			a, b := openPair(t, c, s, so.OK)
			sender, receiver, local, remote := c, s, a, b
			if serverSends {
				sender, receiver, local, remote = s, c, b, a
			}
			local.Link().Write(context.Background(), []byte{1})
			dataID, data, _, e := sender.LeaseNext(65536)
			if e != nil || receiver.Feed(data) != nil {
				t.Fatal("data exchange", e)
			}
			if e := local.Reset(EndCancelled); e != nil {
				t.Fatal(e)
			}
			endID, end, _, e := sender.LeaseNext(65536)
			if e != nil || receiver.Feed(end) != nil {
				t.Fatal("END exchange", e)
			}
			peerID, peerEnd, _, e := receiver.LeaseNext(65536)
			if e != nil || sender.Feed(peerEnd) != nil || receiver.AckLease(peerID) != nil {
				t.Fatal("peer END", e)
			}
			local.Release()
			remote.Release()
			if e := sender.AckLease(endID); e != nil {
				t.Fatal(e)
			}
			if local.Status().EndAcked || !local.Status().PeerEnded || sender.Status().Active != 1 || sender.Status().PendingLeases != 2 {
				t.Fatal("END acknowledgement crossed unacknowledged DATA", local.Status(), sender.Status())
			}
			if serverSends && sender.Status().Retired != 0 {
				t.Fatal("GRANT eligible before receipt prefix")
			}
			if e := sender.AckLease(dataID); e != nil {
				t.Fatal(e)
			}
			if !local.Status().EndAcked || sender.Status().Active != 0 || sender.Status().PendingLeases != 0 {
				t.Fatal("prefix did not retire released stream")
			}
			if serverSends && sender.Status().Retired != 1 {
				t.Fatal("completed stream did not grant capacity")
			}
		})
	}
}

func TestMuxWindowFullAndNormalCloseReceiptBarrier(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		c, _ := flightPair(t)
		var ids []uint64
		for i := 0; i < 4; i++ {
			id, _, _, e := c.LeaseNext(65536)
			if e != nil {
				t.Fatal(e)
			}
			ids = append(ids, id)
		}
		before := c.Status()
		if _, _, _, e := c.LeaseNext(65536); !errors.Is(e, ErrLeaseBusy) || c.Status() != before {
			t.Fatal("mux full window mutated", e)
		}
		for i := 3; i >= 0; i-- {
			if e := c.AckLease(ids[i]); e != nil {
				t.Fatal(e)
			}
		}
		if c.Status().PendingLeases != 0 {
			t.Fatal("mux receipts did not drain")
		}
	})
	for _, acknowledged := range []bool{false, true} {
		name := "pending"
		if acknowledged {
			name = "acknowledged"
		}
		t.Run(name, func(t *testing.T) {
			c, s := flightPair(t)
			c.Drain()
			xfer(t, c, s)
			xfer(t, s, c)
			if c.PeerDone() != nil || s.PeerDone() != nil {
				t.Fatal("drain completion")
			}
			id, p, eof, e := c.LeaseNext(65536)
			if e != nil || !eof || len(p) != 0 {
				t.Fatal("final empty lease", e)
			}
			if acknowledged {
				if e := c.AckLease(id); e != nil {
					t.Fatal(e)
				}
			}
			c.Close(nil)
			if (c.Status().Error == "") != acknowledged {
				t.Fatal("normal close ignored receipt barrier", c.Status())
			}
		})
	}
}

// HTTP completions may arrive on different goroutines. Even when every receipt
// races with every other receipt, the owner must commit each prefix exactly once.
func TestMuxConcurrentReceipts(t *testing.T) {
	c, s := flightPair(t)
	a, _ := openPair(t, c, s, so.OK)
	if _, e := a.Link().Write(context.Background(), make([]byte, 1536)); e != nil {
		t.Fatal(e)
	}
	var ids [4]uint64
	for i := range ids {
		id, p, _, e := c.LeaseNext(512)
		if e != nil {
			t.Fatal(e)
		}
		if e := s.Feed(p); e != nil {
			t.Fatal(e)
		}
		ids[i] = id
	}
	start := make(chan struct{})
	results := make(chan error, len(ids))
	var workers sync.WaitGroup
	for _, id := range ids {
		workers.Add(1)
		go func(id uint64) {
			defer workers.Done()
			<-start
			results <- c.AckLease(id)
		}(id)
	}
	close(start)
	workers.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	st := c.Status()
	if st.Closed || st.PendingLeases != 0 || st.LeaseCommitted != ids[3] || c.Output().PendingBytes != 0 {
		t.Fatal("concurrent receipts lost or repeated a commit", st)
	}
	if link := a.Link().Status(); link.Acked != 0 || link.PendingLeases != 0 {
		t.Fatal("receipt changed consumer credit or retained child lease", link)
	}
}

func FuzzMuxFlightReceipts(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 1, 5, 9, 13, 2, 3})
	f.Add([]byte{0, 2, 0, 2, 7, 5, 3, 1, 4, 6})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 256 {
			ops = ops[:256]
		}
		c, s := flightPair(t)
		a, b := openPair(t, c, s, so.OK)
		workers := make(chan error, 2)
		for _, stream := range []*Stream{a, b} {
			stream.Link().Write(context.Background(), make([]byte, 256))
			stream.Link().Finish()
			go func(stream *Stream) {
				workers <- stream.Link().Consume(stream.Context(), io.Discard, func() error { return nil })
			}(stream)
		}
		t.Cleanup(func() { c.Close(nil); s.Close(nil); <-workers; <-workers })
		c.Drain()
		s.Drain()
		type receipt struct {
			id  uint64
			ack bool
		}
		var pending [2][]receipt
		for _, op := range ops {
			side := int(op & 1)
			from, to := c, s
			if side == 1 {
				from, to = s, c
			}
			if op&2 == 0 {
				id, p, eof, e := from.LeaseNext(512 + int(op)*64)
				if errors.Is(e, ErrLeaseBusy) {
					if len(pending[side]) != 4 {
						t.Fatal("unexpected busy")
					}
				} else {
					if e != nil || to.Feed(p) != nil {
						t.Fatal("numbered ordered feed", e)
					}
					if eof && to.PeerDone() != nil {
						t.Fatal("numbered EOF")
					}
					pending[side] = append(pending[side], receipt{id: id})
				}
			} else if len(pending[side]) > 0 {
				at := int(op>>2) % len(pending[side])
				if !pending[side][at].ack {
					if e := from.AckLease(pending[side][at].id); e != nil {
						t.Fatal(e)
					}
					pending[side][at].ack = true
					for len(pending[side]) > 0 && pending[side][0].ack {
						pending[side] = pending[side][1:]
					}
				}
			}
			for _, stream := range []*Stream{a, b} {
				select {
				case <-stream.Done():
					stream.Release()
				default:
				}
			}
			for i, m := range []*Session{c, s} {
				st := m.Status()
				if st.Closed || st.PendingLeases != len(pending[i]) || m.Output().PendingBytes > 4*(256<<10) {
					t.Fatal("flight bound")
				}
			}
		}
	})
}
