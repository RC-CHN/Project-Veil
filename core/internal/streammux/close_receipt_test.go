package streammux

import "testing"

func TestNormalCloseRequiresNoPendingLegacyLease(t *testing.T) {
	c, s := pair(t, 1)
	c.Drain()
	xfer(t, c, s)
	xfer(t, s, c)
	if c.PeerDone() != nil || s.PeerDone() != nil {
		t.Fatal("drain completion")
	}
	p, eof, e := c.Lease(65536)
	if e != nil || len(p) != 0 || !eof {
		t.Fatal("final empty legacy lease", e)
	}
	c.Close(nil)
	if c.Status().Error == "" {
		t.Fatal("normal close accepted an unreceipted lease")
	}
}
