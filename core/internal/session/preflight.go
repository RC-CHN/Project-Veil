package session

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"
)

// ConfigReport intentionally contains no seed, private key, backend credential,
// certificate body or filesystem path. It describes validated local inputs;
// it does not claim that a remote endpoint is reachable or trusts this identity.
type ConfigReport struct {
	Role                                                              string
	Version                                                           int
	ModelID, InnerProtocol, Listen, ServerName                        string
	CertificateSHA256                                                 string
	CertificateNotAfter                                               time.Time
	ConnectionLimit, CarrierLimit, StreamsPerCarrier, WindowPerStream int
	ReservedWindowPerCarrier                                          int
	MaxBytesPerStream, MaxBytesPerCarrier                             uint64
	BackendMode                                                       string `json:",omitempty"`
}

// CheckConfig uses the same constructors and defaults as the actual runtime.
// Constructors perform local setup only; Run (which opens listeners, connects
// to a backend and starts workers) is deliberately never invoked.
func CheckConfig(role, path string) (ConfigReport, error) {
	var report ConfigReport
	var cert tls.Certificate
	switch role {
	case "server":
		var cfg ServerConfig
		if err := ReadPrivateJSON(path, &cfg, 64<<10); err != nil {
			return report, err
		}
		server, err := NewServer(cfg, nil)
		if err != nil {
			return report, err
		}
		defer server.store.close()
		cfg = server.cfg
		cert = server.tls.Certificates[0]
		report = ConfigReport{Role: role, Version: cfg.Version, ModelID: server.modelID(), Listen: cfg.Listen, ConnectionLimit: cfg.MaxConnections, CarrierLimit: cfg.MaxSessions, WindowPerStream: cfg.Window, MaxBytesPerStream: cfg.MaxBytes, BackendMode: cfg.BackendMode}
		if cfg.Version >= 2 {
			report.InnerProtocol = runtimeInner(cfg.Version)
			report.StreamsPerCarrier = cfg.Mux.Streams
			report.MaxBytesPerCarrier = cfg.Mux.ConnectionBytes
		}
	case "client":
		var cfg ClientConfig
		if err := ReadPrivateJSON(path, &cfg, 64<<10); err != nil {
			return report, err
		}
		client, err := NewClient(cfg, nil)
		if err != nil {
			return report, err
		}
		cfg = client.cfg
		cert = client.tls.Certificates[0]
		report = ConfigReport{Role: role, Version: cfg.Version, ModelID: client.modelID(), Listen: cfg.Listen, ServerName: client.tls.ServerName, ConnectionLimit: cfg.MaxConnections, CarrierLimit: cfg.MaxCarriers, WindowPerStream: cfg.Window, MaxBytesPerStream: cfg.MaxBytes}
		if cfg.Version >= 2 {
			report.InnerProtocol = runtimeInner(cfg.Version)
			report.StreamsPerCarrier = cfg.Mux.Streams
			report.MaxBytesPerCarrier = cfg.Mux.ConnectionBytes
		}
	default:
		return report, errors.New("configuration role must be client or server")
	}
	if err := certificateUsage(cert.Leaf, role); err != nil {
		return ConfigReport{}, err
	}
	if report.StreamsPerCarrier == 0 {
		report.StreamsPerCarrier = 1
	}
	report.ReservedWindowPerCarrier = report.StreamsPerCarrier * report.WindowPerStream
	report.CertificateSHA256 = hashHex(cert.Leaf.Raw)
	report.CertificateNotAfter = cert.Leaf.NotAfter.UTC()
	return report, nil
}

func certificateUsage(cert *x509.Certificate, role string) error {
	if cert.IsCA {
		return errors.New("identity certificate must not be a CA")
	}
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return errors.New("TLS identity does not permit digital signatures")
	}
	usage := x509.ExtKeyUsageServerAuth
	if role == "client" {
		usage = x509.ExtKeyUsageClientAuth
	}
	if len(cert.ExtKeyUsage) == 0 && len(cert.UnknownExtKeyUsage) == 0 {
		return nil
	}
	for _, v := range cert.ExtKeyUsage {
		if v == usage || v == x509.ExtKeyUsageAny {
			return nil
		}
	}
	return errors.New("certificate does not permit requested TLS role")
}

type CertificateReport struct {
	Role, Name, SHA256                 string
	NotBefore, NotAfter, VerifiedUntil time.Time
	VerifiedChains                     int
}

// CheckCertificate verifies a complete supplied identity for installation.
// An empty CA path uses the system root store, just as a TLS client would.
// No network fetch (including AIA/OCSP) or renewal is performed here.
func CheckCertificate(certPath, keyPath, caPath, role, name string, minValidity time.Duration) (CertificateReport, error) {
	var report CertificateReport
	if role != "server" && role != "client" {
		return report, errors.New("certificate role must be client or server")
	}
	if role == "server" && name == "" || role == "client" && name != "" {
		return report, errors.New("server certificate requires a name; client certificate does not accept a name")
	}
	if minValidity < 0 || minValidity > 365*24*time.Hour {
		return report, errors.New("minimum validity must be between zero and 365 days")
	}
	cert, err := certificate(certPath, keyPath)
	if err != nil {
		return report, err
	}
	if err = certificateUsage(cert.Leaf, role); err != nil {
		return report, err
	}
	pool, err := roots(caPath)
	if err != nil {
		return report, err
	}
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return report, err
		}
		intermediates.AddCert(c)
	}
	usage := x509.ExtKeyUsageServerAuth
	if role == "client" {
		usage = x509.ExtKeyUsageClientAuth
	}
	now := time.Now()
	chains, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, DNSName: name, KeyUsages: []x509.ExtKeyUsage{usage}, CurrentTime: now})
	if err != nil {
		return report, err
	}
	// A cross-signed identity may have multiple chains. At least one complete
	// verified chain must remain valid for the requested installation horizon.
	var until time.Time
	for _, chain := range chains {
		end := cert.Leaf.NotAfter
		for _, c := range chain {
			if c.NotAfter.Before(end) {
				end = c.NotAfter
			}
		}
		if end.After(until) {
			until = end
		}
	}
	if !now.Add(minValidity).Before(until) {
		return report, errors.New("verified certificate chain expires before required validity horizon")
	}
	return CertificateReport{Role: role, Name: name, SHA256: hashHex(cert.Leaf.Raw), NotBefore: cert.Leaf.NotBefore.UTC(), NotAfter: cert.Leaf.NotAfter.UTC(), VerifiedUntil: until.UTC(), VerifiedChains: len(chains)}, nil
}
