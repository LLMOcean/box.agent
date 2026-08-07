# box.agent

> Looking for a step-by-step guide to running box.agent against the
> platform (getting your token, verifying the connection, troubleshooting)?
> See [`docs/USAGE.md`](docs/USAGE.md) — written for the control panel.
>
> Deploying to a remote GPU box as a long-running service (systemd, Docker,
> upgrades)? See [`docs/INSTALL.md`](docs/INSTALL.md).

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
  -provider oss-llama3 \
  -token "$AGENT_TOKEN"
```

`-router`, `-llm-url`, and `-api-url` all default to the greenference.com
platform already, and `-token` doubles as `-api-token` unless you pass that
separately — so `-provider`/`-token` are normally the only flags you need.
Override any of them for a self-hosted router/LLM server:

```
./box-agent \
  -router wss://router.example.com \
  -provider oss-llama3 \
  -token "$AGENT_TOKEN" \
  -llm-url http://localhost:11434
```

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `-router` | — | `wss://llm.greenference.com` | Router base URL (`ws://` or `wss://`). |
| `-provider` | — | *(required)* | Provider name; must match an `agents:` key in the router's `config.yaml`. |
| `-token` | `AGENT_TOKEN` | *(required)* | Shared secret for that provider name; also used as `-api-token` unless that's set separately. |
| `-llm-url` | — | `https://llm.greenference.com` | Base URL of the local OpenAI-compatible LLM server. |
| `-llm-api-key` | `LLM_API_KEY` | *(empty)* | Bearer token for the local LLM server, if it requires one. |
| `-backend-model` | — | *(empty — pass through)* | Override the model name sent to the local LLM backend. Use this when the backend's own model id doesn't match the router's registered model name — e.g. the router only ever forwards the part of `provider/model` after the provider prefix, but vLLM identifies its loaded model by the full repo id it was started with (`tcclaviger/Qwen3.6-40B-...`, not just `Qwen3.6-40B-...`). |
| `-api-url` | — | `https://api.greenference.com` | Base URL of the management API used to register this box-agent instance and sync which model it's serving. |
| `-api-token` | `API_TOKEN` | *(defaults to `-token`)* | Bearer token identifying this agent instance to the management API, if it needs to differ from `-token`. |
| `-is-public` | — | `true` | Whether this model/provider should be publicly listed (sent as `is_public` on registration). |
| `-input-per-million` | — | `0` (unreported) | Input price per million tokens to report at registration. |
| `-output-per-million` | — | `0` (unreported) | Output price per million tokens to report at registration. |

Run multiple instances with the same `-provider`/`-token` (against the same
or different local LLM servers) for redundancy/load distribution — the
router pools and round-robins all connections under one provider name
automatically.

## Installing a model

Two one-shot subcommands manage models on the local Ollama server directly
(no router involvement — run these on the box itself, as root):

```
box-agent install-model [-llm-url URL] <model>
box-agent delete-model  [-llm-url URL] <model>
```

See [`docs/MODEL_DEPLOY.md`](docs/MODEL_DEPLOY.md) for requirements, exit
codes, and a scripted rollout example.

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

## Testing without a real LLM

`cmd/fakellm` is a minimal OpenAI-compatible server for exercising box-agent
without Ollama/vLLM/etc. running:

```
go run ./cmd/fakellm -addr :11434
```

Point `-llm-url http://localhost:11434` at it and box-agent will register a
fake model and echo back whatever the router sends it, on both the
streaming and non-streaming paths. To exercise tool-calling, send a chat
where the last user message is `TOOL:<name>:<json-args>` (e.g.
`TOOL:get_weather:{"city":"Istanbul"}`) — fakellm responds with a matching
tool call instead of a text reply. Note this only fakes the LLM backend;
box-agent's management-API registration (`-api-url`/`-api-token`) still
needs a real or separately-stubbed endpoint to succeed at boot.

## Protocol

This implements router.agent's agent protocol — see
[`docs/AGENT_PROTOCOL.md`](docs/AGENT_PROTOCOL.md) (wire format) and
[`docs/AGENT_BUILD_SPEC.md`](docs/AGENT_BUILD_SPEC.md) (this build, in spec
form; both copied from the router repo). In short: every message is a JSON
frame with a `type` (`"chat"` from the router; `"chunk"` / `"response"` /
`"final"` / `"error"` from the agent) and a `request_id` echoed back on
every reply.

## Deployment

**One-command install** — downloads (or builds, if no release binary is
published for your OS/arch) the box-agent binary, installs it to survive
reboots/crashes, and starts it. `-router`/`-api-url`/`-llm-url` all default
to the platform already and `-token` doubles as `-api-token`, so
`-provider`/`-token` are normally all you need to pass. Full reference (all
flags, overriding for a self-hosted router/LLM server, manual/Docker
alternatives): [`docs/INSTALL.md`](docs/INSTALL.md).

**Linux** (installs a systemd service; run as root):

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.sh \
  | sudo bash -s -- \
      -provider yourns/your-model \
      -token "$REGISTRATION_TOKEN"
```

**macOS** (installs a launchd daemon; run as root — same script as Linux,
OS is auto-detected):

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.sh \
  | sudo bash -s -- \
      -provider yourns/your-model \
      -token "$REGISTRATION_TOKEN"
```

**Windows** (installs a Scheduled Task; run from an elevated PowerShell
prompt):

```powershell
iex "& { $(irm https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.ps1) } -Provider yourns/your-model -Token $env:REGISTRATION_TOKEN"
```

Release binaries are published for `linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`, and `windows/amd64` — on any other OS/arch,
the scripts fall back to building from source, which needs `git` and `go`
on `PATH`.

**systemd** (manual) — see [`deploy/systemd/box-agent.service`](deploy/systemd/box-agent.service).

**Docker** — see [`Dockerfile`](Dockerfile); only needed if the local LLM
server is also containerized on the same host/network.

## Files

- `main.go` — flag parsing, API registration at boot, reconnect loop, `install-model`/`delete-model` subcommands.
- `connection.go` — WebSocket connect/read/dispatch, per-request concurrency.
- `backend.go` — translation to/from the local OpenAI-compatible LLM server.
- `api.go` — registration with the management API.
- `frame.go` — the wire `Frame`/`Message`/`Usage` types.
- `benchmark.go` — host/LLM diagnostic snapshot, see [`docs/BENCHMARK.md`](docs/BENCHMARK.md).
- `ollama.go` — installing/removing models via `ollama`, see [`docs/MODEL_DEPLOY.md`](docs/MODEL_DEPLOY.md).
