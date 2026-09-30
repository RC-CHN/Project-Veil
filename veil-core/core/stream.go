package core

import (
	"context"
	"errors"
	"net"
	"sync"
	"veil/internal/mux"
)

// Stream owns one logical channel. Closing it never cancels another channel
// sharing the same physical connection.
type Stream struct {
	mu                     sync.Mutex
	client                 *Client
	channel                *mux.Stream
	session                *session
	ctx                    context.Context
	cancel                 context.CancelFunc
	stop, stopClient       func() bool
	busy, closed, finished bool
}

// Close aborts unfinished work. A late Close after successful Relay cannot
// close a physical connection that has already been leased to another stream.
func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.finished {
		return nil
	}
	s.closed = true
	// Remote FIN may already have ended the remote reader. Cancel must also
	// unblock the remaining local reader, even when closing TLS produces no I/O error.
	s.cancel()
	s.channel.Close()
	if !s.busy {
		s.finishLocked(false)
	}
	return nil
}

func (s *Stream) finishLocked(reuse bool) {
	if s.finished {
		return
	}
	s.finished = true
	s.client.Stats.ActiveStreams.Add(-1)
	s.stop()
	s.stopClient()
	reuse = reuse && !s.closed && s.ctx.Err() == nil
	s.cancel()
	s.channel.Close()
	s.client.pool.put(s.session)
	if reuse {
		s.client.Stats.Completed.Add(1)
	}
}

// Relay takes ownership of local and closes it on return. local must support
// CloseWrite; an adapter (SOCKS, forwarding, or a future userspace TCP stack)
// supplies the plain byte stream. Per-stream credit bounds receive buffering.
func (s *Stream) Relay(local net.Conn) (err error) {
	defer local.Close()
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return errors.New("veil: stream already in use")
	}
	if s.closed || s.finished {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ctx.Err() != nil {
			err = s.ctx.Err()
		}
		if s.closed && err == nil {
			err = net.ErrClosed
		}
		s.busy = false
		s.finishLocked(err == nil)
	}()
	if err = relay(s.ctx, local, s.channel, s.client.cfg.IdleTimeout); err != nil {
		return err
	}
	if s.channel.Done() {
		return nil
	}
	doneCtx, cancel := context.WithTimeout(s.ctx, s.client.cfg.HandshakeTimeout)
	defer cancel()
	err = s.channel.WaitDone(doneCtx)
	if errors.Is(err, context.DeadlineExceeded) && s.ctx.Err() == nil {
		s.client.pool.drain(s.session)
	}
	return opError("tunnel DONE", err)
}
