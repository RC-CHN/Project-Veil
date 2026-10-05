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

// Short streams start small. Full reads grow through 32 KiB before bulk size.
// Bounded caches cannot retain one large buffer per historical connection.
var relaySmallBuffers = make(chan []byte, 32)
var relayMediumBuffers = make(chan []byte, 8)
var relayLargeBuffers = make(chan []byte, 8)

func relayBufferCache(size int) chan []byte {
	switch size {
	case 4096:
		return relaySmallBuffers
	case 32768:
		return relayMediumBuffers
	default:
		return relayLargeBuffers
	}
}

func takeRelayBuffer(size int) []byte {
	select {
	case b := <-relayBufferCache(size):
		return b
	default:
		return make([]byte, size)
	}
}

func putRelayBuffer(b []byte) {
	select {
	case relayBufferCache(len(b)) <- b:
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
	timer              *time.Timer
}

var relayStates = make(chan *relayState, 32)

func takeRelayState(idle time.Duration) *relayState {
	var r *relayState
	select {
	case r = <-relayStates:
		r.timer.Reset(idle)
	default:
		r = &relayState{results: make(chan error, 2), timer: time.NewTimer(idle)}
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
	buffer := takeRelayBuffer(4096)
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
			size := 32768
			if len(buffer) == size {
				size = mux.PayloadSize
			}
			putRelayBuffer(buffer)
			buffer = takeRelayBuffer(size)
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
	// Quiet applications may wait much longer than blocked forwarding. Recheck
	// quiet relays once per write budget so a write begun later cannot
	// inherit the remaining thirty-minute timer. No per-packet timer resets.
	writeBudget := min(idle, 2*time.Minute)
	r := takeRelayState(writeBudget)
	defer r.release()
	r.local, r.remote = local, remote
	r.workers.Add(2)
	go r.pump(true)
	go r.pump(false)
	var first error
	cancel := ctx.Done()
	var remoteContext context.Context
	var remoteStopped <-chan struct{}
	if remote, ok := remote.(interface{ Context() context.Context }); ok {
		remoteContext = remote.Context()
		remoteStopped = remoteContext.Done()
	}
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
				first = context.Cause(ctx)
			}
			cancel = nil
			local.Close()
			remote.Close()
		case <-remoteStopped:
			// FIN ends only the receive pump. A later RESET or transport
			// failure must still interrupt the local reader in the other pump.
			if first == nil {
				first = opError("tunnel", context.Cause(remoteContext))
			}
			remoteStopped = nil
			local.Close()
			remote.Close()
		case <-r.timer.C:
			last := r.origin.Add(time.Duration(r.touched.Load()))
			if p, ok := remote.(interface{ LastActivity() time.Time }); ok {
				if t := p.LastActivity(); t.After(last) {
					last = t
				}
			}
			elapsed := time.Since(last)
			budget := idle
			blocked := r.sending.Load() == 1 || r.receiving.Load() == 1
			if blocked {
				budget = writeBudget
			}
			if elapsed >= budget {
				if first == nil {
					send := [...]string{"local read", "tunnel write", "finished"}
					receive := [...]string{"tunnel read", "local write", "finished"}
					cause := ErrIdleTimeout
					if budget < idle {
						cause = ErrWriteStall
					}
					first = &OpError{Op: "idle", Err: cause, Send: send[r.sending.Load()], Receive: receive[r.receiving.Load()]}
				}
				local.Close()
				remote.Close()
			} else {
				// Activity postpones expiry; cap the next check to catch a
				// write that begins while the application would otherwise be idle.
				r.timer.Reset(min(budget-elapsed, writeBudget))
			}
		}
	}
	r.workers.Wait()
	return first
}
