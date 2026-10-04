package transport

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	utls "github.com/metacubex/utls"
)

func verifyCoverCertificate(s Settings, certificates []*x509.Certificate) error {
	if s.ServerName == "" {
		return errors.New("cover: server name required")
	}
	if len(certificates) == 0 {
		return errors.New("cover: missing certificate")
	}
	if s.CAFile != "" && s.CAPEM != "" {
		return errors.New("cover: choose ca_file or ca_pem")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	pem := []byte(s.CAPEM)
	if s.CAFile != "" {
		pem, err = os.ReadFile(s.CAFile)
		if err != nil {
			return err
		}
	}
	if (s.CAFile != "" || s.CAPEM != "") && !roots.AppendCertsFromPEM(pem) {
		return errors.New("cover: invalid CA")
	}
	opts := x509.VerifyOptions{DNSName: s.ServerName, Roots: roots, Intermediates: x509.NewCertPool()}
	for _, cert := range certificates[1:] {
		opts.Intermediates.AddCert(cert)
	}
	_, err = certificates[0].Verify(opts)
	return err
}

var errCoverBudget = errors.New("cover request byte budget exhausted")

// Budgets are application bytes. TLS may already hold bounded ciphertext
// read-ahead; this wrapper exists only on the unauthenticated cover path.
type coverBudgetConn struct {
	net.Conn
	readMu, writeMu     sync.Mutex
	readLeft, writeLeft int
}

func (c *coverBudgetConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readLeft == 0 {
		return 0, errCoverBudget
	}
	if len(p) > c.readLeft {
		p = p[:c.readLeft]
	}
	n, err := c.Conn.Read(p)
	c.readLeft -= n
	return n, err
}
func (c *coverBudgetConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeLeft == 0 {
		return 0, errCoverBudget
	}
	wanted := len(p)
	if len(p) > c.writeLeft {
		p = p[:c.writeLeft]
	}
	n, err := c.Conn.Write(p)
	c.writeLeft -= n
	if err == nil && n < wanted {
		err = errCoverBudget
	}
	return n, err
}

// A cover certificate never becomes a Veil connection. Finish one bounded
// HTTPS request synchronously, without redirects, retries or detached requests.
func browseCover(parent context.Context, c *utls.UConn, serverName, version string) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	raw := c.NetConn()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer func() { cancel(); raw.Close(); c.Close(); stop() }()
	deadline, _ := ctx.Deadline()
	if err := c.SetDeadline(deadline); err != nil {
		return
	}
	conn := &coverBudgetConn{Conn: c, readLeft: 64 << 10, writeLeft: 16 << 10}
	req := &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: serverName, Path: "/"}, Host: serverName, Header: make(http.Header)}
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", version))
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	protocols := new(http.Protocols)
	switch c.ConnectionState().NegotiatedProtocol {
	case "h2":
		protocols.SetUnencryptedHTTP2(true)
	case "", "http/1.1":
		protocols.SetHTTP1(true)
	default:
		return
	}
	// TLS and certificate validation are already complete. NewClientConn sees
	// the decrypted application stream; it must not perform a second TLS
	// handshake or dial another address. The HTTP request's scheme is https.
	tr := &http.Transport{
		Protocols: protocols, DisableCompression: true, MaxResponseHeaderBytes: 16 << 10,
		HTTP2:       &http.HTTP2Config{MaxReadFrameSize: 16 << 10},
		DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil },
	}
	client, err := tr.NewClientConn(ctx, "http", net.JoinHostPort(serverName, "443"))
	if err != nil {
		return
	}
	defer client.Close()
	response, err := client.RoundTrip(req)
	if err != nil {
		return
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
}
