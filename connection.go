package main

import (
	"context"
	"errors"
	"fmt"
	json "github.com/goccy/go-json"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// agentDialer dials the router the same way websocket.DefaultDialer does,
// except it disables Nagle's algorithm (TCP_NODELAY) on the raw TCP
// connection before the (optional) TLS handshake wraps it. Every chat/chunk
// frame on this connection is a small, latency-sensitive, request/response-
// shaped write; left on its OS default, Nagle's algorithm can hold each one
// back tens of milliseconds waiting to coalesce with more data that never
// comes, directly inflating TTFT.
var agentDialer = websocket.Dialer{
	NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.SetNoDelay(true)
		}
		return conn, nil
	},
}

// connectAndServe dials the router and serves chat requests until the
// connection drops, at which point it returns so the caller can reconnect.
// It waits on gate.waitHealthy before dialing - so once the local backend is
// flagged unhealthy and this connection is cut (see routerGate, gate.go),
// it doesn't immediately redial straight back into the same outage - and
// registers the live conn with gate under connIdx so monitorBackendHealth
// can force it closed the moment the backend goes unhealthy.
func connectAndServe(connectURL, token string, backend *llmBackend, caps *Capabilities, gate *routerGate, connIdx int, metrics *requestMetrics) error {
	gate.waitHealthy()

	conn, _, err := agentDialer.Dial(connectURL, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		return fmt.Errorf("dial %s: %w", connectURL, err)
	}
	defer conn.Close()
	gate.setConn(connIdx, conn)
	defer gate.clearConn(connIdx)
	log.Printf("connected: %s", connectURL)

	// gorilla/websocket forbids concurrent writers on one connection — every
	// send (including pong replies) goes through this mutex.
	var writeMu sync.Mutex
	send := func(f Frame) error {
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, data)
	}

	// One-time, optional capability self-report - not fatal if it fails to
	// send, since the router treats a missing hello exactly like an agent
	// that predates this handshake (see docs/AGENT_PROTOCOL.md §1a).
	if err := send(Frame{Type: "hello", Capabilities: caps}); err != nil {
		log.Printf("failed to send hello frame: %v", err)
	}

	// Answer router-initiated pings so the connection isn't reaped as dead.
	conn.SetPingHandler(func(appData string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.PongMessage, []byte(appData))
	})

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		decodeStart := time.Now()
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			log.Printf("bad frame: %v", err)
			continue
		}
		// recvAt marks when this frame came off the wire, before dispatch -
		// the baseline handleChat measures backend-only TTFT against, so it
		// can be compared to the router's own ttft_ms (see
		// router.agent/handlers/proxy.go's serveStream) for the same
		// request_id to see how much of TTFT is the network hop vs the
		// backend itself.
		recvAt := time.Now()
		if f.Type == "chat" {
			// One decode per incoming request (the router only ever sends
			// "chat" frames here, per docs/AGENT_PROTOCOL.md §7) - safe to
			// log unconditionally under DEBUG without spam, unlike the
			// per-chunk sends this agent emits back.
			debugLog("[agent] request_id=%s frame_decode_us=%d payload_bytes=%d", f.RequestID, recvAt.Sub(decodeStart).Microseconds(), len(data))
		}
		switch f.Type {
		case "chat":
			// Multiple requests can be in flight on one connection at once —
			// handle each independently so a slow request doesn't block others.
			wg.Add(1)
			go func(req Frame, recvAt time.Time) {
				defer wg.Done()
				handleChat(req, recvAt, backend, caps, send, metrics)
			}(f, recvAt)
		case "benchmark":
			wg.Add(1)
			go func(req Frame) {
				defer wg.Done()
				handleBenchmark(req, backend, send)
			}(f)
		}
	}
}

func handleBenchmark(req Frame, backend *llmBackend, send func(Frame) error) {
	result := runBenchmark(backend, req.Model, req.Prompt)
	send(Frame{Type: "benchmark_result", RequestID: req.RequestID, Benchmark: &result})
}

func handleChat(req Frame, recvAt time.Time, backend *llmBackend, caps *Capabilities, send func(Frame) error, metrics *requestMetrics) {
	window := metrics.window()
	window.recordRequestStart()

	msgs := make([]openaiMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, openaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
		})
	}

	maxTokens, clamped := clampMaxTokens(req.MaxTokens, caps)
	if clamped {
		log.Printf("request_id=%s max_tokens=%d exceeds this backend's max_output_length=%d - clamping", req.RequestID, req.MaxTokens, caps.MaxOutputLength)
	}

	if !req.Stream {
		content, reasoningContent, usage, finishReason, toolCalls, logprobs, err := backend.chat(req.Model, msgs, maxTokens, req.Tools, req.ToolChoice, req.SamplingParams)
		if err != nil {
			window.recordError()
			send(errorFrame(req.RequestID, err))
			return
		}
		window.recordNonStreamLatency(time.Since(recvAt))
		window.recordTokens(usage)
		debugLog("[agent] request_id=%s backend_latency_ms=%d (non-streaming)", req.RequestID, time.Since(recvAt).Milliseconds())
		resp := Frame{Type: "response", RequestID: req.RequestID, Content: content, ReasoningContent: reasoningContent, Usage: usage, FinishReason: finishReason, ToolCalls: toolCalls, LogprobsResult: logprobs}
		if clamped {
			resp.EffectiveMaxTokens = maxTokens
		}
		send(resp)
		return
	}

	firstChunk := true // backend.stream calls onChunk synchronously from one goroutine - no lock needed
	err := backend.stream(req.Model, msgs, maxTokens, req.Tools, req.ToolChoice, req.SamplingParams,
		func(chunk string, logprobs json.RawMessage) {
			if firstChunk {
				firstChunk = false
				backendTTFT := time.Since(recvAt)
				window.recordTTFT(backendTTFT)
				// Times just this first chunk's Frame marshal + WebSocket
				// write - the one JSON-codec cost on this agent's side that
				// could plausibly land inside TTFT (every later chunk this
				// stream sends only affects total throughput, not TTFT, so
				// isn't worth timing/logging per-chunk).
				sendStart := time.Now()
				send(Frame{Type: "chunk", RequestID: req.RequestID, Content: chunk, LogprobsResult: logprobs})
				debugLog("[agent] request_id=%s backend_ttft_ms=%d first_chunk_marshal_send_us=%d",
					req.RequestID, backendTTFT.Milliseconds(), time.Since(sendStart).Microseconds())
				return
			}
			send(Frame{Type: "chunk", RequestID: req.RequestID, Content: chunk, LogprobsResult: logprobs})
		},
		func(reasoningChunk string) {
			send(Frame{Type: "chunk", RequestID: req.RequestID, ReasoningContent: reasoningChunk})
		},
		func(usage *Usage, finishReason string, toolCalls []ToolCall) {
			window.recordTokens(usage)
			final := Frame{Type: "final", RequestID: req.RequestID, Usage: usage, FinishReason: finishReason, ToolCalls: toolCalls}
			if clamped {
				final.EffectiveMaxTokens = maxTokens
			}
			send(final)
		})
	if err != nil {
		window.recordError()
		send(errorFrame(req.RequestID, err))
	}
}

// clampMaxTokens caps requested against caps.MaxOutputLength (this backend's
// own advertised output limit - see -max-output-length in main.go), so a
// caller-supplied max_tokens far beyond what the backend can actually
// produce (e.g. 999999) isn't forwarded as-is and silently misbehaves
// downstream. requested <= 0 means the caller didn't set one at all and is
// passed through unchanged - there's nothing to clamp. clamped reports
// whether the returned value differs from requested, so callers know when
// to surface it back to the router via Frame.EffectiveMaxTokens.
func clampMaxTokens(requested int, caps *Capabilities) (effective int, clamped bool) {
	if requested <= 0 || caps == nil || caps.MaxOutputLength <= 0 || requested <= caps.MaxOutputLength {
		return requested, false
	}
	return caps.MaxOutputLength, true
}

// errorFrame builds an "error" Frame for err, attaching the backend's real
// HTTP status when err is a *backendError (a non-200 response the backend
// itself sent, as opposed to a network-level failure reaching it at all -
// see backend.go) so the router can classify a client-caused 400 apart from
// a genuine connect failure instead of treating every error alike.
func errorFrame(requestID string, err error) Frame {
	f := Frame{Type: "error", RequestID: requestID, Error: err.Error()}
	var be *backendError
	if errors.As(err, &be) {
		f.ErrorStatusCode = be.StatusCode
	}
	return f
}
