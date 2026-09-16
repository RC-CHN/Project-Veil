package streammux

import "testing"

func TestCleanDrainCloseKeepsErrorEmpty(t *testing.T) {
	c, s := pair(t, 1)
	a, b := rejectedPair(t, c, s)
	a.Release()
	b.Release()
	xfer(t, s, c)
	c.Drain()
	s.Drain()
	xfer(t, c, s)
	xfer(t, s, c)
	xfer(t, c, s)
	xfer(t, s, c)
	for _, m := range []*Session{c, s} {
		if !m.Status().SourceEOF || !m.Status().PeerEOF {
			t.Fatal("not fully drained")
		}
		m.Close(nil)
		st := m.Status()
		if !st.Closed || st.Error != "" || st.Active != 0 {
			t.Fatal("clean close changed terminal outcome", st)
		}
	}
}
