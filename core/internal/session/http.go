package session

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	os "veil.local/core/internal/objectstore"
)

type wireCount struct{ Read, Written, Connections atomic.Int64 }
type wireSnapshot struct{ Read, Written, Connections int64 }

func (w *wireCount) snapshot() wireSnapshot {
	return wireSnapshot{w.Read.Load(), w.Written.Load(), w.Connections.Load()}
}

type countedConn struct {
	net.Conn
	w *wireCount
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, e := c.Conn.Read(p)
	c.w.Read.Add(int64(n))
	return n, e
}
func (c *countedConn) Write(p []byte) (int, error) {
	n, e := c.Conn.Write(p)
	c.w.Written.Add(int64(n))
	return n, e
}
func dialCount(ctx context.Context, network, address string, w *wireCount) (net.Conn, error) {
	c, e := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
	if e != nil {
		return nil, e
	}
	w.Connections.Add(1)
	return &countedConn{c, w}, nil
}
func hashHex(p []byte) string { h := sha256.Sum256(p); return hex.EncodeToString(h[:]) }
func mac(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

var plainPath = regexp.MustCompile(`^/[a-z0-9./-]+$`)

// Deliberately restricted SigV4 subset: no query, escaping, streaming, session
// token or duplicate headers. It is only used against the owned test backend.
func sign(req *http.Request, body []byte, access, secret string, now time.Time) error {
	if !plainPath.MatchString(req.URL.Path) || req.URL.RawQuery != "" || req.URL.RawPath != "" || strings.Contains(req.URL.Path, "..") {
		return errors.New("signer path outside fixture subset")
	}
	stamp := now.UTC().Format("20060102T150405Z")
	day := stamp[:8]
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", hashHex(body))
	headers := map[string]string{"host": req.URL.Host}
	for k, vs := range req.Header {
		if len(vs) != 1 {
			return errors.New("duplicate signing header")
		}
		if strings.EqualFold(k, "Authorization") {
			return errors.New("already signed")
		}
		headers[strings.ToLower(k)] = strings.Join(strings.Fields(vs[0]), " ")
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&canonical, "%s:%s\n", k, headers[k])
	}
	signed := strings.Join(keys, ";")
	request := req.Method + "\n" + req.URL.Path + "\n\n" + canonical.String() + "\n" + signed + "\n" + hashHex(body)
	scope := day + "/us-east-1/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hashHex([]byte(request))
	key := mac(mac(mac(mac([]byte("AWS4"+secret), day), "us-east-1"), "s3"), "aws4_request")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+access+"/"+scope+",SignedHeaders="+signed+",Signature="+hex.EncodeToString(mac(key, toSign)))
	return nil
}

type transaction struct {
	Label, Method, Path, Protocol       string
	Status, RequestBytes, ResponseBytes int
	RequestSHA256, ResponseSHA256, ETag string
	TLSVersion                          uint16
	ElapsedUS                           int64
	WriteUS, FirstByteUS                int64  `json:",omitempty"`
	Error                               string `json:",omitempty"`
}
type reply struct {
	status int
	header http.Header
	body   []byte
}

func exchange(client *http.Client, req *http.Request, body []byte, label string) (reply, transaction, error) {
	start := time.Now()
	t := transaction{Label: label, Method: req.Method, Path: req.URL.Path, RequestBytes: len(body), RequestSHA256: hashHex(body)}
	r, e := client.Do(req)
	if e != nil {
		t.Error = e.Error()
		t.ElapsedUS = time.Since(start).Microseconds()
		return reply{}, t, e
	}
	defer r.Body.Close()
	p, e := io.ReadAll(io.LimitReader(r.Body, maxObject+1))
	if len(p) > maxObject {
		e = errors.New("response body bound")
	}
	t.Status, t.Protocol, t.ResponseBytes, t.ResponseSHA256, t.ETag = r.StatusCode, r.Proto, len(p), hashHex(p), r.Header.Get("ETag")
	if r.TLS != nil {
		t.TLSVersion = r.TLS.Version
	}
	t.ElapsedUS = time.Since(start).Microseconds()
	if e != nil {
		t.Error = e.Error()
	}
	return reply{r.StatusCode, r.Header.Clone(), p}, t, e
}

type backend struct {
	local               *os.Store
	url, access, secret string
	client              *http.Client
	transport           *http.Transport
	wire                wireCount
	mu                  sync.Mutex
	log                 []transaction
	requests            uint64
	failures            uint64
}

func newLocalBackend(bucket string, objects int) (*backend, error) {
	s, e := os.New(os.Config{Bucket: bucket, Objects: objects, Bytes: objects * maxObject})
	if e != nil {
		return nil, e
	}
	return &backend{local: s}, nil
}
func (b *backend) close() {
	if b.transport != nil {
		b.transport.CloseIdleConnections()
	}
	if b.local != nil {
		b.local.Close()
	}
}

func newBackend(url, access, secret string, roots *x509.CertPool) *backend {
	b := &backend{url: url, access: access, secret: secret}
	b.transport = &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, MaxConnsPerHost: 8, MaxIdleConnsPerHost: 8, MaxResponseHeaderBytes: 32 << 10, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, DialContext: func(ctx context.Context, n, a string) (net.Conn, error) { return dialCount(ctx, n, a, &b.wire) }}
	b.client = &http.Client{Transport: b.transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }}
	return b
}
func (b *backend) do(ctx context.Context, label, method, path string, p []byte, headers http.Header) (reply, error) {
	if b.local != nil {
		start := time.Now()
		v, e := b.local.Do(ctx, method, path, p, headers)
		r := reply{status: v.Status, header: v.Header, body: v.Body}
		t := transaction{Label: label, Method: method, Path: path, Protocol: "local-object-v1", Status: v.Status, RequestBytes: len(p), ResponseBytes: len(v.Body), RequestSHA256: hashHex(p), ResponseSHA256: hashHex(v.Body), ETag: v.Header.Get("ETag"), ElapsedUS: time.Since(start).Microseconds()}
		if e != nil {
			t.Error = e.Error()
		}
		b.record(r, t, e)
		return r, e
	}
	req, e := http.NewRequestWithContext(ctx, method, b.url+path, bytes.NewReader(p))
	if e != nil {
		return reply{}, e
	}
	req.GetBody = nil
	for k, vs := range headers {
		req.Header[k] = append([]string(nil), vs...)
	}
	if e = sign(req, p, b.access, b.secret, time.Now()); e != nil {
		return reply{}, e
	}
	r, t, e := exchange(b.client, req, p, label)
	b.record(r, t, e)
	return r, e
}
func (b *backend) record(r reply, t transaction, e error) {
	b.mu.Lock()
	b.requests++
	if e != nil || r.status >= 400 {
		b.failures++
	}
	if len(b.log) < 64 {
		b.log = append(b.log, t)
	}
	b.mu.Unlock()
}
func (b *backend) must(ctx context.Context, label, method, path string, p []byte, headers http.Header, status int) (reply, error) {
	r, e := b.do(ctx, label, method, path, p, headers)
	if e == nil && r.status != status {
		e = fmt.Errorf("%s: status %d, wanted %d", label, r.status, status)
	}
	return r, e
}
