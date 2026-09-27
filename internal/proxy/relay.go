package proxy

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

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		b = b[n:]
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}
func closeWrite(c net.Conn) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("TCP half-close unavailable")
}

// relay has one writer per direction and no payload queue. On error it closes
// both connections, then joins both workers before returning. A successful
// relay leaves the TLS connection at the FIN/DONE reuse barrier.
func relay(ctx context.Context, local, remote net.Conn, r *wire.Reader, idle time.Duration) error {
	var touched atomic.Int64
	touched.Store(time.Now().UnixNano())
	touch := func() { touched.Store(time.Now().UnixNano()) }
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		b := make([]byte, wire.MaxData+wire.HeaderSize)
		for {
			n, err := local.Read(b[wire.HeaderSize:])
			if n > 0 {
				touch()
				if e := wire.WriteBuffer(remote, wire.Data, b, n); e != nil {
					results <- e
					return
				}
				touch()
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = wire.Write(remote, wire.Fin, nil)
				}
				results <- err
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			t, p, err := r.Read()
			if err != nil {
				results <- err
				return
			}
			touch()
			switch t {
			case wire.Data:
				if err = writeAll(local, p); err != nil {
					results <- err
					return
				}
				touch()
			case wire.Fin:
				results <- closeWrite(local)
				return
			default:
				results <- wire.ErrProtocol
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
			if time.Since(time.Unix(0, touched.Load())) >= idle {
				if first == nil {
					first = errors.New("veil: stream idle timeout")
				}
				local.Close()
				remote.Close()
			}
		}
	}
	workers.Wait()
	r.Release()
	return first
}
