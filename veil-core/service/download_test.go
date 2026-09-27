package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in because the intentional CDN pause takes 30 seconds. It exercises a
// redirected HTTPS download inside a long-lived REALITY stream, without HF,
// public network traffic, credentials or changes to the deployed service.
func TestLongHTTPSDownloadPause(t *testing.T) {
	if os.Getenv("VEIL_LONG_DOWNLOAD_TEST") != "1" {
		t.Skip("set VEIL_LONG_DOWNLOAD_TEST=1 for the 30-second pause test")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	st, ct := settings(t, "reality")
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, IdleSeconds: 60})
	_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, IdleSeconds: 60})
	block := bytes.Repeat([]byte("Veil-long-download-fixture"), 4096)
	const blocks = 1024
	total := len(block) * blocks
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(total))
		for i := range blocks {
			if i == 64 {
				w.(http.Flusher).Flush()
				t.Log("CDN paused for 30 seconds with the TCP/TLS connection still open")
				select {
				case <-time.After(30 * time.Second):
				case <-r.Context().Done():
					return
				}
			}
			if _, err := w.Write(block); err != nil {
				return
			}
		}
	}))
	defer cdn.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/weights", http.StatusFound)
	}))
	defer origin.Close()
	dir := t.TempDir()
	ca, out := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "weights")
	certs := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cdn.Certificate().Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})...)
	if err := os.WriteFile(ca, certs, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "curl", "--noproxy", "", "--socks5-hostname", entry, "--location",
		"--cacert", ca, "--max-time", "65", "--silent", "--show-error", "--output", out, origin.URL+"/resolve/weights")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("long download: %v: %s", err, output)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, want := sha256.New(), sha256.New()
	n, err := io.Copy(got, f)
	for range blocks {
		want.Write(block)
	}
	if err != nil || n != int64(total) || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatalf("download truncated/corrupted: %d/%d: %v", n, total, err)
	}
	t.Logf("HTTPS redirect + %d-byte single-connection CDN download + 30-second pause: hash matched", n)
}

// A downloader normally leaves its send side open. A remote EOF/RST must still
// reach it promptly, before any application watchdog or the stream idle timer.
func TestCurlPrematureUpstreamClose(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	for _, mode := range []string{"tls", "reality"} {
		for _, reset := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reset=%v", mode, reset), func(t *testing.T) {
				st, ct := settings(t, mode)
				_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, IdleSeconds: 60})
				_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, IdleSeconds: 60})
				dst := target(t, func(c net.Conn) {
					c.SetDeadline(time.Now().Add(3 * time.Second))
					r := bufio.NewReader(c)
					for {
						line, err := r.ReadString('\n')
						if err != nil {
							return
						}
						if line == "\r\n" {
							break
						}
					}
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 1048576\r\n\r\n")
					writeAll(c, bytes.Repeat([]byte{'x'}, 64*1024))
					time.Sleep(10 * time.Millisecond)
					if reset {
						c.(*net.TCPConn).SetLinger(0)
					}
				})
				cmd := exec.Command("curl", "--noproxy", "", "--socks5-hostname", entry, "--max-time", "3",
					"--silent", "--show-error", "--output", "/dev/null", "http://"+dst+"/large-file")
				out, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || (exit.ExitCode() != 18 && exit.ExitCode() != 56) {
					t.Fatalf("expected immediate truncated/reset download, got %v: %s", err, out)
				}
			})
		}
	}
}

func TestDownloadActivityAndPause(t *testing.T) {
	st, ct := settings(t, "tls")
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, IdleSeconds: 1})
	_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, IdleSeconds: 1})
	dst := target(t, func(c net.Conn) {
		// Only the downstream is active, for longer than the one-second timeout.
		for range 8 {
			if err := writeAll(c, bytes.Repeat([]byte{'x'}, 1024)); err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		// A below-budget CDN pause must not be mistaken for EOF or timeout.
		time.Sleep(350 * time.Millisecond)
		writeAll(c, []byte("tail"))
	})
	c, err := socksDial(entry, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil || len(b) != 8*1024+4 || !bytes.HasSuffix(b, []byte("tail")) {
		t.Fatalf("one-way activity or pause interrupted download: len=%d err=%v", len(b), err)
	}
}

func TestReverseTrafficWhileDownloadBlocked(t *testing.T) {
	st, ct := settings(t, "tls")
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr})
	received := make(chan string, 1)
	var sent atomic.Int64
	dst := target(t, func(c net.Conn) {
		c.(*net.TCPConn).SetWriteBuffer(4096)
		done := make(chan struct{})
		go func() {
			defer close(done)
			b := make([]byte, 64*1024)
			for range 1024 {
				n, err := c.Write(b)
				sent.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()
		c.SetReadDeadline(time.Now().Add(4 * time.Second))
		b := make([]byte, 4)
		_, err := io.ReadFull(c, b)
		if err == nil {
			received <- string(b)
		}
		c.Close()
		<-done
	})
	c, err := socksDial(entry, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadBuffer(4096)
	c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	await(t, func() bool { return sent.Load() > 0 })
	// Wait for the downstream writer to stop progressing under backpressure.
	previous := int64(-1)
	deadline := time.Now().Add(2 * time.Second)
	for {
		time.Sleep(100 * time.Millisecond)
		n := sent.Load()
		if n == previous {
			break
		}
		previous = n
		if time.Now().After(deadline) {
			t.Fatal("download did not reach backpressure")
		}
	}
	if err := writeAll(c, []byte("STOP")); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-received:
		if s != "STOP" {
			t.Fatal("reverse data corrupted")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked download starved the upload pump")
	}
}
