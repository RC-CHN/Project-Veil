package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, mode string) (*client, *server, *atomic.Int32) {
	t.Helper()
	h := &server{secret: strings.Repeat("s", 32), sessions: make(map[string]*pair)}
	s := httptest.NewUnstartedServer(h)
	s.EnableHTTP2 = true
	s.Config.HTTP2 = h2Config()
	count := new(atomic.Int32)
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			count.Add(1)
		}
	}
	s.StartTLS()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := newClient(config{Mode: mode, Server: s.Listener.Addr().String(), Secret: h.secret, CAFile: path})
	if err != nil {
		t.Fatal(err)
	}
	c.up.TLSClientConfig.ServerName = "example.com"
	c.down.TLSClientConfig.ServerName = "example.com"
	t.Cleanup(func() { h.close(); c.close(); s.Close() })
	return c, h, count
}

func target(t *testing.T, handle func(*net.TCPConn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); c.SetDeadline(time.Now().Add(5 * time.Second)); handle(c.(*net.TCPConn)) }()
		}
	}()
	return l.Addr().String()
}

func open(t *testing.T, c *client, address string) *tunnel {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	p, err := c.open(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func drained(t *testing.T, h *server) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		h.mu.Lock()
		n := len(h.sessions)
		h.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("paired session not released")
}

func TestHalfCloseReuse(t *testing.T) {
	for _, mode := range []string{"shared", "split"} {
		t.Run(mode, func(t *testing.T) {
			c, h, connections := fixture(t, mode)
			address := target(t, func(c *net.TCPConn) {
				b, err := io.ReadAll(c)
				if err == nil {
					c.Write(append([]byte("ack:"), b...))
				}
			})
			for range 4 {
				p := open(t, c, address)
				payload := bytes.Repeat([]byte("sample"), 16000)
				if _, err := p.Write(payload); err != nil {
					t.Fatal(err)
				}
				if err := p.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(p)
				if err != nil || !bytes.Equal(got, append([]byte("ack:"), payload...)) {
					t.Fatalf("half-close/body: %v, bytes=%d", err, len(got))
				}
				p.Close()
				drained(t, h)
			}
			want := int32(1)
			if mode == "split" {
				want = 2
			}
			if connections.Load() != want {
				t.Fatalf("physical TLS connections=%d, want %d", connections.Load(), want)
			}
		})
	}
}

func TestRemoteHalfClose(t *testing.T) {
	c, h, _ := fixture(t, "split")
	got := make(chan []byte, 1)
	address := target(t, func(c *net.TCPConn) { c.Write([]byte("ready")); c.CloseWrite(); b, _ := io.ReadAll(c); got <- b })
	p := open(t, c, address)
	b, err := io.ReadAll(p)
	if err != nil || string(b) != "ready" {
		t.Fatalf("remote FIN: %q %v", b, err)
	}
	if _, err := p.Write([]byte("after FIN")); err != nil {
		t.Fatal(err)
	}
	if err := p.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if string(b) != "after FIN" {
			t.Fatalf("upload: %q", b)
		}
	case <-time.After(time.Second):
		t.Fatal("upload stalled after remote FIN")
	}
	p.Close()
	drained(t, h)
}

func TestResetCancelAndRefused(t *testing.T) {
	for _, mode := range []string{"shared", "split"} {
		t.Run(mode, func(t *testing.T) {
			c, h, _ := fixture(t, mode)
			reset := target(t, func(c *net.TCPConn) { var b [1]byte; c.Read(b[:]); c.SetLinger(0) })
			p := open(t, c, reset)
			p.Write([]byte{1})
			var b [1]byte
			if _, err := p.Read(b[:]); err == nil || err == io.EOF {
				t.Fatalf("RST became clean EOF: %v", err)
			}
			p.Close()
			drained(t, h)
			closed := make(chan struct{}, 1)
			address := target(t, func(c *net.TCPConn) { io.Copy(io.Discard, c); closed <- struct{}{} })
			p = open(t, c, address)
			p.Close()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("cancel did not close target")
			}
			drained(t, h)
			l, _ := net.Listen("tcp", "127.0.0.1:0")
			unavailable := l.Addr().String()
			l.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if p, err := c.open(ctx, unavailable); err == nil {
				p.Close()
				t.Fatal("reported success for refused target")
			}
			drained(t, h)
		})
	}
}

func TestUploadDuringDownloadBackpressure(t *testing.T) {
	for _, mode := range []string{"shared", "split"} {
		t.Run(mode, func(t *testing.T) {
			c, h, _ := fixture(t, mode)
			marker := make(chan byte, 1)
			finished := make(chan struct{}, 1)
			address := target(t, func(c *net.TCPConn) {
				go func() {
					var b [1]byte
					if _, err := io.ReadFull(c, b[:]); err == nil {
						marker <- b[0]
					}
				}()
				c.Write(make([]byte, 16<<20))
				finished <- struct{}{}
			})
			p := open(t, c, address)
			time.Sleep(50 * time.Millisecond)
			if _, err := p.Write([]byte{42}); err != nil {
				t.Fatal(err)
			}
			select {
			case v := <-marker:
				if v != 42 {
					t.Fatal("marker mismatch")
				}
			case <-time.After(time.Second):
				t.Fatal("backpressure blocked upload")
			}
			p.Close()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("cancel did not unblock download")
			}
			drained(t, h)
		})
	}
}
