package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"veil/service"
)

type Profile struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Enabled bool            `json:"enabled"`
	RelayID string          `json:"relay_id,omitempty"`
	Config  service.Config  `json:"config"`
	Inlets  []service.Inlet `json:"inlets"`
}
type ProbeResult struct {
	OK           bool      `json:"ok"`
	Milliseconds int64     `json:"milliseconds"`
	Error        string    `json:"error,omitempty"`
	At           time.Time `json:"at"`
	Target       string    `json:"target"`
}
type ConnectionStatus struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Server      string          `json:"server"`
	Enabled     bool            `json:"enabled"`
	RelayID     string          `json:"relay_id,omitempty"`
	RelayName   string          `json:"relay_name,omitempty"`
	RelayServer string          `json:"relay_server,omitempty"`
	Inlets      []service.Inlet `json:"inlets"`
	Revision    string          `json:"revision"`
	Pending     bool            `json:"pending"`
	service.Snapshot
	Probe *ProbeResult `json:"probe,omitempty"`
}
type catalogFile struct {
	Version  int       `json:"version"`
	Profiles []Profile `json:"profiles"`
}
type connectionEntry struct {
	testing                                 bool
	server, relayID, relayName, relayServer string
	transport                               string
	run                                     *service.Connection
	active                                  string
	err                                     string
	probe                                   *ProbeResult
	last                                    *service.Snapshot
}
type Catalog struct {
	mu       sync.Mutex
	path     string
	profiles map[string]Profile
	entries  map[string]*connectionEntry
	closed   bool
}

var profileID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,47}$`)

func openCatalog(dir string) (*Catalog, error) {
	c := &Catalog{path: filepath.Join(dir, "connections.json"), profiles: map[string]Profile{}, entries: map[string]*connectionEntry{}}
	f, err := os.Open(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, service.MaxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > service.MaxConfigSize {
		return nil, errors.New("connection store too large")
	}
	var data catalogFile
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&data); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("expected one connection store object")
	}
	if len(data.Profiles) > 64 {
		return nil, errors.New("too many saved profiles")
	}
	if data.Version != 1 {
		return nil, errors.New("unsupported connection store version")
	}
	for _, p := range data.Profiles {
		if !profileID.MatchString(p.ID) {
			return nil, errors.New("invalid saved connection ID")
		}
		if _, ok := c.profiles[p.ID]; ok {
			return nil, errors.New("duplicate saved connection ID")
		}
		c.profiles[p.ID] = p
	}
	return c, nil
}
func revision(p Profile) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func cloneProfile(p Profile) Profile {
	p.Inlets = slices.Clone(p.Inlets)
	p.Config.TLS.Fingerprints = slices.Clone(p.Config.TLS.Fingerprints)
	if p.Config.Traffic != nil {
		traffic := *p.Config.Traffic
		p.Config.Traffic = &traffic
	}
	return p
}
func (c *Catalog) entry(id string) *connectionEntry {
	e := c.entries[id]
	if e == nil {
		e = &connectionEntry{}
		c.entries[id] = e
	}
	return e
}
func (c *Catalog) list() []ConnectionStatus {
	rows := make([]ConnectionStatus, 0, len(c.profiles))
	for id, p := range c.profiles {
		e := c.entry(id)
		s := ConnectionStatus{ID: id, Name: p.Name, Kind: p.Kind, Server: p.Config.Server, Enabled: p.Enabled, RelayID: p.RelayID, Inlets: slices.Clone(p.Inlets), Revision: revision(p), Snapshot: service.Snapshot{State: "stopped"}}
		if e.probe != nil {
			probe := *e.probe
			s.Probe = &probe
		}
		if r, ok := c.profiles[p.RelayID]; ok {
			s.RelayName = r.Name
			s.RelayServer = r.Config.Server
		}
		if e.last != nil && e.run == nil {
			s.Snapshot = *e.last
		}
		if p.Kind == "relay" {
			s.State = "ready"
		} else if e.run != nil {
			s.Snapshot = e.run.Snapshot()
			s.Pending = e.active != c.runtimeRevision(p)
			s.Server = e.server
			s.RelayID = e.relayID
			s.RelayName = e.relayName
			s.RelayServer = e.relayServer
			s.Inlets = e.run.Inlets()
		}
		if e.err != "" {
			s.Error = e.err
		}
		rows = append(rows, s)
	}
	slices.SortFunc(rows, func(a, b ConnectionStatus) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return rows
}
func (c *Catalog) transportRevision(p Profile) string { p.Inlets = nil; return c.runtimeRevision(p) }

func (c *Catalog) runtimeRevision(p Profile) string {
	p.Name = ""
	p.Enabled = false
	s := revision(p)
	if r, ok := c.profiles[p.RelayID]; ok {
		r.Name = ""
		s += revision(r)
	}
	return s
}
func (c *Catalog) persist(next map[string]Profile) error {
	f := catalogFile{Version: 1, Profiles: make([]Profile, 0, len(next))}
	for _, p := range next {
		f.Profiles = append(f.Profiles, p)
	}
	slices.SortFunc(f.Profiles, func(a, b Profile) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > service.MaxConfigSize {
		return errors.New("connection store too large")
	}
	committed, err := writeConfig(c.path, append(b, '\n'))
	if committed {
		c.profiles = next
	}
	return err
}
func (c *Catalog) save(p Profile) error {
	next := make(map[string]Profile, len(c.profiles)+1)
	for id, v := range c.profiles {
		next[id] = v
	}
	next[p.ID] = p
	return c.persist(next)
}
func (c *Catalog) validate(p *Profile) error {
	if !profileID.MatchString(p.ID) || len(p.Name) == 0 || len(p.Name) > 120 {
		return errors.New("name and a valid connection ID are required")
	}
	if len(c.profiles) >= 64 {
		if _, exists := c.profiles[p.ID]; !exists {
			return errors.New("at most 64 profiles")
		}
	}
	if p.Kind != "connection" && p.Kind != "relay" {
		return errors.New("invalid profile kind")
	}
	if old, ok := c.profiles[p.ID]; ok && old.Kind != p.Kind {
		return errors.New("profile kind cannot change")
	}
	p.Config.Role = "client"
	p.Config.Target = ""
	p.Config.Inbound = ""
	p.Config.Listen = "127.0.0.1:0"
	if err := p.Config.Defaults(); err != nil {
		return err
	}
	if err := service.Validate(p.Config); err != nil {
		return err
	}
	if _, port, err := net.SplitHostPort(p.Config.Server); err != nil || port == "" {
		return errors.New("server must include host and port")
	}
	if p.Kind == "relay" {
		if p.RelayID != "" || len(p.Inlets) > 0 || p.Enabled {
			return errors.New("relay profiles have no listeners or enable switch")
		}
		return nil
	}
	if p.RelayID != "" {
		r, ok := c.profiles[p.RelayID]
		if !ok || r.Kind != "relay" {
			return errors.New("select an existing relay")
		}
	}
	if len(p.Inlets) > 8 {
		return errors.New("at most 8 listeners per connection")
	}
	if p.Enabled && len(p.Inlets) == 0 {
		return errors.New("enable at least one proxy listener")
	}
	for i, a := range p.Inlets {
		if a.Protocol != "socks" && a.Protocol != "http" && a.Protocol != "mixed" {
			return errors.New("invalid proxy protocol")
		}
		host, port, err := net.SplitHostPort(a.Listen)
		n, e := strconv.Atoi(port)
		if err != nil || net.ParseIP(host) == nil || e != nil || n < 1 || n > 65535 {
			return errors.New("listener requires a local IP address and port 1–65535")
		}
		for _, b := range p.Inlets[:i] {
			if overlap(a.Listen, b.Listen) {
				return fmt.Errorf("overlapping listeners: %s", a.Listen)
			}
		}
		for id, other := range c.profiles {
			if id == p.ID {
				continue
			}
			for _, b := range other.Inlets {
				if overlap(a.Listen, b.Listen) {
					return fmt.Errorf("port is assigned to %s: %s", other.Name, a.Listen)
				}
			}
		}
		// Probe new binds without disturbing a currently owned listener.
		owned := false
		if e := c.entries[p.ID]; e != nil && e.run != nil {
			for _, b := range e.run.Inlets() {
				if a.Listen == b.Listen {
					owned = true
				}
			}
		}
		if !owned {
			ln, err := net.Listen("tcp", a.Listen)
			if err != nil {
				return fmt.Errorf("listener %s: %w", a.Listen, err)
			}
			ln.Close()
		}
	}
	return nil
}
func overlap(a, b string) bool {
	ah, ap, _ := net.SplitHostPort(a)
	bh, bp, _ := net.SplitHostPort(b)
	if ap != bp {
		return false
	}
	aIP, bIP := net.ParseIP(ah), net.ParseIP(bh)
	return ah == bh || aIP != nil && bIP != nil && (aIP.Equal(bIP) || aIP.IsUnspecified() || bIP.IsUnspecified())
}
func (c *Catalog) stop(id string) {
	e := c.entry(id)
	if e.run != nil {
		e.run.Close()
		last := e.run.Snapshot()
		e.last = &last
		e.run = nil
	}
	e.active = ""
}
func (c *Catalog) apply(p Profile) error {
	if p.Kind == "relay" {
		for _, v := range c.profiles {
			if v.RelayID == p.ID && v.Enabled {
				if err := c.apply(v); err != nil {
					return fmt.Errorf("%s: %w", v.Name, err)
				}
			}
		}
		return nil
	}
	e := c.entry(p.ID)
	wanted := c.runtimeRevision(p)
	if e.run != nil && e.active == wanted && p.Enabled {
		if r, ok := c.profiles[p.RelayID]; ok {
			e.relayName = r.Name
		}
		return nil
	}
	if e.run != nil && p.Enabled && e.transport == c.transportRevision(p) {
		if err := e.run.SetInlets(p.Inlets); err != nil {
			e.err = err.Error()
			return err
		}
		e.active = wanted
		e.err = ""
		return nil
	}
	c.stop(p.ID)
	e.err = ""
	e.probe = nil
	if !p.Enabled {
		return nil
	}
	var relay *service.Config
	if p.RelayID != "" {
		r, ok := c.profiles[p.RelayID]
		if !ok {
			return errors.New("relay does not exist")
		}
		relay = &r.Config
	}
	run, err := service.OpenConnection(p.Config, p.Inlets, relay)
	if err != nil {
		e.err = err.Error()
		return err
	}
	e.run = run
	e.active = wanted
	e.transport = c.transportRevision(p)
	e.server = p.Config.Server
	e.relayID = p.RelayID
	e.relayName = ""
	e.relayServer = ""
	if r, ok := c.profiles[p.RelayID]; ok {
		e.relayName = r.Name
		e.relayServer = r.Config.Server
	}
	return nil
}
func (c *Catalog) Handle(q Request) Response {
	if q.Action == "connection_test" {
		return c.test(q)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if q.Version != Version {
		return Fail("unsupported_version", errors.New("expected control protocol version 1"))
	}
	if c.closed {
		return Fail("closed", errors.New("manager is closed"))
	}
	if strings.HasPrefix(q.Action, "connections_") {
		return c.handleSnapshot(q)
	}
	if q.Action == "connections" {
		return Response{Version: Version, Connections: c.list()}
	}
	p, exists := c.profiles[q.ID]
	if q.Action == "connection_save" {
		if q.Profile == nil {
			return Fail("invalid_config", errors.New("profile is required"))
		}
		q.ID = q.Profile.ID
		p, exists = c.profiles[q.ID]
	}
	mutating := q.Action == "connection_save" || q.Action == "connection_delete" || q.Action == "connection_start" || q.Action == "connection_stop"
	if mutating {
		rev := ""
		if exists {
			rev = revision(p)
		}
		if q.ExpectedRevision == nil || *q.ExpectedRevision != rev {
			return Fail("conflict", errors.New("configuration changed; reload before editing"))
		}
	}
	if !exists && q.Action != "connection_save" {
		return Fail("not_found", errors.New("connection not found"))
	}
	response := Response{Version: Version}
	switch q.Action {
	case "connection_get":
		p = cloneProfile(p)
		response.Profile = &p
	case "connection_export":
		return c.export(p)
	case "connection_save":
		next := cloneProfile(*q.Profile)
		if err := c.saveConnection(&next, q.Relay); err != nil {
			return Fail("invalid_config", err)
		}
		response.Revision = revision(next)
		if q.Apply {
			if err := c.apply(next); err != nil {
				response.Error = &Failure{"start_failed", err.Error()}
			}
		}
	case "connection_start", "connection_stop":
		if p.Kind != "connection" {
			return Fail("invalid_config", errors.New("relay starts with its connections"))
		}
		p.Enabled = q.Action == "connection_start"
		if p.Enabled {
			if err := c.validate(&p); err != nil {
				return Fail("invalid_config", err)
			}
		}
		if err := c.save(p); err != nil {
			return Fail("save_failed", err)
		}
		if err := c.apply(p); err != nil {
			response.Error = &Failure{"start_failed", err.Error()}
		}
	case "connection_delete":
		for _, v := range c.profiles {
			if v.RelayID == p.ID {
				return Fail("in_use", fmt.Errorf("relay is used by %s", v.Name))
			}
		}
		next := make(map[string]Profile, len(c.profiles))
		for id, v := range c.profiles {
			if id != p.ID {
				next[id] = v
			}
		}
		if err := c.persist(next); err != nil {
			return Fail("save_failed", err)
		}
		c.stop(p.ID)
		delete(c.entries, p.ID)

	default:
		return Fail("unknown_action", errors.New("unknown connection action"))
	}
	response.Connections = c.list()
	return response
}
func (c *Catalog) Autostart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.profiles {
		if p.Kind == "connection" && p.Enabled {
			if err := c.apply(p); err != nil {
				c.entry(p.ID).err = err.Error()
			}
		}
	}
}
func (c *Catalog) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id := range c.entries {
		c.stop(id)
	}
}

// MigrateConnection imports the old single-client config once, without starting it.
func (m *Manager) MigrateConnection() error {
	c := m.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := os.Stat(c.path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if m.cfg == nil {
		return nil
	}
	cfg := *m.cfg
	if cfg.Role != "client" || cfg.Target != "" {
		return errors.New("connections mode requires a client configuration")
	}
	protocol := cfg.Inbound
	if protocol == "" {
		protocol = "socks"
	}
	p := Profile{ID: "default", Name: cfg.Server, Kind: "connection", Enabled: true, Config: cfg, Inlets: []service.Inlet{{Protocol: protocol, Listen: cfg.Listen}}}
	return c.save(p)
}

// Network probes run outside the catalog lock so a slow route cannot block
// another connection's configuration, status or lifecycle operations.
func (c *Catalog) test(q Request) Response {
	c.mu.Lock()
	if q.Version != Version || c.closed {
		c.mu.Unlock()
		return Fail("unavailable", errors.New("invalid version or closed service"))
	}
	p, exists := c.profiles[q.ID]
	if !exists {
		c.mu.Unlock()
		return Fail("not_found", errors.New("connection not found"))
	}
	if p.Kind != "connection" {
		c.mu.Unlock()
		return Fail("invalid_config", errors.New("test a complete connection that uses this relay"))
	}
	entry := c.entry(p.ID)
	if entry.testing {
		c.mu.Unlock()
		return Fail("busy", errors.New("a test is already running for this connection"))
	}
	entry.testing = true
	revision := c.runtimeRevision(p)
	run := entry.run
	temporary := run == nil || entry.active != revision
	var relay *service.Config
	if p.RelayID != "" {
		r := c.profiles[p.RelayID]
		relay = &r.Config
	}
	c.mu.Unlock()
	started := time.Now()
	result := &ProbeResult{At: started, Target: "https://www.google.com/generate_204"}
	var err error
	if temporary {
		run, err = service.OpenConnection(p.Config, nil, relay)
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err = run.ProbeHTTP(ctx, result.Target)
		cancel()
		if temporary {
			run.Close()
		}
	}
	result.Milliseconds = time.Since(started).Milliseconds()
	result.OK = err == nil
	if err != nil {
		result.Error = err.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.testing = false
	if current, ok := c.profiles[p.ID]; ok && !c.closed && c.runtimeRevision(current) == revision && c.entries[p.ID] == entry {
		saved := *result
		entry.probe = &saved
	}
	return Response{Version: Version, Probe: result, Connections: c.list()}
}
