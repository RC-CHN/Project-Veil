// Package mux carries independent, flow-controlled byte streams over one TLS
// connection. Authentication belongs to the caller; there is no extra handshake.
package mux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	headerSize   = 8
	blockSize    = 32 << 10
	batchSize    = 128 << 10
	windowBlocks = 256 // At most 8 MiB of unread payload per stream.
	MaxStreams   = 8
	maxControls  = 8 * MaxStreams
	// PayloadSize reserves four DATA headers inside a full single-stream batch,
	// avoiding a small tail write when a relay reads a full input buffer.
	PayloadSize = batchSize - (batchSize/blockSize)*headerSize
)

const (
	open byte = iota + 1
	opened
	failed
	data
	fin
	reset
	credit
	finished
)

var (
	ErrProtocol = errors.New("veil mux: invalid frame or state")
	ErrReset    = errors.New("veil mux: stream reset")
	ErrFull     = errors.New("veil mux: connection at stream limit")
	// Distinguish our missing-reply budget from a caller's shorter deadline.
	ErrOpenTimeout = fmt.Errorf("veil mux: OPEN response: %w", context.DeadlineExceeded)
)

type OpenError byte

func (e OpenError) Error() string { return "veil mux: destination rejected" }

type frame struct {
	typ     byte
	id      uint32
	payload []byte
}

func appendFrame(dst []byte, typ byte, id uint32, p []byte) []byte {
	n := len(dst)
	dst = append(dst, typ, 0, 0, 0, 0, byte(len(p)>>16), byte(len(p)>>8), byte(len(p)))
	binary.BigEndian.PutUint32(dst[n+1:n+5], id)
	return append(dst, p...)
}

func readHeader(r io.Reader, h *[headerSize]byte) (byte, uint32, int, error) {
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, 0, 0, err
	}
	t, id, n := h[0], binary.BigEndian.Uint32(h[1:5]), int(h[5])<<16|int(h[6])<<8|int(h[7])
	valid := id != 0 && id&1 == 1
	switch t {
	case open:
		valid = valid && n > 0 && n <= 512
	case opened, fin, reset, finished:
		valid = valid && n == 0
	case failed:
		valid = valid && n == 1
	case credit:
		valid = valid && n == 2
	case data:
		valid = valid && n > 0 && n <= blockSize
	default:
		valid = false
	}
	if !valid {
		return 0, 0, 0, ErrProtocol
	}
	return t, id, n, nil
}

// Separate payload and metadata avoid rounding a 32 KiB block up to 40 KiB.
// Size classes keep tiny frames out of the bulk cache. sync.Pool allows GC to
// reclaim historical peaks without forcing allocation during each bulk burst.
var blockCaches [3]sync.Pool
var blockCapacities = [...]int{256, 4096, blockSize}

type chunk struct {
	b                 []byte
	start, end        int
	complete, discard bool
}

func takeChunk(n int) *chunk {
	for i, size := range blockCapacities {
		if n <= size {
			if cached := blockCaches[i].Get(); cached != nil {
				c := cached.(*chunk)
				c.start, c.end, c.complete, c.discard = 0, 0, false, false
				return c
			}
			return &chunk{b: make([]byte, size)}
		}
	}
	panic("mux: oversized receive chunk")
}

func putChunk(c *chunk) {
	for i, size := range blockCapacities {
		if cap(c.b) == size {
			blockCaches[i].Put(c)
			return
		}
	}
}
