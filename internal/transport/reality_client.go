// SPDX-License-Identifier: GPL-3.0-or-later
// REALITY client handshake adapted from SagerNet/sing-box v1.15.0-alpha.9,
// common/tls/reality_client.go, Copyright (C) 2022 nekohasekai.
// Veil uses public certificate parsing, rejects non-REALITY peers, and disables
// TLS renegotiation to permit channel-bound TLS 1.3 exporter authentication.
package transport

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	utls "github.com/metacubex/utls"
	"golang.org/x/crypto/hkdf"
)

func realityClient(s Settings) (Handshake, error) {
	public, err := DecodeKey(s.RealityPublicKey)
	if err != nil {
		return nil, err
	}
	id, err := shortID(s.ShortID)
	if err != nil {
		return nil, err
	}
	if s.Fingerprint != "" && s.Fingerprint != "chrome" {
		return nil, errors.New("v0 supports the pinned chrome REALITY fingerprint")
	}
	publicKey, err := ecdh.X25519().NewPublicKey(public)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, raw net.Conn) (net.Conn, error) {
		var err error
		var authKey []byte
		verified := false
		cfg := &utls.Config{ServerName: s.ServerName, MinVersion: utls.VersionTLS13, MaxVersion: utls.VersionTLS13, SessionTicketsDisabled: true, InsecureSkipVerify: true}
		// InsecureSkipVerify is paired with mandatory REALITY HMAC authentication.
		// A normal trusted website certificate is insufficient for this transport.
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 || len(authKey) != 32 {
				return errors.New("REALITY authentication failed")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			pub, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok {
				return errors.New("REALITY authentication failed")
			}
			h := hmac.New(sha512.New, authKey)
			h.Write(pub)
			if !hmac.Equal(h.Sum(nil), cert.Signature) {
				return errors.New("REALITY authentication failed")
			}
			verified = true
			return nil
		}
		c := utls.UClient(raw, cfg, utls.HelloChrome_Auto)
		if err := c.BuildHandshakeState(); err != nil {
			return nil, err
		}
		for _, ext := range c.Extensions {
			switch e := ext.(type) {
			case *utls.SupportedCurvesExtension:
				out := e.Curves[:0]
				for _, v := range e.Curves {
					if v != utls.X25519MLKEM768 {
						out = append(out, v)
					}
				}
				e.Curves = out
			case *utls.KeyShareExtension:
				out := e.KeyShares[:0]
				for _, v := range e.KeyShares {
					if v.Group != utls.X25519MLKEM768 {
						out = append(out, v)
					}
				}
				e.KeyShares = out
			case *utls.RenegotiationInfoExtension:
				e.Renegotiation = utls.RenegotiateNever
			}
		}
		if err := c.BuildHandshakeState(); err != nil {
			return nil, err
		}
		cfg.Renegotiation = utls.RenegotiateNever
		hello := c.HandshakeState.Hello
		if len(hello.Raw) < 71 || len(hello.Random) != 32 {
			return nil, errors.New("unsupported REALITY ClientHello layout")
		}
		hello.SessionId = make([]byte, 32)
		copy(hello.Raw[39:71], hello.SessionId)
		hello.SessionId[0], hello.SessionId[1], hello.SessionId[2] = 1, 8, 1
		binary.BigEndian.PutUint32(hello.SessionId[4:8], uint32(time.Now().Unix()))
		copy(hello.SessionId[8:16], id[:])
		ks := c.HandshakeState.State13.KeyShareKeys
		if ks == nil || ks.Ecdhe == nil {
			return nil, errors.New("REALITY requires X25519 key share")
		}
		authKey, err = ks.Ecdhe.ECDH(publicKey)
		if err != nil {
			return nil, err
		}
		if _, err = io.ReadFull(hkdf.New(sha256.New, authKey, hello.Random[:20], []byte("REALITY")), authKey); err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(authKey)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		gcm.Seal(hello.SessionId[:0], hello.Random[20:], hello.SessionId[:16], hello.Raw)
		copy(hello.Raw[39:71], hello.SessionId)
		if err = c.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, err
		}
		if !verified {
			c.Close()
			return nil, errors.New("REALITY authentication failed")
		}
		return c, nil
	}, nil
}
