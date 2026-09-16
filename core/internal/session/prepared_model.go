package session

import (
	"crypto/sha256"
	"encoding/hex"

	b "veil.local/core/internal/behavior"
)

const ProfileBulk192Prepared = "bulk192-prepared-v1"

// These are the native local-object-v1 ETags of the exclusive initial objects.
// They certify initial object state, not identity or authority.
func preparedETags() (string, string) {
	etag := func(body []byte) string {
		sum := sha256.Sum256(body)
		return "\"" + hex.EncodeToString(sum[:16]) + "\""
	}
	return etag([]byte("initial")), etag(streamEncode(0, nil, false))
}

func prepared192Model() b.BatchModel {
	m := bulk192Model()
	up, down := preparedETags()
	m.Registers[0].Initial = dig([]byte(up))
	m.Registers[1].Initial = dig([]byte(down))
	m.Registers[2].Initial = dig(streamEncode(0, nil, false))
	// First offer is zero, but object sequence one must differ from the
	// exclusive initial download object even when the first lease is empty.
	m.Registers[5].Initial = b.Value{Number: 1}
	m.Stages[0] = m.Stages[1]
	m.Stages[0].Name = "establish"
	m.Stages[0].From = 0 // Branch cases own the destination; To remains canonical zero.
	return m
}

func preparedFlightModel(instances int) FlightModel {
	m := flightModel(instances)
	m.Version = 2
	m.Child = prepared192Model()
	return m
}

func (p *flightProgram) objectOffset() uint64 {
	if p.version >= 2 {
		return 1
	}
	return 0
}

func earlyOpenFlightModel(instances int) FlightModel {
	m := preparedFlightModel(instances)
	m.Version = 3
	return m
}

func (p *flightProgram) earlyOpen() bool { return p.version == 3 || p.version == 4 }
