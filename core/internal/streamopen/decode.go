package streamopen

import (
	"encoding/binary"
	"errors"
)

// DecodeRequest validates exactly one complete OPEN prefix, without trailing data.
// Streaming users continue to use Handshake, which also enforces phase ordering.
func DecodeRequest(p []byte) (Request, error) {
	if !completePrefix(p, 1) {
		return Request{}, errors.New("complete OPEN prefix")
	}
	return decodeRequest(p[Header:])
}

// DecodeResult validates exactly one complete RESULT prefix. The caller still
// checks its phase and that successful limits do not expand the offered limits.
func DecodeResult(p []byte) (Result, error) {
	if !completePrefix(p, 2) {
		return Result{}, errors.New("complete RESULT prefix")
	}
	return decodeResult(p[Header:])
}

func completePrefix(p []byte, kind byte) bool {
	return len(p) >= Header && len(p) <= Header+MaxBody && p[0] == 1 && p[1] == kind && p[2] == 0 && p[3] == 0 && int(binary.BigEndian.Uint32(p[4:8])) == len(p)-Header
}
