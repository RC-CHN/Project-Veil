package service

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A plain HTTP upload has an extra body-serialization goroutine. Incomplete
// bodies must not pin it, its stream or a listener after disconnect or shutdown.
func TestIncompleteHTTPUploadReleasesResources(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		for _, action := range []string{"disconnect", "remove", "stop"} {
			t.Run(fmt.Sprintf("chunked=%t/%s", chunked, action), func(t *testing.T) {
				st, ct := settings(t, "tls")
				server, addr := start(t, Config{Role: "server", Secret: testKey, TLS: st})
				inlets := []Inlet{{"socks", connectionAddress(t)}, {"http", connectionAddress(t)}}
				c, err := OpenConnection(Config{Role: "client", Server: addr, Secret: testKey, TLS: ct}, inlets, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				echo := target(t, func(n net.Conn) { io.Copy(n, n) })
				peer, err := socksDial(inlets[0].Listen, echo)
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
				started, ended := make(chan struct{}), make(chan struct{})
				origin := target(t, func(n net.Conn) {
					defer close(ended)
					req, err := http.ReadRequest(bufio.NewReader(n))
					if err != nil {
						return
					}
					defer req.Body.Close()
					if _, err := io.ReadFull(req.Body, make([]byte, 1)); err != nil {
						return
					}
					close(started)
					io.Copy(io.Discard, req.Body)
				})
				local, err := net.DialTimeout("tcp", inlets[1].Listen, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer local.Close()
				local.SetDeadline(time.Now().Add(3 * time.Second))
				// Cross net/http's 4 KiB body buffer while leaving the upload open.
				header, body := "Content-Length: 1048576", strings.Repeat("x", 8192)
				if chunked {
					header, body = "Transfer-Encoding: chunked", "2000\r\n"+body+"\r\n"
				}
				if _, err := fmt.Fprintf(local, "POST http://%s/upload HTTP/1.1\r\nHost: %s\r\n%s\r\n\r\n%s", origin, origin, header, body); err != nil {
					t.Fatal(err)
				}
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("upload body never reached origin")
				}
				done := make(chan error, 1)
				go func() {
					switch action {
					case "disconnect":
						done <- local.Close()
					case "remove":
						done <- c.SetInlets(inlets[:1])
					case "stop":
						c.Close()
						done <- nil
					}
				}()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("incomplete upload blocked lifecycle operation")
				}
				select {
				case <-ended:
				case <-time.After(time.Second):
					t.Fatal("upload retained destination connection")
				}
				if action != "stop" {
					// The other inlet's live stream shares the same physical tunnel.
					halfEchoPayload(t, peer, []byte("surviving inlet"))
				}
				await(t, func() bool {
					s := c.Snapshot().Stats
					return s.ActiveConnections == 0 && s.ActiveStreams == 0 && server.Stats.ActiveStreams.Load() == 0
				})
			})
		}
	}
}
