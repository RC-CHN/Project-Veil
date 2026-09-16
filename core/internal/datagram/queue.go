package datagram

import (
	"context"
	"io"
	"sync"
	"veil.local/core/transport"
)

const QueuePackets = 32
const QueueBytes = 256 << 10

type QueueStatus struct {
	Packets, Bytes, MaxPackets, MaxBytes int
	Dropped                              uint64
	Closed                               bool
}
type Queue struct {
	mu                                     sync.Mutex
	items                                  [QueuePackets][]byte
	head, count, bytes, maxCount, maxBytes int
	dropped                                uint64
	closed                                 bool
	changed                                chan struct{}
}

func NewQueue() *Queue   { return &Queue{changed: make(chan struct{})} }
func (q *Queue) signal() { close(q.changed); q.changed = make(chan struct{}) }

// Push owns a copy only after admission. A rejected packet never enters the byte stream.
func (q *Queue) Push(p []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pushLocked(p)
}
func (q *Queue) PushContext(ctx context.Context, p []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return e
	}
	if q.closed {
		return io.ErrClosedPipe
	}
	if !q.pushLocked(p) {
		return transport.ErrCapacity
	}
	return nil
}
func (q *Queue) pushLocked(p []byte) bool {
	if q.closed || len(p) > MaxRecord || q.count == QueuePackets || q.bytes+len(p) > QueueBytes {
		q.dropped++
		return false
	}
	q.items[(q.head+q.count)%QueuePackets] = append([]byte(nil), p...)
	q.count++
	q.bytes += len(p)
	q.maxCount = max(q.maxCount, q.count)
	q.maxBytes = max(q.maxBytes, q.bytes)
	q.signal()
	return true
}
func (q *Queue) Pop(ctx context.Context) ([]byte, error) {
	for {
		q.mu.Lock()
		if e := ctx.Err(); e != nil {
			q.mu.Unlock()
			return nil, e
		}
		if q.count > 0 {
			p := q.items[q.head]
			q.items[q.head] = nil
			q.head = (q.head + 1) % QueuePackets
			q.count--
			q.bytes -= len(p)
			q.mu.Unlock()
			return p, nil
		}
		if q.closed {
			q.mu.Unlock()
			return nil, io.EOF
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
func (q *Queue) Close(discard bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	if discard {
		for i, p := range q.items {
			clear(p)
			q.items[i] = nil
		}
		q.count = 0
		q.bytes = 0
	}
	q.signal()
}
func (q *Queue) Status() QueueStatus {
	q.mu.Lock()
	defer q.mu.Unlock()
	return QueueStatus{q.count, q.bytes, q.maxCount, q.maxBytes, q.dropped, q.closed}
}
