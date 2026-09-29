package core

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"veil/internal/mux"
)

// Short streams start small. Full reads promote once to the bulk buffer size.
// Bounded caches cannot retain one large buffer per historical connection.
var relaySmallBuffers = make(chan []byte, 32)
var relayLargeBuffers = make(chan []byte, 8)

func takeRelayBuffer(large bool) []byte {
	cache, size := relaySmallBuffers, 4096
	if large {
		cache, size = relayLargeBuffers, mux.PayloadSize
	}
	select {
	case b := <-cache:
		return b
	default:
		return make([]byte, size)
	}
}

func putRelayBuffer(b []byte) {
	cache := relaySmallBuffers
	if len(b) == mux.PayloadSize {
		cache = relayLargeBuffers
	}
	select {
	case cache <- b:
	default:
	}
}

// Reused only after both pumps have exited and both results were consumed.
// No connection, context or caller-owned buffer is retained in this cache.
type relayState struct {
	local              net.Conn
	remote             io.ReadWriteCloser
	origin             time.Time
	touched            atomic.Int64
	sending, receiving atomic.Uint32
	results            chan error
	workers            sync.WaitGroup
	timer              *time.Ticker
}

var relayStates = make(chan *relayState, 32)

func takeRelayState(idle time.Duration) *relayState {
	interval := min(idle/4, time.Second)
	var r *relayState
	select {
	case r = <-relayStates:
		r.timer.Reset(interval)
	default:
		r = &relayState{results: make(chan error, 2), timer: time.NewTicker(interval)}
	}
	r.origin = time.Now()
	r.touched.Store(0)
	r.sending.Store(0)
	r.receiving.Store(0)
	return r
}

func (r *relayState) release() {
	r.timer.Stop()
	r.local, r.remote = nil, nil
	select {
	case relayStates <- r:
	default:
	}
}

func (r *relayState) touch() { r.touched.Store(int64(time.Since(r.origin))) }

func (r *relayState) pump(outbound bool) {
	var src io.Reader = r.remote
	var dst io.Writer = r.local
	state := &r.receiving
	readOp, writeOp, closeOp := "tunnel read", "local write", "local half-close"
	if outbound {
		src, dst, state = r.local, r.remote, &r.sending
		readOp, writeOp, closeOp = "local read", "tunnel write", "tunnel FIN"
	}
	defer r.workers.Done()
	defer state.Store(2)
	buffer := takeRelayBuffer(false)
	defer func() { putRelayBuffer(buffer) }()
	for {
		state.Store(0)
		n, readErr := src.Read(buffer)
		if n > 0 {
			r.touch()
			state.Store(1)
			p := buffer[:n]
			for len(p) > 0 {
				written, err := dst.Write(p)
				p = p[written:]
				if written > 0 {
					r.touch()
				}
				if err == nil && written == 0 {
					err = io.ErrNoProgress
				}
				if err != nil {
					r.results <- opError(writeOp, err)
					return
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				state.Store(1)
				r.results <- opError(closeOp, closeWrite(dst))
			} else {
				r.results <- opError(readOp, readErr)
			}
			return
		}
		if n == 0 {
			r.results <- opError(readOp, io.ErrNoProgress)
			return
		}
		if n == len(buffer) && len(buffer) < mux.PayloadSize {
			putRelayBuffer(buffer)
			buffer = takeRelayBuffer(true)
		}
	}
}

func closeWrite(c any) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		err := cw.CloseWrite()
		// The peer may already have closed after delivering its final bytes.
		// This FIN is redundant; read/write errors still come from the pumps.
		if errors.Is(err, errNotConnected) {
			return nil
		}
		return err
	}
	return errors.New("TCP half-close unavailable")
}

// Two independent pumps preserve half-close. Cancellation resets only this
// logical stream. Partial mux frame activity participates in the idle check.
func relay(ctx context.Context, local net.Conn, remote io.ReadWriteCloser, idle time.Duration) error {
	r := takeRelayState(idle)
	defer r.release()
	r.local, r.remote = local, remote
	r.workers.Add(2)
	go r.pump(true)
	go r.pump(false)
	var first error
	cancel := ctx.Done()
	for left := 2; left > 0; {
		select {
		case err := <-r.results:
			left--
			if err != nil && first == nil {
				first = err
				local.Close()
				remote.Close()
			}
		case <-cancel:
			if first == nil {
				first = ctx.Err()
			}
			cancel = nil
			local.Close()
			remote.Close()
		case <-r.timer.C:
			last := r.origin.Add(time.Duration(r.touched.Load()))
			if p, ok := remote.(interface{ LastActivity() time.Time }); ok {
				if t := p.LastActivity(); t.After(last) {
					last = t
				}
			}
			if time.Since(last) >= idle {
				if first == nil {
					send := [...]string{"local read", "tunnel write", "finished"}
					receive := [...]string{"tunnel read", "local write", "finished"}
					first = &OpError{Op: "idle", Err: ErrIdleTimeout, Send: send[r.sending.Load()], Receive: receive[r.receiving.Load()]}
				}
				local.Close()
				remote.Close()
			}
		}
	}
	r.workers.Wait()
	return first
}
