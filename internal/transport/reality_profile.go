package transport

import (
	"errors"
	"fmt"

	utls "github.com/metacubex/utls"
)

// Resolve immutable template IDs once. Mutable specs and keys are created by
// each handshake, including when the same Handshake is called concurrently.
func realityProfiles(s Settings) ([]utls.ClientHelloID, error) {
	if s.Fingerprint != "" && s.Fingerprints != nil {
		return nil, errors.New("transport: use fingerprint or fingerprints, not both")
	}
	names := s.Fingerprints
	if names == nil {
		name := s.Fingerprint
		if name == "" {
			name = "chrome"
		}
		names = []string{name}
	}
	if len(names) == 0 {
		return nil, errors.New("transport: fingerprints must not be empty")
	}
	profiles := make([]utls.ClientHelloID, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		var id utls.ClientHelloID
		switch name {
		case "chrome", "chrome133":
			id = utls.HelloChrome_133
		case "chrome131":
			id = utls.HelloChrome_131
		case "chrome120":
			id = utls.HelloChrome_120
		default:
			return nil, fmt.Errorf("transport: unsupported REALITY fingerprint %q", name)
		}
		if seen[id.Str()] {
			return nil, fmt.Errorf("transport: duplicate REALITY fingerprint %q", name)
		}
		seen[id.Str()] = true
		profiles = append(profiles, id)
	}
	return profiles, nil
}

func realitySpec(id utls.ClientHelloID) (utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(id)
	if err != nil {
		return spec, err
	}
	for _, ext := range spec.Extensions {
		switch e := ext.(type) {
		case *utls.SupportedCurvesExtension:
			out := e.Curves[:0]
			for _, v := range e.Curves {
				if v != utls.X25519MLKEM768 {
					out = append(out, v)
				}
			}
			e.Curves = out
		case *utls.KeyShareExtension:
			out := e.KeyShares[:0]
			for _, v := range e.KeyShares {
				if v.Group != utls.X25519MLKEM768 {
					out = append(out, v)
				}
			}
			e.KeyShares = out
		case *utls.RenegotiationInfoExtension:
			e.Renegotiation = utls.RenegotiateNever
		}
	}
	return spec, nil
}
