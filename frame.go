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
	Type         string           `json:"type"` // "chat" | "chunk" | "response" | "final" | "error"
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
