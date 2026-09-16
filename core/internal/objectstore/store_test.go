package objectstore

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

func owned(t *testing.T, objects, capacity int) *Store {
	t.Helper()
	s, e := New(Config{Bucket: "owned-test", Objects: objects, Bytes: capacity})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	r, e := s.Do(context.Background(), "PUT", "/owned-test", nil, nil)
	if e != nil || r.Status != 200 {
		t.Fatal(r, e)
	}
	return s
}
func call(t *testing.T, s *Store, method, key string, p []byte, h http.Header, status int) Response {
	t.Helper()
	r, e := s.Do(context.Background(), method, "/owned-test/"+key, p, h)
	if e != nil || r.Status != status {
		t.Fatalf("%s %s: %+v %v; want %d", method, key, r, e, status)
	}
	return r
}
func TestConditionalObjectsAndIndependentSnapshots(t *testing.T) {
	s := owned(t, 2, 16)
	input := []byte("abc")
	h := http.Header{"If-None-Match": []string{"*"}, "X-Amz-Meta-Receipt": []string{"receipt"}}
	r := call(t, s, "PUT", "prefix/upload", input, h, 200)
	if r.Header.Get("ETag") != "\"ba7816bf8f01cfea414140de5dae2223\"" {
		t.Fatal(r.Header)
	}
	input[0] = 'z'
	h.Set("X-Amz-Meta-Receipt", "changed")
	read := call(t, s, "GET", "prefix/upload", nil, nil, 200)
	if string(read.Body) != "abc" || read.Header.Get("X-Amz-Meta-Receipt") != "receipt" {
		t.Fatal(read)
	}
	read.Body[0] = 'x'
	read.Header.Set("X-Amz-Meta-Receipt", "mutated")
	read = call(t, s, "GET", "prefix/upload", nil, nil, 200)
	if string(read.Body) != "abc" || read.Header.Get("X-Amz-Meta-Receipt") != "receipt" {
		t.Fatal("snapshot aliased", read)
	}
	call(t, s, "PUT", "prefix/upload", []byte("bad"), http.Header{"If-None-Match": []string{"*"}}, 412)
	condition := http.Header{"If-Match": []string{r.Header.Get("ETag")}}
	newObject := call(t, s, "PUT", "prefix/upload", []byte("new"), condition, 200)
	call(t, s, "PUT", "prefix/upload", []byte("old"), condition, 412)
	read = call(t, s, "GET", "prefix/upload", nil, nil, 200)
	if string(read.Body) != "new" || read.Header.Get("X-Amz-Meta-Receipt") != "" {
		t.Fatal("failed mutation or metadata replacement", read)
	}
	for _, method := range []string{"GET", "HEAD"} {
		r := call(t, s, method, "prefix/upload", nil, http.Header{"If-None-Match": []string{newObject.Header.Get("ETag")}}, 304)
		if len(r.Body) != 0 {
			t.Fatal("304 body")
		}
	}
	call(t, s, "PUT", "prefix/empty", nil, nil, 200)
	call(t, s, "GET", "prefix/empty", nil, nil, 200)
	call(t, s, "DELETE", "prefix/upload", nil, condition, 412)
	call(t, s, "DELETE", "prefix/upload", nil, nil, 204)
	call(t, s, "DELETE", "prefix/upload", nil, nil, 204)
	call(t, s, "GET", "prefix/upload", nil, nil, 404)
	call(t, s, "PUT", "prefix/upload", nil, condition, 412)
}
func TestCapacityAndInvalidRequestsDoNotMutate(t *testing.T) {
	s := owned(t, 2, 7)
	call(t, s, "PUT", "a", []byte("1234"), nil, 200)
	call(t, s, "PUT", "b", []byte("567"), nil, 200)
	call(t, s, "PUT", "c", nil, nil, 507)
	call(t, s, "PUT", "a", []byte("12345"), nil, 507)
	for _, key := range []string{"a/../x", "../x", "a//x", "a/./x", "a/", "a?query", "a%2fb", "/a", "A", "a\x00"} {
		call(t, s, "PUT", key, []byte("x"), nil, 400)
	}
	for _, h := range []http.Header{
		{"If-Match": []string{"*", "*"}}, {"If-Match": []string{"*"}, "if-match": []string{"*"}},
		{"If-Match": []string{"*"}, "If-None-Match": []string{"*"}}, {"If-Match": []string{"W/\"123\""}},
		{"X-Amz-Meta-Receipt": []string{"bad\r\nheader"}}, {"X-Amz-Meta-Mode": []string{string(bytes.Repeat([]byte{'x'}, 129))}},
	} {
		call(t, s, "PUT", "a", []byte("x"), h, 400)
	}
	call(t, s, "PUT", "a", make([]byte, MaxObject+1), nil, 413)
	call(t, s, "GET", "a", []byte("x"), nil, 413)
	call(t, s, "POST", "a", nil, nil, 405)
	if st := s.Status(); st.Bytes != 7 || st.Objects != 2 || st.Writes != 2 || st.MaximumBytes != 7 || st.MaximumObjects != 2 {
		t.Fatal(st)
	}
	if string(call(t, s, "GET", "a", nil, nil, 200).Body) != "1234" {
		t.Fatal("invalid requests changed object")
	}
	call(t, s, "PUT", "a", []byte("x"), nil, 200)
	call(t, s, "DELETE", "b", nil, nil, 204)
	call(t, s, "PUT", "c", []byte("abcdef"), nil, 200)
	if s.Status().Bytes != 7 {
		t.Fatal(s.Status())
	}
}
func TestConcurrentConditionalReplaceHasOneWinner(t *testing.T) {
	s := owned(t, 1, 64)
	r := call(t, s, "PUT", "key", []byte("base"), nil, 200)
	condition := r.Header.Get("ETag")
	start := make(chan struct{})
	results := make(chan int, 16)
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			r, e := s.Do(context.Background(), "PUT", "/owned-test/key", []byte{byte(i)}, http.Header{"If-Match": []string{condition}})
			if e != nil {
				results <- 0
			} else {
				results <- r.Status
			}
		}(i)
	}
	close(start)
	workers.Wait()
	close(results)
	success, failed := 0, 0
	for code := range results {
		if code == 200 {
			success++
		} else if code == 412 {
			failed++
		} else {
			t.Fatal(code)
		}
	}
	if success != 1 || failed != 15 || s.Status().Writes != 2 || s.Status().Bytes != 1 {
		t.Fatal(success, failed, s.Status())
	}
}
func TestCancellationAndCloseKeepCapacity(t *testing.T) {
	s := owned(t, 1, MaxObject)
	call(t, s, "PUT", "key", make([]byte, MaxObject), nil, 200)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.Do(ctx, "DELETE", "/owned-test/key", nil, nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if s.Status().Bytes != MaxObject {
		t.Fatal("cancelled delete committed")
	}
	s.Close()
	s.Close()
	st := s.Status()
	if !st.Closed || st.Objects != 0 || st.Bytes != 0 || st.MaximumBytes != MaxObject {
		t.Fatal(st)
	}
	call(t, s, "GET", "key", nil, nil, 503)
	call(t, s, "PUT", "key", nil, nil, 503)
}
func FuzzBoundedObjectOperations(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte("PUT/GET conditional object"))
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 128 {
			return
		}
		s := owned(t, 4, 64)
		keys := []string{"a", "b", "c", "d", "e", "bad/../key"}
		methods := []string{"PUT", "GET", "HEAD", "DELETE"}
		for i, v := range ops {
			method := methods[int(v)%len(methods)]
			key := keys[int(v>>2)%len(keys)]
			var p []byte
			if method == "PUT" {
				p = bytes.Repeat([]byte{byte(i)}, int(v>>3))
			}
			h := make(http.Header)
			if v&64 != 0 {
				h.Set("If-None-Match", "*")
			}
			_, e := s.Do(context.Background(), method, "/owned-test/"+key, p, h)
			if e != nil {
				t.Fatal(e)
			}
			st := s.Status()
			if st.Objects > 4 || st.Bytes > 64 || st.Objects < 0 || st.Bytes < 0 {
				t.Fatal(st)
			}
			total, count := 0, 0
			for _, k := range keys[:5] {
				r, e := s.Do(context.Background(), "GET", "/owned-test/"+k, nil, nil)
				if e != nil {
					t.Fatal(e)
				}
				if r.Status == 200 {
					total += len(r.Body)
					count++
				}
			}
			if total != st.Bytes || count != st.Objects {
				t.Fatal("accounting differs from snapshots", total, count, st)
			}
		}
	})
}
