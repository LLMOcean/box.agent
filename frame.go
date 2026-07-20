package main

import "encoding/json"

// Frame is the wire message exchanged with router.agent over the
// /v1/agents/connect WebSocket connection. One flat shape covers every
// message kind in both directions; fields not relevant to a given Type are
// omitted. See router.agent's docs/AGENT_PROTOCOL.md for the full spec.
//
// Tools/ToolChoice only ever arrive on a "chat" frame (router -> agent).
// ToolCalls is only ever sent back on a "response"/"final" frame (agent ->
// router) - accumulated across a stream and attached once, like Usage and
// FinishReason, rather than streamed as incremental argument deltas.
type Frame struct {
	Type         string           `json:"type"` // "chat" | "hello" | "chunk" | "response" | "final" | "error"
	RequestID    string           `json:"request_id"`
	Model        string           `json:"model,omitempty"`
	System       string           `json:"system,omitempty"`
	Messages     []Message        `json:"messages,omitempty"`
	Stream       bool             `json:"stream,omitempty"`
	MaxTokens    int              `json:"max_tokens,omitempty"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
	ToolChoice   json.RawMessage  `json:"tool_choice,omitempty"`
	Content      string           `json:"content,omitempty"`
	Usage        *Usage           `json:"usage,omitempty"`
	FinishReason string           `json:"finish_reason,omitempty"`
	ToolCalls    []ToolCall       `json:"tool_calls,omitempty"`
	Error        string           `json:"error,omitempty"`
	Capabilities *Capabilities    `json:"capabilities,omitempty"` // "hello" only, sent once right after connecting

	// Sampling/output-control parameters - "chat" only, forwarded to the
	// local backend verbatim (same field names/types OpenAI's API uses; see
	// SamplingParams). Whether the local backend actually honors any given
	// one isn't this agent's concern - forward what's set and let the
	// backend ignore what it doesn't support.
	SamplingParams
}

// SamplingParams is the optional sampling/output-control block of a "chat"
// frame - see router.agent's docs/openrouter/02-sampling-parameters.md.
// Embedded directly into Frame (not nested under a JSON sub-object) so the
// wire shape matches router.agent's own wsagent.Frame field-for-field.
type SamplingParams struct {
	Temperature       *float64           `json:"temperature,omitempty"`
	TopP              *float64           `json:"top_p,omitempty"`
	TopK              *int               `json:"top_k,omitempty"`
	FrequencyPenalty  *float64           `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64           `json:"presence_penalty,omitempty"`
	RepetitionPenalty *float64           `json:"repetition_penalty,omitempty"`
	MinP              *float64           `json:"min_p,omitempty"`
	TopA              *float64           `json:"top_a,omitempty"`
	Seed              *int64             `json:"seed,omitempty"`
	Stop              []string           `json:"stop,omitempty"`
	LogitBias         map[string]float64 `json:"logit_bias,omitempty"`
	Logprobs          bool               `json:"logprobs,omitempty"`
	TopLogprobs       *int               `json:"top_logprobs,omitempty"`
	ResponseFormat    json.RawMessage    `json:"response_format,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	Reasoning         json.RawMessage    `json:"reasoning,omitempty"`
	ReasoningEffort   string             `json:"reasoning_effort,omitempty"`
}

// Capabilities is this agent's optional, operator-asserted one-time
// self-report of what its local backend supports, sent in a "hello" frame
// immediately after connecting (see router.agent's docs/AGENT_PROTOCOL.md
// §1a). Every field is optional — omit what you don't know; the router
// treats a missing/never-sent Capabilities exactly as it did before this
// frame type existed.
type Capabilities struct {
	ContextLength     int      `json:"context_length,omitempty"`
	MaxOutputLength   int      `json:"max_output_length,omitempty"`
	InputModalities   []string `json:"input_modalities,omitempty"`
	OutputModalities  []string `json:"output_modalities,omitempty"`
	Quantization      string   `json:"quantization,omitempty"`
	SupportedFeatures []string `json:"supported_features,omitempty"`
}

// Message is a single chat turn, used only inside a "chat" Frame.
// ToolCalls is set on an assistant message that invoked tools; ToolCallID
// is set on a "tool"-role message supplying one tool's result (in Content).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolDefinition describes one callable function the model may invoke.
// Mirrors OpenAI's tools shape (router.agent's models.ToolDefinition) -
// vLLM's OpenAI-compatible server accepts this as-is.
type ToolDefinition struct {
	Type     string       `json:"type"` // always "function" today
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function body of a ToolDefinition.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is a single function invocation requested by the model.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function" today
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction names the function to call and its arguments, encoded
// as a JSON string (not a nested object) - OpenAI's convention, which is
// also what vLLM's OpenAI-compatible API produces.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage carries token counts back to the router for cost computation.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
