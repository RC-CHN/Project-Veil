package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func h2Config() *http.HTTP2Config {
	return &http.HTTP2Config{MaxConcurrentStreams: 128, MaxReceiveBufferPerConnection: 4 << 20, MaxReceiveBufferPerStream: 1 << 20}
}

type client struct {
	url, secret string
	up, down    *http.Transport
}

func newClient(cfg config) (*client, error) {
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid fixture CA")
	}
	makeTransport := func() *http.Transport {
		return &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "cover.test",
			MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, DynamicRecordSizingDisabled: true},
			HTTP2: h2Config(), MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: time.Minute,
			TLSHandshakeTimeout: 5 * time.Second, DisableCompression: true}
	}
	c := &client{url: "https://" + cfg.Server, secret: cfg.Secret, up: makeTransport()}
	switch cfg.Mode {
	case "shared":
		c.down = c.up
	case "split":
		c.down = makeTransport()
	default:
		return nil, errors.New("mode must be shared or split")
	}
	return c, nil
}

func (c *client) close() { c.up.CloseIdleConnections(); c.down.CloseIdleConnections() }

type tunnel struct {
	reader  io.ReadCloser
	writer  *io.PipeWriter
	upload  io.ReadCloser
	request *io.PipeReader
	cancel  context.CancelFunc
	once    sync.Once
}

func (t *tunnel) Read(b []byte) (int, error)  { return t.reader.Read(b) }
func (t *tunnel) Write(b []byte) (int, error) { return t.writer.Write(b) }
func (t *tunnel) CloseWrite() error {
	if err := t.writer.Close(); err != nil {
		return err
	}
	_, err := io.Copy(io.Discard, t.upload)
	return err
}
func (t *tunnel) Close() error {
	t.once.Do(func() {
		t.cancel()
		t.writer.CloseWithError(context.Canceled)
		t.request.CloseWithError(context.Canceled)
		t.reader.Close()
		t.upload.Close()
	})
	return nil
}

func (c *client) open(ctx context.Context, target string) (*tunnel, error) {
	if !loopback(target) {
		return nil, errors.New("fixture target must be loopback")
	}
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(10*time.Second, cancel)
	defer timer.Stop()
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		cancel()
		return nil, err
	}
	path := c.url + "/stream/" + hex.EncodeToString(id)
	pr, pw := io.Pipe()
	type result struct {
		index    int
		response *http.Response
		err      error
	}
	results := make(chan result, 2)
	for i, method := range []string{"POST", "GET"} {
		var body io.Reader
		tr := c.down
		if i == 0 {
			body, tr = pr, c.up
		}
		req, err := http.NewRequestWithContext(ctx, method, path, body)
		if err != nil {
			cancel()
			pr.Close()
			pw.Close()
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.secret)
		req.Header.Set("X-Target", target)
		req.Header.Set("Content-Type", "application/octet-stream")
		go func() { resp, err := tr.RoundTrip(req); results <- result{i, resp, err} }()
	}
	var responses [2]*http.Response
	var failure error
	for range 2 {
		r := <-results
		responses[r.index] = r.response
		if r.err != nil {
			failure = r.err
		} else if r.response.StatusCode != 200 || r.response.ProtoMajor != 2 {
			failure = fmt.Errorf("HTTP tunnel: %s (%s)", r.response.Status, r.response.Proto)
		}
		if failure != nil {
			cancel()
			pw.CloseWithError(failure)
			pr.CloseWithError(failure)
		}
	}
	if failure != nil {
		for _, r := range responses {
			if r != nil {
				r.Body.Close()
			}
		}
		return nil, failure
	}
	return &tunnel{reader: responses[1].Body, upload: responses[0].Body, writer: pw, request: pr, cancel: cancel}, nil
}

// Use bounded application copy buffers, retaining independent half-closes.
// Do not let TCP ReaderFrom bypass the intended userspace benchmark path.
type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }

func relay(local net.Conn, t *tunnel) error {
	done := make(chan error, 2)
	go func() {
		_, err := io.CopyBuffer(writerOnly{t}, readerOnly{local}, make([]byte, 128<<10))
		if err == nil {
			err = t.CloseWrite()
		}
		done <- err
	}()
	go func() {
		_, err := io.CopyBuffer(writerOnly{local}, readerOnly{t}, make([]byte, 128<<10))
		if err == nil {
			err = local.(interface{ CloseWrite() error }).CloseWrite()
		}
		done <- err
	}()
	var first error
	for range 2 {
		if err := <-done; err != nil {
			if first == nil {
				first = err
			}
			t.Close()
			local.Close()
		}
	}
	return first
}

type pair struct {
	ctx    context.Context
	cancel context.CancelFunc
	ready  chan struct{}
	mu     sync.Mutex
	conn   net.Conn
	err    error
	target string
	slots  [2]bool // protected by server.mu
	refs   int     // protected by server.mu
}

func (p *pair) close() {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		p.conn.Close()
	}
}

func (p *pair) dial() {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.target)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil && c != nil {
		c.Close()
		c = nil
		err = p.ctx.Err()
	}
	p.conn, p.err = c, err
	close(p.ready)
}

type server struct {
	secret   string
	mu       sync.Mutex
	sessions map[string]*pair
	closed   bool
}

func (s *server) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, p := range s.sessions {
		p.close()
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		io.WriteString(w, "owned HTTPS fixture\n")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/stream/")
	_, idErr := hex.DecodeString(id)
	if r.ProtoMajor != 2 || len(id) != 32 || idErr != nil || !loopback(r.Header.Get("X-Target")) ||
		(r.Method != "GET" && r.Method != "POST") || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.secret)) != 1 {
		http.NotFound(w, r)
		return
	}
	slot := 0
	if r.Method == "GET" {
		slot = 1
	}
	s.mu.Lock()
	p := s.sessions[id]
	if s.closed || (p == nil && len(s.sessions) >= 128) {
		s.mu.Unlock()
		http.Error(w, "busy", 503)
		return
	}
	if p == nil {
		ctx, cancel := context.WithCancel(context.Background())
		p = &pair{ctx: ctx, cancel: cancel, ready: make(chan struct{}), target: r.Header.Get("X-Target")}
		s.sessions[id] = p
	}
	if p.slots[slot] || p.target != r.Header.Get("X-Target") {
		s.mu.Unlock()
		http.Error(w, "conflict", 409)
		return
	}
	p.slots[slot], p.refs = true, p.refs+1
	if p.slots[0] && p.slots[1] {
		go p.dial()
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		p.refs--
		if p.refs == 0 {
			delete(s.sessions, id)
			p.close()
		}
	}()
	stop := context.AfterFunc(r.Context(), p.close)
	defer stop()
	rc := http.NewResponseController(w)
	aborted := make(chan struct{})
	stopAbort := context.AfterFunc(p.ctx, func() {
		defer close(aborted)
		r.Body.Close()
		rc.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stopAbort() {
			<-aborted
		}
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-p.ready:
		if p.err != nil {
			http.Error(w, "target unavailable", 502)
			return
		}
	case <-p.ctx.Done():
		http.Error(w, "cancelled", 503)
		return
	case <-timer.C:
		p.close()
		http.Error(w, "pair timeout", 408)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	if err := rc.Flush(); err != nil {
		p.close()
		return
	}
	var err error
	if slot == 0 {
		_, err = io.CopyBuffer(writerOnly{p.conn}, readerOnly{r.Body}, make([]byte, 128<<10))
		if err == nil {
			err = p.conn.(*net.TCPConn).CloseWrite()
		}
	} else {
		buffer := make([]byte, 128<<10)
		for {
			var n int
			n, err = p.conn.Read(buffer)
			if n > 0 {
				if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
					err = writeErr
					break
				}
				if flushErr := rc.Flush(); flushErr != nil {
					err = flushErr
					break
				}
			}
			if err != nil {
				break
			}
		}
		if err == io.EOF {
			err = nil
		}
	}
	if err != nil {
		p.close()
		panic(http.ErrAbortHandler)
	}
}
