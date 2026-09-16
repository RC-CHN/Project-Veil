package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	b "veil.local/core/internal/behavior"
)

type FlightModel struct {
	Version, Instances, Window, FrameVersion int
	Child                                    b.BatchModel
}

type flightProgram struct {
	id                string
	instances         int
	version           int
	child             *b.BatchProgram
	generationBatches uint64
	generationAge     time.Duration
}

func flightModel(instances int) FlightModel {
	return FlightModel{Version: 1, Instances: instances, Window: instances, FrameVersion: 1, Child: bulk192Model()}
}

func compileFlight(model FlightModel) (*flightProgram, error) {
	if (model.Version < 1 || model.Version > 4) || model.Instances < 1 || model.Instances > 4 || model.Window != model.Instances || model.FrameVersion != 1 {
		return nil, errors.New("flight model bounds or version")
	}
	child, e := b.CompileBatch(model.Child)
	if e != nil {
		return nil, e
	}
	wantModel := bulk192Model()
	if model.Version >= 2 {
		wantModel = prepared192Model()
	}
	want, e := b.CompileBatch(wantModel)
	if e != nil || child.ID() != want.ID() {
		return nil, errors.New("unsupported flight child model")
	}
	raw, e := json.Marshal(model)
	if e != nil {
		return nil, e
	}
	id := sha256.Sum256(raw)
	return &flightProgram{id: hex.EncodeToString(id[:]), instances: model.Instances, version: model.Version, child: child, generationBatches: flightGenerationBatches, generationAge: flightGenerationAge}, nil
}

func (p *flightProgram) material(cs *tls.ConnectionState) ([32]byte, error) {
	var out [32]byte
	if cs == nil || cs.Version != tls.VersionTLS13 || cs.NegotiatedProtocol != "h2" {
		return out, errors.New("flight requires TLS1.3/H2")
	}
	value, e := cs.ExportKeyingMaterial("EXPORTER-veil-object-flight-v1", []byte(p.id), 32)
	copy(out[:], value)
	return out, e
}

func (p *flightProgram) laneMaterial(master [32]byte, lane int) ([32]byte, error) {
	if p.renewable() {
		return p.generationMaterial(master, lane, 0)
	}
	var out [32]byte
	if lane < 0 || lane >= p.instances {
		return out, errors.New("flight instance bound")
	}
	h := hmac.New(sha256.New, master[:])
	h.Write([]byte("veil-flight-instance-1\x00"))
	var number [4]byte
	binary.BigEndian.PutUint32(number[:], uint32(lane))
	h.Write(number[:])
	copy(out[:], h.Sum(nil))
	return out, nil
}
