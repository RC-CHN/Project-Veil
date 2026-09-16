package session

import (
	"context"
	"errors"
	"time"

	d "veil.local/core/internal/destination"
	sm "veil.local/core/internal/streammux"
	so "veil.local/core/internal/streamopen"
)

func muxDestinationCode(e error) byte {
	switch {
	case errors.Is(e, d.ErrDenied):
		return so.Denied
	case errors.Is(e, d.ErrBusy):
		return so.Busy
	case errors.Is(e, d.ErrResolve):
		return so.ResolutionFailed
	case errors.Is(e, context.DeadlineExceeded):
		return so.TimedOut
	default:
		return so.ConnectFailed
	}
}
func waitMuxTerminal(stream *sm.Stream) error {
	select {
	case <-stream.Done():
	case <-stream.Context().Done():
	}
	st := stream.Status()
	if st.Error != "" {
		return errors.New(st.Error)
	}
	if !st.EndAcked || !st.PeerEnded || st.EndCode != sm.EndNormal || st.PeerEndCode != sm.EndNormal {
		if e := stream.Context().Err(); e != nil {
			return e
		}
		return errors.New("incomplete mux stream terminal")
	}
	return nil
}
func muxOpenStatus(st sm.StreamStatus, client bool) so.Status {
	v := so.Status{Client: client, RequestReceived: !client, Request: st.Request, Result: st.Result, ResultKnown: st.ResultKnown, Established: st.ResultKnown && st.Result.Code == so.OK, Rejected: st.ResultKnown && st.Result.Code != so.OK}
	req, _ := so.EncodeRequest(st.Request)
	v.PrefixSent = st.OpenSent
	if client {
		if st.OpenSent {
			v.PrefixSentSHA256 = hashHex(req)
		}
	} else {
		v.PrefixReceivedSHA256 = hashHex(req)
		v.PrefixSent = st.ResultSent
	}
	if st.ResultKnown {
		r, _ := so.EncodeResult(st.Result)
		if client {
			v.PrefixReceivedSHA256 = hashHex(r)
		} else if st.ResultSent {
			v.PrefixSentSHA256 = hashHex(r)
		}
	}
	return v
}
func completeMuxStreamEvent(stats *counters, event *SessionEvent, stream *sm.Stream, pump *socketLink, cause error, client bool) {
	if pump != nil {
		event.Pump = pump.close(cause)
		if udp, ok := pump.conn.(*datagramSocket); ok {
			v := udp.status()
			event.UDP = &v
		}
	}
	st := stream.Status()
	event.Stream = &st
	event.Open = muxOpenStatus(st, client)
	event.Link = st.Link
	event.EndedNS = time.Now().UnixNano()
	event.Error = errorText(cause)
	event.Outcome = "stream_complete"
	if cause != nil {
		event.Outcome = "stream_error"
		stats.streamsFailed.Add(1)
	} else if st.Result.Code != so.OK {
		event.Outcome = "open_rejected"
		stats.streamsRejected.Add(1)
	} else {
		stats.streamsCompleted.Add(1)
	}
}
