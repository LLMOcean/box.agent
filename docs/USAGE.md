# Using box.agent

box.agent lets you connect your own GPU box, running any OpenAI-compatible
LLM server (Ollama, vLLM, llama.cpp-server, text-generation-inference), to
the platform as a remote inference provider. It dials **out** to the
router, so your machine never needs an inbound-reachable address — only
outbound internet access is required.

```
Router  <--WebSocket tunnel--  box-agent  --HTTP-->  your local LLM server
```

## 1. Prerequisites

- A local, running OpenAI-compatible LLM server (e.g. Ollama on
  `http://localhost:11434`, or vLLM's `--served-model-name`/OpenAI server
  mode) with a model already loaded.
- A **registration token** issued from the control panel. This single
  token is used for both:
  - registering your instance with the management API (so the platform
    knows which model you're serving), and
  - authenticating the agent connection to the router.
- A **provider name** for your instance, e.g. `plusclouds/qwen5.0-with-extras`
  (`<namespace>/<model-name>` — the part after the `/` is what users will
  request as the model; a bare name with no `/` uses itself as the model
  too).

## 2. Get the binary

The quickest way to get running: download and run in one step with
[`deploy/install-and-run.sh`](../deploy/install-and-run.sh) — it fetches
the latest release binary and execs it with whatever flags you pass it:

```
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install-and-run.sh | bash -s -- \
  -router ws://chat.llmocean.com:8080 \
  -provider plusclouds/qwen5.0-with-extras \
  -token "$REGISTRATION_TOKEN" \
  -api-url https://api.plusclouds.com \
  -api-token "$REGISTRATION_TOKEN" \
  -llm-url http://localhost:11434
```

Set `VERSION=v1.1.2` (env var) to pin a specific release instead of
`latest`.

Otherwise, download a release binary directly from
[github.com/LLMOcean/box.agent/releases](https://github.com/LLMOcean/box.agent/releases),
or build from source:

```
git clone git@github.com:LLMOcean/box.agent.git
cd box.agent
go build -o box-agent .
```

## 3. Run it

```
./box-agent \
  -router ws://chat.llmocean.com:8080 \
  -provider plusclouds/qwen5.0-with-extras \
  -token "$REGISTRATION_TOKEN" \
  -api-url https://api.plusclouds.com \
  -api-token "$REGISTRATION_TOKEN" \
  -llm-url http://localhost:11434
```

On startup it will:

1. Ask your local LLM server (`-llm-url`) which model it's serving — via
   `GET /v1/models` — unless you override this with `-backend-model`.
2. Register that model with the management API (`-api-url`/`-api-token`).
   **This step is required** — the agent exits if it fails.
3. Connect to the router (`-router`) under your provider name and start
   serving chat requests.

A successful run logs:

```
registered with API: model=qwen5.0-with-extras
connected: ws://chat.llmocean.com:8080/v1/agents/connect?provider=plusclouds%2Fqwen5.0-with-extras
```

## 4. CLI reference

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `-router` | — | `ws://localhost:8080` | Router WebSocket URL. |
| `-provider` | — | *(required)* | Your provider name, e.g. `plusclouds/qwen5.0-with-extras`. |
| `-token` | `AGENT_TOKEN` | *(required)* | Your registration token, used to authenticate the router connection. |
| `-llm-url` | — | `http://localhost:11434` | Base URL of your local OpenAI-compatible LLM server. |
| `-llm-api-key` | `LLM_API_KEY` | *(empty)* | Bearer token for your local LLM server, if it requires one. |
| `-backend-model` | — | *(empty — auto-detect)* | Override the model id sent to your LLM server, if it differs from what you registered (e.g. vLLM needs the full repo id it was started with, like `tcclaviger/Qwen3.6-40B-...`). |
| `-api-url` | — | `http://localhost:8000` | Management API base URL. Use `https://api.plusclouds.com` in production. |
| `-api-token` | `API_TOKEN` | *(required)* | Your registration token, used to authenticate with the management API. |

## 5. Verify you're connected

The router exposes a status endpoint showing live connection counts per
provider:

```
curl https://chat.llmocean.com:8080/v1/agents/status
```

```json
{"plusclouds/qwen5.0-with-extras": 1}
```

If your provider name is missing or the count is `0`, the agent isn't
connected — check its logs.

## 6. Troubleshooting

| Symptom | Meaning | Fix |
|---|---|---|
| `determine running model: ... connection refused` | box-agent can't reach `-llm-url`. | Make sure your local LLM server is running and reachable at that address before starting box-agent. |
| `register with API: ... 422 ... "The model field is required."` | The model name resolved to empty. | Make sure your LLM server's `/v1/models` returns at least one model, or set `-backend-model` explicitly. |
| `connection error: ... websocket: bad handshake` (with a `401` on the underlying request) | Router rejected your `-token`. | Double check the registration token is current and hasn't been revoked/rotated. |
| `connection error: ... websocket: bad handshake` (with a `503` on the underlying request) | The router's own auth backend is temporarily unavailable. | Transient — box-agent retries every 5s automatically; no action needed unless it persists. |
| Agent shows connected, but chat requests never arrive | Client is likely using the wrong token or model string. | Client requests go to `POST /v1/chat` with a separate end-user token (not the agent registration token), and `"model"` must match your provider name (e.g. `"plusclouds/qwen5.0-with-extras"`). |

## 7. Running unattended

For a long-running deployment, use a process supervisor rather than a bare
foreground process:

- **systemd** — see [`deploy/systemd/box-agent.service`](../deploy/systemd/box-agent.service).
- **Docker** — see [`Dockerfile`](../Dockerfile); only needed if your LLM
  server is also containerized on the same host/network.

box.agent reconnects automatically on connection loss (fixed 5s delay), so
it's safe to run continuously — no external retry logic needed.
