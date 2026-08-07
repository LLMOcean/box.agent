// Command fakellm is a minimal OpenAI-compatible chat server for testing
// box-agent without a real local LLM (Ollama, vLLM, ...) running. Point
// box-agent's -llm-url at it and it answers /v1/models and
// /v1/chat/completions (streaming and non-streaming) with a canned,
// deterministic reply so you can exercise the router<->agent<->backend
// path end to end.
//
// Trigger a fake tool call instead of a text reply by making the last
// user message "TOOL:<name>:<json-args>", e.g.:
//
//	TOOL:get_weather:{"city":"Istanbul"}
package main

import (
	"flag"
	"fmt"
	json "github.com/goccy/go-json"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":11434", "address to listen on")
	model := flag.String("model", "fake-model", "model id reported by /v1/models and echoed back in responses")
	chunkDelay := flag.Duration("chunk-delay", 30*time.Millisecond, "delay between streamed chunks, to simulate real generation")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		handleModels(w, *model)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		handleChatCompletions(w, r, *model, *chunkDelay)
	})

	log.Printf("fakellm listening on %s (model=%q)", *addr, *model)
	log.Fatal(http.ListenAndServe(*addr, logRequests(mux)))
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func handleModels(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": model, "object": "model"},
		},
	})
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
	Tools  []struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolCallFunction `json:"function"`
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request, model string, chunkDelay time.Duration) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}

	var lastUser string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = req.Messages[i].Content
			break
		}
	}

	promptTokens := countTokens(req.Messages)

	if name, args, ok := parseToolTrigger(lastUser); ok {
		tc := toolCall{ID: "call_fake_1", Type: "function", Function: toolCallFunction{Name: name, Arguments: args}}
		if req.Stream {
			streamToolCall(w, model, tc, promptTokens, chunkDelay)
		} else {
			respondToolCall(w, model, tc, promptTokens)
		}
		return
	}

	reply := fmt.Sprintf("Echo: %s", lastUser)
	if req.Stream {
		streamText(w, model, reply, promptTokens, chunkDelay)
	} else {
		respondText(w, model, reply, promptTokens)
	}
}

// parseToolTrigger recognizes "TOOL:<name>:<json-args>" in the last user
// message and splits it into a function name and raw JSON arguments string.
func parseToolTrigger(content string) (name, args string, ok bool) {
	if !strings.HasPrefix(content, "TOOL:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(content, "TOOL:")
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func countTokens[T any](msgs []T) int {
	// Rough stand-in for a real tokenizer: word count is close enough for
	// exercising usage-reporting plumbing in tests.
	total := 0
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		total += len(strings.Fields(string(b)))
	}
	return total
}

func respondText(w http.ResponseWriter, model, content string, promptTokens int) {
	completionTokens := len(strings.Fields(content))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      "fakellm-1",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	})
}

func respondToolCall(w http.ResponseWriter, model string, tc toolCall, promptTokens int) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      "fakellm-1",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": nil, "tool_calls": []toolCall{tc}},
				"finish_reason": "tool_calls",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": 0,
			"total_tokens":      promptTokens,
		},
	})
}

func streamText(w http.ResponseWriter, model, content string, promptTokens int, chunkDelay time.Duration) {
	flusher, ok := startStream(w)
	if !ok {
		return
	}

	words := strings.Fields(content)
	for i, word := range words {
		piece := word
		if i < len(words)-1 {
			piece += " "
		}
		writeChunk(w, map[string]any{
			"id": "fakellm-1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": piece}, "finish_reason": nil}},
		})
		flusher.Flush()
		time.Sleep(chunkDelay)
	}

	writeChunk(w, map[string]any{
		"id": "fakellm-1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage": map[string]any{
			"prompt_tokens": promptTokens, "completion_tokens": len(words), "total_tokens": promptTokens + len(words),
		},
	})
	flusher.Flush()
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func streamToolCall(w http.ResponseWriter, model string, tc toolCall, promptTokens int, chunkDelay time.Duration) {
	flusher, ok := startStream(w)
	if !ok {
		return
	}

	writeChunk(w, map[string]any{
		"id": "fakellm-1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": tc.ID, "function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			}}},
			"finish_reason": nil,
		}},
	})
	flusher.Flush()
	time.Sleep(chunkDelay)

	writeChunk(w, map[string]any{
		"id": "fakellm-1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		"usage": map[string]any{
			"prompt_tokens": promptTokens, "completion_tokens": 0, "total_tokens": promptTokens,
		},
	})
	flusher.Flush()
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func startStream(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	return flusher, true
}

func writeChunk(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
}
