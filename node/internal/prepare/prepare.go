// Package prepare creates a new pair of node configurations. It is initial
// provisioning only: it never updates a live identity, installation or model.
package prepare

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"time"

	core "veil.local/core"
	"veil.local/core/endpoint"
	"veil.local/core/identity"
	"veil.local/core/model"
	"veil.local/node/internal/config"
)

type Writer interface {
	// WriteNew publishes a new, private directory and refuses existing paths.
	WriteNew(path string, files map[string][]byte) error
}

type UnsupportedWriter struct{}

func (UnsupportedWriter) WriteNew(string, map[string][]byte) error {
	return errors.New("private bundle creation is not implemented on this platform")
}

type Options struct {
	Output, ServerAddress, ServerName, ServerListen, ClientListen string
	ServerCertificate, ServerKey, ServerCA                        string
	LocalTest                                                     bool
	Lanes                                                         int
	Limits                                                        config.Limits
	MinValidity                                                   time.Duration
}

type Report struct {
	Kind              string    `json:"kind"`
	Version           string    `json:"version"`
	LocalTest         bool      `json:"local_test"`
	ModelID           string    `json:"model_id"`
	ModelSHA256       string    `json:"model_sha256"`
	ServerFingerprint string    `json:"server_fingerprint"`
	ClientFingerprint string    `json:"client_fingerprint"`
	ServerExpiry      time.Time `json:"server_not_after"`
	ClientExpiry      time.Time `json:"client_not_after"`
	EndToEnd          string    `json:"end_to_end"`
}

func Create(o Options, reader config.FileReader, writer Writer) (Report, error) {
	var report Report
	address, err := netip.ParseAddrPort(o.ServerAddress)
	if err != nil || address.Port() == 0 || address.Addr().Zone() != "" || address.Addr().IsUnspecified() || address.Addr().IsMulticast() || o.Output == "" {
		return report, errors.New("output and a numeric unicast server-address with a nonzero port are required")
	}
	if o.ServerName == "" {
		o.ServerName = address.Addr().String()
	}
	name, err := endpoint.Parse(o.ServerName, address.Port())
	if err != nil {
		return report, fmt.Errorf("server-name: %w", err)
	}
	if o.ServerListen == "" {
		o.ServerListen = net.JoinHostPort("0.0.0.0", fmt.Sprint(address.Port()))
		if address.Addr().Is6() {
			o.ServerListen = net.JoinHostPort("::", fmt.Sprint(address.Port()))
		}
		if o.LocalTest {
			o.ServerListen = address.String()
		}
	}
	listen, err := netip.ParseAddrPort(o.ServerListen)
	if err != nil || listen.Addr().Zone() != "" {
		return report, errors.New("server-listen must be a numeric address and port")
	}
	if o.ClientListen == "" {
		o.ClientListen = "127.0.0.1:1080"
	}
	clientListen, err := netip.ParseAddrPort(o.ClientListen)
	if err != nil || !clientListen.Addr().IsLoopback() || clientListen.Addr().Zone() != "" {
		return report, errors.New("client-listen must be numeric loopback")
	}
	if err = o.Limits.Validate(); err != nil {
		return report, err
	}
	if o.MinValidity < 0 || o.MinValidity > 365*24*time.Hour {
		return report, errors.New("min-validity must be between zero and 365 days")
	}
	if o.LocalTest && (!address.Addr().IsLoopback() || !listen.Addr().IsLoopback() || o.ServerCertificate != "" || o.ServerKey != "" || o.ServerCA != "") {
		return report, errors.New("local-test requires loopback addresses and generates its own server identity")
	}
	files := make(map[string][]byte)
	defer func() {
		for _, data := range files {
			clear(data)
		}
	}()
	var serverCert, serverKey, serverCA []byte
	if o.LocalTest {
		ca, key, caPEM, _, e := newCA("Veil local test server CA", 24*time.Hour)
		if e != nil {
			return report, e
		}
		serverCert, serverKey, err = newLeaf(ca, key, identity.Server, name.Host(), 24*time.Hour)
		serverCA = caPEM
	} else {
		if o.ServerCertificate == "" || o.ServerKey == "" {
			return report, errors.New("server-cert and server-key are required (local tests may explicitly use --local-test)")
		}
		serverCert, err = reader.Read(o.ServerCertificate, 1<<20, false)
		if err == nil {
			serverKey, err = reader.Read(o.ServerKey, 64<<10, true)
		}
		if err == nil && o.ServerCA != "" {
			serverCA, err = reader.Read(o.ServerCA, 1<<20, false)
		}
	}
	defer clear(serverKey)
	if err != nil {
		return report, fmt.Errorf("server identity: %w", err)
	}
	serverID, err := identity.Parse(serverCert, serverKey, identity.Server)
	if err != nil {
		return report, err
	}
	var roots *x509.CertPool
	if len(serverCA) != 0 {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(serverCA) {
			return report, errors.New("server CA is invalid")
		}
	}
	cert := serverID.Certificate()
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		c, e := x509.ParseCertificate(der)
		if e != nil {
			return report, e
		}
		intermediates.AddCert(c)
	}
	verify := x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: name.Host(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if _, err = cert.Leaf.Verify(verify); err != nil {
		return report, fmt.Errorf("server chain/name: %w", err)
	}
	verify.CurrentTime = time.Now().Add(o.MinValidity)
	if _, err = cert.Leaf.Verify(verify); err != nil {
		return report, fmt.Errorf("server validity horizon: %w", err)
	}
	ca, caKey, caPEM, caKeyPEM, err := newCA("Veil client CA", 365*24*time.Hour)
	if err != nil {
		return report, err
	}
	files["admin/client-ca-key.pem"] = caKeyPEM
	files["admin/client-ca.pem"] = caPEM
	clientCert, clientKey, err := newLeaf(ca, caKey, identity.Client, "", 30*24*time.Hour)
	if err != nil {
		return report, err
	}
	files["client/client-key.pem"] = clientKey
	clientID, err := identity.Parse(clientCert, clientKey, identity.Client)
	if err != nil {
		return report, err
	}
	if o.Lanes == 0 {
		o.Lanes = 4
	}
	m, err := model.Generate(o.Lanes)
	if err != nil {
		return report, err
	}
	client := config.Config{Version: 1, Role: "client", Listen: o.ClientListen,
		ServerURL: "https://" + net.JoinHostPort(name.Host(), fmt.Sprint(address.Port())), DialAddress: address.String(),
		Bucket: "veil-node", ModelFile: "model.json", Certificate: "client.pem", PrivateKey: "client-key.pem", MaxConnections: 8, Limits: o.Limits}
	server := config.Config{Version: 1, Role: "server", Listen: o.ServerListen,
		Bucket: client.Bucket, ModelFile: "model.json", Certificate: "server.pem", PrivateKey: "server-key.pem", CA: "client-ca.pem",
		ClientFingerprints: []string{clientID.Fingerprint()}, MaxConnections: 8, Limits: o.Limits}
	if o.LocalTest {
		server.AllowCIDRs = []string{"127.0.0.0/8", "::1/128"}
	}
	if len(serverCA) != 0 {
		client.CA = "server-ca.pem"
		files["client/server-ca.pem"] = serverCA
	}
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(ca)
	if _, err = (config.Loaded{Config: client, Model: m, Identity: clientID, Roots: roots}).Client(); err != nil {
		return report, err
	}
	if _, err = (config.Loaded{Config: server, Model: m, Identity: serverID, Roots: clientRoots}).Server(); err != nil {
		return report, err
	}
	files["client/model.json"], files["server/model.json"] = m.Bytes(), m.Bytes()
	files["client/client.pem"], files["server/client-ca.pem"] = clientCert, caPEM
	files["server/server.pem"], files["server/server-key.pem"] = serverCert, serverKey
	files["client/node.json"], _ = json.MarshalIndent(client, "", "  ")
	files["server/node.json"], _ = json.MarshalIndent(server, "", "  ")
	report = Report{Kind: "prepared", Version: core.Version, LocalTest: o.LocalTest, ModelID: m.ID(), ModelSHA256: m.SHA256(),
		ServerFingerprint: serverID.Fingerprint(), ClientFingerprint: clientID.Fingerprint(), ServerExpiry: serverID.NotAfter(), ClientExpiry: clientID.NotAfter(), EndToEnd: "not_checked"}
	files["prepared.json"], _ = json.MarshalIndent(report, "", "  ")
	if err = writer.WriteNew(o.Output, files); err != nil {
		return Report{}, err
	}
	return report, nil
}

func serial() (*big.Int, error) {
	// Nonzero positive serials, independent of model seeds and TLS randomness.
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

func newCA(name string, validity time.Duration) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: sn, Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true,
		MaxPathLenZero: true, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(validity), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer clear(keyDER)
	return ca, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func newLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, role identity.Role, name string, validity time.Duration) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: sn, NotBefore: now.Add(-time.Minute), NotAfter: minTime(now.Add(validity), ca.NotAfter),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if role == identity.Server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = []net.IP{ip}
		} else {
			template.DNSNames = []string{name}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	defer clear(keyDER)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
