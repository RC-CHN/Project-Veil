// Package objectstore provides bounded, ephemeral conditional object storage.
// It performs no network, filesystem or application callback I/O.
package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

const MaxObject = 256 << 10

type Config struct {
	Bucket         string
	Objects, Bytes int
}
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}
type Status struct {
	Closed, BucketExists                                                 bool
	Objects, Bytes, MaximumObjects, MaximumBytes, ObjectLimit, ByteLimit int
	Requests, Writes, Deletes                                            uint64
}
type object struct {
	body     []byte
	etag     string
	metadata http.Header
}
type Store struct {
	mu      sync.Mutex
	cfg     Config
	state   Status
	objects map[string]object
}

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
var keyPattern = regexp.MustCompile(`^[a-z0-9._/-]+$`)
var etagPattern = regexp.MustCompile(`^"[a-f0-9]{32}"$`)

func New(cfg Config) (*Store, error) {
	if !bucketPattern.MatchString(cfg.Bucket) || cfg.Objects < 1 || cfg.Objects > 64 || cfg.Bytes < 1 || cfg.Bytes > 64*MaxObject {
		return nil, errors.New("local object capacity or bucket")
	}
	return &Store{cfg: cfg, objects: make(map[string]object), state: Status{ObjectLimit: cfg.Objects, ByteLimit: cfg.Bytes}}, nil
}
func (s *Store) Status() Status { s.mu.Lock(); defer s.mu.Unlock(); return s.state }
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Closed = true
	s.state.BucketExists = false
	clear(s.objects)
	s.state.Objects = 0
	s.state.Bytes = 0
}
func response(code int) Response { return Response{Status: code, Header: make(http.Header)} }
func (s *Store) path(path string) (string, bool) {
	if path == "/"+s.cfg.Bucket {
		return "", true
	}
	prefix := "/" + s.cfg.Bucket + "/"
	if len(path) > 256 || !strings.HasPrefix(path, prefix) {
		return "", false
	}
	key := strings.TrimPrefix(path, prefix)
	if !keyPattern.MatchString(key) {
		return "", false
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return key, true
}
func headers(input http.Header) (http.Header, bool) {
	out := make(http.Header)
	for name, values := range input {
		name = http.CanonicalHeaderKey(name)
		switch name {
		case "If-Match", "If-None-Match", "X-Amz-Meta-Receipt", "X-Amz-Meta-Mode":
		default:
			continue
		}
		if len(values) != 1 || out[name] != nil || len(values[0]) > 128 {
			return nil, false
		}
		v := values[0]
		for _, ch := range v {
			if ch < 32 || ch > 126 {
				return nil, false
			}
		}
		if (name == "If-Match" || name == "If-None-Match") && v != "*" && !etagPattern.MatchString(v) {
			return nil, false
		}
		out[name] = []string{v}
	}
	return out, !(out.Get("If-Match") != "" && out.Get("If-None-Match") != "")
}

// Do returns a snapshot. Input and output bodies are never aliases of storage.
func (s *Store) Do(ctx context.Context, method, path string, body []byte, input http.Header) (Response, error) {
	if ctx == nil {
		return response(400), errors.New("nil object context")
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	key, valid := s.path(path)
	h, headersOK := headers(input)
	if !valid || !headersOK {
		return response(400), nil
	}
	if len(body) > MaxObject || method != "PUT" && len(body) != 0 {
		return response(413), nil
	}
	if method != "HEAD" && method != "GET" && method != "PUT" && method != "DELETE" {
		return response(405), nil
	}
	// Hash outside the commit lock. The caller owns body until Do returns.
	var etag string
	if method == "PUT" {
		sum := sha256.Sum256(body)
		etag = "\"" + hex.EncodeToString(sum[:16]) + "\""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if s.state.Closed {
		return response(503), nil
	}
	s.state.Requests++
	if key == "" {
		if len(body) != 0 || len(h) != 0 {
			return response(400), nil
		}
		switch method {
		case "PUT":
			s.state.BucketExists = true
			return response(200), nil
		case "HEAD":
			if s.state.BucketExists {
				return response(200), nil
			}
			return response(404), nil
		case "DELETE":
			if len(s.objects) != 0 {
				return response(409), nil
			}
			s.state.BucketExists = false
			return response(204), nil
		default:
			return response(405), nil
		}
	}
	if !s.state.BucketExists {
		return response(404), nil
	}
	old, exists := s.objects[key]
	if match := h.Get("If-Match"); match != "" && (!exists || match != "*" && match != old.etag) {
		return response(412), nil
	}
	if match := h.Get("If-None-Match"); match != "" && exists && (match == "*" || match == old.etag) {
		code := 412
		if method == "GET" || method == "HEAD" {
			code = 304
		}
		r := response(code)
		r.Header.Set("ETag", old.etag)
		return r, nil
	}
	switch method {
	case "PUT":
		next := s.state.Bytes - len(old.body) + len(body)
		if !exists && len(s.objects) >= s.cfg.Objects || next > s.cfg.Bytes {
			return response(507), nil
		}
		metadata := make(http.Header)
		for _, name := range []string{"X-Amz-Meta-Receipt", "X-Amz-Meta-Mode"} {
			if v, ok := h[name]; ok {
				metadata[name] = append([]string(nil), v...)
			}
		}
		s.objects[key] = object{body: bytes.Clone(body), etag: etag, metadata: metadata}
		s.state.Bytes = next
		s.state.Objects = len(s.objects)
		s.state.MaximumBytes = max(s.state.MaximumBytes, next)
		s.state.MaximumObjects = max(s.state.MaximumObjects, s.state.Objects)
		s.state.Writes++
		r := response(200)
		r.Header.Set("ETag", etag)
		return r, nil
	case "DELETE":
		if exists {
			delete(s.objects, key)
			s.state.Bytes -= len(old.body)
			s.state.Objects = len(s.objects)
			s.state.Deletes++
		}
		return response(204), nil
	default:
		if !exists {
			return response(404), nil
		}
		r := Response{Status: 200, Header: old.metadata.Clone()}
		r.Header.Set("ETag", old.etag)
		if method == "GET" {
			r.Body = bytes.Clone(old.body)
		}
		return r, nil
	}
}
