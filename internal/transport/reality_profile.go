package transport

import (
	"errors"
	"fmt"

	utls "github.com/metacubex/utls"
)

// Measured against Chrome for Testing 149.0.7827.55 on Linux. This covers a
// fresh ClientHello only, not browser HTTP behavior or trust-anchor retries.
var helloChrome149 = utls.ClientHelloID{Client: "Chrome", Version: "149"}

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
		case "chrome149":
			id = helloChrome149
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
	base := id
	if id == helloChrome149 {
		base = utls.HelloChrome_133
	}
	spec, err := utls.UTLSIdToSpec(base)
	if err != nil {
		return spec, err
	}
	if id == helloChrome149 {
		// Empty requested trust-anchor vector, as emitted by the measured
		// fresh browser. Keep the final GREASE extension in its original place.
		n := len(spec.Extensions)
		last := spec.Extensions[n-1]
		spec.Extensions = append(spec.Extensions[:n-1], &utls.GenericExtension{Id: 0xca34, Data: []byte{0, 0}}, last)
		spec.Extensions = utls.ShuffleChromeTLSExtensions(spec.Extensions)
	}
	for _, ext := range spec.Extensions {
		if e, ok := ext.(*utls.RenegotiationInfoExtension); ok {
			// This changes local TLS 1.2 behavior, not the extension bytes.
			// Keep the template's key shares, including ML-KEM, intact.
			e.Renegotiation = utls.RenegotiateNever
		}
	}
	return spec, nil
}
