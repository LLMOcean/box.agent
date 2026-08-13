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

- A local, running OpenAI-compatible LLM server (e.g. Ollama, or vLLM's
  `--served-model-name`/OpenAI server mode) with a model already loaded.
  `-llm-url` defaults to `http://localhost:8000`; pass it explicitly if
  your server listens elsewhere (Ollama's own out-of-the-box default is
  `http://localhost:11434`).
- A **registration token** issued from the control panel. This single
  token is used for both:
  - registering your instance with the management API (so the platform
    knows which model you're serving), and
  - authenticating the agent connection to the router.
- A **provider name** for your instance, e.g. `plusclouds/qwen5.0-with-extras`
  (`<namespace>/<model-name>` — the part after the `/` is what users will
  request as the model; a bare name with no `/` uses itself as the model
  too).
- The exact **model name** your LLM server is serving, to pass as
  `-backend-model`. box-agent doesn't detect this for you — see
  [§3](#3-run-it).

## 2. Get the binary

The quickest way to get running: download and run in one step with
[`deploy/install-and-run.sh`](../deploy/install-and-run.sh) — it fetches
the latest release binary and execs it with whatever flags you pass it:

```
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install-and-run.sh | bash -s -- \
  -provider plusclouds/qwen5.0-with-extras \
  -token "$REGISTRATION_TOKEN" \
  -backend-model "qwen5.0-with-extras"
```

(`-router`/`-api-url` default to the platform already, `-llm-url` defaults
to a local server on this box, and `-token` doubles as `-api-token` — pass
any of them explicitly to override, same as any other box-agent flag.
`-provider`/`-token`/`-backend-model` are the three that are always
required, though.)

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
  -provider plusclouds/qwen5.0-with-extras \
  -token "$REGISTRATION_TOKEN" \
  -backend-model "qwen5.0-with-extras"
```

`-provider`, `-token`, and `-backend-model` are always required — box-agent
never guesses the provider or model name for you (an earlier version tried
auto-detecting the model via `GET {-llm-url}/v1/models`, but that silently
registered whatever came back first, which broke badly if `-llm-url`
wasn't a genuine single-model backend; it now requires `-backend-model`
explicitly instead). `-router` and `-api-url` default to the platform
already (`llm.eu.greenference.com`/`api.greenference.com`), `-llm-url`
defaults to a local server on this box (`http://localhost:8000`), and
`-token` doubles as `-api-token`. Pass any of those explicitly to override
(e.g. pointing `-llm-url` at wherever your Ollama/vLLM server actually
listens):

```
./box-agent \
  -provider plusclouds/qwen5.0-with-extras \
  -token "$REGISTRATION_TOKEN" \
  -llm-url http://localhost:11434 \
  -backend-model "qwen5.0-with-extras"
```

On startup it will:

1. Register `-backend-model` with the management API
   (`-api-url`/`-api-token`). **This step is required** — the agent exits
   if it fails.
2. Connect to the router (`-router`) under your provider name and start
   serving chat requests, forwarding each one to `-llm-url` with
   `-backend-model` as the model name.

A successful run logs:

```
registered with API: model=qwen5.0-with-extras
connected: ws://chat.llmocean.com:8080/v1/agents/connect?provider=plusclouds%2Fqwen5.0-with-extras
```

## 4. CLI reference

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `-router` | — | `wss://llm.eu.greenference.com` | Router WebSocket URL. |
| `-provider` | — | *(required)* | Your provider name, e.g. `plusclouds/qwen5.0-with-extras`. |
| `-token` | `AGENT_TOKEN` | *(required)* | Your registration token, used to authenticate the router connection. Also used as `-api-token` unless that's set separately — normally the only token you need to pass. |
| `-llm-url` | — | `http://localhost:8000` | Base URL of your local OpenAI-compatible LLM server. |
| `-llm-api-key` | `LLM_API_KEY` | *(empty)* | Bearer token for your local LLM server, if it requires one. |
| `-backend-model` | — | *(required)* | The model name — registered with the management API and sent to your LLM server. E.g. vLLM needs the full repo id it was started with, like `tcclaviger/Qwen3.6-40B-...`, not just what you registered as the model. |
| `-api-url` | — | `https://api.greenference.com` | Management API base URL. |
| `-api-token` | `API_TOKEN` | *(defaults to `-token`)* | Your registration token, used to authenticate with the management API, if it needs to differ from `-token`. |
| `-is-public` | — | `true` | Whether this model/provider should be publicly listed. |
| `-input-per-million` | — | `0` (unreported) | Input price per million tokens to report at registration. |
| `-output-per-million` | — | `0` (unreported) | Output price per million tokens to report at registration. |

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
| `-provider, -token (or AGENT_TOKEN env var), and -backend-model are all required` | One of the three was left unset. | Pass all three explicitly — box-agent won't guess or detect any of them. |
| `register with API: ... 422 ...` | The management API rejected the registration payload. | Double check `-backend-model` is a value the API accepts (not empty, not malformed). |
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
