package service

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"veil/internal/mux"
	"veil/internal/transport"
	"veil/internal/wire"
)

func certs(t *testing.T) (string, string) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cover.test"}, DNSNames: []string{"cover.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	priv, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}), 0600)
	return cp, kp
}
func cover(t *testing.T, cp, kp string) string {
	t.Helper()
	return coverGroup(t, cp, kp, "X25519")
}

func coverGroup(t *testing.T, cp, kp, group string) string {
	t.Helper()
	if _, e := exec.LookPath("openssl"); e != nil {
		t.Skip("openssl required for REALITY integration")
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command("openssl", "s_server", "-accept", addr, "-cert", cp, "-key", kp, "-tls1_3", "-ciphersuites", "TLS_AES_128_GCM_SHA256", "-groups", group, "-www", "-alpn", "http/1.1")
	var log bytes.Buffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for range 100 {
		c, e := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if e == nil {
			c.Close()
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cover did not start")
	return ""
}
func settings(t *testing.T, mode string) (transport.Settings, transport.Settings) {
	cp, kp := certs(t)
	s := transport.Settings{Mode: mode, ServerName: "cover.test", Certificate: cp, PrivateKeyFile: kp}
	c := transport.Settings{Mode: mode, ServerName: "cover.test", CAFile: cp}
	if mode == "reality" {
		key, e := ecdh.X25519().GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		id := make([]byte, 8)
		rand.Read(id)
		s.RealityPrivateKey = base64.RawURLEncoding.EncodeToString(key.Bytes())
		c.RealityPublicKey = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
		s.ShortID = hex.EncodeToString(id)
		c.ShortID = s.ShortID
		s.CoverAddress = cover(t, cp, kp)
	}
	return s, c
}
func start(t *testing.T, cfg Config) (*Service, string) {
	t.Helper()
	svc, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	svc.OnError = func(err error) { t.Log(err) }
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(3 * time.Second):
			t.Error("service did not shut down")
		}
	})
	return svc, ln.Addr().String()
}
func socksDial(addr, target string) (*net.TCPConn, error) {
	c, e := net.DialTimeout("tcp", addr, time.Second)
	if e != nil {
		return nil, e
	}
	raw := c.(*net.TCPConn)
	raw.SetDeadline(time.Now().Add(8 * time.Second))
	fail := func(e error) (*net.TCPConn, error) { raw.Close(); return nil, e }
	if _, e = raw.Write([]byte{5, 1, 0}); e != nil {
		return fail(e)
	}
	var method [2]byte
	if _, e = io.ReadFull(raw, method[:]); e != nil {
		return fail(e)
	}
	if method != [2]byte{5, 0} {
		return fail(fmt.Errorf("SOCKS method %x", method))
	}
	p, e := wire.EncodeAddress(target)
	if e != nil {
		return fail(e)
	}
	if _, e = raw.Write(append([]byte{5, 1, 0}, p...)); e != nil {
		return fail(e)
	}
	var reply [10]byte
	if _, e = io.ReadFull(raw, reply[:]); e != nil {
		return fail(e)
	}
	if reply[1] != 0 {
		return fail(fmt.Errorf("SOCKS error %d", reply[1]))
	}
	return raw, nil
}
func target(t *testing.T, fn func(net.Conn)) string {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); defer c.Close(); c.SetDeadline(time.Now().Add(8 * time.Second)); fn(c) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done; wg.Wait() })
	return ln.Addr().String()
}

var testKey = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{73}, 32))

func TestEndToEndHalfCloseReuse(t *testing.T) {
	for _, name := range []string{"tls", "reality", "chrome120", "chrome131", "chrome133", "chrome149", "pool", "padding"} {
		t.Run(name, func(t *testing.T) {
			mode := "reality"
			if name == "tls" {
				mode = "tls"
			}
			st, ct := settings(t, mode)
			switch name {
			case "chrome120", "chrome131", "chrome133", "chrome149":
				ct.Fingerprint = name
			case "pool":
				ct.Fingerprints = []string{"chrome120", "chrome131", "chrome133", "chrome149"}
			case "padding":
				ct.Fingerprint = "chrome149"
				ct.RecordPadding, st.RecordPadding = true, true
			}
			server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HandshakeSeconds: 3, IdleSeconds: 3})
			client, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, HandshakeSeconds: 3, IdleSeconds: 3})
			dst := target(t, func(c net.Conn) {
				b, e := io.ReadAll(c)
				if e != nil {
					return
				}
				writeAll(c, b)
			})
			payload := bytes.Repeat([]byte("half-close integrity\x00"), 20000)
			for i := 0; i < 3; i++ {
				c, e := socksDial(entry, dst)
				if e != nil {
					t.Fatal(e)
				}
				if e = writeAll(c, payload); e != nil {
					t.Fatal(e)
				}
				if e = c.CloseWrite(); e != nil {
					t.Fatal(e)
				}
				b, e := io.ReadAll(c)
				c.Close()
				if e != nil || !bytes.Equal(b, payload) {
					t.Fatalf("round %d bytes=%d error=%v", i, len(b), e)
				}
				deadline := time.Now().Add(time.Second)
				for client.Stats.Completed.Load() < uint64(i+1) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			if server.Stats.Authenticated.Load() != 1 {
				t.Fatalf("expected reuse, authenticated=%d failures=%d", server.Stats.Authenticated.Load(), server.Stats.Failed.Load())
			}
			if client.Stats.Completed.Load() != 3 {
				t.Fatalf("completion barrier: %d", client.Stats.Completed.Load())
			}
		})
	}
}
func TestConcurrentEchoAndWrongSecret(t *testing.T) {
	st, ct := settings(t, "tls")
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HandshakeSeconds: 2, IdleSeconds: 2})
	_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, HandshakeSeconds: 2, IdleSeconds: 2})
	dst := target(t, func(c net.Conn) { io.Copy(c, c) })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := socksDial(entry, dst)
			if e != nil {
				t.Error(e)
				return
			}
			defer c.Close()
			p := bytes.Repeat([]byte{4}, 4096)
			for range 10 {
				if e = writeAll(c, p); e != nil {
					t.Error(e)
					return
				}
				got := make([]byte, len(p))
				if _, e = io.ReadFull(c, got); e != nil {
					t.Error(e)
					return
				}
				if !bytes.Equal(p, got) {
					t.Error("corrupted echo")
					return
				}
			}
			c.CloseWrite()
			io.Copy(io.Discard, c)
		}()
	}
	wg.Wait()
	bad := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	_, badentry := start(t, Config{Role: "client", Secret: bad, TLS: ct, Server: addr, HandshakeSeconds: 2})
	if c, e := socksDial(badentry, dst); e == nil {
		c.Close()
		t.Fatal("wrong secret accepted")
	}
}
func TestIdleAndConnectionLimit(t *testing.T) {
	st, _ := settings(t, "tls")
	s, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, MaxConnections: 1, MaxIdle: 1, HandshakeSeconds: 1})
	a, e := net.Dial("tcp", addr)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	time.Sleep(30 * time.Millisecond)
	b, e := net.Dial("tcp", addr)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	b.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, e = b.Read(one[:]); e == nil {
		t.Fatal("limit not enforced")
	}
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, e = a.Read(one[:]); e == nil {
		t.Fatal("handshake timeout not enforced")
	}
	if s.Stats.Rejected.Load() != 1 {
		t.Fatal("rejection not accounted")
	}
}

func directTLS(t *testing.T, ct transport.Settings, addr string) net.Conn {
	t.Helper()
	h, e := transport.Client(ct)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := net.DialTimeout("tcp", addr, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	raw.SetDeadline(time.Now().Add(3 * time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, e := h(ctx, raw)
	if e != nil {
		raw.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func TestReplayAndStateRejection(t *testing.T) {
	st, ct := settings(t, "reality")
	server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, HandshakeSeconds: 2})
	c := directTLS(t, ct, addr)
	exporter, e := transport.Export(c)
	if e != nil {
		t.Fatal(e)
	}
	key, _ := transport.DecodeKey(testKey)
	proof, _ := wire.AuthPayload(key, exporter)
	var accepted atomic.Int32
	dst := target(t, func(c net.Conn) { accepted.Add(1); io.Copy(io.Discard, c) })
	address, _ := wire.EncodeAddress(dst)
	var prefix bytes.Buffer
	wire.Write(&prefix, wire.Auth, proof)
	m, e := mux.New(c, mux.Options{Profile: mux.DefaultProfile(), Prefix: prefix.Bytes()})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, e := m.Open(ctx, address)
	if e != nil {
		t.Fatal(e)
	}
	stream.Close()
	m.Close()
	m.Wait()
	c2 := directTLS(t, ct, addr)
	replay, e := mux.New(c2, mux.Options{Profile: mux.DefaultProfile(), Prefix: prefix.Bytes()})
	if e != nil {
		t.Fatal(e)
	}
	if stream, e = replay.Open(ctx, address); e == nil {
		stream.Close()
		t.Fatal("cross-connection proof replay accepted")
	}
	replay.Close()
	replay.Wait()
	c3 := directTLS(t, ct, addr)
	ekm, e := transport.Export(c3)
	if e != nil {
		t.Fatal(e)
	}
	proof, _ = wire.AuthPayload(key, ekm)
	wire.Write(c3, wire.Auth, proof)
	// A DATA frame for a never-opened stream must fail before any allocation.
	c3.Write([]byte{4, 0, 0, 0, 1, 0, 0, 1, 42})
	c3.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, e = c3.Read(one[:]); e == nil {
		t.Fatal("DATA before OPEN accepted")
	}
	if server.Stats.Authenticated.Load() != 2 {
		t.Fatal("authentication accounting")
	}
	if accepted.Load() != 1 {
		t.Fatalf("replayed or invalid request reached target: %d", accepted.Load())
	}
}

func TestOpenFailureDoesNotReportSOCKSSuccess(t *testing.T) {
	st, ct := settings(t, "tls")
	server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	client, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dst := ln.Addr().String()
	ln.Close()
	if c, err := socksDial(entry, dst); err == nil {
		c.Close()
		t.Fatal("SOCKS success before target connected")
	}
	idle := client.client.PoolStats().Idle
	if idle != 1 {
		t.Fatal("failed logical OPEN destroyed the physical connection")
	}
	good := target(t, func(c net.Conn) { p, _ := io.ReadAll(c); writeAll(c, p) })
	c, err := socksDial(entry, good)
	if err != nil {
		t.Fatal(err)
	}
	halfEchoPayload(t, c, []byte("after a failed destination"))
	if server.Stats.Authenticated.Load() != 1 {
		t.Fatal("dial failure prevented physical reuse")
	}
}
func TestActiveIdleTimeout(t *testing.T) {
	st, ct := settings(t, "tls")
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, IdleSeconds: 1})
	_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, IdleSeconds: 1})
	dst := target(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	c, e := socksDial(entry, dst)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b [1]byte
	_, e = c.Read(b[:])
	if e == nil {
		t.Fatal("idle stream remained open")
	}
	if ne, ok := e.(net.Error); ok && ne.Timeout() {
		t.Fatal("client deadline fired before protocol idle limit")
	}
}
func TestPoolExpiry(t *testing.T) {
	st, ct := settings(t, "tls")
	server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	client, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, PoolSeconds: 1})
	dst := target(t, func(c net.Conn) { io.Copy(c, c) })
	c, e := socksDial(entry, dst)
	if e != nil {
		t.Fatal(e)
	}
	c.CloseWrite()
	io.Copy(io.Discard, c)
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n := client.client.PoolStats().Total
		if n == 0 && client.Stats.Completed.Load() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	n := client.client.PoolStats().Total
	if n != 0 {
		t.Fatal("idle connection not reclaimed")
	}
	c, e = socksDial(entry, dst)
	if e != nil {
		t.Fatal(e)
	}
	c.CloseWrite()
	io.Copy(io.Discard, c)
	c.Close()
	if server.Stats.Authenticated.Load() != 2 {
		t.Fatal("expired connection reused")
	}
}

func TestSlowConsumerIsCancelled(t *testing.T) {
	st, ct := settings(t, "tls")
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st, IdleSeconds: 1})
	client, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr, IdleSeconds: 1})
	dst := target(t, func(c net.Conn) {
		b := make([]byte, 128*1024)
		for range 1024 {
			if e := writeAll(c, b); e != nil {
				return
			}
		}
	})
	c, e := socksDial(entry, dst)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	end := time.Now().Add(4 * time.Second)
	for client.Stats.Failed.Load() == 0 && time.Now().Before(end) {
		time.Sleep(20 * time.Millisecond)
	}
	if client.Stats.Failed.Load() == 0 {
		t.Fatal("slow consumer did not cancel blocked transfer")
	}
}
func TestConcurrentReality(t *testing.T) {
	st, ct := settings(t, "reality")
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	_, entry := start(t, Config{Role: "client", Secret: testKey, TLS: ct, Server: addr})
	dst := target(t, func(c net.Conn) { io.Copy(c, c) })
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := socksDial(entry, dst)
			if e != nil {
				t.Error(e)
				return
			}
			defer c.Close()
			p := []byte("concurrent REALITY")
			if e = writeAll(c, p); e != nil {
				t.Error(e)
				return
			}
			out := make([]byte, len(p))
			if _, e = io.ReadFull(c, out); e != nil || !bytes.Equal(out, p) {
				t.Error("concurrent echo", e)
			}
			c.CloseWrite()
			io.Copy(io.Discard, c)
		}()
	}
	wg.Wait()
}
func TestCancelActiveService(t *testing.T) {
	st, ct := settings(t, "tls")
	_, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
	cfg := Config{Role: "client", Secret: testKey, TLS: ct, Server: addr}
	svc, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Serve(ctx, ln) }()
	dst := target(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	c, e := socksDial(ln.Addr().String(), dst)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	cancel()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown left active transfer running")
	}
}

func writeAll(w io.Writer, b []byte) error { _, err := io.Copy(w, bytes.NewReader(b)); return err }
