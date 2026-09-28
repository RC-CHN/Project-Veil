package control

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
)

// saveConnection imports an optional relay and its connection in one commit.
// Imported IDs never replace an unrelated relay, even when a UI retries a save.
func (c *Catalog) saveConnection(p *Profile, relay *Profile) error {
	staged := &Catalog{profiles: make(map[string]Profile, len(c.profiles)+2), entries: c.entries}
	for id, v := range c.profiles {
		staged.profiles[id] = v
	}
	if relay != nil {
		r := cloneProfile(*relay)
		if p.Kind != "connection" || r.Kind != "relay" || p.RelayID != r.ID || p.ID == r.ID {
			return errors.New("embedded relay does not match the connection")
		}
		if err := staged.validate(&r); err != nil {
			return err
		}
		if old, exists := staged.profiles[r.ID]; exists && revision(old) != revision(r) {
			return errors.New("embedded relay ID already exists; import with a new ID")
		}
		staged.profiles[r.ID] = r
	}
	if err := staged.validate(p); err != nil {
		return err
	}
	staged.profiles[p.ID] = *p
	return c.persist(staged.profiles)
}

// Portable profiles contain the trust certificates themselves, never a path on
// the exporting machine. Only certificate PEM blocks can leave a saved CA file.
func portableProfile(p Profile) (Profile, error) {
	p = cloneProfile(p)
	s := &p.Config.TLS
	if s.CAFile != "" {
		f, err := os.Open(s.CAFile)
		if err != nil {
			return p, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
		if err != nil {
			return p, err
		}
		if len(data) > 64<<10 {
			return p, errors.New("CA file exceeds 64 KiB")
		}
		s.CAPEM = ""
		for len(data) > 0 {
			block, rest := pem.Decode(data)
			if block == nil {
				break
			}
			data = rest
			if block.Type != "CERTIFICATE" {
				continue
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return p, err
			}
			s.CAPEM += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))
		}
		if s.CAPEM == "" {
			return p, errors.New("CA file contains no certificates")
		}
		s.CAFile = ""
	}
	s.Certificate, s.PrivateKeyFile, s.RealityPrivateKey, s.CoverAddress = "", "", "", ""
	return p, nil
}

func (c *Catalog) export(p Profile) Response {
	p, err := portableProfile(p)
	if err != nil {
		return Fail("export_failed", err)
	}
	response := Response{Version: Version, Profile: &p}
	if p.RelayID != "" {
		r, exists := c.profiles[p.RelayID]
		if !exists {
			return Fail("export_failed", errors.New("relay is missing"))
		}
		r, err = portableProfile(r)
		if err != nil {
			return Fail("export_failed", err)
		}
		response.Relay = &r
	}
	return response
}
