package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"time"
	"veil/core"
)

func httpFallback(cfg Config) (*core.Fallback, error) {
	if cfg.HTTPFallback == nil {
		return nil, nil
	}
	settings := *cfg.HTTPFallback
	var backendTLS *tls.Config
	if settings.HTTPS != nil {
		backendTLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: settings.HTTPS.ServerName}
		if settings.HTTPS.CAFile != "" {
			pem, err := os.ReadFile(settings.HTTPS.CAFile)
			if err != nil {
				return nil, fmt.Errorf("HTTPS fallback CA: %w", err)
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("HTTPS fallback roots: %w", err)
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("HTTPS fallback CA: no certificates in PEM")
			}
			backendTLS.RootCAs = roots
		}
	}
	protocols := []string{"http/1.1"}
	if settings.H2C != "" || settings.HTTPS != nil && settings.HTTPS.HTTP2 {
		protocols = []string{"h2", "http/1.1"}
	}
	dialer := &net.Dialer{Timeout: sec(cfg.DialSeconds)}
	return &core.Fallback{Protocols: protocols, Handler: func(ctx context.Context, c net.Conn, protocol string) error {
		// The destination is configuration, never HTTP Host, URL or CONNECT.
		if backendTLS != nil {
			if protocol == "" {
				protocol = "http/1.1"
			}
			conf := backendTLS.Clone()
			conf.NextProtos = []string{protocol}
			target, err := (&tls.Dialer{NetDialer: dialer, Config: conf}).DialContext(ctx, "tcp", settings.HTTPS.Address)
			if err != nil {
				return fmt.Errorf("HTTPS fallback dial: %w", err)
			}
			negotiated := target.(*tls.Conn).ConnectionState().NegotiatedProtocol
			// HTTPS without ALPN is HTTP/1. HTTP/2 must be explicitly negotiated.
			if negotiated != protocol && !(protocol == "http/1.1" && negotiated == "") {
				target.Close()
				return fmt.Errorf("HTTPS fallback ALPN: expected %q, got %q", protocol, negotiated)
			}
			return core.RelayTCP(ctx, c, target, min(sec(cfg.IdleSeconds), 2*time.Minute))
		}
		address := settings.HTTP1
		if protocol == "h2" {
			address = settings.H2C
		}
		target, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("HTTP fallback dial: %w", err)
		}
		return core.RelayTCP(ctx, c, target, min(sec(cfg.IdleSeconds), 2*time.Minute))
	}}, nil
}
