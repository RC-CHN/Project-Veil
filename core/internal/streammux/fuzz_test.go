package streammux

import (
	"context"
	"testing"

	so "veil.local/core/internal/streamopen"
)

// This exercises valid API actions with crossed directional leases/receipts.
// Neither input direction is duplicated or reordered. Such schedules must not
// turn cancellation, ownership handoff or draining into a carrier violation.
func FuzzMuxScheduling(f *testing.F) {
	f.Add([]byte{0, 2, 3, 4, 1, 5, 6, 7, 2, 3, 4, 9, 137, 5, 6, 7})
	f.Add([]byte{0, 2, 3, 4, 129, 5, 6, 7, 11, 2, 8, 5, 6, 7, 3, 4, 2, 3, 4, 9, 137})
	f.Add([]byte{0, 8, 9, 0, 2, 138, 5, 6, 7, 3, 4, 1, 5, 6, 7, 2, 3, 4, 9, 137})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 512 {
			return
		}
		cfg := settings()
		cfg.Streams = 4
		cfg.Opened = 32
		c, s := pairWithLimits(t, cfg)
		var clients, servers []*Stream
		type packet struct {
			body           []byte
			eof, delivered bool
		}
		var up, down *packet
		pick := func(list []*Stream, b byte) *Stream {
			if len(list) == 0 {
				return nil
			}
			return list[int(b>>4)%len(list)]
		}
		lease := func(m *Session, p **packet) {
			if *p != nil {
				return
			}
			body, eof, e := m.Lease(32768)
			if e != nil {
				t.Fatal("scheduled lease", e)
			}
			*p = &packet{body: body, eof: eof}
		}
		deliver := func(m *Session, p *packet, b byte) {
			if p == nil || p.delivered {
				return
			}
			stride := 1 + int(b>>4)
			for at := 0; at < len(p.body); at += stride {
				if e := m.Feed(p.body[at:min(at+stride, len(p.body))]); e != nil {
					t.Fatal("scheduled delivery", e)
				}
			}
			if p.eof {
				if e := m.PeerDone(); e != nil {
					t.Fatal("scheduled EOF", e)
				}
			}
			p.delivered = true
		}
		commit := func(m *Session, p **packet) {
			if *p == nil || !(*p).delivered {
				return
			}
			if e := m.Commit(); e != nil {
				t.Fatal("scheduled receipt", e)
			}
			*p = nil
		}
		for _, b := range ops {
			switch b & 15 {
			case 0:
				if v, e := c.Open(request()); e == nil {
					clients = append(clients, v)
				}
			case 1:
				if len(s.accept) > 0 {
					v, e := s.Accept(context.Background())
					if e != nil {
						t.Fatal(e)
					}
					servers = append(servers, v)
					r := so.Result{Code: so.Busy}
					if b&128 != 0 {
						r = so.Result{Code: so.OK, Limits: v.Request().Limits}
					}
					_ = v.Respond(r)
				}
			case 2:
				lease(c, &up)
			case 3:
				deliver(s, up, b)
			case 4:
				commit(c, &up)
			case 5:
				lease(s, &down)
			case 6:
				deliver(c, down, b)
			case 7:
				commit(s, &down)
			case 8:
				if v := pick(clients, b); v != nil {
					_ = v.Reset(EndCancelled)
				}
			case 9:
				list := clients
				if b&128 != 0 {
					list = servers
				}
				if v := pick(list, b); v != nil {
					v.Release()
				}
			case 10:
				if b&128 != 0 {
					s.Drain()
				} else {
					c.Drain()
				}
			case 11:
				if v := pick(clients, b); v != nil {
					if l := v.Link(); l != nil && !l.Status().Closed {
						_, _ = l.Write(v.Context(), []byte{b})
						if b&128 != 0 {
							_ = l.Finish()
						}
					}
				}
			}
			for _, m := range []*Session{c, s} {
				st := m.Status()
				if st.Active > 4 || st.MaximumActive > 4 || st.ReservedWindow > MaxReservedWindow || st.Closed {
					t.Fatal("scheduled resource invariant", st)
				}
			}
		}
		c.Close(nil)
		s.Close(nil)
		for _, v := range append(clients, servers...) {
			v.Release()
		}
		if c.Status().Active != 0 || s.Status().Active != 0 {
			t.Fatal("scheduled cleanup did not finish", c.Status(), s.Status())
		}
	})
}
