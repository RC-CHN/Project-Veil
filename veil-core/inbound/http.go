package inbound

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
	"veil/core"
	"veil/internal/wire"
)

type readerConn struct {
	net.Conn
	reader io.Reader
}

func (c *readerConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
func (c *readerConn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return errors.New("inbound connection does not support half-close")
}

// HTTP accepts HTTPS CONNECT tunnels and absolute-form HTTP requests. Plain
// HTTP uses one request per local connection; bodies stream with backpressure.
func HTTP(client *core.Client, timeout time.Duration) Handler {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return func(ctx context.Context, local net.Conn) error {
		defer local.Close()
		stop := context.AfterFunc(ctx, func() { local.Close() })
		defer stop()
		local.SetDeadline(time.Now().Add(timeout))
		const maxHeader = 64 << 10
		limited := &io.LimitedReader{R: local, N: maxHeader + 1}
		reader := bufio.NewReader(limited)
		req, err := http.ReadRequest(reader)
		if err != nil || maxHeader+1-limited.N-int64(reader.Buffered()) > maxHeader {
			httpFailure(local, http.StatusBadRequest)
			return errors.New("invalid or oversized HTTP proxy header")
		}
		limited.N = math.MaxInt64
		connect := req.Method == http.MethodConnect
		address := req.URL.Host
		if connect {
			if req.ContentLength > 0 || len(req.TransferEncoding) != 0 {
				httpFailure(local, http.StatusBadRequest)
				return errors.New("CONNECT must not contain a request body")
			}
		} else {
			if req.URL.Scheme != "http" || req.URL.Hostname() == "" || req.URL.User != nil {
				httpFailure(local, http.StatusBadRequest)
				return errors.New("HTTP proxy requires an absolute http URL or CONNECT")
			}
			if req.URL.Port() == "" {
				address = net.JoinHostPort(req.URL.Hostname(), "80")
			}
		}
		if _, err := wire.EncodeAddress(address); err != nil {
			httpFailure(local, http.StatusBadRequest)
			return err
		}
		stream, err := client.Open(ctx, address)
		// CONNECT, errors and 100 Continue must survive a slow remote OPEN.
		local.SetWriteDeadline(time.Now().Add(timeout))
		if err != nil {
			httpFailure(local, http.StatusBadGateway)
			return err
		}
		defer stream.Close()
		if connect {
			if _, err := io.WriteString(local, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
				return err
			}
			local.SetDeadline(time.Time{})
			return stream.Relay(&readerConn{Conn: local, reader: reader})
		}
		// The local proxy, rather than the destination, handles Expect. This
		// avoids a deadlock when the client waits for 100 before sending a body.
		if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
			if _, err := io.WriteString(local, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
				return err
			}
			req.Header.Del("Expect")
		}
		for _, value := range req.Header.Values("Connection") {
			for _, name := range strings.Split(value, ",") {
				req.Header.Del(strings.TrimSpace(name))
			}
		}
		for _, name := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Keep-Alive", "Upgrade", "TE"} {
			req.Header.Del(name)
		}
		req.Close = true
		req.Host = req.URL.Host
		req.RequestURI = ""
		local.SetDeadline(time.Time{})
		body, writer := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer req.Body.Close()
			writer.CloseWithError(req.Write(writer))
		}()
		defer func() {
			body.Close()
			local.Close()
			<-done
		}()
		return stream.Relay(&readerConn{Conn: local, reader: body})
	}
}

func httpFailure(conn net.Conn, status int) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
}

// Mixed accepts SOCKS5 and HTTP on one listener, sharing the same client pool.
func Mixed(client *core.Client, timeout time.Duration) Handler {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	socks, http := SOCKS5(client, timeout), HTTP(client, timeout)
	return func(ctx context.Context, conn net.Conn) error {
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		conn.SetDeadline(time.Now().Add(timeout))
		reader := bufio.NewReader(conn)
		first, err := reader.Peek(1)
		if err != nil {
			return err
		}
		buffered := &readerConn{Conn: conn, reader: reader}
		if first[0] == 5 {
			return socks(ctx, buffered)
		}
		return http(ctx, buffered)
	}
}
