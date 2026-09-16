// Package config owns on-disk application configuration and material loading.
package config

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
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
	return core.NewClient(core.ClientOptions{ServerURL: c.ServerURL, DialAddress: c.DialAddress, Bucket: c.Bucket, Model: l.Model, Identity: l.Identity, Roots: l.Roots, MaxConnections: c.MaxConnections})
}
func (l Loaded) Server() (*core.Server, error) {
	c := l.Config
	return core.NewServer(core.ServerOptions{Bucket: c.Bucket, Model: l.Model, Identity: l.Identity, ClientRoots: l.Roots, ClientFingerprints: c.ClientFingerprints, AllowCIDRs: c.AllowCIDRs, DenyCIDRs: c.DenyCIDRs, DNSAddress: c.DNSAddress, MaxConnections: c.MaxConnections})
}
