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
  -token "$AGENT_TOKEN" \
  -backend-model "llama3"
```

`-provider`, `-token`, and `-backend-model` are always required — box-agent
never guesses the provider or model name (it used to auto-detect the model
via `GET {-llm-url}/v1/models`, but that silently registered garbage
whenever `-llm-url` wasn't a genuine single-model backend; it now requires
`-backend-model` explicitly instead). `-router` and `-api-url` default to
the greenference.com platform, `-llm-url` defaults to a local server on
this box (`http://localhost:8000`), and `-token` doubles as `-api-token`
unless you pass that separately. Override any of them for a self-hosted
router or a differently-addressed LLM server:

```
./box-agent \
  -router wss://router.example.com \
  -provider oss-llama3 \
  -token "$AGENT_TOKEN" \
  -llm-url http://localhost:8000 \
  -backend-model "llama3"
```

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `-router` | — | `wss://llm.eu.greenference.com` | Router base URL (`ws://` or `wss://`). |
| `-provider` | — | *(required)* | Provider name; must match an `agents:` key in the router's `config.yaml`. |
| `-token` | `AGENT_TOKEN` | *(required)* | Shared secret for that provider name; also used as `-api-token` unless that's set separately. |
| `-backend-model` | — | *(required)* | The model name — registered with the management API and sent to the local LLM backend. E.g. vLLM expects the full repo id it was started with (`tcclaviger/Qwen3.6-40B-...`), not just the router's `provider/model` suffix. |
| `-llm-url` | — | `http://localhost:8000` | Base URL of the local OpenAI-compatible LLM server. |
| `-llm-api-key` | `LLM_API_KEY` | *(empty)* | Bearer token for the local LLM server, if it requires one. |
| `-api-url` | — | `https://api.greenference.com` | Base URL of the management API used to register this box-agent instance and sync which model it's serving. |
| `-api-token` | `API_TOKEN` | *(defaults to `-token`)* | Bearer token identifying this agent instance to the management API, if it needs to differ from `-token`. |
| `-is-public` | — | `true` | Whether this model/provider should be publicly listed (sent as `is_public` on registration). |
| `-input-per-million` | — | `0` (unreported) | Input price per million tokens to report at registration. |
| `-output-per-million` | — | `0` (unreported) | Output price per million tokens to report at registration. |
| `-connections` | — | `1` | Number of parallel WebSocket connections this one process opens to the router, all proxying to the same `-llm-url`. See below - almost always leave this at `1`. |
| `-version` | — | `false` | Print the build version and exit. Compares what's actually running against what's published - a git tag alone doesn't mean a matching GitHub Release with binary assets exists; `install.sh` downloads the latter. `dev` means a local `go build` with no `-ldflags` version stamp. |

Run multiple **processes** with the same `-provider`/`-token` (against the
same or different local LLM servers) for redundancy/load distribution — the
router pools and round-robins all connections under one provider name
automatically. This is the normal way to add capacity, and the only way that
adds real capacity when each process points at an independent backend (its
own GPU/vLLM instance).

`-connections` is a different, narrower thing: it opens N connections from
**one process to the same backend**, an experiment for isolating whether a
single connection's write-serialization ever becomes a measurable bottleneck
under very high concurrent chunk-emission, independent of backend compute.
Unlike running separate processes against independent backends, this does
**not** add real inference capacity — the same backend is still doing the
same work either way. Leave it at the default of `1` unless you're
specifically testing that hypothesis.

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

- On boot, registers `-backend-model` with the management API
  (`-api-url`/`-api-token`), so it can keep track of which model is running
  behind each agent instance's token. Registration happens once at startup,
  before the router connect loop; a failure here is fatal.
- Before registering, tries to auto-detect the model's context length and
  quantization by querying a running Ollama server's `/api/show` (a no-op
  against any other backend, e.g. vLLM, which has no such endpoint) -
  `-context-length`/`-quantization` still win if set explicitly, detection
  only fills in what's left unreported. It also derives a Hugging Face repo
  id from `-backend-model` itself, when the name carries one: Ollama's
  `hf.co/<repo>[:<quant tag>]` direct-pull form, or vLLM's convention of the
  bare repo id (`tcclaviger/Qwen3.6-40B-...`) as `-backend-model`. All three
  are sent as `context_length`/`quantization`/`hugging_face_id` on
  registration alongside the existing fields - as of this writing the
  management API only reads `model`/`provider`/`name` from that request and
  silently drops the rest, same as `-is-public`/`-input-per-million`/
  `-output-per-million` already did, so this is future-ready rather than
  live end-to-end yet.
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
go run ./cmd/fakellm -addr :8000
```

Point `-llm-url http://localhost:8000 -backend-model fake-model` at it
(`fake-model` is fakellm's own `-model` default — match whatever you passed
it) and box-agent will echo back whatever the router sends it, on both the
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
reboots/crashes, and starts it. `-provider`/`-token`/`-backend-model` are
always required — box-agent never guesses the provider or model name.
`-router`/`-api-url` default to the platform already, `-llm-url` defaults
to a local server on this box, and `-token` doubles as `-api-token`. Full
reference (all flags, overriding for a self-hosted router/LLM server,
manual/Docker alternatives): [`docs/INSTALL.md`](docs/INSTALL.md).

**One-time / no persistence** — same download, but runs box-agent directly
in the foreground instead of installing a systemd/launchd service: no root
required (beyond whatever Ollama's own installer needs), nothing survives a
reboot or Ctrl-C. Add `-install-model` to also pull `-backend-model` via
Ollama first, so this one command deploys the LLM, deploys box-agent, and
registers it with the router. Ollama gets started (if not already running)
on `-llm-url`, `http://localhost:8000` by default:

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install-and-run.sh \
  | bash -s -- \
      -provider yourns/your-model \
      -token "$REGISTRATION_TOKEN" \
      -backend-model "your-model-id" \
      -install-model
```

**Multiple routers at once** — box-agent only ever dials one `-router` per
process, so registering one local model under several routers means
running several instances. [`deploy/install-and-run-multi.sh`](deploy/install-and-run-multi.sh)
downloads one shared binary and launches one process per line of a config
file you provide — typically the same `-backend-model`/`-llm-url` on
every line (one model, several router registrations), differing only in
`-router`/`-provider`/`-token`. Ctrl-C, or any instance exiting, stops
them all together. Pass `-install-model` to the script itself (not
inside the config file) to pull the model via Ollama once, before any
instance starts, rather than once per instance:

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install-and-run-multi.sh \
  -o install-and-run-multi.sh
cat > routers.conf <<'CONF'
-router wss://llm.eu.greenference.com -provider yourns/your-model -backend-model "your-model-id" -token "$EU_TOKEN" -llm-url http://localhost:8000
-router wss://llm.na.greenference.com -provider yourns/your-model -backend-model "your-model-id" -token "$NA_TOKEN" -llm-url http://localhost:8000
CONF
bash install-and-run-multi.sh -config routers.conf -install-model "your-model-id"
```

For **N genuinely different models** on one host (e.g. 5 local models each
registered to 2 routers — 10 instances but only 5 distinct pulls), use
`-models FILE` instead of `-install-model`/`-llm-url`: a file with one
`MODEL LLM_URL` pair per line, each pulled via Ollama exactly once before
any instance starts. Pair it with a `-config` file containing all 10
lines (one per model × router combination). Run the script with `-h` for
the exact format.

(Needs the config file on disk first, so unlike the single-router script
it can't be run as one `curl | bash` line — see the script's `-h` for
details.)

**Linux** (installs a systemd service; run as root):

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.sh \
  | sudo bash -s -- \
      -provider yourns/your-model \
      -token "$REGISTRATION_TOKEN" \
      -backend-model "your-model-id"
```

**macOS** (installs a launchd daemon; run as root — same script as Linux,
OS is auto-detected):

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.sh \
  | sudo bash -s -- \
      -provider yourns/your-model \
      -token "$REGISTRATION_TOKEN" \
      -backend-model "your-model-id"
```

**Windows** (installs a Scheduled Task; run from an elevated PowerShell
prompt):

```powershell
iex "& { $(irm https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.ps1) } -Provider yourns/your-model -Token $env:REGISTRATION_TOKEN -BackendModel 'your-model-id'"
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
