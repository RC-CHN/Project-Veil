package session

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
	b "veil.local/core/internal/behavior"
)

type adaptiveSource interface {
	lease(int) ([]byte, bool, error)
	commit() error
	wait(context.Context, time.Duration) error
	status() streamQueueStatus
	close()
}
type adaptiveHooks struct {
	Generation     uint64
	Continue       *adaptiveDriveResult     // Bounded cumulative observation across execution generations.
	Pause          func(b.BatchStatus) bool // Called only after complete validation, delivery and upload commit.
	Prepared       bool                     // Only a compiled prepared flight model enables this adapter mode.
	AfterPlan      func(b.BatchOffer)
	Deliver        func([]byte, bool) error
	AfterBootstrap func()
	AfterBatch     func(b.BatchOffer, uint64, int64, time.Duration, b.BatchStatus)
}
type adaptiveDriveResult struct {
	paused           bool
	upETag, downETag string
	Trace            CarrierTrace
	TransactionCount uint64
	MaxClientWaitUS  int64
	Transactions     []adaptiveTransaction
	DownloadEOF      bool
}

// driveAdaptive cannot inspect the peer model or application totals.
func driveAdaptive(ctx context.Context, session *b.BatchSession, client *http.Client, baseURL, bucket string, up adaptiveSource, hooks adaptiveHooks) (result adaptiveDriveResult, e error) {
	if hooks.Continue != nil {
		result = *hooks.Continue
		result.paused = false
	}
	upETag, downETag := "", ""
	var objectOffset uint64
	if hooks.Prepared {
		upETag, downETag = preparedETags()
		objectOffset = 1
	}
	downEOF := result.DownloadEOF
	startedSources := false
	for session.Status().State != 2 {
		o, err := session.Plan()
		if err != nil {
			e = err
			break
		}
		if hooks.AfterPlan != nil {
			hooks.AfterPlan(o)
		}
		transfer := len(o.Members) == 2 && o.Members[0].Name == "upload"
		started := time.Now()
		snapshot := session.Status()
		mode := uint64(0)
		if transfer && snapshot.Registers[3].Number == 1 && snapshot.Registers[4].Number == 1 {
			canClose := up.status().Used == 0
			if closing, ok := up.(interface{ readyClosing() bool }); ok {
				canClose = canClose && closing.readyClosing()
			}
			if canClose {
				mode = 1
			}
		}
		waitStart := time.Now()
		if transfer {
			e = up.wait(ctx, 10*time.Millisecond)
		}
		clientWait := time.Since(waitStart).Microseconds()
		result.MaxClientWaitUS = max(result.MaxClientWaitUS, clientWait)
		if e != nil {
			break
		}
		batchCtx, batchCancel := context.WithCancel(ctx)
		type pendingRequest struct {
			req    *http.Request
			body   []byte
			name   string
			budget int
		}
		requests := make([]pendingRequest, len(o.Members))
		for i, a := range o.Members {
			method, path, body := "PUT", "/"+bucket+"/upload", []byte(nil)
			budget := 0
			switch a.Name {
			case "discover_upload":
				method = "HEAD"
			case "discover_download", "download":
				method = "GET"
				path = "/" + bucket + "/download"
				for _, response := range a.Replies {
					if response.Code == 200 {
						budget = response.Sizes[1]
					}
				}
			case "upload":
				budget = a.RequestSizes[3]
				var data []byte
				var eof bool
				data, eof, e = up.lease((budget - 16) * 4 / 5)
				if e == nil {
					body = streamEncode(o.Sequence+objectOffset, data, eof)
				}
			}
			if e != nil {
				break
			}
			req, err := http.NewRequestWithContext(batchCtx, method, baseURL+path, bytes.NewReader(body))
			if err != nil {
				e = err
				break
			}
			req.GetBody = nil
			if a.Name == "upload" {
				req.Header.Set("If-Match", upETag)
				req.Header.Set("X-Amz-Meta-Receipt", hex.EncodeToString(snapshot.Registers[2].Bytes))
				req.Header.Set("X-Amz-Meta-Mode", strconv.FormatUint(mode, 10))
			}
			if a.Name == "download" {
				req.Header.Set("If-None-Match", downETag)
			}
			values, err := adaptiveRequest(a.Name, req, body)
			if err != nil {
				e = err
				break
			}
			if e = session.Request(o.Sequence, i, values); e != nil {
				break
			}
			requests[i] = pendingRequest{req, body, a.Name, budget}
		}
		if e != nil {
			batchCancel()
			break
		}
		type memberResult struct {
			index    int
			response reply
			tx       adaptiveTransaction
			err      error
		}
		results := make(chan memberResult, len(requests))
		launch := make(chan struct{})
		for i, request := range requests {
			go func(i int, request pendingRequest) {
				<-launch
				tx := adaptiveTransaction{parallelTransaction: parallelTransaction{Sequence: o.Sequence, Member: i, StartedNS: time.Now().UnixNano(), Receipt: request.req.Header.Get("X-Amz-Meta-Receipt")}, Mode: mode, Budget: request.budget}
				if request.name == "upload" {
					tx.Mode, _ = strconv.ParseUint(request.req.Header.Get("X-Amz-Meta-Mode"), 10, 64)
					seq, data, eof, _ := streamDecode(request.body)
					tx.ObjectSequence, tx.PayloadBytes, tx.EOF, tx.PayloadSHA256 = seq, len(data), eof, hashHex(data)
				}
				response, httpTx, err := adaptiveExchange(client, request.req, request.body, request.name, request.budget)
				tx.HTTP = httpTx
				if err == nil {
					if request.name == "download" && response.status == 200 {
						seq, data, eof, decodeErr := streamDecode(response.body)
						tx.ObjectSequence, tx.PayloadBytes, tx.EOF, tx.PayloadSHA256 = seq, len(data), eof, hashHex(data)
						if decodeErr != nil {
							err = decodeErr
						} else if downEOF && (len(data) > 0 || !eof) {
							err = errors.New("data after downstream EOF")
						}
					}
					if err == nil {
						values, decodeErr := adaptiveResponse(request.name, response)
						err = decodeErr
						if err == nil {
							waitStarted := time.Now()
							err = session.WaitResponse(batchCtx, o.Sequence, i)
							tx.ModelWaitUS = time.Since(waitStarted).Microseconds()
						}
						if err == nil {
							err = session.Response(o.Sequence, i, uint64(response.status), values)
						}
					}
				}
				if err != nil {
					tx.Error = err.Error()
				}
				tx.EndedNS = time.Now().UnixNano()
				results <- memberResult{i, response, tx, err}
			}(i, request)
		}
		close(launch)
		for range requests {
			r := <-results
			result.TransactionCount++
			point := CarrierPoint{Generation: hooks.Generation, Sequence: o.Sequence, Member: r.index, StartedNS: r.tx.StartedNS, EndedNS: r.tx.EndedNS,
				Capacity: max(0, (r.tx.Budget-16)*4/5), Payload: r.tx.PayloadBytes, ExchangeUS: r.tx.HTTP.ElapsedUS,
				WriteUS: r.tx.HTTP.WriteUS, FirstByteUS: r.tx.HTTP.FirstByteUS, ModelWaitUS: r.tx.ModelWaitUS, Failed: r.err != nil}
			if hooks.Prepared {
				point.InitialTransfer = o.Sequence == 0 && transfer
				point.ObjectSequence = r.tx.ObjectSequence
			}
			if transfer && r.index == 0 {
				point.WaitUS = clientWait
			}
			result.Trace.add(point)
			if len(result.Transactions) < 64 {
				result.Transactions = append(result.Transactions, r.tx)
			}
			if r.err != nil {
				if e == nil {
					e = r.err
				}
				session.Close()
				batchCancel()
				continue
			}
			name := requests[r.index].name
			if name == "discover_upload" || name == "upload" {
				upETag = r.response.header.Get("ETag")
			}
			if (name == "discover_download" || name == "download") && r.response.status == 200 {
				downETag = r.response.header.Get("ETag")
				if name == "download" {
					_, data, eof, _ := streamDecode(r.response.body)
					if hooks.Deliver != nil {
						if err := hooks.Deliver(data, eof); err != nil {
							if e == nil {
								e = err
							}
							session.Close()
							batchCancel()
							continue
						}
					}
					downEOF = downEOF || eof
				}
			}
		}
		batchCancel()
		if e != nil {
			break
		}
		if transfer {
			if e = up.commit(); e != nil {
				break
			}
		}
		if hooks.AfterBatch != nil {
			hooks.AfterBatch(o, mode, clientWait, time.Since(started), session.Status())
		}
		if !startedSources {
			startedSources = true
			if hooks.AfterBootstrap != nil {
				hooks.AfterBootstrap()
			}
		}
		st := session.Status()
		if st.State != 2 && hooks.Pause != nil && hooks.Pause(st) {
			result.paused = true
			break
		}
	}
	result.DownloadEOF = downEOF
	result.upETag, result.downETag = upETag, downETag
	if e != nil {
		session.Close()
	}
	return
}
