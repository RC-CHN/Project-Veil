package service

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"veil/internal/transport"
)

// A warmed, persistent stream complements the new-stream serial benchmark.
// SetBytes counts both directions; the echo fixture and validation are included.
func BenchmarkBulkEcho(b *testing.B) {
	st, ct := settings(b, "reality")
	st.RecordPadding, ct.RecordPadding = true, true
	backend := target(b, func(c net.Conn) {
		c.SetDeadline(time.Time{})
		io.Copy(c, c)
	})
	_, remote := start(b, Config{Role: "server", Secret: testKey, TLS: st})
	_, local := start(b, Config{Role: "client", Server: remote, Secret: testKey, TLS: ct})
	c, err := socksDial(local, backend)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Minute))
	payload := bytes.Repeat([]byte("bulk-integrity!\x00"), 1<<16)
	received := make([]byte, len(payload))
	echo := func() {
		if err := writeAll(c, payload); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(c, received); err != nil {
			b.Fatal(err)
		}
		if !bytes.Equal(payload, received) {
			b.Fatal("bulk payload corrupted")
		}
	}
	echo()
	b.SetBytes(int64(2 * len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		echo()
	}
	b.StopTimer()
	if err := c.CloseWrite(); err != nil {
		b.Fatal(err)
	}
	if n, err := io.Copy(io.Discard, c); err != nil || n != 0 {
		b.Fatalf("bulk half-close: %d %v", n, err)
	}
}

// Includes client, server, SOCKS and target allocations in one process. TLS
// handshake, setup and warmup are excluded. Uses the SOCKS/open/echo/half-close
// lifecycle of S1; fixture allocations are included in both comparisons.
func BenchmarkSerialShort(b *testing.B) {
	for _, mode := range []string{"tls", "tls-padding", "reality-padding"} {
		b.Run(mode, func(b *testing.B) {
			tlsMode := "tls"
			if mode == "reality-padding" {
				tlsMode = "reality"
			}
			st, ct := settings(b, tlsMode)
			st.RecordPadding, ct.RecordPadding = mode != "tls", mode != "tls"
			runSerialShort(b, st, ct)
		})
	}
}

func runSerialShort(b *testing.B, st, ct transport.Settings) {
	runSerialSized(b, st, ct, 64)
}

func BenchmarkSerialSized(b *testing.B) {
	for _, size := range []int{4097, 16384, 32769, 65536} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			st, ct := settings(b, "reality")
			st.RecordPadding, ct.RecordPadding = true, true
			runSerialSized(b, st, ct, size)
		})
	}
}

func runSerialSized(b *testing.B, st, ct transport.Settings, size int) {
	backend := target(b, func(c net.Conn) {
		p := make([]byte, size)
		if _, err := io.ReadFull(c, p); err != nil {
			return
		}
		if err := writeAll(c, p); err != nil {
			return
		}
		var last [1]byte
		c.Read(last[:])
	})
	_, remote := start(b, Config{Role: "server", Secret: testKey, TLS: st})
	client, local := start(b, Config{Role: "client", Server: remote, Secret: testKey, TLS: ct})
	request := func() {
		c, err := socksDial(local, backend)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		p := make([]byte, size)
		if err = writeAll(c, p); err != nil {
			b.Fatal(err)
		}
		if _, err = io.ReadFull(c, p); err != nil {
			b.Fatal(err)
		}
		if err = c.CloseWrite(); err != nil {
			b.Fatal(err)
		}
		if n, err := io.Copy(io.Discard, c); err != nil || n != 0 {
			b.Fatalf("close: %d %v", n, err)
		}
	}
	for range 32 {
		request()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		request()
	}
	b.StopTimer()
	if client.Stats.Rejected.Load() != 0 {
		b.Fatal("rejected requests")
	}
}

// Run with -benchtime=1x -count=1 in a fresh process for each sample. Measures
// live heap while 64 short streams are idle, including both endpoints and TLS
// lanes. Linux process RSS is reported separately from incremental live heap.
func BenchmarkIdleStreams(b *testing.B) {
	benchmarkIdleStreams(b, 64)
}

func BenchmarkIdleMediumStreams(b *testing.B) {
	for _, size := range []int{4097, 16384, 32769, 65536} {
		b.Run(fmt.Sprint(size), func(b *testing.B) { benchmarkIdleStreams(b, size) })
	}
}

func benchmarkIdleStreams(b *testing.B, size int) {
	st, ct := settings(b, "tls")
	backend := target(b, func(c net.Conn) { io.Copy(c, c) })
	_, remote := start(b, Config{Role: "server", Secret: testKey, TLS: st})
	_, local := start(b, Config{Role: "client", Server: remote, Secret: testKey, TLS: ct})
	const streams = 64
	for range b.N {
		runtime.GC()
		var before, active runtime.MemStats
		runtime.ReadMemStats(&before)
		conns := make([]*net.TCPConn, 0, streams)
		for range streams {
			c, err := socksDial(local, backend)
			if err != nil {
				b.Fatal(err)
			}
			conns = append(conns, c)
			data := make([]byte, size)
			if err = writeAll(c, data); err != nil {
				b.Fatal(err)
			}
			if _, err = io.ReadFull(c, data); err != nil {
				b.Fatal(err)
			}
		}
		runtime.GC()
		runtime.ReadMemStats(&active)
		if status, err := os.ReadFile("/proc/self/statm"); err == nil {
			if fields := strings.Fields(string(status)); len(fields) >= 2 {
				if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					b.ReportMetric(float64(pages)*float64(os.Getpagesize()), "rss-B")
				}
			}
		}
		b.ReportMetric(float64(int64(active.HeapAlloc)-int64(before.HeapAlloc))/streams, "live-B/stream")
		b.ReportMetric(float64(int64(active.StackInuse)-int64(before.StackInuse))/streams, "stack-B/stream")
		for _, c := range conns {
			c.CloseWrite()
			io.Copy(io.Discard, c)
			c.Close()
		}
	}
}
