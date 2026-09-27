package core

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"veil/internal/wire"
)

// Each relay pump exclusively owns its scratch buffer until it exits. Pooling
// avoids allocating 128 KiB for every short stream; GC can reclaim idle buffers.
var relayBuffers = sync.Pool{New: func() any { return new([wire.MaxData + wire.HeaderSize]byte) }}

type progressReader struct {
	io.Reader
	touch func()
}

func (r progressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.touch()
	}
	return n, err
}

func closeWrite(c net.Conn) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("TCP half-close unavailable")
}

// relay has one writer per direction and no payload queue. On error it closes
// both connections, then joins both workers before returning. A successful
// relay leaves the TLS connection at the FIN/DONE reuse barrier. The server may
// defer its FIN if the peer's FIN has arrived, then send FIN+DONE after the join.
func relay(ctx context.Context, local, remote net.Conn, r *wire.Reader, idle time.Duration, coalesceFIN bool) (finPending bool, err error) {
	var touched atomic.Int64
	var receivedFIN atomic.Bool
	// Durations from one origin preserve Go's monotonic clock across wall-clock
	// adjustments. Activity includes partial headers/payloads and partial writes.
	origin := time.Now()
	touch := func() { touched.Store(int64(time.Since(origin))) }
	var sending, receiving atomic.Uint32 // 0: read, 1: write, 2: finished
	originalReader := r.R
	r.R = progressReader{Reader: originalReader, touch: touch}
	defer func() { r.R = originalReader }()
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		defer sending.Store(2)
		buffer := relayBuffers.Get().(*[wire.MaxData + wire.HeaderSize]byte)
		defer relayBuffers.Put(buffer)
		b := buffer[:]
		for {
			sending.Store(0)
			n, err := local.Read(b[wire.HeaderSize:])
			if n > 0 {
				touch()
				sending.Store(1)
				if e := wire.WriteBuffer(remote, wire.Data, b, n); e != nil {
					results <- opError("tunnel write", e)
					return
				}
				touch()
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					if coalesceFIN && receivedFIN.Load() {
						finPending, err = true, nil
					} else {
						sending.Store(1)
						err = wire.Write(remote, wire.Fin, nil)
					}
					results <- opError("tunnel FIN", err)
				} else {
					results <- opError("local read", err)
				}
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		defer receiving.Store(2)
		buffer := relayBuffers.Get().(*[wire.MaxData + wire.HeaderSize]byte)
		defer relayBuffers.Put(buffer)
		for {
			receiving.Store(0)
			t, remaining, err := r.ReadHeader()
			if err != nil {
				results <- opError("tunnel header", err)
				return
			}
			switch t {
			case wire.Data:
				if err := copyPayload(local, r.R, buffer[:], remaining, &receiving, touch); err != nil {
					results <- err
					return
				}
			case wire.Fin:
				receivedFIN.Store(true)
				results <- opError("local half-close", closeWrite(local))
				return
			default:
				results <- opError("tunnel state", wire.ErrProtocol)
				return
			}
		}
	}()
	period := min(idle/4, time.Second)
	timer := time.NewTicker(period)
	defer timer.Stop()
	left := 2
	var first error
	cancel := ctx.Done()
	for left > 0 {
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
			if time.Since(origin)-time.Duration(touched.Load()) >= idle {
				if first == nil {
					send := [...]string{"local read", "tunnel write", "finished"}
					receive := [...]string{"tunnel read", "local write", "finished"}
					first = &OpError{Op: "idle", Err: ErrIdleTimeout,
						Send: send[sending.Load()], Receive: receive[receiving.Load()]}
				}
				local.Close()
				remote.Close()
			}
		}
	}
	workers.Wait()
	r.Release()
	return finPending, first
}

func copyPayload(dst io.Writer, src io.Reader, buffer []byte, remaining int, state *atomic.Uint32, touch func()) error {
	for remaining > 0 {
		state.Store(0)
		// TLS returns only authenticated plaintext, potentially several
		// buffered records. Never wait for a whole Veil frame or cross it.
		n, readErr := src.Read(buffer[:min(remaining, len(buffer))])
		if n > 0 {
			state.Store(1)
			p := buffer[:n]
			for len(p) > 0 {
				written, writeErr := dst.Write(p)
				p = p[written:]
				if written > 0 {
					touch()
				}
				if writeErr == nil && written == 0 {
					writeErr = io.ErrNoProgress
				}
				if writeErr != nil {
					return opError("local write", writeErr)
				}
			}
			remaining -= n
		}
		if readErr == io.EOF {
			readErr = io.ErrUnexpectedEOF
		}
		if readErr == nil && n == 0 {
			readErr = io.ErrNoProgress
		}
		if readErr != nil {
			return opError("tunnel payload", readErr)
		}
	}
	return nil
}
