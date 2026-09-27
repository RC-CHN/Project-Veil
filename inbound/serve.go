package inbound

import (
	"context"
	"errors"
	"net"
	"sync"
	"veil/veil-core"
)

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
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				acceptErr = err
			}
			break
		}
		select {
		case slots <- struct{}{}:
		default:
			opts.Stats.Rejected.Add(1)
			conn.Close()
			continue
		}
		opts.Stats.Accepted.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
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
