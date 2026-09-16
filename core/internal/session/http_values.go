package session

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	b "veil.local/core/internal/behavior"
)

func parallelMaterial(t *tls.ConnectionState, id string) ([32]byte, error) {
	var m [32]byte
	if t == nil || t.Version != tls.VersionTLS13 || t.NegotiatedProtocol != "h2" {
		return m, errors.New("expected TLS1.3/HTTP2")
	}
	p, e := t.ExportKeyingMaterial("EXPORTER-veil-object-session-v1", []byte(id), 32)
	copy(m[:], p)
	return m, e
}

func parallelRequest(name string, r *http.Request, body []byte) ([]b.Value, error) {
	switch name {
	case "upload", "acknowledge":
		if !objectETag.MatchString(r.Header.Get("If-Match")) {
			return nil, errors.New("if-match format")
		}
		receipt, e := hex.DecodeString(r.Header.Get("X-Amz-Meta-Receipt"))
		if e != nil || len(receipt) != 32 {
			return nil, errors.New("receipt metadata format")
		}
		if name == "acknowledge" {
			if !bytes.Equal(receipt, body) {
				return nil, errors.New("final receipt body")
			}
			return []b.Value{dig([]byte(r.Header.Get("If-Match"))), {Bytes: body}}, nil
		}
		return []b.Value{dig([]byte(r.Header.Get("If-Match"))), {Bytes: receipt}, {Bytes: body}}, nil
	case "download":
		if !objectETag.MatchString(r.Header.Get("If-None-Match")) {
			return nil, errors.New("if-none-match format")
		}
		return []b.Value{dig([]byte(r.Header.Get("If-None-Match")))}, nil
	}
	return nil, nil
}

func parallelResponse(name string, r reply) ([]b.Value, error) {
	if r.status == 304 && name == "download" {
		if len(r.body) != 0 {
			return nil, errors.New("304 body")
		}
		return nil, nil
	}
	if r.status != 200 {
		return nil, fmt.Errorf("terminal native status %d", r.status)
	}
	if !objectETag.MatchString(r.header.Get("ETag")) {
		return nil, errors.New("response etag format")
	}
	if name == "download" || name == "discover_download" {
		if _, _, _, e := streamDecode(r.body); e != nil {
			return nil, e
		}
		return []b.Value{dig([]byte(r.header.Get("ETag"))), {Bytes: r.body}}, nil
	}
	if len(r.body) != 0 {
		return nil, errors.New("unexpected upload/head body")
	}
	return []b.Value{dig([]byte(r.header.Get("ETag")))}, nil
}

type parallelEvent struct {
	Sequence                                                              uint64
	Member                                                                int
	Action                                                                string
	AcceptedNS, NativeStartNS, ResponseStartNS, ValidatedNS, HandlerEndNS int64
	QueueWaitUS                                                           int64
	NativeStatus                                                          int
	Receipt                                                               string `json:",omitempty"`
	Error                                                                 string `json:",omitempty"`
}

func writeParallelReply(w http.ResponseWriter, method string, r reply) {
	for k, vs := range r.header {
		if k == "Content-Length" || k == "Connection" || k == "Transfer-Encoding" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if method == "HEAD" {
		w.Header().Set("Content-Length", r.header.Get("Content-Length"))
	} else if r.status != 304 {
		w.Header().Set("Content-Length", fmt.Sprint(len(r.body)))
	}
	w.WriteHeader(r.status)
	if method != "HEAD" && r.status != 304 {
		_, _ = w.Write(r.body)
	}
}
