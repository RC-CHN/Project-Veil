package core

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
	"veil/internal/wire"
)

// Stream is a single-use lease, not the physical TLS connection. Close may run
// concurrently with Relay. Only a successful FIN/DONE barrier permits reuse.
type Stream struct {
	mu                     sync.Mutex
	client                 *Client
	channel                *session
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
	s.stop()
	s.stopClient()
	reuse = reuse && !s.closed && s.ctx.Err() == nil
	s.cancel()
	if reuse {
		s.client.pool.put(s.channel)
		s.client.Stats.Completed.Add(1)
	} else {
		s.channel.r.Release()
		s.client.pool.drop(s.channel)
	}
}

// Relay takes ownership of local and closes it on return. local must support
// CloseWrite; an adapter (SOCKS, forwarding, or a future userspace TCP stack)
// supplies the plain byte stream. No extra payload queue or framing is added.
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
	channel := s.channel
	if _, err = relay(s.ctx, local, channel, &channel.r, s.client.cfg.IdleTimeout, false); err != nil {
		return err
	}
	channel.SetReadDeadline(time.Now().Add(s.client.cfg.HandshakeTimeout))
	err = opError("tunnel DONE", wire.Expect(&channel.r, wire.Done))
	channel.SetReadDeadline(time.Time{})
	return err
}
