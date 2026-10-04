package inbound

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
	"veil/core"
)

var ErrConnectionLimit = errors.New("veil: inbound connection limit reached")

type ServeOptions struct {
	MaxConnections int
	Stats          *core.Stats
	// OnError can be invoked concurrently; set it before Serve.
	OnError func(error)
}

// Serve owns ln and joins all handlers on shutdown. It never closes a shared
// core.Client: the embedding application owns that lifetime. Each listener has
// its own accept limit; the Client also enforces its aggregate pool limit.
func Serve(ctx context.Context, ln net.Listener, handle Handler, opts ServeOptions) error {
	defer ln.Close()
	if opts.MaxConnections == 0 {
		opts.MaxConnections = 64
	}
	if opts.MaxConnections < 1 || opts.MaxConnections > 4096 || handle == nil {
		return errors.New("invalid listener limit or handler")
	}
	if opts.Stats == nil {
		opts.Stats = new(core.Stats)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	slots := make(chan struct{}, opts.MaxConnections)
	var wg sync.WaitGroup
	var acceptErr error
	var retry time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
					// Resource pressure must not cancel established streams. Back off
					// while descriptors recover, but let shutdown interrupt the wait.
					retry = min(max(2*retry, 5*time.Millisecond), time.Second)
					if opts.OnError != nil {
						opts.OnError(err)
					}
					timer := time.NewTimer(retry)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
					}
					continue
				}
				acceptErr = err
			}
			break
		}
		retry = 0
		select {
		case slots <- struct{}{}:
		default:
			opts.Stats.Rejected.Add(1)
			conn.Close()
			if opts.OnError != nil {
				opts.OnError(ErrConnectionLimit)
			}
			continue
		}
		opts.Stats.Accepted.Add(1)
		opts.Stats.ActiveConnections.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer opts.Stats.ActiveConnections.Add(-1)
			defer func() { <-slots }()
			if err := handle(ctx, conn); err != nil {
				opts.Stats.Failed.Add(1)
				if opts.OnError != nil {
					opts.OnError(err)
				}
			}
		}()
	}
	cancel()
	ln.Close()
	wg.Wait()
	return acceptErr
}
