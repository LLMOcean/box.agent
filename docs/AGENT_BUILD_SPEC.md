# Standalone Agent — Build Spec (Go)

> Copied from [`router.agent`](https://github.com/LLMOcean/router.agent)'s
> `docs/AGENT_BUILD_SPEC.md`. This repository (`box.agent`) is the standalone
> implementation this spec describes — `main.go`, `connection.go`,
> `backend.go`, and `frame.go` at the repo root already follow it exactly.
> Kept here as the design reference for anyone maintaining or re-deriving
> this implementation; re-sync from the router repo if it changes there.

This is a self-contained spec for building the remote WebSocket agent as its
**own Go module**, deployable independently of `router.agent` (e.g. on a GPU
box running vLLM/Ollama, with no access to the router's source). Hand this
document to a developer or a code-generation tool and the result should be a
working agent without needing anything else from `router.agent`.

For the wire-level contract this implements, see
[`AGENT_PROTOCOL.md`](AGENT_PROTOCOL.md) — this document is the superset:
protocol + connection lifecycle + config + packaging.

---

## 1. What this binary does

1. Dials out to a router's `/v1/agents/connect` endpoint over WebSocket and
   authenticates with a static bearer token.
2. Stays connected, reconnecting with backoff if the connection drops —
   there is no session to resume, every request is self-contained.
3. For every `"chat"` frame it receives, forwards the request to a local
   OpenAI-compatible LLM server (Ollama, vLLM, llama.cpp-server, text-
   generation-inference — anything serving `POST /v1/chat/completions`) and
   translates the HTTP response back into the router's wire frames.
4. Handles many requests concurrently on the one WebSocket connection,
   correlating frames by `request_id`.

## 2. Explicit non-goals

- It does not pick the model, apply rate limits, compute cost, or do
  anything billing-related — the router owns all of that.
- It does not implement retries/fallback across multiple local LLM servers —
  one agent process talks to one local backend. Run multiple agent processes
  (see §8) if you want redundancy; the router round-robins across however
  many connect under the same provider name.
- It does not need a database, config API, or any credential beyond the one
  static token — see `AGENT_PROTOCOL.md` §1 for why (and the planned,
  not-yet-built upgrade path for per-instance orchestrator-issued tokens).

## 3. Requirements checklist

A correct implementation must:

- [ ] Dial `<router>/v1/agents/connect?provider=<name>` with
      `Authorization: Bearer <token>`.
- [ ] Reconnect on any connection error/close, with a delay between
      attempts (fixed 5s is sufficient — see §4).
- [ ] Answer WebSocket pings from the router with pongs (most libraries do
      this automatically; verify yours does, or wire it explicitly).
- [ ] Parse every incoming text frame as JSON; ignore any frame whose
      `type` isn't `"chat"` (forward compatibility).
- [ ] Process multiple `"chat"` frames concurrently — never block reading
      the next frame on finishing the previous request.
- [ ] For `stream: false` requests, send exactly one `"response"` frame.
- [ ] For `stream: true` requests, send one or more `"chunk"` frames
      followed by exactly one `"final"` frame.
- [ ] On any failure (backend unreachable, non-200 from the backend, bad
      response shape), send exactly one `"error"` frame instead of
      `"response"`/`"final"` — never leave a request_id hanging with no
      reply.
- [ ] Echo `request_id` verbatim on every reply frame for a request.
- [ ] Normalize `finish_reason` to one of `"stop"`, `"length"`,
      `"tool_calls"`, `"content_filter"` (pass through whatever the local
      backend reports — OpenAI-compatible servers already use this
      vocabulary).

## 4. Connection lifecycle

```
loop forever:
    dial (router_url + "/v1/agents/connect?provider=" + provider), header Authorization: Bearer <token>
    if dial fails:
        log error, sleep 5s, retry

    on connect:
        log "connected"
        set a ping handler that replies with a pong
        loop:
            read one message
            if read fails (connection closed/reset):
                break out to the outer loop (triggers reconnect)
            parse as a Frame; if type != "chat", ignore
            spawn a concurrent task to handle this request (do not block the read loop)
```

A fixed 5-second retry delay is what this implementation uses and is
sufficient — this mirrors `router.agent`'s philosophy of simple, predictable
behavior over exotic resilience (no circuit breakers anywhere else in that
system either). Exponential backoff/jitter is a reasonable upgrade if you
expect frequent restarts at scale, but is not required.

There is no handshake beyond the HTTP upgrade — no protocol version
negotiation, no capability exchange. If the protocol ever needs to evolve,
new frame `type` values are added and old agents simply ignore types they
don't recognize (per the requirement above).

## 5. Wire protocol (reference — see `AGENT_PROTOCOL.md` for the full spec)

Every message, both directions, is one JSON text frame:

```go
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

type Message struct {
    Role    string `json:"role"`
    Content string `json:"content"`
}

type Usage struct {
    InputTokens  int `json:"input_tokens"`
    OutputTokens int `json:"output_tokens"`
}
```

Define these locally in the standalone module — do not depend on
`router.agent`'s Go module to get them. They are deliberately tiny and
stable. (This repo defines them in `frame.go`.)

The router only ever sends `"chat"` frames. The agent only ever sends
`"chunk"`, `"response"`, `"final"`, or `"error"` frames.

## 6. Concurrency model

One physical WebSocket connection carries many requests in flight at once.
`request_id` is the only thing that tells them apart — it is generated by
the router and must be echoed back unchanged on every reply frame for that
request.

Required shape:

- **One reader.** A single goroutine owns `ReadMessage` in a loop. Never
  read from the same connection concurrently from two goroutines.
- **Synchronized writer.** Multiple goroutines will want to write
  (one per in-flight request) — serialize all writes through a mutex (most
  WebSocket libraries, including gorilla/websocket, are not safe for
  concurrent writers on one connection).
- **Per-request goroutine.** On receiving a `"chat"` frame, spawn a
  goroutine to handle it and immediately go back to reading the next
  message. A slow or streaming request must never block the read loop, or
  every other concurrent request on that connection stalls behind it.

## 7. Local LLM backend integration

Ollama, vLLM, llama.cpp-server, and text-generation-inference all implement
the OpenAI chat-completions wire format at `POST /v1/chat/completions`, so
one client implementation covers all of them:

**Non-streaming request:**
```json
{"model": "<model>", "messages": [{"role":"user","content":"..."}]}
```
**Non-streaming response (fields used):**
```json
{"choices":[{"message":{"content":"..."},"finish_reason":"stop"}],
 "usage":{"prompt_tokens":5,"completion_tokens":4}}
```

**Streaming request:** same body plus `"stream": true`. Response is
Server-Sent Events (`data: {...}\n\n` lines, terminated by `data: [DONE]`).
Each event has `choices[0].delta.content` (the incremental text) and
optionally `choices[0].finish_reason` and a final `usage` object on the
last real event before `[DONE]`.

Translate this 1:1 into the agent's outgoing frames:
- non-streaming → one `"response"` frame with `content`, `usage`,
  `finish_reason`.
- streaming → one `"chunk"` frame per `delta.content` (skip empty deltas),
  then one `"final"` frame with the accumulated `usage`/`finish_reason`.
- any HTTP error or unexpected body → one `"error"` frame with a
  human-readable message (include the backend's status code and body if
  available — vague messages make debugging a deployed agent much harder).

### Reference Go implementation

This is the exact logic already verified working end-to-end against the
router — see `backend.go` in this repo. Reproduced here for spec
completeness:

```go
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
```

### Connection + dispatch loop

This is reproduced from `connection.go` and `main.go` in this repo:

```go
func main() {
    routerURL := flag.String("router", "ws://localhost:8080", "router base URL (ws:// or wss://)")
    provider := flag.String("provider", "", "agent provider name (must match an agents: key in the router's config.yaml)")
    token := flag.String("token", os.Getenv("AGENT_TOKEN"), "shared token for this provider (or set AGENT_TOKEN)")
    llmURL := flag.String("llm-url", "http://localhost:11434", "base URL of the local OpenAI-compatible LLM server")
    llmAPIKey := flag.String("llm-api-key", os.Getenv("LLM_API_KEY"), "bearer token for the local LLM server, if required")
    flag.Parse()

    if *provider == "" || *token == "" {
        log.Fatal("both -provider and -token (or AGENT_TOKEN env var) are required")
    }

    connectURL := fmt.Sprintf("%s/v1/agents/connect?provider=%s", *routerURL, url.QueryEscape(*provider))
    backend := &llmBackend{baseURL: strings.TrimSuffix(*llmURL, "/"), apiKey: *llmAPIKey, client: &http.Client{}}

    for {
        if err := connectAndServe(connectURL, *token, backend); err != nil {
            log.Printf("connection error: %v — reconnecting in 5s", err)
        }
        time.Sleep(5 * time.Second)
    }
}

func connectAndServe(connectURL, token string, backend *llmBackend) error {
    conn, _, err := websocket.DefaultDialer.Dial(connectURL, http.Header{"Authorization": {"Bearer " + token}})
    if err != nil {
        return fmt.Errorf("dial %s: %w", connectURL, err)
    }
    defer conn.Close()
    log.Printf("connected: %s", connectURL)

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
        msgs = append(msgs, openaiMessage{Role: m.Role, Content: m.Content})
    }

    if !req.Stream {
        content, usage, finishReason, err := backend.chat(req.Model, msgs)
        if err != nil {
            send(Frame{Type: "error", RequestID: req.RequestID, Error: err.Error()})
            return
        }
        send(Frame{Type: "response", RequestID: req.RequestID, Content: content, Usage: usage, FinishReason: finishReason})
        return
    }

    err := backend.stream(req.Model, msgs,
        func(chunk string) { send(Frame{Type: "chunk", RequestID: req.RequestID, Content: chunk}) },
        func(usage *Usage, finishReason string) {
            send(Frame{Type: "final", RequestID: req.RequestID, Usage: usage, FinishReason: finishReason})
        })
    if err != nil {
        send(Frame{Type: "error", RequestID: req.RequestID, Error: err.Error()})
    }
}
```

## 8. Configuration

CLI flags (with env var fallback for secrets, so they don't show up in
`ps`/process listings):

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `-router` | — | `ws://localhost:8080` | Router base URL (`ws://` or `wss://`). |
| `-provider` | — | *(required)* | Provider name; must match an `agents:` key in the router's config. |
| `-token` | `AGENT_TOKEN` | *(required)* | Shared secret for that provider name. |
| `-llm-url` | — | `http://localhost:11434` | Base URL of the local OpenAI-compatible LLM server. |
| `-llm-api-key` | `LLM_API_KEY` | *(empty)* | Bearer token for the local LLM server, if it requires one. |

Running two instances with the same `-provider`/`-token` (against the same
or different local LLM servers) is exactly how you get redundancy/load
distribution — the router pools and round-robins all connections under one
provider name automatically; no special flag or coordination needed on the
agent side.

## 9. Module setup

This repo is already set up this way:

```
go mod init github.com/LLMOcean/box.agent
go get github.com/gorilla/websocket
```

Split into `frame.go` / `backend.go` / `connection.go` / `main.go` as this
repo does, or a single file — no functional difference either way.

## 10. Deployment

**systemd unit** — see [`../deploy/systemd/box-agent.service`](../deploy/systemd/box-agent.service)
in this repo for the actual unit file used.

**Docker** — see [`../Dockerfile`](../Dockerfile) in this repo; only needed
if the local LLM server is also containerized on the same host/network.

Logs go to stdout/stderr via the standard `log` package — let systemd/Docker
capture them; there's no internal log file to manage.

## 11. Acceptance criteria

Build the binary, then verify against a running router:

1. Start it: `./box-agent -router ws://<router-host>:8080 -provider <name> -token <token> -llm-url http://localhost:11434`.
   Expect a `connected: ...` log line.
2. `curl <router>/v1/agents/status` → the provider name should show a live
   connection count ≥ 1.
3. Send a non-streaming chat request through the router for that provider
   and confirm content comes back.
4. Repeat with `"stream": true` and confirm SSE chunks arrive incrementally,
   ending in a final usage/finish_reason event.
5. Kill the local LLM server (not the agent) mid-request and confirm the
   router gets a `502` with a clear error message rather than hanging.
6. Kill the agent process mid-request and confirm the router's customer
   request fails promptly instead of hanging — then restart the agent and
   confirm it reconnects and `/v1/agents/status` shows it live again within
   ~5 seconds.
7. Start a second instance with the same `-provider`/`-token` and confirm
   `/v1/agents/status` shows 2 connections, and that requests alternate
   between the two (round-robin).
