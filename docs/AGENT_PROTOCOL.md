# Agent Protocol — Router ↔ Remote WebSocket Agent

> Copied from [`router.agent`](https://github.com/LLMOcean/router.agent)'s
> `docs/AGENT_PROTOCOL.md` — kept here as the protocol reference for this
> implementation. The canonical copy lives in the router repo; if it changes
> there, re-sync this copy.

This document defines the wire protocol between `router.agent` and a remote
**agent** process — typically something you run alongside a self-hosted,
open-source LLM (vLLM, Ollama, llama.cpp-server, etc.) on a machine the router
cannot dial into directly (private network, NAT, no public IP). The agent
dials *out* to the router and keeps a persistent connection open; the router
pushes chat requests down that connection and reads results back.

It is independent of language/runtime — implement it in whatever the LLM
host's ecosystem favors (Go, Python, ...). This repo (`box.agent`) is a
working Go implementation of it — see `main.go`, `connection.go`,
`backend.go`, and `frame.go` at the repo root.

If you want the fuller build spec this implementation follows (connection
lifecycle, concurrency, configuration, packaging on top of the protocol
defined here), see [`AGENT_BUILD_SPEC.md`](AGENT_BUILD_SPEC.md) in this same
folder.

---

## 1. Connect handshake

```
GET /v1/agents/connect?provider=<provider>/<model>
Authorization: Bearer <token>
```

- `token` — a per-instance token issued by the orchestrator (`POST
  /llmocean/agent-instances/authenticate` — see `router.agent`'s
  `docs/ORCHESTRATOR_API.md` §7). The router validates it against the
  orchestrator on every connect, purely for audit/logging (agent instance ID,
  account, etc.) — it carries no provider/model declaration of its own.
- `provider` — declared by the agent itself at connect time; there's no
  static declaration of this anywhere else. Split on the first `/`, same
  convention used everywhere else in the router:
  - `<provider>/<model>` (e.g. `plusclouds/deepseek-v3`) registers a
    connection under the provider namespace `plusclouds`, serving model
    `deepseek-v3`. Customers route to it with
    `{"model": "plusclouds/deepseek-v3"}` — same "provider/model" addressing
    used for `claude`/`openai`. A provider namespace can host several
    different models across different connections; the router only ever
    routes a request to a connection actually serving the requested model
    (see `GET /v1/agents/status`, which reports live counts per
    `provider/model` pair).
  - A bare value with no `/` (e.g. `deepseek-v3`) registers under that name
    with itself as both provider and model — customers address it with
    just `{"model": "deepseek-v3"}`, no prefix. This is the same behavior as
    before provider namespaces existed.
  - Multiple agent instances declaring the identical `provider/model` (or
    identical bare name) pool together and the router round-robins requests
    across them; this is the only load-distribution mechanism — it's
    name-based, not orchestrator-authorized grouping.
- This implementation determines the model by querying its local
  OpenAI-compatible backend's `GET /v1/models` and using the first entry (or
  the operator-supplied `-backend-model` override), optionally prefixed with
  a `-provider` namespace — see `backend.go`'s `runningModel()` and
  `main.go`.

On success, the router responds with the standard WebSocket `101 Switching
Protocols` upgrade. On failure (missing `provider`, missing/malformed bearer
token, or the orchestrator rejects it), the router responds with a plain
`400`/`401` and does not upgrade; a `503` means the orchestrator itself was
unreachable — safe to retry with backoff, unlike a `401`.

The agent should reconnect with backoff if the connection drops — there is no
session to resume; every request is self-contained (see below).

## 1a. Capability self-report (`hello` frame)

Immediately after the WebSocket upgrade completes — before any `chat`
traffic — the agent may send one `hello` frame declaring what it serves:

```json
{
  "type": "hello",
  "capabilities": {
    "context_length": 32768,
    "max_output_length": 4096,
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "quantization": "fp8",
    "supported_features": ["tools"]
  }
}
```

| Field | Description |
|---|---|
| `context_length` | Context window size in tokens. |
| `max_output_length` | Max output tokens the backend supports. This implementation also enforces it: a `chat` frame's `max_tokens` above this value is clamped down before being sent to the local backend, and the clamped value is echoed back as `effective_max_tokens` on the matching `response`/`final` (see §3). |
| `input_modalities` / `output_modalities` | e.g. `["text"]`, `["text","image"]`. |
| `quantization` | e.g. `"fp8"`, `"int4"`. |
| `supported_features` | Capability tags the backend actually supports, e.g. `["tools","json_mode","supports_reasoning"]`. |

All fields are optional and operator-asserted — the router does not verify
them against the backend. They feed the router's `/v1/models` catalog
(taking precedence over any statically-declared metadata for that model,
since a connected agent is the more current ground truth) — see
`router.agent`'s `docs/openrouter/01-provider-integration.md`.

Sending `hello` is optional and this is purely additive: an agent that never
sends one behaves exactly as agents did before this frame type existed — the
router just has zero-value capability data for it. `hello` is never sent in
response to anything and has no reply.

This implementation sends `hello` unconditionally right after connecting
(see `connectAndServe` in `connection.go`), built from the
`-context-length`/`-max-output-length`/`-quantization`/`-input-modalities`/
`-output-modalities`/`-supported-features` flags (all optional; see
`AGENT_BUILD_SPEC.md` §8). `"supports_reasoning"` specifically is not left to
the operator to remember: this implementation adds it automatically whenever
`-vllm-reasoning-parser` is set, the same way it already refuses to declare
`"tools"` without the matching tool-calling flags (see `main.go`) — a
reasoning model booted without that tag is a config bug, not a valid catalog
state.

## 2. Keepalive

The router sends a WebSocket ping every ~30 seconds and expects a pong within
~60 seconds; otherwise it considers the connection dead and removes it from
the pool. Most WebSocket client libraries answer pings automatically — if
yours doesn't, reply to each ping with a pong of the same payload.

## 3. Frame format

Every message in both directions is a single JSON text frame with this flat
shape (fields not relevant to a given `type` are omitted):

```json
{
  "type": "chat",
  "request_id": "a3f1c2d4e5b6a7f8c9d0e1f2a3b4c5d6",
  "model": "llama-3.1-70b-instruct",
  "system": "You are a helpful assistant.",
  "messages": [{"role": "user", "content": "What is 2+2?"}],
  "stream": false,
  "max_tokens": 1024,
  "content": "",
  "usage": {"input_tokens": 0, "output_tokens": 0, "cost_usd": 0},
  "finish_reason": "",
  "error": ""
}
```

| Field | Type | Used by | Description |
|---|---|---|---|
| `type` | string | all | `"chat"` (router→agent), `"chunk"`, `"response"`, `"final"`, `"error"` (agent→router), `"hello"` (agent→router, optional, once) |
| `request_id` | string | all | Correlates every frame of one request. Generated by the router on `"chat"`; echoed back verbatim on every response frame. One physical connection carries many concurrent requests at once — never reuse another request's ID. Omitted/empty on `"hello"`, which has no reply. |
| `model` | string | `chat` | Exact model string to run. |
| `system` | string | `chat` | System prompt, if any. |
| `messages` | array | `chat` | `[{"role": "user"\|"assistant", "content": "..."}]`. System-role messages are never sent here — they're already folded into `system`. |
| `stream` | bool | `chat` | `true` → respond with one or more `"chunk"` frames followed by exactly one `"final"` frame. `false` → respond with exactly one `"response"` frame. |
| `max_tokens` | int | `chat` | Output token cap, if the caller specified one. This is a single combined budget covering both `reasoning_content` and `content` — vLLM has no separate cap for reasoning today, so a reasoning model can spend the entire budget thinking and return `content: ""` with `finish_reason: "length"`, having still billed the full `max_tokens` as `output_tokens`. Watch `reasoning_tokens` in `usage` to tell that case apart from a genuinely empty response: `reasoning_tokens` at or near `output_tokens` with empty `content` means the cap was too low for this model, not that it had nothing to say. Callers of reasoning models should budget `max_tokens` well above what they'd set for a non-reasoning model of the same size. If `max_tokens` exceeds this agent's own `max_output_length` (see §1a), it's silently clamped down before reaching the backend rather than forwarded as-is — see `effective_max_tokens` below. |
| `tools` / `tool_choice` | array / raw | `chat` | Forwarded to the local backend as-is; see `backend.go`. |
| `temperature`, `top_p`, `top_k`, `frequency_penalty`, `presence_penalty`, `repetition_penalty`, `min_p`, `top_a`, `seed`, `stop`, `logit_bias`, `logprobs`, `top_logprobs`, `response_format`, `parallel_tool_calls`, `reasoning`, `reasoning_effort` | various | `chat` | Sampling/output-control parameters, forwarded to the local backend verbatim - same field names/types OpenAI's API uses. This agent doesn't filter by what the backend actually supports; unsupported fields are the backend's concern to ignore. See router.agent's `docs/openrouter/02-sampling-parameters.md` and this repo's `SamplingParams` in `frame.go`. All optional. |
| `content` | string | `chunk`, `response` | Output text. For `chunk`, an incremental delta (not cumulative). For `response`, the full output. |
| `reasoning_content` | string | `chunk`, `response` | A reasoning model's chain-of-thought, kept separate from `content` so it's never mistaken for the visible answer. Same split as vLLM's own `--reasoning-parser` output (`message.reasoning_content` / `delta.reasoning_content`). Only present when the local backend actually reports it — most backends/models leave this unset, in which case any reasoning the model does is invisibly folded into billed `output_tokens` with nothing to show for it (see `reasoning_tokens` below and the `max_tokens` note under §4). |
| `logprobs_result` | object | `chunk`, `response` | The local backend's own `choices[0].logprobs` object, forwarded verbatim as opaque JSON — every OpenAI-compatible backend (vLLM included) already emits this in OpenAI's documented shape, so there's nothing to re-model. Only present when the request's `logprobs` field asked for it. Named distinctly from the request-side `logprobs` (a bool) since they're different things on this one flat frame shape. |
| `usage` | object | `response`, `final` | `{"input_tokens": int, "output_tokens": int, "reasoning_tokens": int}`. `reasoning_tokens` is the subset of `output_tokens` spent on `reasoning_content` rather than `content` — omitted/`0` unless the backend reports `usage.completion_tokens_details.reasoning_tokens` (recent vLLM only; see backend.go). Omit `cost_usd` — the router computes cost from its own pricing config. |
| `finish_reason` | string | `response`, `final` | One of `"stop"`, `"length"`, `"tool_calls"`, `"content_filter"` — normalize to this vocabulary regardless of what the underlying LLM server reports. |
| `tool_calls` | array | `response`, `final` | Accumulated across a stream and attached once, like `usage`/`finish_reason`, not streamed incrementally. |
| `effective_max_tokens` | int | `response`, `final` | Only present when the request's `max_tokens` was clamped down to this agent's `max_output_length` (see §1a) before reaching the local backend — omitted when `max_tokens` was honored as requested. |
| `error` | string | `error` | Human-readable error message. Sent instead of `response`/`final` if the request failed. |
| `error_status_code` | int | `error` | The local backend's real HTTP status, when `error` came from a non-200 response it sent (e.g. `400` for a bad `tools` request) rather than a network-level failure to reach it at all. Omit (or `0`) when there's no real status to report — the router then falls back to `502`. See `backendError` in `backend.go`. |
| `capabilities` | object | `hello` | See §1a. |

## 4. Non-streaming exchange

```
Router → Agent:  {"type":"chat","request_id":"r1","model":"llama-3.1-70b-instruct",
                   "messages":[{"role":"user","content":"hi"}],"stream":false}

Agent  → Router: {"type":"response","request_id":"r1","content":"Hello!",
                   "usage":{"input_tokens":5,"output_tokens":3},"finish_reason":"stop"}
```

## 5. Streaming exchange

```
Router → Agent:  {"type":"chat","request_id":"r2","model":"llama-3.1-70b-instruct",
                   "messages":[{"role":"user","content":"hi"}],"stream":true}

Agent  → Router: {"type":"chunk","request_id":"r2","reasoning_content":"The user is greeting me, "}
Agent  → Router: {"type":"chunk","request_id":"r2","reasoning_content":"a simple reply suffices."}
Agent  → Router: {"type":"chunk","request_id":"r2","content":"Hel"}
Agent  → Router: {"type":"chunk","request_id":"r2","content":"lo!"}
Agent  → Router: {"type":"final","request_id":"r2",
                   "usage":{"input_tokens":5,"output_tokens":19,"reasoning_tokens":16},"finish_reason":"stop"}
```

`reasoning_content` and `content` chunks are never mixed on the same
`"chunk"` frame — a reasoning model emits a run of `reasoning_content`
chunks first, then switches to `content` once it starts the visible answer
(or never switches, if `max_tokens` runs out first — see the `max_tokens`
row above).

The router forwards each `chunk` to the customer as an SSE event as soon as
it arrives — there's no buffering on the router side, so latency between an
agent's chunks shows up directly to the end customer.

## 6. Errors

```
Agent → Router: {"type":"error","request_id":"r3","error":"model not loaded"}
Agent → Router: {"type":"error","request_id":"r4","error":"llm backend error (status 400): ...","error_status_code":400}
```

Send this instead of a `response`/`final` frame (not in addition to one). The
router surfaces it to the customer as a `502` with that message. There is no
retry on the router side today — a request fails on the first error from
whichever agent instance it was routed to.

## 7. Concurrency

One WebSocket connection carries many requests in flight simultaneously —
`request_id` is how the router tells them apart on the way back. Your agent
implementation must be able to interleave frames for different `request_id`s
on the same connection (e.g. start streaming chunks for `r2` while `r1` is
still being processed). It does not need to process requests in the order
they arrive.
