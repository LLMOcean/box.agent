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
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiChatRequest struct {
	Model    string          `json:"model"`
	Messages []openaiMessage `json:"messages"`
	Stream   bool            `json:"stream,omitempty"`
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
// usage, and normalized finish reason.
func (b *llmBackend) chat(model string, msgs []openaiMessage) (string, *Usage, string, error) {
	httpReq, err := b.newRequest(openaiChatRequest{Model: model, Messages: msgs})
	if err != nil {
		return "", nil, "", err
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return "", nil, "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, "", fmt.Errorf("llm backend error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var or struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &or); err != nil {
		return "", nil, "", fmt.Errorf("unmarshal response: %w", err)
	}

	var content, finishReason string
	if len(or.Choices) > 0 {
		content = or.Choices[0].Message.Content
		finishReason = or.Choices[0].FinishReason
	}
	usage := &Usage{InputTokens: or.Usage.PromptTokens, OutputTokens: or.Usage.CompletionTokens}
	return content, usage, finishReason, nil
}

// stream performs a streaming completion, calling onChunk for every
// incremental delta and onFinal exactly once when the stream ends.
func (b *llmBackend) stream(model string, msgs []openaiMessage, onChunk func(string), onFinal func(*Usage, string)) error {
	httpReq, err := b.newRequest(openaiChatRequest{Model: model, Messages: msgs, Stream: true})
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
					Content string `json:"content"`
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
		}
	}

	onFinal(&Usage{InputTokens: inputTokens, OutputTokens: outputTokens}, finishReason)
	return scanner.Err()
}
