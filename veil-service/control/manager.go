// Package control owns one saved profile and its runtime. It has no dependency
// on systemd, rc.d, HTTP, routing policy or a desktop framework.
package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"veil/service"
)

type Status struct {
	service.Snapshot
	SavedRevision   string `json:"saved_revision,omitempty"`
	ActiveRevision  string `json:"active_revision,omitempty"`
	RestartRequired bool   `json:"restart_required"`
}

type Manager struct {
	catalog       *Catalog
	mu            sync.Mutex
	runtime       service.Runtime
	path          string
	cfg           *service.Config
	saved, active string
	lastError     string
	closed        bool
}

// Open loads the profile without starting it. The caller must exclusively own
// dir for the lifetime of Manager; the daemon holds a process lock for this.
func Open(dir string) (*Manager, error) {
	if err := PrivateDir(dir); err != nil {
		return nil, err
	}
	catalog, err := openCatalog(dir)
	if err != nil {
		return nil, err
	}
	m := &Manager{path: filepath.Join(dir, "config.json"), catalog: catalog}
	f, err := os.Open(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, service.MaxConfigSize+1))
	if err != nil {
		return nil, err
	}
	cfg, _, rev, err := parse(b, false)
	if err != nil {
		return nil, fmt.Errorf("saved configuration: %w", err)
	}
	m.cfg, m.saved = &cfg, rev
	return m, nil
}

func parse(b []byte, validate bool) (service.Config, []byte, string, error) {
	cfg, err := service.Parse(bytes.NewReader(b))
	if err == nil && validate {
		err = service.Validate(cfg)
	}
	if err != nil {
		return cfg, nil, "", err
	}
	b, err = json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return cfg, nil, "", err
	}
	b = append(b, '\n')
	if len(b) > service.MaxConfigSize {
		return cfg, nil, "", errors.New("normalized configuration exceeds 1 MiB")
	}
	h := sha256.Sum256(b)
	return cfg, b, hex.EncodeToString(h[:]), nil
}

func (m *Manager) status() Status {
	s := m.runtime.Snapshot()
	if s.State == "stopped" && m.lastError != "" {
		s.Error = m.lastError
	}
	active := m.active
	if s.State != "running" {
		active = ""
	}
	return Status{Snapshot: s, SavedRevision: m.saved, ActiveRevision: active,
		RestartRequired: s.State == "running" && active != m.saved}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status()
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.catalog.Close()
	return m.runtime.Close()
}

func writeConfig(path string, b []byte) (committed bool, err error) {
	f, err := configTemp(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return false, err
	}
	if err = f.Sync(); err != nil {
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	return replaceConfig(f.Name(), path)
}

// StartConnections restores independently enabled connection profiles.
func (m *Manager) StartConnections() { m.catalog.Autostart() }

// WritePrivateFile atomically exports a user-selected file with the same private
// permissions as saved configuration, including when replacing an existing file.
func WritePrivateFile(path string, data []byte) error {
	_, err := writeConfig(path, data)
	return err
}
