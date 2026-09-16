package streammux

import "testing"

func TestResetReasonSurvivesCleanupBeforeWireLease(t *testing.T) {
	c, s := pair(t, 1)
	a, b := openPair(t, c, s, 0)
	defer a.Release()
	defer b.Release()
	if err := a.Reset(EndBudget); err != nil {
		t.Fatal(err)
	}
	// Runtime cleanup may cancel the stream again before the next outer batch.
	for _, code := range []byte{EndCancelled, EndInternal} {
		if err := a.Reset(code); err != nil {
			t.Fatal(err)
		}
		if got := a.Status().EndCode; got != EndBudget {
			t.Fatalf("first reset reason overwritten: %d", got)
		}
	}
	xfer(t, c, s)
	if got := b.Status().PeerEndCode; got != EndBudget {
		t.Fatalf("peer received wrong reason: %d", got)
	}
	xfer(t, s, c)
	if !a.Status().EndAcked || !b.Status().EndAcked {
		t.Fatal("reset terminal incomplete")
	}
}
