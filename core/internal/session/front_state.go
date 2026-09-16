package session

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"sync"
	b "veil.local/core/internal/behavior"
)

func (f *parallelFront) abort() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.session != nil {
		f.session.Close()
	}
	f.cancel()
}

func (f *parallelFront) claim(r *http.Request) (b.BatchOffer, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	version := f.innerVersion
	if version == 0 {
		version = 1
	}
	var m [32]byte
	var e error
	if f.materialFor != nil {
		m, e = f.materialFor(r.TLS)
	} else {
		m, e = runtimeMaterial(r.TLS, f.program.ID(), version)
	}
	if e != nil {
		return b.BatchOffer{}, 0, e
	}
	if f.session == nil {
		f.material = m
		f.session, e = b.NewBatchSession(f.ctx, f.program, f.seed, m)
		if e != nil {
			return b.BatchOffer{}, 0, e
		}
	} else if f.material != m {
		return b.BatchOffer{}, 0, errors.New("different TLS connection")
	}
	st := f.session.Status()
	if st.Closed {
		return b.BatchOffer{}, 0, errors.New("closed session")
	}
	if f.claimed == nil || st.Completed > f.offer.Sequence {
		f.offer, e = f.session.Plan()
		if e != nil {
			return b.BatchOffer{}, 0, e
		}
		f.claimed = make([]bool, len(f.offer.Members))
		if f.onPlan != nil {
			f.onPlan(f.offer)
		}
	}
	index := -1
	for i, a := range f.offer.Members {
		method, path := "PUT", "/"+f.bucket+"/upload"
		if a.Name == "discover_upload" {
			method = "HEAD"
		}
		if a.Name == "discover_download" || a.Name == "download" {
			method = "GET"
			path = "/" + f.bucket + "/download"
		}
		if r.Method == method && r.URL.Path == path {
			index = i
			break
		}
	}
	if index < 0 || f.claimed[index] {
		return b.BatchOffer{}, 0, errors.New("operation or duplicate request")
	}
	f.claimed[index] = true
	return f.offer, index, nil
}

type parallelFront struct {
	materialFor  func(*tls.ConnectionState) ([32]byte, error)
	innerVersion int
	onPlan       func(b.BatchOffer)
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	program      *b.BatchProgram
	session      *b.BatchSession
	seed         [32]byte
	material     [32]byte
	offer        b.BatchOffer
	claimed      []bool
	active       chan struct{}
	store        *backend
	bucket       string
}
