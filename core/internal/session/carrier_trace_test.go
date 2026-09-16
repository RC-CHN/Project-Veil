package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestCarrierTraceBoundedAndTotalsContinue(t *testing.T) {
	var trace CarrierTrace
	for sequence := uint64(0); sequence <= 4096; sequence++ {
		for member := 0; member < 2; member++ {
			trace.add(CarrierPoint{Sequence: sequence, Member: member, Capacity: 65536, Payload: 64000,
				StartedNS: 1789435862931697391, EndedNS: 1789435862931697391 + 100000000,
				WaitUS: 10000, ExchangeUS: 100000, WriteUS: 1000, FirstByteUS: 99000,
				ModelWaitUS: 1000, ReadUS: 10000, BackendUS: 1000})
		}
	}
	if len(trace.Points) != carrierTraceLimit || trace.Count != 8194 || trace.Up.Count != 4097 || trace.Down.Count != 4097 {
		t.Fatalf("trace prefix/count mismatch: %d/%d", len(trace.Points), trace.Count)
	}
	for _, d := range []CarrierDirection{trace.Up, trace.Down} {
		if d.Payload != 4096*64000 || d.Capacity != 4096*65536 || d.NearFull != 4096 || d.WaitUS != 4097*10000 {
			t.Fatalf("truncated aggregate: %+v", d)
		}
	}
	p, e := json.Marshal(trace)
	if e != nil || len(p) >= 40<<10 {
		t.Fatalf("trace must leave space under 64KiB event bound: %d, %v", len(p), e)
	}
}

func TestAdaptiveExchangeTraceCompletesAndRejectsOversize(t *testing.T) {
	body := streamEncode(1, bytes.Repeat([]byte{7}, 100), false)
	for _, capacity := range []int{len(body), 16} {
		clientConn, serverConn := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer serverConn.Close()
			if _, e := http.ReadRequest(bufio.NewReader(serverConn)); e != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
			fmt.Fprintf(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
			serverConn.Write(body)
		}()
		transport := &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		}}
		t.Cleanup(func() { clientConn.Close(); serverConn.Close(); transport.CloseIdleConnections(); <-done })
		r, _ := http.NewRequest("GET", "http://owned.invalid/download", nil)
		reply, tx, e := adaptiveExchange(&http.Client{Transport: transport, Timeout: time.Second}, r, nil, "download", capacity)
		if tx.FirstByteUS <= 0 || tx.WriteUS <= 0 || tx.ElapsedUS < tx.FirstByteUS {
			t.Fatalf("incomplete HTTP timing: %+v", tx)
		}
		if capacity == len(body) {
			if e != nil || !bytes.Equal(reply.body, body) {
				t.Fatalf("valid exchange changed: %v", e)
			}
		} else if e == nil || tx.Error != "response content length outside model budget" {
			t.Fatalf("trace bypassed read bound: %+v, %v", tx, e)
		}
	}
}
