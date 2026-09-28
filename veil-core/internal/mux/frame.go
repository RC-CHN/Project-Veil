// Package mux carries independent, flow-controlled byte streams over one TLS
// connection. Authentication belongs to the caller; there is no extra handshake.
package mux

import (
	"encoding/binary"
	"errors"
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

var blocks = sync.Pool{New: func() any { return new(chunk) }}

type chunk struct {
	b                 [blockSize]byte
	start, end        int
	complete, discard bool
}
