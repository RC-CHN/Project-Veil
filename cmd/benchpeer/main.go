// benchpeer is a loopback-only benchmark target/load generator, not a proxy.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"time"
	"veil/internal/wire"
)

func write(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, e := w.Write(p)
		p = p[n:]
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}
func transfer(c net.Conn, mode byte, n int64, server bool) error {
	b := make([]byte, 128*1024)
	send := (mode == 'U' && !server) || (mode == 'D' && server)
	for left := n; left > 0; {
		part := int64(len(b))
		if left < part {
			part = left
		}
		if send {
			if e := write(c, b[:part]); e != nil {
				return e
			}
		} else {
			if _, e := io.ReadFull(c, b[:part]); e != nil {
				return e
			}
		}
		left -= part
	}
	return nil
}
func request(c net.Conn, mode byte, n int64) error {
	var h [9]byte
	h[0] = mode
	binary.BigEndian.PutUint64(h[1:], uint64(n))
	if e := write(c, h[:]); e != nil {
		return e
	}
	if mode == 'E' {
		b := make([]byte, n)
		if e := write(c, b); e != nil {
			return e
		}
		_, e := io.ReadFull(c, b)
		return e
	}
	if e := transfer(c, mode, n, false); e != nil {
		return e
	}
	var ack [1]byte
	_, e := io.ReadFull(c, ack[:])
	return e
}
func backend(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(120 * time.Second))
	for {
		var h [9]byte
		if _, e := io.ReadFull(c, h[:]); e != nil {
			return
		}
		n := int64(binary.BigEndian.Uint64(h[1:]))
		if n < 0 || n > 1<<40 {
			return
		}
		if h[0] == 'E' {
			if n > 1<<20 {
				return
			}
			b := make([]byte, n)
			if _, e := io.ReadFull(c, b); e != nil {
				return
			}
			if e := write(c, b); e != nil {
				return
			}
		} else if h[0] == 'U' || h[0] == 'D' {
			if e := transfer(c, h[0], n, true); e != nil {
				return
			}
			if e := write(c, []byte{1}); e != nil {
				return
			}
		} else {
			return
		}
	}
}
func dial(socks, target string) (net.Conn, error) {
	c, e := net.DialTimeout("tcp", socks, 5*time.Second)
	if e != nil {
		return nil, e
	}
	c.SetDeadline(time.Now().Add(120 * time.Second))
	fail := func(e error) (net.Conn, error) { c.Close(); return nil, e }
	if e = write(c, []byte{5, 1, 0}); e != nil {
		return fail(e)
	}
	var m [2]byte
	if _, e = io.ReadFull(c, m[:]); e != nil {
		return fail(e)
	}
	if m[1] != 0 {
		return fail(fmt.Errorf("SOCKS method"))
	}
	p, e := wire.EncodeAddress(target)
	if e != nil {
		return fail(e)
	}
	if e = write(c, append([]byte{5, 1, 0}, p...)); e != nil {
		return fail(e)
	}
	var h [4]byte
	if _, e = io.ReadFull(c, h[:]); e != nil {
		return fail(e)
	}
	if h[1] != 0 {
		return fail(fmt.Errorf("SOCKS status %d", h[1]))
	}
	n := 0
	switch h[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var l [1]byte
		if _, e = io.ReadFull(c, l[:]); e != nil {
			return fail(e)
		}
		n = int(l[0])
	default:
		return fail(fmt.Errorf("SOCKS address"))
	}
	if _, e = io.CopyN(io.Discard, c, int64(n+2)); e != nil {
		return fail(e)
	}
	return c, nil
}
func main() {
	role := flag.String("role", "load", "")
	listen := flag.String("listen", "127.0.0.1:19090", "")
	socks := flag.String("socks", "127.0.0.1:1080", "")
	target := flag.String("target", "127.0.0.1:19090", "")
	mode := flag.String("mode", "U", "")
	count := flag.Int("connections", 1, "")
	total := flag.Int64("bytes", 3<<30, "")
	rounds := flag.Int("rounds", 10000, "")
	flag.Parse()
	if *role == "backend" {
		host, _, _ := net.SplitHostPort(*listen)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			panic("benchmark backend requires loopback")
		}
		l, e := net.Listen("tcp", *listen)
		if e != nil {
			panic(e)
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true, "address": l.Addr().String()})
		for {
			c, e := l.Accept()
			if e != nil {
				panic(e)
			}
			go backend(c)
		}
	}
	if *count < 1 || *count > 64 || *total < 0 || *rounds < 1 || (*mode != "U" && *mode != "D" && *mode != "E" && *mode != "C") {
		panic("invalid benchmark parameters")
	}
	conns := make([]net.Conn, *count)
	errs := make(chan error, *count)
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if *mode == "C" {
				return
			}
			c, e := dial(*socks, *target)
			if e != nil {
				errs <- e
				return
			}
			conns[i] = c
			for range 16 {
				if e = request(c, 'E', 1024); e != nil {
					errs <- e
					return
				}
			}
			for _, m := range []byte{'U', 'D'} {
				if e = request(c, m, 1<<20); e != nil {
					errs <- e
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		panic(<-errs)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true})
	if _, e := bufio.NewReader(os.Stdin).ReadString('\n'); e != nil {
		panic(e)
	}
	start := time.Now()
	latencies := make([][]int64, *count)
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if *mode == "C" {
				for range *rounds {
					t := time.Now()
					cc, e := dial(*socks, *target)
					if e == nil {
						e = request(cc, 'E', 64)
						cc.Close()
					}
					if e != nil {
						errs <- e
						return
					}
					latencies[i] = append(latencies[i], time.Since(t).Nanoseconds())
				}
				return
			}
			if *mode == "E" {
				for range *rounds {
					t := time.Now()
					if e := request(c, 'E', 64); e != nil {
						errs <- e
						return
					}
					latencies[i] = append(latencies[i], time.Since(t).Nanoseconds())
				}
			} else {
				n := *total / int64(*count)
				if i == 0 {
					n += *total % int64(*count)
				}
				if e := request(c, (*mode)[0], n); e != nil {
					errs <- e
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if len(errs) > 0 {
		panic(<-errs)
	}
	result := map[string]any{"seconds": elapsed, "bytes": *total, "connections": *count, "mode": *mode}
	if *mode == "E" || *mode == "C" {
		var all []int64
		for _, a := range latencies {
			all = append(all, a...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
		result["p50_us"] = float64(all[len(all)/2]) / 1e3
		result["p99_us"] = float64(all[(len(all)*99)/100]) / 1e3
		result["requests"] = len(all)
		result["bytes"] = len(all) * 128
	}
	json.NewEncoder(os.Stdout).Encode(result)
	for _, c := range conns {
		if c != nil {
			c.Close()
		}
	}
}
