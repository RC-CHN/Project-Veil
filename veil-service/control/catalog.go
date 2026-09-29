package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"veil/service"
)

// Catalog snapshots let platform configuration stores validate and persist a
// complete desired state before asking the running daemon to apply it.
func (c *Catalog) snapshot() ([]byte, string) {
	data := catalogFile{Version: 1, Profiles: make([]Profile, 0, len(c.profiles))}
	for _, p := range c.profiles {
		data.Profiles = append(data.Profiles, cloneProfile(p))
	}
	slices.SortFunc(data.Profiles, func(a, b Profile) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	b, _ := json.Marshal(data)
	hash := sha256.Sum256(b)
	return b, hex.EncodeToString(hash[:])
}

func (c *Catalog) validateSnapshot(raw []byte) (*Catalog, error) {
	if len(raw) > service.MaxConfigSize {
		return nil, errors.New("catalog exceeds 1 MiB")
	}
	var data catalogFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("expected one catalog object")
	}
	if data.Version != 1 || len(data.Profiles) > 64 {
		return nil, errors.New("invalid catalog version or size")
	}
	staged := &Catalog{profiles: map[string]Profile{}, entries: c.entries}
	for _, p := range data.Profiles {
		if _, exists := staged.profiles[p.ID]; exists {
			return nil, errors.New("duplicate profile ID")
		}
		if old, exists := c.profiles[p.ID]; exists && old.Kind != p.Kind {
			return nil, errors.New("profile kind cannot change")
		}
		staged.profiles[p.ID] = p
	}
	for _, p := range data.Profiles {
		if err := staged.validate(&p); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		staged.profiles[p.ID] = p
	}
	return staged, nil
}

// Called with the catalog lock held. Get/validate contain credentials and must
// receive the same platform permissions as connection_get/connection_save.
func (c *Catalog) handleSnapshot(q Request) Response {
	current, rev := c.snapshot()
	if q.Action == "connections_get" {
		return Response{Version: Version, Config: current, Revision: rev, Connections: c.list()}
	}
	if q.Action != "connections_validate" && q.Action != "connections_save" {
		return Fail("unknown_action", errors.New("unknown catalog action"))
	}
	if q.Action == "connections_save" && (q.ExpectedRevision == nil || *q.ExpectedRevision != rev) {
		return Fail("conflict", errors.New("catalog changed; reload before saving"))
	}
	staged, err := c.validateSnapshot(q.Config)
	if err != nil {
		return Fail("invalid_config", err)
	}
	data, next := staged.snapshot()
	response := Response{Version: Version, Config: data, Revision: next}
	if q.Action == "connections_validate" {
		return response
	}
	if err := c.persist(staged.profiles); err != nil {
		// A directory sync failure may happen after the atomic rename. Report
		// the actual saved state so platform stores can reconcile correctly.
		data, revision := c.snapshot()
		return Response{Version: Version, Config: data, Revision: revision,
			Connections: c.list(), Error: &Failure{Code: "save_failed", Message: err.Error()}}
	}
	// Removed entries must stop; apply=false keeps surviving running instances.
	for id := range c.entries {
		if _, exists := c.profiles[id]; !exists {
			c.stop(id)
			delete(c.entries, id)
		}
	}
	if q.Apply {
		if q.ID != "" {
			if p, ok := c.profiles[q.ID]; ok {
				err = c.apply(p)
			}
		} else {
			for _, p := range c.profiles {
				if p.Kind == "connection" {
					err = errors.Join(err, c.apply(p))
				}
			}
		}
		if err != nil {
			response.Error = &Failure{Code: "start_failed", Message: err.Error()}
		}
	}
	response.Connections = c.list()
	return response
}
