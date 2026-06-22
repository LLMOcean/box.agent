package main

// Frame is the wire message exchanged with router.agent over the
// /v1/agents/connect WebSocket connection. One flat shape covers every
// message kind in both directions; fields not relevant to a given Type are
// omitted. See router.agent's docs/AGENT_PROTOCOL.md for the full spec.
type Frame struct {
	Type         string    `json:"type"` // "chat" | "chunk" | "response" | "final" | "error"
	RequestID    string    `json:"request_id"`
	Model        string    `json:"model,omitempty"`
	System       string    `json:"system,omitempty"`
	Messages     []Message `json:"messages,omitempty"`
	Stream       bool      `json:"stream,omitempty"`
	MaxTokens    int       `json:"max_tokens,omitempty"`
	Content      string    `json:"content,omitempty"`
	Usage        *Usage    `json:"usage,omitempty"`
	FinishReason string    `json:"finish_reason,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// Message is a single chat turn, used only inside a "chat" Frame.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage carries token counts back to the router for cost computation.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
