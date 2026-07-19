# box.agent

A remote WebSocket agent for [router.agent](https://github.com/LLMOcean/router.agent).
Run this next to a self-hosted, open-source LLM server (Ollama, vLLM,
llama.cpp-server, text-generation-inference — anything OpenAI-compatible).
It dials *out* to a router, so it works even when the LLM server has no
inbound-reachable address (private network, NAT, no public IP) — only
outbound access is needed.

```
Router  <--WebSocket tunnel--  box-agent  --HTTP-->  local LLM server
```

## Build

```
go build -o box-agent .
```

## Run

```
./box-agent \
  -router wss://router.example.com \
  -provider oss-llama3 \
  -token "$AGENT_TOKEN" \
  -llm-url http://localhost:11434 \
  -api-token "$API_TOKEN"
```

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `-router` | — | `ws://localhost:8080` | Router base URL (`ws://` or `wss://`). |
| `-provider` | — | *(required)* | Provider name; must match an `agents:` key in the router's `config.yaml`. |
| `-token` | `AGENT_TOKEN` | *(required)* | Shared secret for that provider name. |
| `-llm-url` | — | `http://localhost:11434` | Base URL of the local OpenAI-compatible LLM server. |
| `-llm-api-key` | `LLM_API_KEY` | *(empty)* | Bearer token for the local LLM server, if it requires one. |
| `-backend-model` | — | *(empty — pass through)* | Override the model name sent to the local LLM backend. Use this when the backend's own model id doesn't match the router's registered model name — e.g. the router only ever forwards the part of `provider/model` after the provider prefix, but vLLM identifies its loaded model by the full repo id it was started with (`tcclaviger/Qwen3.6-40B-...`, not just `Qwen3.6-40B-...`). |
| `-api-url` | — | `http://localhost:8000` | Base URL of the management API used to register this box-agent instance and sync which model it's serving. |
| `-api-token` | `API_TOKEN` | *(required)* | Bearer token identifying this agent instance to the management API. |

Run multiple instances with the same `-provider`/`-token` (against the same
or different local LLM servers) for redundancy/load distribution — the
router pools and round-robins all connections under one provider name
automatically.

## Behavior

- On boot, registers with the management API (`-api-url`/`-api-token`),
  reporting which model this instance is serving — either `-backend-model`
  if set, or whatever the local LLM server's `/v1/models` endpoint reports
  as loaded. This lets the API keep track of which model is running behind
  each agent instance's token. Registration happens once at startup, before
  the router connect loop; a failure here is fatal.
- Reconnects automatically (fixed 5s delay) if the connection to the router
  drops.
- Answers the router's WebSocket pings so the connection isn't reaped as
  dead.
- Handles many requests concurrently on the one connection, correlated by
  `request_id` — a slow or streaming request never blocks others.
- On any backend failure, sends back an `"error"` frame rather than hanging
  the request.

## Protocol

This implements router.agent's agent protocol — see
[`docs/AGENT_PROTOCOL.md`](docs/AGENT_PROTOCOL.md) (wire format) and
[`docs/AGENT_BUILD_SPEC.md`](docs/AGENT_BUILD_SPEC.md) (this build, in spec
form; both copied from the router repo). In short: every message is a JSON
frame with a `type` (`"chat"` from the router; `"chunk"` / `"response"` /
`"final"` / `"error"` from the agent) and a `request_id` echoed back on
every reply.

## Deployment

**systemd** — see [`deploy/systemd/box-agent.service`](deploy/systemd/box-agent.service).

**Docker** — see [`Dockerfile`](Dockerfile); only needed if the local LLM
server is also containerized on the same host/network.

## Files

- `main.go` — flag parsing, API registration at boot, reconnect loop.
- `connection.go` — WebSocket connect/read/dispatch, per-request concurrency.
- `backend.go` — translation to/from the local OpenAI-compatible LLM server.
- `api.go` — registration with the management API.
- `frame.go` — the wire `Frame`/`Message`/`Usage` types.
