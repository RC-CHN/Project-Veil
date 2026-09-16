package streamlink

import (
	"context"
	"errors"
	"io"
	"time"
)

// Read returns application-consumed bytes, releasing exactly their credit.
// Its caller serializes pull reads. It cannot be combined with Consume.
func (l *Link) Read(ctx context.Context, p []byte) (int, error) {
	if ctx == nil {
		return 0, errors.New("nil read context")
	}
	for {
		l.mu.Lock()
		if l.consumerStarted && !l.pullConsumer {
			l.mu.Unlock()
			return 0, errors.New("duplicate consumer")
		}
		l.consumerStarted = true
		l.pullConsumer = true
		if e := l.live(); e != nil {
			l.mu.Unlock()
			return 0, e
		}
		if e := ctx.Err(); e != nil {
			l.mu.Unlock()
			return 0, e
		}
		if len(p) == 0 {
			l.mu.Unlock()
			return 0, nil
		}
		if l.rx.n > 0 {
			n := min(len(p), l.rx.n)
			at := min(n, len(l.rx.p)-l.rx.head)
			copy(p, l.rx.p[l.rx.head:l.rx.head+at])
			copy(p[at:n], l.rx.p[:n-at])
			l.rx.drop(n)
			l.consumed += uint64(n)
			l.creditObservation.consumed(time.Now())
			if l.rx.n == 0 && l.remoteFIN {
				l.remoteDelivered = true
			}
			l.signal()
			l.mu.Unlock()
			return n, nil
		}
		if l.remoteFIN {
			l.remoteDelivered = true
			l.signal()
			l.mu.Unlock()
			return 0, io.EOF
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-l.ctx.Done():
			return 0, l.ctx.Err()
		}
	}
}
