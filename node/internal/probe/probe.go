// Package probe tests an existing SOCKS node against explicitly supplied echo
// targets. It never falls back to a direct target connection or resolves a
// target domain locally. No service manager is required.
package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"veil.local/core/endpoint"
)

type Options struct {
	SOCKS, TCPEcho, UDPEcho string
	Timeout                 time.Duration
}

type Check struct {
	Name          string `json:"name"`
	Passed        bool   `json:"passed"`
	VerifiedBytes int    `json:"verified_bytes"`
	Milliseconds  int64  `json:"milliseconds"`
	Error         string `json:"error,omitempty"`
}

type Report struct {
	Kind     string    `json:"kind"`
	At       time.Time `json:"at"`
	EndToEnd string    `json:"end_to_end"`
	Scope    string    `json:"scope"`
	Checks   []Check   `json:"checks"`
}

func Run(parent context.Context, o Options) (Report, error) {
	report := Report{Kind: "probe", At: time.Now().UTC(), EndToEnd: "failed", Scope: "requested_echo_targets"}
	socks, err := netip.ParseAddrPort(o.SOCKS)
	if err != nil || !socks.Addr().IsLoopback() || socks.Addr().Zone() != "" || socks.Port() == 0 || o.Timeout < 100*time.Millisecond || o.Timeout > 2*time.Minute || o.TCPEcho == "" && o.UDPEcho == "" {
		return report, errors.New("probe requires a loopback SOCKS endpoint, a TCP/UDP echo target, and timeout 100ms..2m")
	}
	for _, target := range []string{o.TCPEcho, o.UDPEcho} {
		if target != "" {
			if _, err = endpoint.ParseAddress(target); err != nil {
				return report, fmt.Errorf("echo target: %w", err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(parent, o.Timeout)
	defer cancel()
	for _, entry := range []struct {
		name, target string
		fn           func(context.Context, string, endpoint.Endpoint, []byte) error
	}{{"tcp_echo_half_close", o.TCPEcho, tcpEcho}, {"udp_echo", o.UDPEcho, udpEcho}} {
		if entry.target == "" {
			continue
		}
		check := Check{Name: entry.name}
		start := time.Now()
		nonce := make([]byte, 32)
		_, e := rand.Read(nonce)
		if e == nil {
			target, _ := endpoint.ParseAddress(entry.target)
			e = entry.fn(ctx, socks.String(), target, nonce)
		}
		check.Milliseconds = time.Since(start).Milliseconds()
		check.Passed = e == nil
		if e != nil {
			check.Error = e.Error()
		} else {
			check.VerifiedBytes = len(nonce)
		}
		report.Checks = append(report.Checks, check)
	}
	for _, check := range report.Checks {
		if !check.Passed {
			return report, errors.New("end-to-end probe failed")
		}
	}
	report.EndToEnd = "passed"
	return report, nil
}

func connect(ctx context.Context, socks string, command byte, target endpoint.Endpoint) (*net.TCPConn, endpoint.Endpoint, error) {
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "tcp", socks)
	if err != nil {
		return nil, endpoint.Endpoint{}, fmt.Errorf("SOCKS connect: %w", err)
	}
	tcp := c.(*net.TCPConn)
	stop := context.AfterFunc(ctx, func() { tcp.Close() })
	defer stop()
	success := false
	defer func() {
		if !success {
			tcp.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	tcp.SetDeadline(deadline)
	if _, err = tcp.Write([]byte{5, 1, 0}); err != nil {
		return nil, endpoint.Endpoint{}, err
	}
	var hello [2]byte
	if _, err = io.ReadFull(tcp, hello[:]); err != nil {
		return nil, endpoint.Endpoint{}, err
	}
	if hello != [2]byte{5, 0} {
		return nil, endpoint.Endpoint{}, errors.New("SOCKS method rejected")
	}
	request := append([]byte{5, command, 0}, encodeAddress(target)...)
	if command == 3 {
		request[len(request)-2], request[len(request)-1] = 0, 0
	}
	if _, err = tcp.Write(request); err != nil {
		return nil, endpoint.Endpoint{}, err
	}
	var header [3]byte
	if _, err = io.ReadFull(tcp, header[:]); err != nil {
		return nil, endpoint.Endpoint{}, err
	}
	if header[0] != 5 || header[1] != 0 || header[2] != 0 {
		return nil, endpoint.Endpoint{}, fmt.Errorf("SOCKS request rejected (reply=%d)", header[1])
	}
	bound, err := readAddress(tcp, command == 1)
	if err != nil {
		return nil, endpoint.Endpoint{}, err
	}
	success = true
	return tcp, bound, nil
}

func tcpEcho(ctx context.Context, socks string, target endpoint.Endpoint, nonce []byte) error {
	c, _, err := connect(ctx, socks, 1, target)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if _, err = c.Write(nonce); err != nil {
		return err
	}
	if err = c.CloseWrite(); err != nil {
		return err
	}
	got, err := io.ReadAll(io.LimitReader(c, int64(len(nonce)+1)))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, nonce) {
		return errors.New("TCP echo content/length mismatch")
	}
	return nil
}

func udpEcho(ctx context.Context, socks string, target endpoint.Endpoint, nonce []byte) error {
	zero, _ := endpoint.Parse("0.0.0.0", 1)
	// ASSOCIATE requests an unconstrained source. Its port is encoded as zero.
	control, bound, err := connect(ctx, socks, 3, zero)
	if err != nil {
		return err
	}
	defer control.Close()
	ip, err := netip.ParseAddr(bound.Host())
	if err != nil || !ip.IsLoopback() {
		return errors.New("SOCKS UDP relay must be numeric loopback")
	}
	udp, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, bound.Port())))
	if err != nil {
		return err
	}
	defer udp.Close()
	stop := context.AfterFunc(ctx, func() { control.Close(); udp.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	udp.SetDeadline(deadline)
	packet := append([]byte{0, 0, 0}, encodeAddress(target)...)
	packet = append(packet, nonce...)
	if _, err = udp.Write(packet); err != nil {
		return err
	}
	var buffer [1024]byte
	n, err := udp.Read(buffer[:])
	if err != nil {
		return err
	}
	if n < 4 || !bytes.Equal(buffer[:3], []byte{0, 0, 0}) {
		return errors.New("invalid SOCKS UDP response header")
	}
	r := bytes.NewReader(buffer[3:n])
	from, err := readAddress(r, false)
	if err != nil || from.Port() != target.Port() {
		return errors.New("invalid SOCKS UDP response address")
	}
	if targetIP, e := netip.ParseAddr(target.Host()); e == nil {
		fromIP, e := netip.ParseAddr(from.Host())
		if e != nil || fromIP.Unmap() != targetIP.Unmap() {
			return errors.New("unexpected UDP echo source")
		}
	}
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, nonce) {
		return errors.New("UDP echo content/length mismatch")
	}
	return nil
}

func encodeAddress(e endpoint.Endpoint) []byte {
	var out []byte
	if ip, err := netip.ParseAddr(e.Host()); err == nil {
		if ip.Is4() {
			out = []byte{1}
		} else {
			out = []byte{4}
		}
		out = append(out, ip.AsSlice()...)
	} else {
		out = append([]byte{3, byte(len(e.Host()))}, e.Host()...)
	}
	return binary.BigEndian.AppendUint16(out, e.Port())
}

func readAddress(r io.Reader, allowZeroPort bool) (endpoint.Endpoint, error) {
	var tag [1]byte
	if _, err := io.ReadFull(r, tag[:]); err != nil {
		return endpoint.Endpoint{}, err
	}
	var host string
	switch tag[0] {
	case 1, 4:
		size := 4
		if tag[0] == 4 {
			size = 16
		}
		p := make([]byte, size)
		if _, err := io.ReadFull(r, p); err != nil {
			return endpoint.Endpoint{}, err
		}
		ip, _ := netip.AddrFromSlice(p)
		host = ip.String()
	case 3:
		if _, err := io.ReadFull(r, tag[:]); err != nil {
			return endpoint.Endpoint{}, err
		}
		p := make([]byte, int(tag[0]))
		if _, err := io.ReadFull(r, p); err != nil {
			return endpoint.Endpoint{}, err
		}
		host = string(p)
	default:
		return endpoint.Endpoint{}, errors.New("invalid SOCKS address type")
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return endpoint.Endpoint{}, err
	}
	// CONNECT servers may legitimately return 0.0.0.0:0. That bound address
	// is unused; represent its port as 1 solely for the value-type invariant.
	n := binary.BigEndian.Uint16(port[:])
	if n == 0 {
		if !allowZeroPort {
			return endpoint.Endpoint{}, errors.New("zero SOCKS relay/response port")
		}
		n = 1
	}
	return endpoint.Parse(host, n)
}
