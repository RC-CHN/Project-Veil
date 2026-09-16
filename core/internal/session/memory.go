package session

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
	b "veil.local/core/internal/behavior"
)

// MemoryInputs is internal migration glue; public APIs expose owned model and
// identity value objects rather than legacy file paths or internal types.
type MemoryInputs struct {
	Model       []byte
	Certificate tls.Certificate
	Roots       *x509.CertPool
}

func decodeModel(raw []byte) ([32]byte, *b.BatchProgram, *flightProgram, error) {
	var seed [32]byte
	if len(raw) == 0 || len(raw) > 256<<10 {
		return seed, nil, nil, errors.New("model size limit")
	}
	var bundle FlightBundle
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&bundle); e != nil {
		return seed, nil, nil, e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return seed, nil, nil, errors.New("model trailing JSON")
	}
	if bundle.Version != 5 || bundle.InnerProtocol != RenewingFlightInnerProtocol || bundle.Model.Version != 4 {
		return seed, nil, nil, errors.New("core requires Config5/FlightModel4/inner-v3")
	}
	rawSeed, e := hex.DecodeString(bundle.SeedHex)
	if e != nil || len(rawSeed) != 32 {
		return seed, nil, nil, errors.New("model seed")
	}
	copy(seed[:], rawSeed)
	p, e := compileFlight(bundle.Model)
	if e != nil {
		return seed, nil, nil, e
	}
	return seed, p.child, p, nil
}
func CheckModelBytes(raw []byte) (string, error) {
	_, _, p, e := decodeModel(raw)
	if e != nil {
		return "", e
	}
	return p.id, nil
}
func GenerateModelBytes(instances int) ([]byte, error) {
	b, e := GenerateRenewingFlightBundle(instances)
	if e != nil {
		return nil, e
	}
	return json.MarshalIndent(b, "", "  ")
}
func memoryInputs(i *MemoryInputs, role string) ([32]byte, *b.BatchProgram, *flightProgram, tls.Certificate, *x509.CertPool, error) {
	seed, p, f, e := decodeModel(i.Model)
	if e != nil {
		return seed, nil, nil, tls.Certificate{}, nil, e
	}
	cert := i.Certificate
	if len(cert.Certificate) == 0 {
		return seed, nil, nil, cert, nil, errors.New("missing identity")
	}
	cert.Leaf, e = x509.ParseCertificate(cert.Certificate[0])
	if e == nil {
		e = certificateUsage(cert.Leaf, role)
	}
	if e == nil && (time.Now().Before(cert.Leaf.NotBefore) || !time.Now().Before(cert.Leaf.NotAfter)) {
		e = errors.New("identity validity")
	}
	return seed, p, f, cert, i.Roots, e
}
func clientInputs(cfg ClientConfig, i *MemoryInputs) ([32]byte, *b.BatchProgram, *flightProgram, tls.Certificate, *x509.CertPool, error) {
	if i != nil {
		return memoryInputs(i, "client")
	}
	seed, p, f, e := loadRuntimeModel(cfg.ModelFile, cfg.Version)
	if e != nil {
		return seed, p, f, tls.Certificate{}, nil, e
	}
	cert, e := certificate(cfg.Certificate, cfg.PrivateKey)
	if e != nil {
		return seed, p, f, cert, nil, e
	}
	ca, e := roots(cfg.CA)
	return seed, p, f, cert, ca, e
}
func serverInputs(cfg ServerConfig, i *MemoryInputs) ([32]byte, *b.BatchProgram, *flightProgram, tls.Certificate, *x509.CertPool, error) {
	if i != nil {
		return memoryInputs(i, "server")
	}
	seed, p, f, e := loadRuntimeModel(cfg.ModelFile, cfg.Version)
	if e != nil {
		return seed, p, f, tls.Certificate{}, nil, e
	}
	if f != nil && cfg.MaxSessions*f.instances*2 > 64 {
		return seed, p, f, tls.Certificate{}, nil, errors.New("flight object capacity exceeds 64")
	}
	cert, e := certificate(cfg.Certificate, cfg.PrivateKey)
	if e != nil {
		return seed, p, f, cert, nil, e
	}
	ca, e := roots(cfg.ClientCA)
	return seed, p, f, cert, ca, e
}
func NewMemoryClient(cfg ClientConfig, i MemoryInputs, o Observer) (*Client, error) {
	if cfg.Version != 5 {
		return nil, errors.New("memory client requires Config5")
	}
	return newClient(cfg, o, &i)
}
func NewMemoryServer(cfg ServerConfig, i MemoryInputs, o Observer) (*Server, error) {
	if cfg.Version != 5 || cfg.BackendMode != "local-object-v1" || i.Roots == nil {
		return nil, errors.New("memory server requires Config5, local objects and explicit client trust")
	}
	cfg.ClientCA = "in-memory"
	return newServer(cfg, o, &i)
}
