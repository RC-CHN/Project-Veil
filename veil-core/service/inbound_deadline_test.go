package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"veil/core"
	"veil/inbound"
)

func TestProxyReplyAfterSlowOpen(t *testing.T) {
	for _, protocol := range []string{"socks", "http", "continue", "mixed-socks", "mixed-http"} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reject=%v", protocol, reject), func(t *testing.T) {
				st, ct := settings(t, "tls")
				server, err := core.NewServer(core.ServerConfig{
					Config: core.Config{Secret: testKey, TLS: st},
					DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
						// Remote dialing has its own, longer budget than local parsing.
						select {
						case <-time.After(150 * time.Millisecond):
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						if reject {
							return nil, errors.New("injected destination failure")
						}
						return (&net.Dialer{}).DialContext(ctx, network, address)
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				addr, _ := startHandler(t, server.Handle)
				client := newCoreClient(t, core.ClientConfig{Config: core.Config{Secret: testKey, TLS: ct}, Server: addr})
				const timeout = 75 * time.Millisecond
				handler := inbound.HTTP(client, timeout)
				if protocol == "socks" {
					handler = inbound.SOCKS5(client, timeout)
				} else if strings.HasPrefix(protocol, "mixed-") {
					handler = inbound.Mixed(client, timeout)
				}
				entry, _ := startHandler(t, handler)
				dst := target(t, func(c net.Conn) { p, _ := io.ReadAll(c); writeAll(c, p) })
				if strings.HasSuffix(protocol, "socks") {
					c, err := socksDial(entry, dst)
					if reject {
						if err == nil {
							c.Close()
							t.Fatal("rejected target succeeded")
						}
						if !strings.Contains(err.Error(), "SOCKS error 1") {
							t.Fatal("missing SOCKS failure reply:", err)
						}
						return
					}
					if err != nil {
						t.Fatal("missing SOCKS success reply:", err)
					}
					halfEchoPayload(t, c, []byte("slow open"))
					return
				}
				c, err := net.Dial("tcp", entry)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				want := http.StatusOK
				if protocol == "continue" {
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						io.Copy(w, r.Body)
					}))
					defer origin.Close()
					fmt.Fprintf(c, "POST %s/upload HTTP/1.1\r\nHost: test\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n", origin.URL)
					want = http.StatusContinue
				} else {
					fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", dst, dst)
				}
				if reject {
					want = http.StatusBadGateway
				}
				reader := bufio.NewReader(c)
				response, err := http.ReadResponse(reader, nil)
				if err != nil || response.StatusCode != want {
					t.Fatalf("reply after slow open: want %d, got %v, err %v", want, response, err)
				}
				if reject {
					response.Body.Close()
					return
				}
				io.WriteString(c, "body")
				var body io.Reader = reader
				if protocol == "continue" {
					response, err = http.ReadResponse(reader, nil)
					if err != nil || response.StatusCode != http.StatusOK {
						t.Fatalf("HTTP body response: %v %v", response, err)
					}
					defer response.Body.Close()
					body = response.Body
				} else {
					c.(*net.TCPConn).CloseWrite()
				}
				p, err := io.ReadAll(body)
				if err != nil || string(p) != "body" {
					t.Fatalf("payload after reply: %q %v", p, err)
				}
			})
		}
	}
}
