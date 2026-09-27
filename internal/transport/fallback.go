package transport

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

// fallback owns only raw website forwarding. The two pumps share one activity
// deadline, so a download does not time out merely because its upload is idle.
// Successful REALITY sessions never enter these pumps.
type fallback struct {
	handshake context.Context
	idle      time.Duration
	mu        sync.Mutex
	active    bool
}

func (f *fallback) touch(a, b net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active {
		deadline := time.Now().Add(f.idle)
		a.SetDeadline(deadline)
		b.SetDeadline(deadline)
	}
}

func (f *fallback) copy(dst, src net.Conn, first []byte) {
	closeBoth := func() { dst.Close(); src.Close() }
	if first != nil {
		// Only the downstream pump may lift the handshake deadline, after
		// REALITY has selected fallback and the cover has actually responded.
		// A silent cover or an incomplete ClientHello keeps the original bound.
		deadline, bounded := f.handshake.Deadline()
		if f.handshake.Err() != nil || (bounded && !time.Now().Before(deadline)) {
			closeBoth()
			return
		}
		f.mu.Lock()
		f.active = true
		f.mu.Unlock()
		f.touch(dst, src)
	}
	w := fallbackWriter{dst: dst, src: src, state: f}
	if len(first) != 0 {
		if _, err := w.Write(first); err != nil {
			closeBoth()
			return
		}
	}
	if _, err := io.Copy(w, src); err != nil {
		closeBoth()
		return
	}
	// Preserve a response after the client has finished sending its request.
	// Closing both sockets here would truncate TCP half-close protocols.
	if c, ok := dst.(interface{ CloseWrite() error }); ok {
		if err := c.CloseWrite(); err != nil {
			closeBoth()
		}
	} else {
		closeBoth()
	}
}

type fallbackWriter struct {
	dst, src net.Conn
	state    *fallback
}

func (w fallbackWriter) Write(p []byte) (int, error) {
	w.state.touch(w.dst, w.src) // successful read, before a potentially blocked write
	n, err := w.dst.Write(p)
	if n > 0 {
		w.state.touch(w.dst, w.src)
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
