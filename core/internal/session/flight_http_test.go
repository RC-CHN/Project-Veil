package session

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

type flightPipeListener struct {
	conn   net.Conn
	taken  atomic.Bool
	once   sync.Once
	closed chan struct{}
}

func (l *flightPipeListener) Accept() (net.Conn, error) {
	if l.taken.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *flightPipeListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (l *flightPipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}
}

func flightTLSConfigs(t *testing.T) (*tls.Config, *tls.Config, string) {
	t.Helper()
	caKey, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	caDER, e := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if e != nil {
		t.Fatal(e)
	}
	ca, e = x509.ParseCertificate(caDER)
	if e != nil {
		t.Fatal(e)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf := func(serial int64, usage x509.ExtKeyUsage) tls.Certificate {
		key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"owned.test"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, e := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if e != nil {
			t.Fatal(e)
		}
		return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}
	}
	serverCert, clientCert := leaf(2, x509.ExtKeyUsageServerAuth), leaf(3, x509.ExtKeyUsageClientAuth)
	server := &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}
	client := &tls.Config{Certificates: []tls.Certificate{clientCert}, RootCAs: pool, ServerName: "owned.test", MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}
	return server, client, hashHex(clientCert.Certificate[0])
}

func TestFlightHTTP2PairedStreamAndClosure(t *testing.T) {
	flightHTTP2PairedStreamAndClosure(t, false, false)
}
func TestPreparedHTTP2PairedStreamAndClosure(t *testing.T) {
	flightHTTP2PairedStreamAndClosure(t, true, false)
}
func TestEarlyOpenHTTP2PairedStreamAndClosure(t *testing.T) {
	flightHTTP2PairedStreamAndClosure(t, true, true)
}
func TestRenewingHTTP2PairedStreamAndClosure(t *testing.T) {
	flightHTTP2PairedStreamAndClosure(t, true, true, "batches")
}
func TestRenewingHTTP2AgeBoundaryAndClosure(t *testing.T) {
	flightHTTP2PairedStreamAndClosure(t, true, true, "age")
}
func flightHTTP2PairedStreamAndClosure(t *testing.T, prepared, earlyOpen bool, renewal ...string) {
	renew := len(renewal) == 1
	age := renew && renewal[0] == "age"
	for _, instances := range []int{1, 2, 3, 4} {
		name := fmt.Sprintf("instances-%d", instances)
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			model := flightModel(instances)
			if prepared {
				model = preparedFlightModel(instances)
			}
			if earlyOpen {
				model = earlyOpenFlightModel(instances)
			}
			if renew {
				model = renewingFlightModel(instances)
			}
			program, e := compileFlight(model)
			if e != nil {
				t.Fatal(e)
			}
			if renew {
				// Exercise many boundaries without a production-length soak.
				// This private test trigger is absent from runtime configuration.
				program.generationBatches = 2
				if age {
					program.generationBatches, program.generationAge = flightGenerationBatches, 5*time.Millisecond
				}
			}
			var seed [32]byte
			seed[0] = 19
			limits := sm.Settings{Streams: 1, Window: 1 << 20, StreamBytes: 4 << 20, ConnectionBytes: 32 << 20, Opened: 8}
			store, e := newLocalBackend("owned-test", 2*instances)
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(store.close)
			if e := ensureBucket(ctx, store, "owned-test"); e != nil {
				t.Fatal(e)
			}
			serverTLS, clientTLS, principal := flightTLSConfigs(t)
			serverRaw, clientRaw := net.Pipe()
			listener := &flightPipeListener{conn: serverRaw, closed: make(chan struct{})}
			var once sync.Once
			var group *flightFront
			var initializeErr error
			groupReady := make(chan *flightFront, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					t.Error("expected real HTTP2")
				}
				once.Do(func() {
					group, initializeErr = newFlightFront(ctx, r, program, seed, principal, "owned-test", store, sm.Config{Limits: limits})
					groupReady <- group
				})
				if initializeErr != nil {
					http.Error(w, initializeErr.Error(), 400)
					return
				}
				// A controlled handler delay allows distinct H2 streams to
				// overlap. This is a correctness fixture, not a timing benchmark.
				if r.Method == "PUT" {
					time.Sleep(3 * time.Millisecond)
				}
				group.ServeHTTP(w, r)
			})
			httpServer := &http.Server{Handler: handler, TLSConfig: serverTLS, ReadHeaderTimeout: time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 8 * time.Second, MaxHeaderBytes: 32 << 10}
			serverDone := make(chan error, 1)
			go func() { serverDone <- httpServer.ServeTLS(listener, "", "") }()
			outer := tls.Client(clientRaw, clientTLS)
			var dialed atomic.Bool
			transport := &http.Transport{ForceAttemptHTTP2: true, Proxy: nil, MaxConnsPerHost: 1, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: clientTLS, DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				if !dialed.CompareAndSwap(false, true) {
					return nil, errors.New("replacement TLS connection")
				}
				return outer, nil
			}}
			t.Cleanup(func() {
				cancel()
				clientRaw.Close()
				serverRaw.Close()
				transport.CloseIdleConnections()
				httpServer.Close()
				<-serverDone
				once.Do(func() {}) // Join initialization, including failure paths.
				if group != nil && !group.closing {
					group.finish(errors.New("test cleanup"))
				}
			})
			if e := outer.HandshakeContext(ctx); e != nil {
				t.Fatal(e)
			}
			cs := outer.ConnectionState()
			client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect") }}
			source, e := newFlightMuxSource(ctx, sm.Config{EarlyOpen: earlyOpen, Client: true, Limits: limits, MaxPending: instances})
			if e != nil {
				t.Fatal(e)
			}
			defer source.finish(errors.New("test cleanup"))
			var stream *sm.Stream
			request := so.Request{Network: so.NetworkUDP, Limits: so.Limits{Window: limits.Window, MaxBytes: limits.StreamBytes}}
			if earlyOpen {
				stream, e = source.mux.OpenFirst(request)
				if e != nil {
					t.Fatal(e)
				}
				defer stream.Release()
			}
			ready := make(chan struct{})
			var readyOnce sync.Once
			type driveResult struct {
				value flightDriveResult
				err   error
			}
			driven := make(chan driveResult, 1)
			go func() {
				value, e := driveFlight(ctx, program, seed, &cs, client, "https://owned.test", "owned-test", source, func() { readyOnce.Do(func() { close(ready) }) })
				driven <- driveResult{value, e}
			}()
			var peer *flightFront
			select {
			case peer = <-groupReady:
			case value := <-driven:
				t.Fatal("driver before ready", value.err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if peer == nil {
				t.Fatal(initializeErr)
			}
			payload := make([]byte, 1<<20)
			for i := range payload {
				payload[i] = byte(i*131 + 17)
			}
			applicationDone := make(chan error, 1)
			go func() {
				stream, err := peer.source.mux.Accept(ctx)
				if err != nil {
					applicationDone <- err
					return
				}
				defer stream.Release()
				err = stream.Respond(so.Result{Code: so.OK, Limits: so.Limits{Window: limits.Window, MaxBytes: limits.StreamBytes}})
				var received bytes.Buffer
				if err == nil {
					err = stream.Link().Consume(ctx, &received, func() error { return nil })
				}
				if err == nil && !bytes.Equal(received.Bytes(), payload) {
					err = errors.New("server payload differs")
				}
				if err == nil {
					_, err = stream.Link().Write(ctx, payload)
				}
				if err == nil {
					err = stream.Link().Finish()
				}
				if err == nil {
					err = waitMuxTerminal(stream)
				}
				applicationDone <- err
			}()
			select {
			case <-ready:
			case value := <-driven:
				t.Fatal("driver before ready", value.err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if !earlyOpen {
				stream, e = source.mux.Open(request)
				if e != nil {
					t.Fatal(e)
				}
				defer stream.Release()
			}
			if _, _, e := stream.WaitResult(ctx); e != nil {
				t.Fatal(e)
			}
			if _, e := stream.Link().Write(ctx, payload); e != nil {
				t.Fatal(e)
			}
			stream.Link().Finish()
			var output bytes.Buffer
			if e := stream.Link().Consume(ctx, &output, func() error { return nil }); e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(output.Bytes(), payload) {
				t.Fatal("client payload differs")
			}
			if e := waitMuxTerminal(stream); e != nil {
				t.Fatal(e)
			}
			stream.Release()
			if e := <-applicationDone; e != nil {
				t.Fatal(e)
			}
			source.mux.Drain()
			peer.source.mux.Drain()
			value := <-driven
			if value.err != nil {
				t.Fatalf("flight driver: %v; client=%+v; server=%+v", value.err, source.status(), peer.source.status())
			}
			for _, model := range value.value.Models {
				if model.State != 2 || model.Closed {
					t.Fatal("child model incomplete", model)
				}
			}
			clientEvent, serverEvent := value.value.Event, peer.snapshot()
			if earlyOpen && (!source.status().Mux.EarlyOpenSent || !peer.source.status().Mux.EarlyOpenReceived || source.status().Mux.Opened != 1 || peer.source.status().Mux.Opened != 1) {
				t.Fatal("HTTP2 lost or repeated initial OPEN")
			}
			if !flightComplete(clientEvent, instances) || !flightComplete(serverEvent, instances) {
				t.Fatal("incomplete observed model group")
			}
			if clientEvent.ModelVersion != program.version || serverEvent.ModelVersion != program.version || clientEvent.ObjectSequenceOffset != program.objectOffset() || serverEvent.ObjectSequenceOffset != program.objectOffset() {
				t.Fatal("model metadata differs")
			}
			for i, left := range clientEvent.Lanes {
				right := serverEvent.Lanes[i]
				if left.Instance != i || right.Instance != i || left.Generation != right.Generation || !reflect.DeepEqual(left.Model, right.Model) || left.BudgetCount != right.BudgetCount || left.BudgetSHA256 != right.BudgetSHA256 || left.MaterialFingerprint != right.MaterialFingerprint || left.Transactions != right.Transactions {
					t.Fatal("lane observations differ", left, right)
				}
				if renew && (left.Generation < 2 || !age && left.BudgetCount != 2*left.Generation+left.Model.Completed) {
					t.Fatal("missing repeated generation handoff or cumulative budgets", left)
				}
				if renew && (left.Handoff.Completed != left.Generation || right.Handoff.Completed != right.Generation || left.Handoff.Failed != 0 || right.Handoff.Failed != 0 || left.Transactions != 2*left.BudgetCount+left.Handoff.Count) {
					t.Fatal("handoff omitted from complete transaction accounting", left, right)
				}
				if prepared {
					for _, item := range []FlightLaneEvent{left, right} {
						initial := 0
						for _, point := range item.Trace.Points {
							if point.Sequence == 0 && point.Generation == 0 {
								if !point.InitialTransfer || point.Capacity == 0 {
									t.Fatal("initial transfer omitted from trace")
								}
								initial++
							}
							if point.ObjectSequence != 0 && point.ObjectSequence != point.Sequence+1 {
								t.Fatal("object offset observation")
							}
						}
						if initial != 2 {
							t.Fatal("initial transfer points missing")
						}
					}
				}
				if len(left.Trace.Points) > 24 || len(right.Trace.Points) > 24 || len(left.Budgets) > 16 || len(right.Budgets) > 16 {
					t.Fatal("unbounded event prefix")
				}
			}
			for _, flight := range []*FlightEvent{clientEvent, serverEvent} {
				var event SessionEvent
				flight.apply(&event)
				p, e := json.Marshal(event)
				if e != nil || len(p) > 64<<10 {
					t.Fatal("event cannot pass reporter bound", len(p), e)
				}
			}
			up, down := source.hashes()
			peerDown, peerUp := peer.source.hashes()
			if up != peerUp || down != peerDown {
				t.Fatal("global byte hash mismatch")
			}
			source.finish(nil)
			if instances == 4 {
				// Runtime validates EOF before cancelling the connection parent,
				// then joins application workers before deleting group objects.
				peer.source.finish(nil)
				peer.cancel()
			}
			if !peer.finish(nil) {
				t.Fatal("object cleanup failed")
			}
			for _, item := range []*flightMuxSource{source, peer.source} {
				if st := item.status(); st.Error != "" || !st.Closed || st.Mux.Active != 0 || st.Mux.Pending || st.Receive.Bytes != 0 || !st.AckedEOF || !st.PeerEOF {
					t.Fatal("terminal source state", st)
				}
			}
			if st := store.local.Status(); st.Objects != 0 || st.Bytes != 0 || st.Closed {
				t.Fatal("objects retained before store close", st)
			}
			if peer.maximum > 2*instances || peer.active != 0 || instances == 4 && peer.maximum < 4 {
				t.Fatal("HTTP activity bound or no overlap", peer.maximum, peer.active)
			}
			t.Logf("HTTP2 instances=%d maximum active handlers=%d, bidirectional application bytes=%d", instances, peer.maximum, 2*len(payload))
		})
	}
}
