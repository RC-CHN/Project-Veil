package service

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
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
	benchmarkShortRequests(b, st, ct, size, 0)
}

// Include real SOCKS negotiation, destination dialing and FIN/DONE under load.
func BenchmarkConcurrentSized(b *testing.B) {
	for _, size := range []int{64, 65536} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			st, ct := settings(b, "reality")
			st.RecordPadding, ct.RecordPadding = true, true
			benchmarkShortRequests(b, st, ct, size, 64)
		})
	}
}

func benchmarkShortRequests(b *testing.B, st, ct transport.Settings, size, workers int) {
	benchmarkProxyRequests(b, st, ct, size, workers, "", socksDial)
}

// Exercise the production mixed inlet as well as the dedicated HTTP inlet.
// The CONNECT fixture returns a raw TCP stream; no payload is sent until its
// response has been consumed, so there are no buffered tunnel bytes to lose.
func BenchmarkProxyShort(b *testing.B) {
	for _, inlet := range []string{"mixed-socks", "mixed-http", "http"} {
		for _, workers := range []int{0, 64} {
			b.Run(fmt.Sprintf("%s/%d", inlet, workers), func(b *testing.B) {
				st, ct := settings(b, "reality")
				st.RecordPadding, ct.RecordPadding = true, true
				protocol := "mixed"
				dial := socksDial
				if inlet != "mixed-socks" {
					dial = connectBenchmark
				}
				if inlet == "http" {
					protocol = "http"
				}
				benchmarkProxyRequests(b, st, ct, 64, workers, protocol, dial)
			})
		}
	}
}

func connectBenchmark(proxy, target string) (*net.TCPConn, error) {
	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK || reader.Buffered() != 0 {
		conn.Close()
		return nil, fmt.Errorf("CONNECT response: status=%d buffered=%d", response.StatusCode, reader.Buffered())
	}
	return conn.(*net.TCPConn), nil
}

func benchmarkProxyRequests(b *testing.B, st, ct transport.Settings, size, workers int, inlet string, dial func(string, string) (*net.TCPConn, error)) {
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
	client, local := start(b, Config{Role: "client", Server: remote, Secret: testKey, TLS: ct, Inbound: inlet})
	request := func() error {
		c, err := dial(local, backend)
		if err != nil {
			return err
		}
		defer c.Close()
		p := make([]byte, size)
		if err = writeAll(c, p); err != nil {
			return err
		}
		if _, err = io.ReadFull(c, p); err != nil {
			return err
		}
		if err = c.CloseWrite(); err != nil {
			return err
		}
		if n, err := io.Copy(io.Discard, c); err != nil || n != 0 {
			return fmt.Errorf("close: %d %v", n, err)
		}
		return nil
	}
	for range 32 {
		if err := request(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	if workers == 0 {
		for range b.N {
			if err := request(); err != nil {
				b.Fatal(err)
			}
		}
	} else {
		b.SetParallelism(max(1, workers/runtime.GOMAXPROCS(0)))
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if err := request(); err != nil {
					b.Error(err)
					return
				}
			}
		})
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
