// Package local carries the control protocol over a private local connection.
package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
	"veil-service/control"
	"veil/service"
)

const maxRequest = service.MaxConfigSize + 1024
const maxResponse = 4 * service.MaxConfigSize

// ErrStateLocked means another process already owns the instance directory.
var ErrStateLocked = errors.New("another process owns this state directory or socket")

// One bounded JSON line per connection. No HTTP server or background poller is
// needed for this private, local command channel.
func readMessage(r io.Reader, v any) error { return readBoundedMessage(r, v, maxRequest) }

func readBoundedMessage(r io.Reader, v any, limit int) error {
	b, err := bufio.NewReader(io.LimitReader(r, int64(limit)+1)).ReadBytes('\n')
	if err != nil {
		return err
	}
	if len(b) > limit {
		return errors.New("control message too large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("expected one JSON request")
	}
	return nil
}

// Serve owns ln and joins all bounded control handlers on cancellation.
// Manager lifetime is owned by the embedding application, not the transport.
func Serve(ctx context.Context, ln net.Listener, m *control.Manager) error {
	defer ln.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	slots := make(chan struct{}, 16)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			stopping := ctx.Err() != nil
			cancel()
			if stopping || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			c.SetDeadline(time.Now().Add(15 * time.Second))
			var q control.Request
			if err := readMessage(c, &q); err != nil {
				json.NewEncoder(c).Encode(control.Fail("invalid_request", err))
				return
			}
			json.NewEncoder(c).Encode(m.Handle(q))
		})
	}
}

func Call(ctx context.Context, socket string, q control.Request) (control.Response, error) {
	var result control.Response
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := dial(ctx, socket)
	if err != nil {
		return result, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	if err := json.NewEncoder(c).Encode(q); err != nil {
		return result, err
	}
	if err := readBoundedMessage(c, &result, maxResponse); err != nil {
		return result, err
	}
	if result.Version != control.Version {
		return result, errors.New("unsupported response version")
	}
	return result, nil
}
