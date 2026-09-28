package service

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func proxyEntry(t *testing.T, kind string) string {
	t.Helper()
	serverTLS, clientTLS := settings(t, "tls")
	_, server := start(t, Config{Role: "server", Secret: testKey, TLS: serverTLS})
	_, entry := start(t, Config{Role: "client", Server: server, Secret: testKey, TLS: clientTLS, Inbound: kind})
	return entry
}

func TestHTTPAndMixedProxy(t *testing.T) {
	for _, kind := range []string{"http", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			entry := proxyEntry(t, kind)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Hop") != "" {
					t.Error("proxy credentials or connection-specific headers reached origin")
				}
				if r.URL.Path != "/upload" || r.RequestURI != "/upload?q=1" {
					t.Errorf("incorrect forwarded URL: %q", r.RequestURI)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				w.Write(body)
			}))
			defer origin.Close()
			proxy, _ := url.Parse("http://" + entry)
			transport := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
			payload := bytes.Repeat([]byte("bounded streaming body\n"), 50000)
			for _, chunked := range []bool{false, true} {
				var source io.Reader = bytes.NewReader(payload)
				if chunked {
					source = io.NopCloser(source)
				}
				request, _ := http.NewRequest("POST", origin.URL+"/upload?q=1", source)
				request.Header.Set("Proxy-Authorization", "Basic test-secret")
				request.Header.Set("Connection", "X-Hop")
				request.Header.Set("X-Hop", "remove-this")
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || !bytes.Equal(body, payload) {
					t.Fatalf("body mismatch: %d %v", len(body), err)
				}
			}
			secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "HTTPS through CONNECT")
			}))
			defer secure.Close()
			secureClient := secure.Client()
			secureClient.Timeout = 5 * time.Second
			secureClient.Transport.(*http.Transport).Proxy = http.ProxyURL(proxy)
			defer secureClient.CloseIdleConnections()
			secured, err := secureClient.Get(secure.URL)
			if err != nil {
				t.Fatal(err)
			}
			securedBody, err := io.ReadAll(secured.Body)
			secured.Body.Close()
			if err != nil || string(securedBody) != "HTTPS through CONNECT" {
				t.Fatalf("HTTPS body: %q %v", securedBody, err)
			}
			// Early tunnel bytes must survive HTTP parsing, including mixed sniffing.
			echo := target(t, func(c net.Conn) { b, _ := io.ReadAll(c); writeAll(c, b) })
			conn, err := net.Dial("tcp", entry)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nearly tunnel bytes", echo, echo)
			reader := bufio.NewReader(conn)
			response, err := http.ReadResponse(reader, nil)
			if err != nil || response.StatusCode != 200 {
				t.Fatalf("CONNECT: %v %v", response, err)
			}
			conn.(*net.TCPConn).CloseWrite()
			body, err := io.ReadAll(reader)
			if err != nil || string(body) != "early tunnel bytes" {
				t.Fatalf("tunnel body: %q %v", body, err)
			}
			if kind == "mixed" {
				socks, err := socksDial(entry, echo)
				if err != nil {
					t.Fatal(err)
				}
				halfEchoPayload(t, socks, []byte("mixed SOCKS half-close"))
			}
		})
	}
}

func TestHTTPContinueAndRejectedRequests(t *testing.T) {
	entry := proxyEntry(t, "mixed")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Write(b)
	}))
	defer origin.Close()
	c, err := net.Dial("tcp", entry)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "POST %s/test HTTP/1.1\r\nHost: ignored.example\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n", origin.URL)
	reader := bufio.NewReader(c)
	r, err := http.ReadResponse(reader, nil)
	if err != nil || r.StatusCode != 100 {
		t.Fatalf("continue: %v %v", r, err)
	}
	io.WriteString(c, "body")
	r, err = http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || string(b) != "body" {
		t.Fatalf("continue body: %q %v", b, err)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().String()
	closed.Close()
	for _, tc := range []struct {
		request string
		code    int
	}{
		{"GET /relative HTTP/1.1\r\nHost: localhost\r\n\r\n", 400},
		{"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost\r\nContent-Length: 5\r\n\r\n", 400},
		{"GET http://localhost/ HTTP/1.1\r\nHost: localhost\r\nX-Large: " + strings.Repeat("a", 65536) + "\r\n\r\n", 400},
		{fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", refused, refused), 502},
	} {
		conn, err := net.Dial("tcp", entry)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(conn, tc.request)
		result, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil || result.StatusCode != tc.code {
			t.Fatalf("status want=%d got=%v err=%v", tc.code, result, err)
		}
	}
}
