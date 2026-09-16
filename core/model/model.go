// Package model owns validated model bundles. Bytes contain the private seed;
// ID is safe for configuration binding, but does not identify that seed.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"veil.local/core/internal/session"
)

type Bundle struct {
	raw []byte
	id  string
}

func Parse(raw []byte) (Bundle, error) {
	id, e := session.CheckModelBytes(raw)
	if e != nil {
		return Bundle{}, e
	}
	return Bundle{append([]byte(nil), raw...), id}, nil
}
func Generate(lanes int) (Bundle, error) {
	raw, e := session.GenerateModelBytes(lanes)
	if e != nil {
		return Bundle{}, e
	}
	return Parse(raw)
}
func (b Bundle) ID() string     { return b.id }
func (b Bundle) Bytes() []byte  { return append([]byte(nil), b.raw...) }
func (b Bundle) SHA256() string { h := sha256.Sum256(b.raw); return hex.EncodeToString(h[:]) }
func (b Bundle) Validate() error {
	if len(b.raw) == 0 {
		return errors.New("missing model")
	}
	return nil
}
