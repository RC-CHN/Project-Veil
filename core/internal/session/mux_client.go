package session

import (
	"context"
	"errors"
	"net"
	"time"

	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

func (c *Client) serveMuxSOCKS(parent context.Context, conn *net.TCPConn) {
	deadline := time.Now().Add(8 * time.Second)
	conn.SetDeadline(deadline)
	requested, e := readSOCKS(conn, true)
	if e != nil {
		c.stats.streamsFailed.Add(1)
		return
	}
	c.stats.openStream()
	event := SessionEvent{EventType: "mux_stream", Role: "client", ModelID: c.modelID(), StartedNS: time.Now().UnixNano()}
	event.LocalAddress, event.RemoteAddress = conn.LocalAddr().String(), conn.RemoteAddr().String()
	var stream *sm.Stream
	var carrier *clientMuxCarrier
	var pump *socketLink
	var udp *datagramSocket
	var cause error
	var stopIdle func()
	replied := false
	defer func() {
		if cause != nil && stream != nil {
			_ = stream.Reset(sm.EndCancelled)
		}
		if stopIdle != nil {
			stopIdle()
		}
		if pump == nil && udp != nil {
			udp.Close()
		}
		if !replied {
			_ = socksResponse(conn, 1)
		}
		if stream != nil {
			completeMuxStreamEvent(&c.stats, &event, stream, pump, cause, true)
		} else {
			event.EndedNS = time.Now().UnixNano()
			event.Error = errorText(cause)
			event.Outcome = "local_rejected"
			c.stats.streamsFailed.Add(1)
		}
		if cause != nil {
			conn.SetLinger(0)
		}
		if c.observer != nil {
			c.observer(event)
		}
		c.stats.streams.Add(-1)
		if stream != nil {
			stream.Release()
			carrier.workers.Done()
		}
	}()
	openCtx, cancelOpen := context.WithDeadline(parent, deadline)
	defer cancelOpen()
	carrier, stream, cause = c.pool.acquire(openCtx, so.Request{Address: requested.address, Network: requested.network, Limits: so.Limits{Window: c.cfg.Window, MaxBytes: c.cfg.MaxBytes}})
	if cause != nil {
		return
	}
	event.Prefix = carrier.prefix
	event.MaterialFingerprint = hashHex(carrier.material[:])
	r, link, e := stream.WaitResult(openCtx)
	event.OpenResultNS = time.Now().UnixNano()
	if e != nil {
		var rejected so.Rejection
		if errors.As(e, &rejected) {
			cause = socksResponse(conn, socksCode(r.Code))
			replied = cause == nil
			if cause == nil {
				cause = waitMuxTerminal(stream)
			}
			return
		}
		cause = e
		return
	}
	if requested.network == so.NetworkUDP {
		udp, cause = newClientDatagrams(stream.Context(), conn, requested.source)
		if cause != nil {
			return
		}
		cause = socksUDPResponse(conn, udp.LocalAddr())
		if cause != nil {
			return
		}
		replied = true
		conn.SetDeadline(time.Time{})
		udp.startClient()
	} else {
		cause = socksResponse(conn, 0)
		if cause != nil {
			return
		}
		replied = true
		conn.SetDeadline(time.Time{})
	}
	cancelOpen()
	ready := make(chan struct{})
	close(ready)
	if udp != nil {
		pump = startSocketLink(link, udp, ready)
	} else {
		pump = startSocketLink(link, conn, ready)
	}
	stopIdle = watchMuxStreamIdle(stream, time.Duration(c.cfg.IdleTimeoutMS)*time.Millisecond)
	cause = waitMuxTerminal(stream)
}
