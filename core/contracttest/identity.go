// Package contracttest provides isolated test fixtures for core consumers.
package contracttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"testing"
	"time"
	"veil.local/core/identity"
)

func Identities(t testing.TB) (identity.Identity, identity.Identity, *x509.CertPool) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	ca, e = x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := func(serial int64, role identity.Role) identity.Identity {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		usage := x509.ExtKeyUsageClientAuth
		if role == identity.Server {
			usage = x509.ExtKeyUsageServerAuth
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"owned.test"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		raw, e := x509.CreateCertificate(rand.Reader, cert, ca, &k.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		i, e := identity.FromTLS(tls.Certificate{Certificate: [][]byte{raw, der}, PrivateKey: k}, role)
		if e != nil {
			t.Fatal(e)
		}
		return i
	}
	return leaf(2, identity.Server), leaf(3, identity.Client), roots
}
