package session

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePreflightIdentity(t *testing.T, dir, name string, cert tls.Certificate) (string, string, string) {
	t.Helper()
	var chain []byte
	for _, der := range cert.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, caPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key"), filepath.Join(dir, name+"-ca.crt")
	for path, data := range map[string][]byte{certPath: chain, keyPath: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), caPath: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[len(cert.Certificate)-1]})} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return certPath, keyPath, caPath
}

func TestCertificateInstallationPreflight(t *testing.T) {
	server, client, _ := flightTLSConfigs(t)
	dir := t.TempDir()
	cert, key, ca := writePreflightIdentity(t, dir, "server", server.Certificates[0])
	cc, ck, _ := writePreflightIdentity(t, dir, "client", client.Certificates[0])
	other, _, _ := flightTLSConfigs(t)
	_, _, wrongCA := writePreflightIdentity(t, dir, "untrusted", other.Certificates[0])
	for _, tc := range []struct {
		name, cert, key, ca, role, host string
		horizon                         time.Duration
		good                            bool
	}{
		{"valid-server", cert, key, ca, "server", "owned.test", 30 * time.Minute, true},
		{"valid-client", cc, ck, ca, "client", "", 30 * time.Minute, true},
		{"wrong-name", cert, key, ca, "server", "wrong.owned.test", 0, false},
		{"wrong-chain", cert, key, wrongCA, "server", "owned.test", 0, false},
		{"mismatched-key", cert, ck, ca, "server", "owned.test", 0, false},
		{"wrong-purpose", cc, ck, ca, "server", "owned.test", 0, false},
		{"missing-server-name", cert, key, ca, "server", "", 0, false},
		{"client-name", cc, ck, ca, "client", "owned.test", 0, false},
		{"unknown-role", cert, key, ca, "both", "owned.test", 0, false},
		{"too-close-to-expiry", cert, key, ca, "server", "owned.test", 2 * time.Hour, false},
		{"negative-horizon", cert, key, ca, "server", "owned.test", -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := CheckCertificate(tc.cert, tc.key, tc.ca, tc.role, tc.host, tc.horizon)
			if (err == nil) != tc.good {
				t.Fatalf("good=%v, report=%+v, error=%v", tc.good, report, err)
			}
			if tc.good && (report.VerifiedChains < 1 || report.SHA256 == "" || report.VerifiedUntil.Before(time.Now().Add(tc.horizon))) {
				t.Fatal(report)
			}
		})
	}
	if err := os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCertificate(cert, key, ca, "server", "owned.test", 0); err == nil {
		t.Fatal("world-readable private key accepted")
	}
}

func TestCertificatePreflightIncludesIssuerExpiry(t *testing.T) {
	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(101), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute)}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(102), DNSNames: []string{"owned.test"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, ca := writePreflightIdentity(t, t.TempDir(), "short-root", tls.Certificate{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: leafKey})
	if _, err := CheckCertificate(cert, key, ca, "server", "owned.test", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCertificate(cert, key, ca, "server", "owned.test", 30*time.Minute); err == nil || !strings.Contains(err.Error(), "validity horizon") {
		t.Fatal("issuer expiry ignored", err)
	}
}

func TestRuntimeConfigurationPreflight(t *testing.T) {
	runtimeConfigurationPreflight(t, false)
}
func TestRenewingRuntimeConfigurationPreflight(t *testing.T) {
	runtimeConfigurationPreflight(t, true)
}
func runtimeConfigurationPreflight(t *testing.T, renewing bool) {
	version, inner := 4, EarlyOpenFlightInnerProtocol
	if renewing {
		version, inner = 5, RenewingFlightInnerProtocol
	}
	dir := t.TempDir()
	server, client, fingerprint := flightTLSConfigs(t)
	sc, sk, ca := writePreflightIdentity(t, dir, "server", server.Certificates[0])
	cc, ck, _ := writePreflightIdentity(t, dir, "client", client.Certificates[0])
	model := filepath.Join(dir, "model.json")
	bundle, err := GenerateEarlyOpenFlightBundle(4)
	if renewing {
		bundle, err = GenerateRenewingFlightBundle(4)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteFlightBundle(model, bundle); err != nil {
		t.Fatal(err)
	}
	mux := MuxConfig{Streams: 4, Opened: 128, ConnectionBytes: 64 << 20, CarrierIdleMS: 800}
	s := ServerConfig{Version: 4, Listen: "127.0.0.1:8443", Certificate: sc, PrivateKey: sk, ClientCA: ca, ClientFingerprints: []string{fingerprint}, ModelFile: model, Bucket: "veil-session", BackendMode: "local-object-v1", MaxSessions: 2, MaxSessionsPerIdentity: 2, Mux: mux, AllowCIDRs: []string{"192.0.2.0/24"}, DNSAddress: "192.0.2.53:53"}
	c := ClientConfig{Version: 4, Listen: "127.0.0.1:1080", Certificate: cc, PrivateKey: ck, CA: ca, ModelFile: model, Bucket: "veil-session", ServerURL: "https://owned.test:8443", DialAddress: "192.0.2.1:8443", Mux: mux}
	s.Version, c.Version = version, version
	write := func(name string, value any) string {
		t.Helper()
		p := filepath.Join(dir, name)
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sp, cp := write("server.json", s), write("client.json", c)
	sr, err := CheckConfig("server", sp)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := CheckConfig("client", cp)
	if err != nil {
		t.Fatal(err)
	}
	if sr.ModelID != cr.ModelID || sr.Version != version || cr.InnerProtocol != inner || sr.ReservedWindowPerCarrier != 262144 || cr.CertificateSHA256 != fingerprint || cr.ServerName != "owned.test" {
		t.Fatal(sr, cr)
	}
	raw, _ := json.Marshal(cr)
	if strings.Contains(string(raw), bundle.SeedHex) || strings.Contains(string(raw), dir) {
		t.Fatal("private configuration exposed")
	}
	if _, err := CheckConfig("other", cp); err == nil {
		t.Fatal("unknown role accepted")
	}
	s.DNSAddress = "resolver.owned.test:53"
	if _, err := CheckConfig("server", write("bad-dns.json", s)); err == nil {
		t.Fatal("bad DNS configuration accepted")
	}
	c.Version = 3
	if _, err := CheckConfig("client", write("bad-version.json", c)); err == nil {
		t.Fatal("model/version mismatch accepted")
	}
	c.Version = version
	c.Certificate = sc
	c.PrivateKey = sk
	if _, err := CheckConfig("client", write("bad-role.json", c)); err == nil {
		t.Fatal("server-only identity accepted as client")
	}
	unknown := write("unknown.json", map[string]any{"Version": 4, "Mystery": "reject"})
	if _, err := CheckConfig("client", unknown); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err = os.Chmod(cp, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckConfig("client", cp); err == nil {
		t.Fatal("insecure configuration accepted")
	}
}
