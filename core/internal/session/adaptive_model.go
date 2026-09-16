package session

import (
	"errors"
	"net/http"
	"strconv"
	b "veil.local/core/internal/behavior"
)

const streamBudget = 16 + 65536*5/4

// These are complete, individually validated adapter models, not arbitrary
// caller-supplied capacity bounds. Existing generated bundles keep their ID.
const (
	ProfileAdaptive64 = "adaptive64-v1"
	ProfileBulk192    = "bulk192-v1"
)

func bulk192Model() b.BatchModel {
	m := adaptiveModel()
	m.Stages[1].Actions[0].Request[3].Min = 16 + (128<<10)*5/4
	m.Stages[1].Actions[0].Request[3].Max = 16 + (192<<10)*5/4
	m.Stages[1].Actions[1].Replies[0].Fields[1].Min = 16 + (96<<10)*5/4
	m.Stages[1].Actions[1].Replies[0].Fields[1].Max = 16 + (192<<10)*5/4
	return m
}

func adaptiveModel() b.BatchModel {
	d := func(name string) b.Field { return b.Field{Name: name, Kind: "digest"} }
	u := func(name string) b.Field { return b.Field{Name: name, Kind: "u64"} }
	data := func(size int) b.Field { return b.Field{Name: "object", Kind: "bytes", Min: size, Max: size} }
	constant := func(n uint64) b.Expr { return b.Expr{Scope: "constant", Number: n} }
	up := b.BatchReply{Code: 200, Fields: []b.Field{d("etag")}, Assign: []b.Assign{assign(0, expr("response", 0)), assign(3, expr("request", 4)), assign(5, b.Expr{Scope: "register", Index: 5, Op: "inc"})}}
	down := b.BatchReply{Code: 200, Fields: []b.Field{d("etag"), data(streamBudget), u("finished"), u("sequence")}, Checks: []b.Equal{eq(expr("response", 3), expr("register", 5))}, Assign: []b.Assign{assign(1, expr("response", 0)), assign(2, b.Expr{Scope: "response", Index: 1, Op: "sha256"}), assign(4, expr("response", 2))}, Minimums: []b.BatchMinimum{{Field: 1, Min: 16}}}
	bootDown := down
	bootDown.Fields = []b.Field{d("etag"), data(16), u("finished"), u("sequence")}
	bootDown.Minimums = nil
	m := b.BatchModel{Version: 2, States: 3, MaxBatches: 4096, LifetimeMS: 600000, Registers: []b.Register{{Name: "upload_etag", Kind: "digest", Initial: b.Value{Bytes: make([]byte, 32)}}, {Name: "download_etag", Kind: "digest", Initial: b.Value{Bytes: make([]byte, 32)}}, {Name: "download_receipt", Kind: "digest", Initial: b.Value{Bytes: make([]byte, 32)}}, {Name: "upload_finished", Kind: "u64"}, {Name: "download_finished", Kind: "u64"}, {Name: "sequence", Kind: "u64"}}}
	m.Stages = []b.BatchStage{
		{Name: "discover", From: 0, To: 1, Weight: 1, Actions: []b.BatchAction{{Name: "discover_upload", Replies: []b.BatchReply{{Code: 200, Fields: []b.Field{d("etag")}, Assign: []b.Assign{assign(0, expr("response", 0)), assign(5, constant(1))}}}}, {Name: "discover_download", Replies: []b.BatchReply{bootDown}}}},
		{Name: "transfer", From: 1, To: 0, Weight: 1, Actions: []b.BatchAction{
			{Name: "upload", Request: []b.Field{d("condition"), d("receipt"), u("mode"), data(streamBudget), u("finished"), u("sequence")}, RequestMinimums: []b.BatchMinimum{{Field: 3, Min: 16}}, Checks: []b.Equal{eq(expr("request", 0), expr("register", 0)), eq(expr("request", 1), expr("register", 2)), eq(expr("request", 5), expr("register", 5))}, Replies: []b.BatchReply{up}},
			{Name: "download", Request: []b.Field{d("condition")}, Checks: []b.Equal{eq(expr("request", 0), expr("register", 1))}, Replies: []b.BatchReply{down, {Code: 304}}, ResponseAfter: []int{0}},
		}, Branch: &b.BatchBranch{Action: 0, Field: 2, Cases: []b.BatchCase{{Code: 0, To: 1}, {Code: 1, To: 2, Checks: []b.Equal{eq(expr("register", 3), constant(1)), eq(expr("register", 4), constant(1)), eq(expr("request", 4), constant(1))}}}}},
	}
	m.Stages[1].Actions[0].Request[3].Min = 16 + 32768*5/4
	m.Stages[1].Actions[1].Replies[0].Fields[1].Min = 16 + 16384*5/4
	return m
}
func adaptiveRequest(name string, r *http.Request, body []byte) ([]b.Value, error) {
	if name != "upload" {
		return parallelRequest(name, r, body)
	}
	base, e := parallelRequest(name, r, body)
	if e != nil {
		return nil, e
	}
	mode, e := strconv.ParseUint(r.Header.Get("X-Amz-Meta-Mode"), 10, 64)
	if e != nil {
		return nil, e
	}
	seq, _, eof, e := streamDecode(body)
	if e != nil {
		return nil, e
	}
	fin := uint64(0)
	if eof {
		fin = 1
	}
	return []b.Value{base[0], base[1], {Number: mode}, {Bytes: body}, {Number: fin}, {Number: seq}}, nil
}
func adaptiveResponse(name string, r reply) ([]b.Value, error) {
	if (name == "download" || name == "discover_download") && r.status == 200 {
		if !objectETag.MatchString(r.header.Get("ETag")) {
			return nil, errors.New("stream response etag")
		}
		seq, _, eof, e := streamDecode(r.body)
		if e != nil {
			return nil, e
		}
		fin := uint64(0)
		if eof {
			fin = 1
		}
		return []b.Value{dig([]byte(r.header.Get("ETag"))), {Bytes: r.body}, {Number: fin}, {Number: seq}}, nil
	}
	return parallelResponse(name, r)
}
