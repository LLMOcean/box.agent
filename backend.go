package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// llmBackend talks to a local OpenAI-compatible LLM server. Ollama, vLLM,
// llama.cpp-server, and text-generation-inference all implement this wire
// format at POST /v1/chat/completions, so one client covers all of them.
type llmBackend struct {
	baseURL string
	apiKey  string
	client  *http.Client

	// modelOverride, if set, replaces whatever model name arrives in the
	// chat request before it's sent to the backend. The router only ever
	// sends the part of the model string after the provider prefix (e.g.
	// "chat", with "tcclaviger/" stripped off by the router's own
	// provider/model split), but a backend like vLLM identifies its loaded
	// model by the full repo id it was started with (e.g.
	// "tcclaviger/Qwen3.6-40B-..."). Set this when those two don't match.
	modelOverride string
}

// effectiveModel returns the model name to send to the backend: the
// override if one is configured, otherwise whatever the router requested.
func (b *llmBackend) effectiveModel(requested string) string {
	if b.modelOverride != "" {
		return b.modelOverride
	}
	return requested
}

type openaiMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type openaiChatRequest struct {
	Model      string           `json:"model"`
	Messages   []openaiMessage  `json:"messages"`
	Stream     bool             `json:"stream,omitempty"`
	MaxTokens  int              `json:"max_tokens,omitempty"`
	Tools      []ToolDefinition `json:"tools,omitempty"`
	ToolChoice json.RawMessage  `json:"tool_choice,omitempty"`
}

func (b *llmBackend) newRequest(body openaiChatRequest) (*http.Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, b.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	return req, nil
}

// chat performs a non-streaming completion and returns the full content,
// usage, normalized finish reason, and any tool calls the model made.
func (b *llmBackend) chat(model string, msgs []openaiMessage, maxTokens int, tools []ToolDefinition, toolChoice json.RawMessage) (string, *Usage, string, []ToolCall, error) {
	httpReq, err := b.newRequest(openaiChatRequest{
		Model:      b.effectiveModel(model),
		Messages:   msgs,
		MaxTokens:  maxTokens,
		Tools:      tools,
		ToolChoice: toolChoice,
	})
	if err != nil {
		return "", nil, "", nil, err
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return "", nil, "", nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, "", nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, "", nil, fmt.Errorf("llm backend error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var or struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &or); err != nil {
		return "", nil, "", nil, fmt.Errorf("unmarshal response: %w", err)
	}

	var content, finishReason string
	var toolCalls []ToolCall
	if len(or.Choices) > 0 {
		content = or.Choices[0].Message.Content
		finishReason = or.Choices[0].FinishReason
		toolCalls = or.Choices[0].Message.ToolCalls
	}
	usage := &Usage{InputTokens: or.Usage.PromptTokens, OutputTokens: or.Usage.CompletionTokens}
	return content, usage, finishReason, toolCalls, nil
}

// runningModel returns the model id currently served by the local LLM
// backend: the configured override if set, otherwise whatever the
// backend's own /v1/models endpoint reports as loaded.
func (b *llmBackend) runningModel() (string, error) {
	if b.modelOverride != "" {
		return b.modelOverride, nil
	}

	req, err := http.NewRequest(http.MethodGet, b.baseURL+"/v1/models", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm backend error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var lr struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &lr); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}
	if len(lr.Data) == 0 {
		return "", fmt.Errorf("no models reported at %s/v1/models", b.baseURL)
	}
	return lr.Data[0].ID, nil
}

// stream performs a streaming completion, calling onChunk for every
// incremental delta and onFinal exactly once when the stream ends. Tool
// calls are accumulated across the stream (vLLM, like OpenAI, streams them
// as incremental per-index argument deltas) and passed to onFinal whole,
// rather than forwarded chunk by chunk.
func (b *llmBackend) stream(model string, msgs []openaiMessage, maxTokens int, tools []ToolDefinition, toolChoice json.RawMessage, onChunk func(string), onFinal func(*Usage, string, []ToolCall)) error {
	httpReq, err := b.newRequest(openaiChatRequest{
		Model:      b.effectiveModel(model),
		Messages:   msgs,
		Stream:     true,
		MaxTokens:  maxTokens,
		Tools:      tools,
		ToolChoice: toolChoice,
	})
	if err != nil {
		return err
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("llm backend error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var inputTokens, outputTokens int
	var finishReason string
	var toolCallOrder []int
	toolCalls := make(map[int]*ToolCall)

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}

		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta.Content != "" {
				onChunk(chunk.Choices[0].Delta.Content)
			}
			if chunk.Choices[0].FinishReason != "" {
				finishReason = chunk.Choices[0].FinishReason
			}
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				existing, ok := toolCalls[tc.Index]
				if !ok {
					existing = &ToolCall{Type: "function"}
					toolCalls[tc.Index] = existing
					toolCallOrder = append(toolCallOrder, tc.Index)
				}
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Function.Name != "" {
					existing.Function.Name = tc.Function.Name
				}
				existing.Function.Arguments += tc.Function.Arguments
			}
		}
	}

	var collected []ToolCall
	for _, idx := range toolCallOrder {
		collected = append(collected, *toolCalls[idx])
	}
	onFinal(&Usage{InputTokens: inputTokens, OutputTokens: outputTokens}, finishReason, collected)
	return scanner.Err()
}
