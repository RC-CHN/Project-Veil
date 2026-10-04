// SPDX-License-Identifier: GPL-3.0-or-later
// REALITY client handshake adapted from SagerNet/sing-box v1.15.0-alpha.9,
// common/tls/reality_client.go, Copyright (C) 2022 nekohasekai.
// Veil uses parsed certificates and never admits non-REALITY peers to the core.
// TLS renegotiation remains disabled for channel-bound exporter authentication.
package transport

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
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
	profiles, err := realityProfiles(s)
	if err != nil {
		return nil, err
	}
	publicKey, err := ecdh.X25519().NewPublicKey(public)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, raw net.Conn) (net.Conn, error) {
		var err error
		var authKey []byte
		verified := false
		cfg := &utls.Config{ServerName: s.ServerName, MinVersion: utls.VersionTLS13, MaxVersion: utls.VersionTLS13, SessionTicketsDisabled: true, DynamicRecordSizingDisabled: true, InsecureSkipVerify: true}
		// A REALITY proof permits the core transport. A separately verified
		// trusted HTTPS certificate permits only the bounded cover request.
		cfg.VerifyConnection = func(state utls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 || len(authKey) != 32 {
				return errors.New("REALITY authentication failed")
			}
			cert := state.PeerCertificates[0]
			if verifyRealityCertificate(cert, authKey) {
				verified = true
				return nil
			}
			return verifyCoverCertificate(s, state.PeerCertificates)
		}
		// Fix the profile before ApplyPreset generates key shares. Specs contain
		// mutable slices and extension state, so each connection gets its own.
		profile := profiles[0]
		if len(profiles) > 1 {
			// Template selection is not cryptographic; TLS randomness and keys
			// still come from the TLS library's cryptographic random source.
			profile = profiles[rand.IntN(len(profiles))]
		}
		spec, err := realitySpec(profile)
		if err != nil {
			return nil, err
		}
		c := utls.UClient(raw, cfg, utls.HelloCustom)
		if err := c.ApplyPreset(&spec); err != nil {
			return nil, err
		}
		if err := c.BuildHandshakeState(); err != nil {
			return nil, err
		}
		cfg.Renegotiation = utls.RenegotiateNever
		hello := c.HandshakeState.Hello
		if len(hello.Raw) < 71 || len(hello.Random) != 32 || len(hello.SessionId) != 32 || hello.Raw[38] != 32 {
			return nil, errors.New("unsupported REALITY ClientHello layout")
		}
		hello.SessionId = make([]byte, 32)
		copy(hello.Raw[39:71], hello.SessionId)
		hello.SessionId[0], hello.SessionId[1], hello.SessionId[2] = 1, 8, 1
		binary.BigEndian.PutUint32(hello.SessionId[4:8], uint32(time.Now().Unix()))
		copy(hello.SessionId[8:16], id[:])
		ks := c.HandshakeState.State13.KeyShareKeys
		if ks == nil || ks.Ecdhe == nil || ks.Ecdhe.Curve() != ecdh.X25519() {
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
			browseCover(ctx, c, s.ServerName, profile.Version)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("REALITY authentication failed")
		}
		return c, nil
	}, nil
}

func verifyRealityCertificate(cert *x509.Certificate, authKey []byte) bool {
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() || len(authKey) != 32 || len(cert.SubjectKeyId) != 32 {
		return false
	}
	for _, ext := range cert.Extensions {
		if ext.Id.Equal([]int{2, 5, 29, 14}) && ext.Critical {
			return false
		}
	}
	h := hmac.New(sha256.New, authKey)
	h.Write([]byte("Veil-v0.3 server authentication\x00"))
	h.Write(cert.RawSubjectPublicKeyInfo)
	return hmac.Equal(h.Sum(nil), cert.SubjectKeyId) &&
		cert.SignatureAlgorithm == x509.ECDSAWithSHA256 &&
		cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil
}
