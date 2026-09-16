// Package config owns on-disk application configuration and material loading.
package config

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"time"
	core "veil.local/core"
	"veil.local/core/identity"
	"veil.local/core/model"
)

type FileReader interface {
	Read(path string, limit int64, private bool) ([]byte, error)
}

// Version is the node configuration version; the current core wire remains v5.
type Config struct {
	Version            int      `json:"version"`
	Role               string   `json:"role"`
	Listen             string   `json:"listen"`
	ServerURL          string   `json:"server_url,omitempty"`
	DialAddress        string   `json:"dial_address,omitempty"`
	Bucket             string   `json:"bucket"`
	ModelFile          string   `json:"model_file"`
	Certificate        string   `json:"certificate"`
	PrivateKey         string   `json:"private_key"`
	CA                 string   `json:"ca,omitempty"`
	ClientFingerprints []string `json:"client_fingerprints,omitempty"`
	AllowCIDRs         []string `json:"allow_cidrs,omitempty"`
	DenyCIDRs          []string `json:"deny_cidrs,omitempty"`
	DNSAddress         string   `json:"dns_address,omitempty"`
	MaxConnections     int      `json:"max_connections,omitempty"`
	Limits             Limits   `json:"limits,omitempty"`
}

// Limits are lifetime byte caps, not queue allocations. Zero preserves the
// engineering1 defaults. The peers negotiate minima; neither side can enlarge
// its peer's allowance. Windows and Linux use the same configuration contract.
type Limits struct {
	StreamBytes  uint64 `json:"stream_bytes,omitempty"`
	CarrierBytes uint64 `json:"carrier_bytes,omitempty"`
}

func (l Limits) Effective() Limits {
	if l.StreamBytes == 0 {
		l.StreamBytes = 8 << 20
	}
	if l.CarrierBytes == 0 {
		l.CarrierBytes = 64 << 20
	}
	return l
}

func (l Limits) Validate() error {
	l = l.Effective()
	if l.StreamBytes > 1<<40 || l.CarrierBytes < 4096 || l.CarrierBytes > 1<<40 {
		return errors.New("limits: stream_bytes must be 1..1 TiB and carrier_bytes 4096..1 TiB (zero selects defaults)")
	}
	return nil
}

// Report describes local validation only. Configcheck never dials a peer and
// cannot establish negotiated limits or end-to-end health.
type Report struct {
	Kind              string    `json:"kind"`
	Version           string    `json:"version"`
	ConfigVersion     int       `json:"config_version"`
	Role              string    `json:"role"`
	Listen            string    `json:"listen"`
	LocalValidation   string    `json:"local_validation"`
	EndToEnd          string    `json:"end_to_end"`
	ModelID           string    `json:"model_id"`
	ModelSHA256       string    `json:"model_sha256"`
	CertificateSHA256 string    `json:"certificate_sha256"`
	CertificateExpiry time.Time `json:"certificate_not_after"`
	MaxConnections    int       `json:"max_connections"`
	Limits            Limits    `json:"limits"`
}

func (l Loaded) Report() Report {
	return Report{Kind: "config_check", Version: core.Version, ConfigVersion: l.Config.Version,
		Role: l.Config.Role, Listen: l.Config.Listen, LocalValidation: "passed", EndToEnd: "not_checked",
		ModelID: l.Model.ID(), ModelSHA256: l.Model.SHA256(), CertificateSHA256: l.Identity.Fingerprint(),
		CertificateExpiry: l.Identity.NotAfter(), MaxConnections: l.Config.MaxConnections, Limits: l.Config.Limits.Effective()}
}

type Loaded struct {
	Config   Config
	Model    model.Bundle
	Identity identity.Identity
	Roots    *x509.CertPool
}

func Load(path string, reader FileReader) (Loaded, error) {
	var out Loaded
	raw, e := reader.Read(path, 64<<10, true)
	if e != nil {
		return out, e
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&out.Config); e != nil {
		return out, e
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return out, errors.New("trailing configuration JSON")
	}
	c := &out.Config
	if c.Version != 1 || (c.Role != "client" && c.Role != "server") || c.Listen == "" || c.ModelFile == "" || c.Certificate == "" || c.PrivateKey == "" {
		return out, errors.New("node configuration version, role or required fields")
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = 8
	}
	if c.MaxConnections < 1 || c.MaxConnections > 32 {
		return out, errors.New("node connection limit")
	}
	if e = c.Limits.Validate(); e != nil {
		return out, e
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(filepath.Dir(path), p)
	}
	raw, e = reader.Read(resolve(c.ModelFile), 256<<10, true)
	if e != nil {
		return out, e
	}
	out.Model, e = model.Parse(raw)
	clear(raw)
	if e != nil {
		return out, e
	}
	cert, e := reader.Read(resolve(c.Certificate), 1<<20, false)
	if e != nil {
		return out, e
	}
	key, e := reader.Read(resolve(c.PrivateKey), 64<<10, true)
	if e != nil {
		return out, e
	}
	defer clear(key)
	role := identity.Client
	if c.Role == "server" {
		role = identity.Server
	}
	out.Identity, e = identity.Parse(cert, key, role)
	if e != nil {
		return out, e
	}
	if c.CA != "" {
		raw, e = reader.Read(resolve(c.CA), 1<<20, false)
		if e != nil {
			return out, e
		}
		out.Roots = x509.NewCertPool()
		if !out.Roots.AppendCertsFromPEM(raw) {
			return out, errors.New("invalid CA")
		}
	}
	if c.Role == "server" && out.Roots == nil {
		return out, errors.New("server requires explicit client CA")
	}
	return out, nil
}
func (l Loaded) Client() (*core.Client, error) {
	c := l.Config
	limits := c.Limits.Effective()
	return core.NewClient(core.ClientOptions{ServerURL: c.ServerURL, DialAddress: c.DialAddress, Bucket: c.Bucket, Model: l.Model, Identity: l.Identity, Roots: l.Roots, MaxConnections: c.MaxConnections, MaxBytes: limits.StreamBytes, Mux: core.MuxLimits{ConnectionBytes: limits.CarrierBytes}})
}
func (l Loaded) Server() (*core.Server, error) {
	c := l.Config
	limits := c.Limits.Effective()
	return core.NewServer(core.ServerOptions{Bucket: c.Bucket, Model: l.Model, Identity: l.Identity, ClientRoots: l.Roots, ClientFingerprints: c.ClientFingerprints, AllowCIDRs: c.AllowCIDRs, DenyCIDRs: c.DenyCIDRs, DNSAddress: c.DNSAddress, MaxConnections: c.MaxConnections, MaxBytes: limits.StreamBytes, Mux: core.MuxLimits{ConnectionBytes: limits.CarrierBytes}})
}
