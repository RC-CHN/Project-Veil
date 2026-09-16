package session

import (
	"encoding/binary"
	"errors"
)

type streamQueueStatus struct {
	Used, Leased, MaxUsed               int
	Pending, InputEOF, AckedEOF, Closed bool
	BlockedNS                           int64
}

func streamEncode(sequence uint64, data []byte, eof bool) []byte {
	p := make([]byte, 16+len(data)+(len(data)+3)/4)
	binary.BigEndian.PutUint64(p, sequence)
	n := uint64(len(data))
	if eof {
		n |= uint64(1) << 63
	}
	binary.BigEndian.PutUint64(p[8:], n)
	copy(p[16:], data)
	for i := 16 + len(data); i < len(p); i++ {
		p[i] = byte(i*131 + 17)
	}
	return p
}

func streamDecode(p []byte) (sequence uint64, data []byte, eof bool, err error) {
	if len(p) < 16 || len(p) > maxObject {
		return 0, nil, false, errors.New("stream object bound")
	}
	sequence = binary.BigEndian.Uint64(p)
	n := binary.BigEndian.Uint64(p[8:])
	eof = n>>63 == 1
	n &= (uint64(1) << 63) - 1
	if n > uint64(len(p)-16) || 16+n+(n+3)/4 != uint64(len(p)) {
		return 0, nil, false, errors.New("stream length/padding bound")
	}
	for i := 16 + int(n); i < len(p); i++ {
		if p[i] != byte(i*131+17) {
			return 0, nil, false, errors.New("stream padding mismatch")
		}
	}
	return sequence, p[16 : 16+int(n)], eof, nil
}
