package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type recordCapture struct {
	net.Conn
	wire bytes.Buffer
}

func (c *recordCapture) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.wire.Write(p[:n])
	return n, err
}

// Measure actual ciphertext from both transport factories, not just config
// fields. Full-sized first writes must not exhibit Go's growing record ramp;
// a subsequent tiny write must still reach the peer without another write.
func TestTransportRecordSizes(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "record.test"},
		DNSNames: []string{"record.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := Settings{Mode: "tls", ServerName: "record.test", Certificate: filepath.Join(dir, "cert.pem"), PrivateKeyFile: filepath.Join(dir, "key.pem")}
	s.CAFile = s.Certificate
	for path, block := range map[string]*pem.Block{s.Certificate: {Type: "CERTIFICATE", Bytes: der}, s.PrivateKeyFile: {Type: "PRIVATE KEY", Bytes: keyDER}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server, err := Server(s, 5*time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Client(s)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	deadline := time.Now().Add(5 * time.Second)
	a.SetDeadline(deadline)
	b.SetDeadline(deadline)
	ca, cb := &recordCapture{Conn: a}, &recordCapture{Conn: b}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var sc net.Conn
	result := make(chan error, 1)
	go func() {
		var err error
		sc, err = server(ctx, ca)
		result <- err
	}()
	cc, err := client(ctx, cb)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	defer func() {
		a.Close()
		b.Close()
		cc.Close()
		sc.Close()
	}()
	ca.wire.Reset()
	cb.wire.Reset()
	for _, side := range []struct {
		name     string
		from, to net.Conn
		tap      *recordCapture
	}{{"client", cc, sc, cb}, {"server", sc, cc, ca}} {
		t.Run(side.name, func(t *testing.T) {
			for _, size := range []int{32768, 3} {
				payload := bytes.Repeat([]byte{0x5a}, size)
				go func() {
					n, err := side.from.Write(payload)
					if err == nil && n != size {
						err = io.ErrShortWrite
					}
					result <- err
				}()
				got := make([]byte, size)
				if _, err := io.ReadFull(side.to, got); err != nil {
					t.Fatal(err)
				}
				if err := <-result; err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatal("payload changed")
				}
			}
			wire := side.tap.wire.Bytes()
			for _, size := range []int{16401, 16401, 20} {
				if len(wire) < 5 || wire[0] != 23 || int(binary.BigEndian.Uint16(wire[3:5])) != size || len(wire) < 5+size {
					t.Fatalf("expected TLS application record of %d bytes, remaining bytes=%d", size, len(wire))
				}
				wire = wire[5+size:]
			}
			if len(wire) != 0 {
				t.Fatalf("unexpected extra ciphertext: %d bytes", len(wire))
			}
		})
	}
}
