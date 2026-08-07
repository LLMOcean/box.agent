package main

import (
	"context"
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
func connectAndServe(connectURL, token string, backend *llmBackend, caps *Capabilities) error {
	conn, _, err := agentDialer.Dial(connectURL, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		return fmt.Errorf("dial %s: %w", connectURL, err)
	}
	defer conn.Close()
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
				handleChat(req, recvAt, backend, send)
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

func handleChat(req Frame, recvAt time.Time, backend *llmBackend, send func(Frame) error) {
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

	if !req.Stream {
		content, usage, finishReason, toolCalls, err := backend.chat(req.Model, msgs, req.MaxTokens, req.Tools, req.ToolChoice, req.SamplingParams)
		if err != nil {
			send(Frame{Type: "error", RequestID: req.RequestID, Error: err.Error()})
			return
		}
		debugLog("[agent] request_id=%s backend_latency_ms=%d (non-streaming)", req.RequestID, time.Since(recvAt).Milliseconds())
		send(Frame{Type: "response", RequestID: req.RequestID, Content: content, Usage: usage, FinishReason: finishReason, ToolCalls: toolCalls})
		return
	}

	firstChunk := true // backend.stream calls onChunk synchronously from one goroutine - no lock needed
	err := backend.stream(req.Model, msgs, req.MaxTokens, req.Tools, req.ToolChoice, req.SamplingParams,
		func(chunk string) {
			if firstChunk {
				firstChunk = false
				backendTTFT := time.Since(recvAt)
				// Times just this first chunk's Frame marshal + WebSocket
				// write - the one JSON-codec cost on this agent's side that
				// could plausibly land inside TTFT (every later chunk this
				// stream sends only affects total throughput, not TTFT, so
				// isn't worth timing/logging per-chunk).
				sendStart := time.Now()
				send(Frame{Type: "chunk", RequestID: req.RequestID, Content: chunk})
				debugLog("[agent] request_id=%s backend_ttft_ms=%d first_chunk_marshal_send_us=%d",
					req.RequestID, backendTTFT.Milliseconds(), time.Since(sendStart).Microseconds())
				return
			}
			send(Frame{Type: "chunk", RequestID: req.RequestID, Content: chunk})
		},
		func(usage *Usage, finishReason string, toolCalls []ToolCall) {
			send(Frame{Type: "final", RequestID: req.RequestID, Usage: usage, FinishReason: finishReason, ToolCalls: toolCalls})
		})
	if err != nil {
		send(Frame{Type: "error", RequestID: req.RequestID, Error: err.Error()})
	}
}
