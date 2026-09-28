package core

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"veil/internal/mux"
)

var relayBuffers = sync.Pool{New: func() any { return new([mux.PayloadSize]byte) }}

func closeWrite(c any) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		err := cw.CloseWrite()
		// The peer may already have closed after delivering its final bytes.
		// This FIN is redundant; read/write errors still come from the pumps.
		if errors.Is(err, syscall.ENOTCONN) {
			return nil
		}
		return err
	}
	return errors.New("TCP half-close unavailable")
}

// Two independent pumps preserve half-close. Cancellation resets only this
// logical stream. Partial mux frame activity participates in the idle check.
func relay(ctx context.Context, local net.Conn, remote io.ReadWriteCloser, idle time.Duration) error {
	var touched atomic.Int64
	origin := time.Now()
	touch := func() { touched.Store(int64(time.Since(origin))) }
	var sending, receiving atomic.Uint32
	results := make(chan error, 2)
	var workers sync.WaitGroup
	pump := func(src io.Reader, dst io.Writer, state *atomic.Uint32, readOp, writeOp, closeOp string) {
		defer workers.Done()
		defer state.Store(2)
		buffer := relayBuffers.Get().(*[mux.PayloadSize]byte)
		defer relayBuffers.Put(buffer)
		for {
			state.Store(0)
			n, readErr := src.Read(buffer[:])
			if n > 0 {
				touch()
				state.Store(1)
				p := buffer[:n]
				for len(p) > 0 {
					written, err := dst.Write(p)
					p = p[written:]
					if written > 0 {
						touch()
					}
					if err == nil && written == 0 {
						err = io.ErrNoProgress
					}
					if err != nil {
						results <- opError(writeOp, err)
						return
					}
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					state.Store(1)
					results <- opError(closeOp, closeWrite(dst))
				} else {
					results <- opError(readOp, readErr)
				}
				return
			}
			if n == 0 {
				results <- opError(readOp, io.ErrNoProgress)
				return
			}
		}
	}
	workers.Add(2)
	go pump(local, remote, &sending, "local read", "tunnel write", "tunnel FIN")
	go pump(remote, local, &receiving, "tunnel read", "local write", "local half-close")
	timer := time.NewTicker(min(idle/4, time.Second))
	defer timer.Stop()
	var first error
	cancel := ctx.Done()
	for left := 2; left > 0; {
		select {
		case err := <-results:
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
		case <-timer.C:
			last := origin.Add(time.Duration(touched.Load()))
			if p, ok := remote.(interface{ LastActivity() time.Time }); ok {
				if t := p.LastActivity(); t.After(last) {
					last = t
				}
			}
			if time.Since(last) >= idle {
				if first == nil {
					send := [...]string{"local read", "tunnel write", "finished"}
					receive := [...]string{"tunnel read", "local write", "finished"}
					first = &OpError{Op: "idle", Err: ErrIdleTimeout, Send: send[sending.Load()], Receive: receive[receiving.Load()]}
				}
				local.Close()
				remote.Close()
			}
		}
	}
	workers.Wait()
	return first
}
