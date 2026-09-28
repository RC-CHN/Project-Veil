// Package systemproxy manages desktop proxy settings independently of the Veil core.
package systemproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
)

var ErrChanged = errors.New("system proxy settings were changed by another application")
var ErrUnsupported = errors.New("system proxy management is unavailable on this desktop")

type Snapshot map[string]string

type Backend interface {
	Name() string
	Read() (Snapshot, error)
	Write(Snapshot) error
	Manual(Snapshot, string) Snapshot
	Direct(Snapshot) Snapshot
}

type record struct {
	Mode    string   `json:"mode"`
	Backend string   `json:"backend,omitempty"`
	Before  Snapshot `json:"before,omitempty"`
	Applied Snapshot `json:"applied,omitempty"`
}

type Status struct {
	Mode      string `json:"mode"`
	Supported bool   `json:"supported"`
	Managed   bool   `json:"managed"`
	Error     string `json:"error,omitempty"`
}

type Controller struct {
	mu        sync.Mutex
	backend   Backend
	path      string
	state     record
	lastError string
}

// Open recovers a previous run's settings only if they still match our journal.
func Open(path string, backend Backend) (*Controller, error) {
	c := &Controller{backend: backend, path: path, state: record{Mode: "keep"}}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if len(b) > 65536 {
			return nil, errors.New("system proxy journal is too large")
		}
		if err = json.Unmarshal(b, &c.state); err != nil {
			return nil, err
		}
		if c.state.Mode != "keep" && c.state.Mode != "auto" {
			return nil, errors.New("invalid system proxy mode")
		}
		if err = c.restore(); err != nil {
			c.lastError = err.Error()
		}
	}
	return c, nil
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{Mode: c.state.Mode, Supported: c.backend != nil, Managed: c.state.Applied != nil, Error: c.lastError}
}

func (c *Controller) save() error {
	b, err := json.Marshal(c.state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), ".system-proxy-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), c.path)
}

func (c *Controller) SetMode(mode, endpoint string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mode != "keep" && mode != "auto" {
		return errors.New("invalid system proxy mode")
	}
	if mode == "auto" && c.backend == nil {
		return ErrUnsupported
	}
	if mode == "keep" {
		if err := c.restore(); err != nil && !errors.Is(err, ErrChanged) {
			return err
		}
	}
	previous := c.state.Mode
	c.state.Mode = mode
	if err := c.save(); err != nil {
		c.state.Mode = previous
		return err
	}
	c.lastError = ""
	if mode == "auto" && endpoint != "" {
		return c.apply(endpoint)
	}
	return nil
}

func (c *Controller) Apply(endpoint string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Mode != "auto" {
		return nil
	}
	return c.apply(endpoint)
}

func (c *Controller) apply(endpoint string) error {
	if c.backend == nil {
		return ErrUnsupported
	}
	current, err := c.backend.Read()
	if err != nil {
		return err
	}
	if c.state.Applied != nil && !reflect.DeepEqual(current, c.state.Applied) {
		c.state.Before = nil
		c.state.Applied = nil
		if err = c.save(); err != nil {
			return err
		}
		c.lastError = ErrChanged.Error()
		return ErrChanged
	}
	next := c.backend.Manual(clone(current), endpoint)
	previous := c.state
	if c.state.Applied == nil {
		c.state.Before = current
	}
	c.state.Applied = next
	c.state.Backend = c.backend.Name()
	// Persist the recovery record before modifying global desktop settings.
	if err = c.save(); err != nil {
		c.state = previous
		return err
	}
	if err = c.backend.Write(next); err != nil {
		// A failed multi-key write may have partially succeeded. Roll it back now.
		if rollback := c.backend.Write(current); rollback != nil {
			return errors.Join(err, fmt.Errorf("restore: %w", rollback))
		}
		c.state = previous
		return errors.Join(err, c.save())
	}
	c.lastError = ""
	return nil
}

func (c *Controller) Restore() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.restore()
	if err != nil {
		c.lastError = err.Error()
	} else {
		c.lastError = ""
	}
	return err
}

func (c *Controller) restore() error {
	if c.state.Applied == nil {
		return nil
	}
	if c.backend == nil || c.backend.Name() != c.state.Backend {
		return ErrUnsupported
	}
	current, err := c.backend.Read()
	if err != nil {
		return err
	}
	changed := !reflect.DeepEqual(current, c.state.Applied)
	if !changed {
		if err = c.backend.Write(c.state.Before); err != nil {
			return err
		}
	}
	c.state.Before = nil
	c.state.Applied = nil
	if err = c.save(); err != nil {
		return err
	}
	if changed {
		return ErrChanged
	}
	return nil
}

// Clear is an explicit user action; it switches to keep mode so later starts
// do not immediately replace the user's choice to connect directly.
func (c *Controller) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backend == nil {
		return ErrUnsupported
	}
	current, err := c.backend.Read()
	if err != nil {
		return err
	}
	if err = c.backend.Write(c.backend.Direct(clone(current))); err != nil {
		return errors.Join(err, c.backend.Write(current))
	}
	c.state = record{Mode: "keep"}
	c.lastError = ""
	return c.save()
}

func clone(value Snapshot) Snapshot {
	copy := make(Snapshot, len(value))
	for k, v := range value {
		copy[k] = v
	}
	return copy
}
