package service

import (
	"bytes"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// Measure a fresh SOCKS connection to a 64-byte echo target while another
// connection through the same client continuously downloads on loopback.
func BenchmarkDownloadAndShortRequests(b *testing.B) {
	st, ct := settings(b, "tls")
	_, remote := start(b, Config{Role: "server", Secret: testKey, TLS: st})
	_, local := start(b, Config{Role: "client", Server: remote, Secret: testKey, TLS: ct})
	bulk := target(b, func(c net.Conn) {
		c.SetDeadline(time.Time{})
		block := make([]byte, 128<<10)
		for writeAll(c, block) == nil {
		}
	})
	echo := target(b, func(c net.Conn) { io.Copy(c, c) })
	download, err := socksDial(local, bulk)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, download, 1<<20); err != nil {
		download.Close()
		b.Fatal(err)
	}
	download.SetDeadline(time.Time{})
	var downloaded atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64<<10)
		for {
			n, err := download.Read(buf)
			downloaded.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	defer func() { download.Close(); <-done }()

	payload := bytes.Repeat([]byte{0x5a}, 64)
	reply := make([]byte, len(payload))
	request := func() time.Duration {
		started := time.Now()
		c, err := socksDial(local, echo)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		if err := writeAll(c, payload); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(c, reply); err != nil {
			b.Fatal(err)
		}
		latency := time.Since(started)
		if !bytes.Equal(payload, reply) {
			b.Fatal("incorrect echo")
		}
		if err := c.CloseWrite(); err != nil {
			b.Fatal(err)
		}
		if n, err := io.Copy(io.Discard, c); n != 0 || err != nil {
			b.Fatalf("half-close: %d bytes, %v", n, err)
		}
		return latency
	}
	for range 32 {
		request()
	}
	samples := make([]time.Duration, b.N)
	b.ResetTimer()
	begin := time.Now()
	before := downloaded.Load()
	for i := range samples {
		samples[i] = request()
	}
	delta := downloaded.Load() - before
	elapsed := time.Since(begin)
	b.StopTimer()
	select {
	case <-done:
		b.Fatal("background download ended early")
	default:
	}
	if delta == 0 && b.N > 32 {
		b.Fatal("background download stopped making progress")
	}
	slices.Sort(samples)
	b.ReportMetric(float64(samples[(b.N-1)/2])/1e3, "p50-us")
	b.ReportMetric(float64(samples[(99*b.N+99)/100-1])/1e3, "p99-us")
	b.ReportMetric(float64(delta)/(1<<20)/elapsed.Seconds(), "download-MiB/s")
}
