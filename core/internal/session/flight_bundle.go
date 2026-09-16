package session

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"

	b "veil.local/core/internal/behavior"
)

const FlightInnerProtocol = "streammux-flight-v1"
const EarlyOpenFlightInnerProtocol = "streammux-flight-v2"
const RenewingFlightInnerProtocol = "streammux-flight-v3"

type FlightBundle struct {
	Version       int
	SeedHex       string
	InnerProtocol string
	Model         FlightModel
}

func GenerateFlightBundle(instances int) (FlightBundle, error) {
	return GenerateFlightBundleProfile(instances, ProfileBulk192)
}

func GenerateFlightBundleProfile(instances int, profile string) (FlightBundle, error) {
	model := flightModel(instances)
	switch profile {
	case ProfileBulk192:
	case ProfileBulk192Prepared:
		model = preparedFlightModel(instances)
	default:
		return FlightBundle{}, errors.New("unsupported flight profile")
	}
	if _, e := compileFlight(model); e != nil {
		return FlightBundle{}, e
	}
	var seed [32]byte
	if _, e := rand.Read(seed[:]); e != nil {
		return FlightBundle{}, e
	}
	return FlightBundle{Version: 3, SeedHex: hex.EncodeToString(seed[:]), InnerProtocol: FlightInnerProtocol, Model: model}, nil
}

func GenerateEarlyOpenFlightBundle(instances int) (FlightBundle, error) {
	bundle, e := GenerateFlightBundleProfile(instances, ProfileBulk192Prepared)
	if e != nil {
		return FlightBundle{}, e
	}
	bundle.Version, bundle.InnerProtocol = 4, EarlyOpenFlightInnerProtocol
	bundle.Model = earlyOpenFlightModel(instances)
	if _, e = compileFlight(bundle.Model); e != nil {
		return FlightBundle{}, e
	}
	return bundle, nil
}

func GenerateRenewingFlightBundle(instances int) (FlightBundle, error) {
	bundle, err := GenerateEarlyOpenFlightBundle(instances)
	if err != nil {
		return FlightBundle{}, err
	}
	bundle.Version, bundle.InnerProtocol, bundle.Model = 5, RenewingFlightInnerProtocol, renewingFlightModel(instances)
	if _, err := compileFlight(bundle.Model); err != nil {
		return FlightBundle{}, err
	}
	return bundle, nil
}

func WriteFlightBundle(path string, bundle FlightBundle) error {
	return writePrivateBundle(path, bundle)
}

func loadFlightBundle(path string) ([32]byte, *flightProgram, error) {
	var bundle FlightBundle
	var seed [32]byte
	if e := ReadPrivateJSON(path, &bundle, 256<<10); e != nil {
		return seed, nil, e
	}
	raw, e := hex.DecodeString(bundle.SeedHex)
	validVersion := bundle.Version == 3 && bundle.InnerProtocol == FlightInnerProtocol && (bundle.Model.Version == 1 || bundle.Model.Version == 2) || bundle.Version == 4 && bundle.InnerProtocol == EarlyOpenFlightInnerProtocol && bundle.Model.Version == 3
	validVersion = validVersion || bundle.Version == 5 && bundle.InnerProtocol == RenewingFlightInnerProtocol && bundle.Model.Version == 4
	if e != nil || len(raw) != 32 || !validVersion {
		return seed, nil, errors.New("flight bundle version, protocol or seed")
	}
	copy(seed[:], raw)
	p, e := compileFlight(bundle.Model)
	return seed, p, e
}

func loadRuntimeModel(path string, version int) ([32]byte, *b.BatchProgram, *flightProgram, error) {
	if version >= 3 && version <= 5 {
		seed, p, e := loadFlightBundle(path)
		if e != nil {
			return seed, nil, nil, e
		}
		if (version >= 4) != p.earlyOpen() || (version == 5) != p.renewable() {
			return seed, nil, nil, errors.New("runtime and flight bundle version mismatch")
		}
		return seed, p.child, p, nil
	}
	bundle, seed, p, e := LoadBundle(path)
	if e == nil {
		e = checkInnerBundle(bundle, version)
	}
	return seed, p, nil, e
}

// CheckBundle returns the complete model ID, including composition parameters.
func CheckBundle(path string) (string, error) {
	var raw json.RawMessage
	if e := ReadPrivateJSON(path, &raw, 256<<10); e != nil {
		return "", e
	}
	var header struct{ Version int }
	if e := json.Unmarshal(raw, &header); e != nil {
		return "", e
	}
	_, p, f, e := loadRuntimeModel(path, header.Version)
	if e != nil {
		return "", e
	}
	return runtimeModelID(p, f), nil
}

func runtimeModelID(p *b.BatchProgram, f *flightProgram) string {
	if f != nil {
		return f.id
	}
	return p.ID()
}
func runtimeInner(version int) string {
	if version == 5 {
		return RenewingFlightInnerProtocol
	}
	if version == 4 {
		return EarlyOpenFlightInnerProtocol
	}
	if version == 3 {
		return FlightInnerProtocol
	}
	return "streammux-v1"
}
func (c *Client) modelID() string { return runtimeModelID(c.program, c.flight) }
func (s *Server) modelID() string { return runtimeModelID(s.program, s.flight) }
func modelMaterial(cs *tls.ConnectionState, p *b.BatchProgram, f *flightProgram, version int) ([32]byte, error) {
	if f != nil {
		return f.material(cs)
	}
	return runtimeMaterial(cs, p.ID(), version)
}
