package session

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

func sessionPrefix(bucket string, seed, material [32]byte) string {
	h := hmac.New(sha256.New, seed[:])
	h.Write([]byte("veil-object-prefix-1\x00"))
	h.Write(material[:])
	return bucket + "/" + hex.EncodeToString(h.Sum(nil)[:16])
}
func ensureBucket(ctx context.Context, b *backend, bucket string) error {
	r, e := b.do(ctx, "bucket_head", "HEAD", "/"+bucket, nil, nil)
	if e != nil {
		return e
	}
	if r.status == 200 {
		return nil
	}
	if r.status != 404 {
		return errors.New("backend bucket not accessible")
	}
	r, e = b.do(ctx, "bucket_create", "PUT", "/"+bucket, nil, nil)
	if e != nil {
		return e
	}
	if r.status == 200 {
		return nil
	}
	return errors.New("backend bucket creation failed")
}
func prepareObjects(ctx context.Context, b *backend, prefix string, created *[2]bool) error {
	for i, key := range []string{"upload", "download"} {
		body := []byte("initial")
		if i == 1 {
			body = streamEncode(0, nil, false)
		}
		created[i] = true // Unknown write outcomes still require cleanup of this exclusive prefix.
		r, e := b.do(ctx, "session_initial_"+key, "PUT", "/"+prefix+"/"+key, body, http.Header{"If-None-Match": []string{"*"}})
		if e != nil {
			return e
		}
		if r.status != 200 {
			if r.status == 412 {
				created[i] = false
			}
			return errors.New("session object create conflict or failure")
		}
		created[i] = true
	}
	return nil
}
func deleteObjects(b *backend, prefix string, created [2]bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok := true
	for i, key := range []string{"upload", "download"} {
		if created[i] {
			_, e := b.must(ctx, "session_delete_"+key, "DELETE", "/"+prefix+"/"+key, nil, nil, 204)
			ok = ok && e == nil
		}
	}
	return ok
}
func observeIdle(ctx context.Context, cancel context.CancelFunc, source *openSource, limit time.Duration) func() {
	child, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(min(time.Second, limit/2))
		defer ticker.Stop()
		last := time.Now()
		var total uint64
		established := false
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
				st := source.handshake.Status()
				if !st.Established {
					last = time.Now()
					continue
				}
				s := source.linkStatus()
				next := s.Sent + s.Received
				if !established || next != total {
					last = time.Now()
					established = true
					total = next
				}
				if time.Since(last) >= limit {
					cancel()
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }
}
func errorText(e error) string {
	if e == nil {
		return ""
	}
	s := e.Error()
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

type cappedListener struct {
	net.Listener
	slots  chan struct{}
	reject func()
}
type socketCounted struct {
	net.Conn
	release func()
}

// Close interrupts socket I/O but retains the connection work permit.
func (c *socketCounted) Close() error { return c.Conn.Close() }

func releaseAccepted(c net.Conn) {
	if tlsConn, ok := c.(*tls.Conn); ok {
		c = tlsConn.NetConn()
	}
	if counted, ok := c.(*socketCounted); ok {
		counted.release()
	}
}

func (l *cappedListener) Accept() (net.Conn, error) {
	for {
		c, e := l.Listener.Accept()
		if e != nil {
			return nil, e
		}
		select {
		case l.slots <- struct{}{}:
			var once sync.Once
			return &socketCounted{Conn: c, release: func() { once.Do(func() { <-l.slots }) }}, nil
		default:
			c.Close()
			if l.reject != nil {
				l.reject()
			}
		}
	}
}
