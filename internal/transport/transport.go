// Package transport contains the TLS boundary. It does not implement new crypto.
package transport

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	utls "github.com/metacubex/utls"
)

type Settings struct {
	Mode              string   `json:"mode"`
	ServerName        string   `json:"server_name"`
	Certificate       string   `json:"certificate"`
	PrivateKeyFile    string   `json:"private_key_file"`
	CAFile            string   `json:"ca_file"`
	RealityPrivateKey string   `json:"reality_private_key"`
	RealityPublicKey  string   `json:"reality_public_key"`
	ShortID           string   `json:"short_id"`
	CoverAddress      string   `json:"cover_address"`
	Fingerprint       string   `json:"fingerprint"`
	Fingerprints      []string `json:"fingerprints"`
	RecordPadding     bool     `json:"record_padding"`
}

type Handshake func(context.Context, net.Conn) (net.Conn, error)

func Server(s Settings, timeout, idle time.Duration) (Handshake, error) {
	if s.ServerName == "" {
		return nil, errors.New("transport: server_name required")
	}
	switch s.Mode {
	case "tls":
		cert, err := utls.LoadX509KeyPair(s.Certificate, s.PrivateKeyFile)
		if err != nil {
			return nil, err
		}
		cfg := &utls.Config{MinVersion: utls.VersionTLS13, MaxVersion: utls.VersionTLS13, Certificates: []utls.Certificate{cert}, NextProtos: []string{"http/1.1"}, SessionTicketsDisabled: true}
		return func(ctx context.Context, raw net.Conn) (net.Conn, error) {
			c := utls.Server(raw, cfg)
			err := c.HandshakeContext(ctx)
			if err != nil {
				c.Close()
				return nil, err
			}
			return c, nil
		}, nil
	case "reality":
		key, err := DecodeKey(s.RealityPrivateKey)
		if err != nil {
			return nil, err
		}
		id, err := shortID(s.ShortID)
		if err != nil {
			return nil, err
		}
		if _, _, err = net.SplitHostPort(s.CoverAddress); err != nil {
			return nil, fmt.Errorf("cover_address: %w", err)
		}
		d := net.Dialer{Timeout: timeout}
		cfg := &utls.RealityConfig{Type: "tcp", Dest: s.CoverAddress, DialContext: func(ctx context.Context, n, a string) (net.Conn, error) {
			c, err := d.DialContext(ctx, n, a)
			if err == nil {
				c.SetDeadline(time.Now().Add(timeout))
			}
			return c, err
		}, ServerNames: map[string]bool{s.ServerName: true}, PrivateKey: key, ShortIds: map[[8]byte]bool{id: true}, MaxTimeDiff: time.Minute}
		cfg.MinVersion = utls.VersionTLS13
		cfg.MaxVersion = utls.VersionTLS13
		cfg.SessionTicketsDisabled = true
		return func(ctx context.Context, c net.Conn) (net.Conn, error) {
			f := fallback{handshake: ctx, idle: idle}
			return utls.RealityServerWithFallback(ctx, c, cfg, f.copy)
		}, nil
	default:
		return nil, errors.New("transport: mode must be tls or reality")
	}
}
func Client(s Settings) (Handshake, error) {
	if s.ServerName == "" {
		return nil, errors.New("transport: server_name required")
	}
	switch s.Mode {
	case "tls":
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if s.CAFile != "" {
			pem, err := os.ReadFile(s.CAFile)
			if err != nil {
				return nil, err
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("transport: CA file contains no certificates")
			}
		}
		cfg := &utls.Config{MinVersion: utls.VersionTLS13, MaxVersion: utls.VersionTLS13, ServerName: s.ServerName, RootCAs: roots, NextProtos: []string{"http/1.1"}, SessionTicketsDisabled: true}
		return func(ctx context.Context, raw net.Conn) (net.Conn, error) {
			c := utls.Client(raw, cfg)
			err := c.HandshakeContext(ctx)
			if err != nil {
				c.Close()
				return nil, err
			}
			return c, nil
		}, nil
	case "reality":
		return realityClient(s)
	default:
		return nil, errors.New("transport: mode must be tls or reality")
	}
}
func shortID(s string) ([8]byte, error) {
	var out [8]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 8 {
		return out, errors.New("transport: short_id must be 16 hex characters")
	}
	copy(out[:], b)
	return out, nil
}
func DecodeKey(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("expected a 32-byte unpadded base64url key")
	}
	return b, nil
}

// Export binds application authentication to this completed TLS handshake.
func Export(c net.Conn) ([]byte, error) {
	tlsConn, ok := c.(interface{ ConnectionState() utls.ConnectionState })
	if !ok {
		return nil, fmt.Errorf("transport: no TLS exporter on %T", c)
	}
	st := tlsConn.ConnectionState()
	if !st.HandshakeComplete || st.Version != utls.VersionTLS13 {
		return nil, errors.New("transport: completed TLS 1.3 handshake required")
	}
	return st.ExportKeyingMaterial("EXPORTER-Veil-v0.1", nil, 32)
}

// ReleaseBuffers is a no-op with the original TLS library. The patched method
// releases only empty buffers and is invoked after transfer goroutines join.
func ReleaseBuffers(c net.Conn) {
	if x, ok := c.(interface{ VeilReleaseBuffers() }); ok {
		x.VeilReleaseBuffers()
	}
}

// RecordPadding resets only the local sender's budget at a stream boundary.
// TLS 1.3 peers already strip these zero bytes, so no wire negotiation is needed.
func RecordPadding(c net.Conn, enabled bool) error {
	if x, ok := c.(interface{ VeilSetPadding(bool) }); ok {
		x.VeilSetPadding(enabled)
		return nil
	}
	if enabled {
		return errors.New("transport: record padding requires the patched TLS library")
	}
	return nil
}
