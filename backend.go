package main

import (
	"bufio"
	"bytes"
	"fmt"
	json "github.com/goccy/go-json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// backendError classifies a non-200 response from the local LLM backend with
// its real HTTP status, instead of collapsing it into a plain string like
// fmt.Errorf does. connection.go's handleChat reads StatusCode back out via
// errors.As to put it on the "error" Frame sent to the router - without it,
// the router had no way to tell a backend's 400 (bad tools/validation, the
// client's fault) apart from a genuine connect failure, and flattened both
// to a hardcoded 502.
type backendError struct {
	StatusCode int
	Body       string
}

func (e *backendError) Error() string {
	return fmt.Sprintf("llm backend error (status %d): %s", e.StatusCode, e.Body)
}

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

// openaiModelCard is the subset of vLLM's GET /v1/models entry this agent
// reads for auto-detection (see vLLM's ModelCard in
// entrypoints/openai/models/engine/protocol.go: id/root/max_model_len).
// Root is the base model path vLLM was launched with - almost always the
// literal Hugging Face repo id, per -backend-model's own documented
// convention. Other OpenAI-compatible servers (Ollama included) either omit
// these fields entirely or don't answer GET /v1/models with useful values
// here at all, so this is best-effort and only meaningfully populated
// against vLLM.
type openaiModelCard struct {
	ID          string `json:"id"`
	Root        string `json:"root"`
	MaxModelLen int    `json:"max_model_len"`
}

type openaiModelList struct {
	Data []openaiModelCard `json:"data"`
}

// looksLikeHuggingFaceRepoID reports whether s has the shape of a Hugging
// Face repo id ("org/repo") rather than, say, a local filesystem path
// (vLLM's -model can be either) or some other opaque server-assigned id.
func looksLikeHuggingFaceRepoID(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") {
		return false
	}
	parts := strings.Split(s, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// healthCheck probes the local backend with a cheap GET /v1/models, returning
// whatever error the HTTP client itself produced (refused/timeout/DNS -
// exactly the "Cannot connect to host" case that shows up as a 502 three
// hops downstream once a real chat request finally hits it). Any actual HTTP
// response, even a non-200 one, means the process is up and answering, so
// it's treated as healthy here - this is a liveness probe, not a
// correctness check.
func (b *llmBackend) healthCheck() error {
	req, err := http.NewRequest(http.MethodGet, b.baseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// monitorBackendHealth polls backend.healthCheck on an interval for the
// lifetime of the process and reports only state transitions (not every
// tick) to the management API via api.reportHealth - so a healthy steady
// state never spams the API, and an outage is reported once it's flagged,
// not once per failed probe. consecutiveFailThreshold debounces a single
// blip (a GC pause, a slow reload) the same way intelligence-workers'
// transientBackoff debounces a single failed job - only a run of failures
// is treated as "actually down."
//
// vllm, when non-nil (only set under -deploy-vllm, see main.go), is killed
// once a run of failures crosses the same threshold - box-agent owns that
// child process (see vllm.go's vllmSupervisor), so unlike a backend started
// externally, it can actually act on a bad health signal instead of only
// reporting it. The supervisor's own restart-on-exit loop then relaunches
// it, exactly as it would after a crash. bootGrace pauses checks afterward
// so a slow reboot isn't mistaken for a second outage and killed again
// mid-boot.
//
// gate.markUnhealthy/markHealthy track the same transitions as the
// reportHealth calls below: going unhealthy force-closes this agent's
// router connection(s) (routerGate, gate.go) so the router stops routing
// requests into a backend that's down, and going healthy again releases the
// connection loop to redial.
func monitorBackendHealth(backend *llmBackend, api *apiClient, vllm *vllmSupervisor, gate *routerGate) {
	const (
		checkInterval            = 20 * time.Second
		consecutiveFailThreshold = 3
		bootGrace                = 3 * time.Minute
	)

	consecutiveFails := 0
	healthy := true

	for {
		time.Sleep(checkInterval)

		err := backend.healthCheck()
		if err == nil {
			consecutiveFails = 0
			if !healthy {
				log.Printf("local LLM backend recovered — reporting healthy and reconnecting to router")
				if reportErr := api.reportHealth(true, ""); reportErr != nil {
					log.Printf("failed to report backend health recovery: %v", reportErr)
				}
				gate.markHealthy()
				healthy = true
			}
			continue
		}

		consecutiveFails++
		if healthy && consecutiveFails >= consecutiveFailThreshold {
			log.Printf("local LLM backend unreachable (%d consecutive checks): %v — reporting unhealthy and cutting router connection", consecutiveFails, err)
			if reportErr := api.reportHealth(false, err.Error()); reportErr != nil {
				log.Printf("failed to report backend health outage: %v", reportErr)
			}
			gate.markUnhealthy()
			healthy = false
		}

		if vllm != nil && consecutiveFails == consecutiveFailThreshold {
			vllm.kill()
			consecutiveFails = 0
			time.Sleep(bootGrace)
		}
	}
}

// modelInfo queries the generic OpenAI-compatible GET /v1/models endpoint
// for context length and a Hugging Face repo id, as a fallback for backends
// that aren't Ollama (see ollamaModelInfo/huggingFaceRepoID in ollama.go for
// the Ollama-specific and flag-derived paths, tried first). Matches by id
// when there's more than one entry, else falls back to the sole entry (some
// vLLM versions don't echo back -served-model-name in this list). Returns
// zero values, never an error, on any failure - same "unknown, caller falls
// back further" contract as ollamaModelInfo.
func (b *llmBackend) modelInfo(model string) (contextLength int, hfID string) {
	req, err := http.NewRequest(http.MethodGet, b.baseURL+"/v1/models", nil)
	if err != nil {
		return 0, ""
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, ""
	}

	var list openaiModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || len(list.Data) == 0 {
		return 0, ""
	}

	card := list.Data[0]
	for _, c := range list.Data {
		if c.ID == model {
			card = c
			break
		}
	}

	contextLength = card.MaxModelLen
	if looksLikeHuggingFaceRepoID(card.Root) {
		hfID = card.Root
	} else if looksLikeHuggingFaceRepoID(card.ID) {
		hfID = card.ID
	}
	return contextLength, hfID
}

type openaiMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type openaiChatRequest struct {
	Model         string           `json:"model"`
	Messages      []openaiMessage  `json:"messages"`
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions *streamOptions   `json:"stream_options,omitempty"`
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Tools         []ToolDefinition `json:"tools,omitempty"`
	ToolChoice    json.RawMessage  `json:"tool_choice,omitempty"`

	// Sampling parameters, forwarded verbatim from the router's chat frame -
	// same field names most OpenAI-compatible servers (Ollama, vLLM) already
	// expect. See SamplingParams in frame.go.
	SamplingParams
}

// streamOptions requests the final usage-bearing SSE chunk on a streaming
// completion. Without this, vLLM/OpenAI-compatible backends omit usage
// entirely from the stream, which is why stream() below used to report
// InputTokens/OutputTokens as 0 for every streamed request.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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
// usage, normalized finish reason, any tool calls the model made, and the
// backend's own logprobs object verbatim (nil unless params.Logprobs asked
// for it).
func (b *llmBackend) chat(model string, msgs []openaiMessage, maxTokens int, tools []ToolDefinition, toolChoice json.RawMessage, params SamplingParams) (string, *Usage, string, []ToolCall, json.RawMessage, error) {
	httpReq, err := b.newRequest(openaiChatRequest{
		Model:          b.effectiveModel(model),
		Messages:       msgs,
		MaxTokens:      maxTokens,
		Tools:          tools,
		ToolChoice:     toolChoice,
		SamplingParams: params,
	})
	if err != nil {
		return "", nil, "", nil, nil, err
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return "", nil, "", nil, nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, "", nil, nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, "", nil, nil, &backendError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var or struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string          `json:"finish_reason"`
			Logprobs     json.RawMessage `json:"logprobs"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &or); err != nil {
		return "", nil, "", nil, nil, fmt.Errorf("unmarshal response: %w", err)
	}

	var content, finishReason string
	var toolCalls []ToolCall
	var logprobs json.RawMessage
	if len(or.Choices) > 0 {
		content = or.Choices[0].Message.Content
		finishReason = or.Choices[0].FinishReason
		toolCalls = or.Choices[0].Message.ToolCalls
		logprobs = or.Choices[0].Logprobs
	}
	usage := &Usage{InputTokens: or.Usage.PromptTokens, OutputTokens: or.Usage.CompletionTokens}
	return content, usage, finishReason, toolCalls, logprobs, nil
}

// stream performs a streaming completion, calling onChunk for every
// incremental delta and onFinal exactly once when the stream ends. Tool
// calls are accumulated across the stream (vLLM, like OpenAI, streams them
// as incremental per-index argument deltas) and passed to onFinal whole,
// rather than forwarded chunk by chunk.
func (b *llmBackend) stream(model string, msgs []openaiMessage, maxTokens int, tools []ToolDefinition, toolChoice json.RawMessage, params SamplingParams, onChunk func(string, json.RawMessage), onFinal func(*Usage, string, []ToolCall)) error {
	httpReq, err := b.newRequest(openaiChatRequest{
		Model:          b.effectiveModel(model),
		Messages:       msgs,
		Stream:         true,
		StreamOptions:  &streamOptions{IncludeUsage: true},
		MaxTokens:      maxTokens,
		Tools:          tools,
		ToolChoice:     toolChoice,
		SamplingParams: params,
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
		return &backendError{StatusCode: resp.StatusCode, Body: string(respBody)}
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
				FinishReason string          `json:"finish_reason"`
				Logprobs     json.RawMessage `json:"logprobs"`
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
				onChunk(chunk.Choices[0].Delta.Content, chunk.Choices[0].Logprobs)
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
