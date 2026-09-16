// Package identity validates immutable, in-memory TLS identity material.
package identity

import (
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"time"
)

type Role uint8

const (
	Client Role = iota + 1
	Server
)

type Identity struct {
	cert tls.Certificate
	role Role
}

func Parse(certPEM, keyPEM []byte, role Role) (Identity, error) {
	if len(certPEM) > 1<<20 || len(keyPEM) > 64<<10 {
		return Identity{}, errors.New("identity size limit")
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Identity{}, err
	}
	return FromTLS(c, role)
}

// FromTLS copies certificates and validates the key pair. The signer is trusted
// immutable application material and must remain alive while the identity is used.
func FromTLS(c tls.Certificate, role Role) (Identity, error) {
	if role != Client && role != Server {
		return Identity{}, errors.New("identity role")
	}
	if len(c.Certificate) == 0 || len(c.Certificate) > 16 {
		return Identity{}, errors.New("identity chain bound")
	}
	c = clone(c)
	total := 0
	for _, v := range c.Certificate {
		total += len(v)
	}
	if total > 1<<20 {
		return Identity{}, errors.New("identity chain size")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return Identity{}, err
	}
	signer, ok := c.PrivateKey.(crypto.Signer)
	if !ok {
		return Identity{}, errors.New("identity signer")
	}
	pub, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return Identity{}, err
	}
	want, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return Identity{}, err
	}
	if string(pub) != string(want) {
		return Identity{}, errors.New("identity private key mismatch")
	}
	now := time.Now()
	if leaf.IsCA || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return Identity{}, errors.New("identity validity or leaf role")
	}
	if leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return Identity{}, errors.New("identity signature usage")
	}
	usage := x509.ExtKeyUsageClientAuth
	if role == Server {
		usage = x509.ExtKeyUsageServerAuth
	}
	allowed := len(leaf.ExtKeyUsage) == 0 && len(leaf.UnknownExtKeyUsage) == 0
	for _, v := range leaf.ExtKeyUsage {
		if v == usage || v == x509.ExtKeyUsageAny {
			allowed = true
		}
	}
	if !allowed {
		return Identity{}, errors.New("identity extended usage")
	}
	c.Leaf = leaf
	return Identity{c, role}, nil
}
func clone(c tls.Certificate) tls.Certificate {
	out := c
	out.Certificate = make([][]byte, len(c.Certificate))
	for i, p := range c.Certificate {
		out.Certificate[i] = append([]byte(nil), p...)
	}
	out.OCSPStaple = append([]byte(nil), c.OCSPStaple...)
	out.SignedCertificateTimestamps = make([][]byte, len(c.SignedCertificateTimestamps))
	for i, p := range c.SignedCertificateTimestamps {
		out.SignedCertificateTimestamps[i] = append([]byte(nil), p...)
	}
	out.SupportedSignatureAlgorithms = append([]tls.SignatureScheme(nil), c.SupportedSignatureAlgorithms...)
	if len(out.Certificate) > 0 {
		out.Leaf, _ = x509.ParseCertificate(out.Certificate[0])
	}
	return out
}
func (i Identity) Certificate() tls.Certificate { return clone(i.cert) }
func (i Identity) ValidFor(role Role) bool {
	return i.role == role && i.cert.Leaf != nil && time.Now().Before(i.cert.Leaf.NotAfter) && !time.Now().Before(i.cert.Leaf.NotBefore)
}
func (i Identity) Fingerprint() string {
	if i.cert.Leaf == nil {
		return ""
	}
	v := sha256.Sum256(i.cert.Leaf.Raw)
	return hex.EncodeToString(v[:])
}
func (i Identity) NotAfter() time.Time {
	if i.cert.Leaf == nil {
		return time.Time{}
	}
	return i.cert.Leaf.NotAfter
}
