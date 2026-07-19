package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// connectAndServe dials the router and serves chat requests until the
// connection drops, at which point it returns so the caller can reconnect.
func connectAndServe(connectURL, token string, backend *llmBackend) error {
	conn, _, err := websocket.DefaultDialer.Dial(connectURL, http.Header{"Authorization": {"Bearer " + token}})
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

		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			log.Printf("bad frame: %v", err)
			continue
		}
		if f.Type != "chat" {
			continue
		}

		// Multiple requests can be in flight on one connection at once —
		// handle each independently so a slow request doesn't block others.
		wg.Add(1)
		go func(req Frame) {
			defer wg.Done()
			handleChat(req, backend, send)
		}(f)
	}
}

func handleChat(req Frame, backend *llmBackend, send func(Frame) error) {
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
		content, usage, finishReason, toolCalls, err := backend.chat(req.Model, msgs, req.MaxTokens, req.Tools, req.ToolChoice)
		if err != nil {
			send(Frame{Type: "error", RequestID: req.RequestID, Error: err.Error()})
			return
		}
		send(Frame{Type: "response", RequestID: req.RequestID, Content: content, Usage: usage, FinishReason: finishReason, ToolCalls: toolCalls})
		return
	}

	err := backend.stream(req.Model, msgs, req.MaxTokens, req.Tools, req.ToolChoice,
		func(chunk string) { send(Frame{Type: "chunk", RequestID: req.RequestID, Content: chunk}) },
		func(usage *Usage, finishReason string, toolCalls []ToolCall) {
			send(Frame{Type: "final", RequestID: req.RequestID, Usage: usage, FinishReason: finishReason, ToolCalls: toolCalls})
		})
	if err != nil {
		send(Frame{Type: "error", RequestID: req.RequestID, Error: err.Error()})
	}
}
