package session

import (
	"bytes"
	"context"
	"errors"
	"hash"
	"io"
	"net/http"
	"time"
)

type adaptiveEvent struct {
	parallelEvent
	Mode, ObjectSequence            uint64
	BodyBytes, PayloadBytes, Budget int
	EOF                             bool
}
type adaptiveFront struct {
	generation           uint64
	handoff              FlightHandoffTrace
	objectOffset         uint64 // Fixed by the compiled flight model version.
	trace                CarrierTrace
	eventCount           uint64
	maxWaitUS            int64
	inputDelivery        func([]byte, bool) error
	inputDeliveryContext func(context.Context, []byte, bool) error
	*parallelFront
	output       adaptiveSource
	inputHash    hash.Hash
	inputBytes   int
	inputEOF     bool
	mode         uint64
	streamEvents []adaptiveEvent
}

func (f *adaptiveFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case f.active <- struct{}{}:
		defer func() { <-f.active }()
	case <-r.Context().Done():
		return
	case <-f.ctx.Done():
		http.Error(w, "closed", 400)
		return
	default:
		f.abort()
		http.Error(w, "session request capacity", 429)
		return
	}
	o, index, e := f.claim(r)
	if e != nil {
		f.abort()
		http.Error(w, "request rejected", 400)
		return
	}
	a := o.Members[index]
	event := adaptiveEvent{parallelEvent: parallelEvent{Sequence: o.Sequence, Member: index, Action: a.Name, AcceptedNS: time.Now().UnixNano()}}
	started := time.Now()
	point := CarrierPoint{Generation: f.generation, Sequence: o.Sequence, Member: index, StartedNS: event.AcceptedNS}
	if a.Name == "upload" || a.Name == "download" {
		if index == 0 {
			point.Capacity = (a.RequestSizes[3] - 16) * 4 / 5
		} else {
			for _, reply := range a.Replies {
				if reply.Code == 200 {
					point.Capacity = (reply.Sizes[1] - 16) * 4 / 5
				}
			}
		}
	}
	defer func() {
		event.HandlerEndNS = time.Now().UnixNano()
		if f.objectOffset != 0 {
			point.InitialTransfer = o.Sequence == 0 && (a.Name == "upload" || a.Name == "download")
			point.ObjectSequence = event.ObjectSequence
		}
		point.EndedNS, point.Payload, point.WaitUS = event.HandlerEndNS, event.PayloadBytes, event.QueueWaitUS
		point.ExchangeUS, point.Failed = time.Since(started).Microseconds(), event.Error != ""
		f.mu.Lock()
		f.trace.add(point)
		f.eventCount++
		f.maxWaitUS = max(f.maxWaitUS, event.QueueWaitUS)
		if len(f.streamEvents) < 64 {
			f.streamEvents = append(f.streamEvents, event)
		}
		f.mu.Unlock()
	}()
	fail := func(e error) { event.Error = e.Error(); f.abort(); http.Error(w, "transaction rejected", 400) }
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(f.ctx, cancel)
	defer cancel()
	defer stop()
	minimum, budget := int64(0), int64(0)
	if a.Name == "upload" {
		minimum = 16
		budget = int64(a.RequestSizes[3])
	}
	if r.URL.RawQuery != "" || r.URL.RawPath != "" || r.ContentLength < minimum || r.ContentLength > budget || r.Header.Get("Range") != "" {
		fail(errors.New("stream request bounds"))
		return
	}
	for _, key := range []string{"If-Match", "If-None-Match", "X-Amz-Meta-Receipt", "X-Amz-Meta-Mode"} {
		if len(r.Header.Values(key)) > 1 {
			fail(errors.New("duplicate stream header"))
			return
		}
	}
	readStarted := time.Now()
	body, e := io.ReadAll(io.LimitReader(r.Body, budget+1))
	point.ReadUS = time.Since(readStarted).Microseconds()
	if e != nil || int64(len(body)) != r.ContentLength {
		fail(errors.New("stream body length"))
		return
	}
	values, e := adaptiveRequest(a.Name, r, body)
	if e != nil {
		fail(e)
		return
	}
	if a.Name == "upload" {
		event.Mode = values[2].Number
	}
	if e = f.session.Request(o.Sequence, index, values); e != nil {
		fail(e)
		return
	}
	event.BodyBytes, event.Budget = len(body), int(budget)
	if a.Name == "upload" {
		event.Mode = values[2].Number
		seq, data, eof, err := streamDecode(body)
		if err != nil {
			fail(err)
			return
		}
		event.ObjectSequence, event.PayloadBytes, event.EOF = seq, len(data), eof
		f.mu.Lock()
		invalid := f.inputEOF && (len(data) != 0 || !eof)
		f.mode = event.Mode
		f.mu.Unlock()
		if invalid {
			fail(errors.New("data after upload EOF"))
			return
		}
	}
	modelWaitStarted := time.Now()
	e = f.session.WaitResponse(ctx, o.Sequence, index)
	point.ModelWaitUS = time.Since(modelWaitStarted).Microseconds()
	if e != nil {
		fail(e)
		return
	}
	headers := http.Header{}
	for _, key := range []string{"If-Match", "If-None-Match", "X-Amz-Meta-Receipt", "X-Amz-Meta-Mode"} {
		if v := r.Header.Get(key); v != "" {
			headers.Set(key, v)
		}
	}
	event.Receipt = headers.Get("X-Amz-Meta-Receipt")
	var pending []byte
	if a.Name == "download" {
		f.mu.Lock()
		event.Mode = f.mode
		f.mu.Unlock()
		start := time.Now()
		e = f.output.wait(ctx, 20*time.Millisecond)
		event.QueueWaitUS = time.Since(start).Microseconds()
		if e != nil {
			fail(e)
			return
		}
		st := f.output.status()
		if event.Mode == 1 {
			if waiting, ok := f.output.(interface{ waitClosing(context.Context) error }); ok {
				closeStarted := time.Now()
				e = waiting.waitClosing(ctx)
				point.ModelWaitUS += time.Since(closeStarted).Microseconds()
				if e != nil {
					fail(e)
					return
				}
				st = f.output.status()
			}
		}
		if event.Mode == 1 && (st.Used != 0 || !st.AckedEOF) {
			fail(errors.New("closing before acknowledged output EOF"))
			return
		}
		// A fresh generation must learn the already established global EOF in
		// its own checked registers. nextFlight emits an empty EOF control when
		// the global EOF was issued before; it never replays the numbered frame.
		modelEOF := f.generation != 0 && st.InputEOF && f.session.Status().Registers[4].Number == 0
		if st.Used > 0 || st.InputEOF && !st.AckedEOF || modelEOF {
			var planned int
			for _, reply := range a.Replies {
				if reply.Code == 200 {
					planned = reply.Sizes[1]
				}
			}
			data, eof, err := f.output.lease((planned - 16) * 4 / 5)
			if err != nil {
				fail(err)
				return
			}
			pending = streamEncode(o.Sequence+f.objectOffset, data, eof)
			event.BodyBytes, event.PayloadBytes, event.Budget, event.EOF, event.ObjectSequence = len(pending), len(data), planned, eof, o.Sequence+f.objectOffset
			backendStarted := time.Now()
			_, e = f.store.must(ctx, "adaptive_prepare_download", "PUT", r.URL.Path, pending, nil, 200)
			point.BackendUS += time.Since(backendStarted).Microseconds()
			if e != nil {
				fail(e)
				return
			}
		}
	}
	event.NativeStartNS = time.Now().UnixNano()
	backendStarted := time.Now()
	response, e := f.store.do(ctx, "adaptive_"+a.Name, r.Method, r.URL.Path, body, headers)
	point.BackendUS += time.Since(backendStarted).Microseconds()
	if e != nil {
		fail(e)
		return
	}
	event.NativeStatus = response.status
	if a.Name == "download" && ((pending != nil && (response.status != 200 || !bytes.Equal(response.body, pending))) || (pending == nil && response.status != 304)) {
		fail(errors.New("native stream response differs from lease"))
		return
	}
	replyValues, e := adaptiveResponse(a.Name, response)
	if e != nil {
		event.Error = e.Error()
		f.abort()
		writeParallelReply(w, r.Method, response)
		return
	}
	if a.Name == "upload" {
		if f.output.status().Pending {
			if e = f.output.commit(); e != nil {
				fail(e)
				return
			}
		}
		_, data, eof, _ := streamDecode(body)
		deliver := f.inputDelivery
		if f.inputDeliveryContext != nil {
			deliver = func(p []byte, eof bool) error { return f.inputDeliveryContext(ctx, p, eof) }
		}
		if deliver != nil {
			if e = deliver(data, eof); e != nil {
				fail(e)
				return
			}
		}
		f.mu.Lock()
		f.inputHash.Write(data)
		f.inputBytes += len(data)
		f.inputEOF = f.inputEOF || eof
		f.mu.Unlock()

	}
	event.ResponseStartNS = time.Now().UnixNano()
	if e = f.session.Response(o.Sequence, index, uint64(response.status), replyValues); e != nil {
		fail(e)
		return
	}
	event.ValidatedNS = time.Now().UnixNano()

	writeParallelReply(w, r.Method, response)
}
