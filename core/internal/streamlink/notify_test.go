package streamlink

import (
	"context"
	"testing"
)

func TestChangesCanBeCombinedWithoutLostWakeup(t *testing.T) {
	l, e := New(context.Background(), Config{Window: 16, MaxBytes: 32})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close(nil)
	before := l.Changes()
	select {
	case <-before:
		t.Fatal("unchanged channel closed")
	default:
	}
	if _, e = l.Write(context.Background(), []byte{1}); e != nil {
		t.Fatal(e)
	}
	select {
	case <-before:
	default:
		t.Fatal("producer did not signal old snapshot")
	}
	after := l.Changes()
	if l.Status().AvailableBytes == 0 {
		t.Fatal("ready data absent")
	}
	select {
	case <-after:
		t.Fatal("new snapshot already closed")
	default:
	}
	l.Close(nil)
	select {
	case <-after:
	default:
		t.Fatal("close did not signal")
	}
}
