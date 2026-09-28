package control

import (
	"encoding/json"
	"errors"
)

const Version = 1

type Request struct {
	Version int             `json:"version"`
	Action  string          `json:"action"`
	Config  json.RawMessage `json:"config,omitempty"`
	// Optional compare-and-swap guard for save/start/restart from multiple UIs.
	ExpectedRevision *string `json:"expected_revision,omitempty"`
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Response struct {
	Version  int      `json:"version"`
	Status   *Status  `json:"status,omitempty"`
	Revision string   `json:"revision,omitempty"`
	Error    *Failure `json:"error,omitempty"`
	// Config contains credentials and is returned only by an explicit config request.
	Config json.RawMessage `json:"config,omitempty"`
}

func Fail(code string, err error) Response {
	return Response{Version: Version, Error: &Failure{Code: code, Message: err.Error()}}
}

// Handle serializes changes, including the status returned with each change.
// Saving never restarts a running instance. Start never applies pending changes.
func (m *Manager) Handle(q Request) Response {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q.Version != Version {
		return Fail("unsupported_version", errors.New("expected control protocol version 1"))
	}
	if m.closed {
		return Fail("closed", errors.New("manager is closed"))
	}
	if len(q.Config) != 0 && q.Action != "validate" && q.Action != "save" {
		return Fail("invalid_request", errors.New("config is only accepted by validate and save"))
	}
	if q.ExpectedRevision != nil && *q.ExpectedRevision != m.saved {
		return Fail("conflict", errors.New("saved configuration changed; refresh status"))
	}
	var revision string
	var config json.RawMessage
	switch q.Action {
	case "status":
	case "config":
		if m.cfg != nil {
			var err error
			config, err = json.Marshal(m.cfg)
			if err != nil {
				return Fail("config_failed", err)
			}
		}
	case "validate", "save":
		cfg, b, rev, err := parse(q.Config, true)
		if err != nil {
			return Fail("invalid_config", err)
		}
		revision = rev
		if q.Action == "save" {
			committed, err := writeConfig(m.path, b)
			if committed {
				m.cfg, m.saved = &cfg, rev
			}
			if err != nil {
				s := m.status()
				r := Fail("save_failed", err)
				if committed {
					r.Error.Code = "durability_uncertain"
				}
				r.Status = &s
				return r
			}
		}
	case "start", "restart":
		if m.cfg == nil {
			return Fail("no_config", errors.New("save a configuration first"))
		}
		if q.Action == "start" && m.runtime.Snapshot().State == "running" {
			break
		}
		var err error
		if q.Action == "restart" {
			err = m.runtime.Restart(*m.cfg)
		} else {
			err = m.runtime.Start(*m.cfg)
		}
		if err != nil {
			m.lastError = err.Error()
			s := m.status()
			r := Fail("start_failed", err)
			r.Status = &s
			return r
		}
		m.active = m.saved
		m.lastError = ""
	case "stop":
		m.runtime.Stop()
		m.lastError = ""
	default:
		return Fail("unknown_action", errors.New("unknown control action"))
	}
	s := m.status()
	return Response{Version: Version, Status: &s, Revision: revision, Config: config}
}
