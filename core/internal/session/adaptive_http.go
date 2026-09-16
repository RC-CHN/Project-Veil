package session

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// The declared response capacity is checked before reading any response body.
// Error replies have a separate 4 KiB diagnostic bound; they never commit a model.
func adaptiveExchange(client *http.Client, req *http.Request, body []byte, label string, capacity int) (result reply, tx transaction, err error) {
	start := time.Now()
	var traceMu sync.Mutex
	var writtenUS, firstByteUS int64
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			traceMu.Lock()
			writtenUS = time.Since(start).Microseconds()
			traceMu.Unlock()
		},
		GotFirstResponseByte: func() {
			traceMu.Lock()
			firstByteUS = time.Since(start).Microseconds()
			traceMu.Unlock()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	tx = transaction{Label: label, Method: req.Method, Path: req.URL.Path, RequestBytes: len(body), RequestSHA256: hashHex(body)}
	defer func() {
		tx.ElapsedUS = time.Since(start).Microseconds()
		traceMu.Lock()
		tx.WriteUS, tx.FirstByteUS = writtenUS, firstByteUS
		traceMu.Unlock()
		if err != nil {
			tx.Error = err.Error()
		}
	}()
	r, err := client.Do(req)
	if err != nil {
		return reply{}, tx, err
	}
	defer r.Body.Close()
	tx.Status, tx.Protocol, tx.ETag = r.StatusCode, r.Proto, r.Header.Get("ETag")
	if r.TLS != nil {
		tx.TLSVersion = r.TLS.Version
	}
	limit := 4096
	minimum := 0
	if r.StatusCode == 200 {
		limit = 0
		if label == "download" || label == "discover_download" {
			minimum = 16
			limit = capacity
		}
		if req.Method != "HEAD" && (r.ContentLength < int64(minimum) || r.ContentLength > int64(limit)) {
			return reply{}, tx, errors.New("response content length outside model budget")
		}
	} else if r.StatusCode == 304 {
		limit = 0
	}
	if limit < 0 || limit > maxObject || r.ContentLength > int64(limit) && req.Method != "HEAD" {
		return reply{}, tx, errors.New("response read bound")
	}
	p, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	tx.ResponseBytes, tx.ResponseSHA256 = len(p), hashHex(p)
	if err == nil && len(p) > limit {
		err = errors.New("response body exceeds budget")
	}
	return reply{r.StatusCode, r.Header.Clone(), p}, tx, err
}
