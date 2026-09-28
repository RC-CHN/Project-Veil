package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
)

func TestRealityCertificateProof(t *testing.T) {
	authKey := make([]byte, 32)
	rand.Read(authKey)
	certificate := func(curve elliptic.Curve, proofKey []byte, label string) *x509.Certificate {
		t.Helper()
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, proofKey)
		mac.Write([]byte(label))
		mac.Write(spki)
		template := &x509.Certificate{
			SerialNumber: big.NewInt(1), SignatureAlgorithm: x509.ECDSAWithSHA256,
			SubjectKeyId: mac.Sum(nil),
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	const label = "Veil-v0.3 server authentication\x00"
	valid := certificate(elliptic.P256(), authKey, label)
	if !verifyRealityCertificate(valid, authKey) {
		t.Fatal("valid connection-bound certificate rejected")
	}
	for _, test := range []struct {
		name string
		cert *x509.Certificate
		key  []byte
	}{
		{"wrong connection", valid, make([]byte, 32)},
		{"missing connection key", valid, nil},
		{"wrong domain", certificate(elliptic.P256(), authKey, label[:len(label)-1]), authKey},
		{"wrong curve", certificate(elliptic.P384(), authKey, label), authKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			if verifyRealityCertificate(test.cert, test.key) {
				t.Fatal("invalid certificate accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*x509.Certificate){
		"critical proof": func(c *x509.Certificate) {
			c.Extensions = append([]pkix.Extension(nil), c.Extensions...)
			for i := range c.Extensions {
				if c.Extensions[i].Id.Equal([]int{2, 5, 29, 14}) {
					c.Extensions[i].Critical = true
				}
			}
		},
		"missing proof": func(c *x509.Certificate) { c.SubjectKeyId = nil },
		"wrong proof": func(c *x509.Certificate) {
			c.SubjectKeyId = append([]byte(nil), c.SubjectKeyId...)
			c.SubjectKeyId[0] ^= 1
		},
		"changed public key": func(c *x509.Certificate) {
			c.RawSubjectPublicKeyInfo = valid.RawSubjectPublicKeyInfo[:len(valid.RawSubjectPublicKeyInfo)-1]
		},
		"bad self signature": func(c *x509.Certificate) {
			c.Signature = append([]byte(nil), c.Signature...)
			c.Signature[len(c.Signature)-1] ^= 1
		},
		"wrong signature algorithm": func(c *x509.Certificate) { c.SignatureAlgorithm = x509.ECDSAWithSHA384 },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *valid
			mutate(&copy)
			if verifyRealityCertificate(&copy, authKey) {
				t.Fatal("modified certificate accepted")
			}
		})
	}
}
