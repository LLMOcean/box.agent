# Deploying a Model with box.agent

`box-agent` has two one-shot subcommands, separate from its normal daemon
mode, for managing models on the Ollama server running alongside it. They
don't talk to the router at all — run them directly on the box, e.g. over
SSH or from a provisioning script/pipeline.

```
box-agent install-model [-llm-url URL] <model>
box-agent delete-model  [-llm-url URL] <model>
```

They're designed to be scripted: no interactive prompts, a real exit code,
and idempotent (safe to re-run).

## Requirements

- **Root.** `install-model` will, on a box that doesn't have Ollama yet, run
  Ollama's official install script (`curl -fsSL https://ollama.com/install.sh
  | sh`), which writes to `/usr/local/bin` and registers a systemd service —
  both require root. If it isn't run as root, the script's own error
  (`sudo: a password is required` or similar) surfaces directly; box.agent
  doesn't try to detect or escalate privileges itself. `delete-model` doesn't
  need root once Ollama is already running.
- **Outbound network access** to `ollama.com` (install script + Ollama's
  model library) and, for Hugging-Face-hosted models, to `huggingface.co`.
- The box must already have `curl` and `sh` (used only for the one-time
  install script, not for every pull).

## `install-model`

```
box-agent install-model [-llm-url URL] <model>
```

1. Checks whether `ollama` is on `PATH`. If not, installs it via the
   official script and re-verifies it's now present.
2. Runs `ollama pull <model>`, streaming Ollama's own progress output
   directly to stdout/stderr (no reimplementation of the download/progress
   protocol on box.agent's side).

`<model>` is whatever `ollama pull` itself accepts:

- A library name/tag, e.g. `llama3.1:70b`, `qwen2.5:32b-instruct`.
- A Hugging-Face-hosted GGUF, via `hf.co/<repo>[:<quant>]`, e.g.
  `hf.co/TheBloke/Llama-3-70B-GGUF:Q4_K_M`.

Re-running `install-model` for a model that's already installed is a no-op
download-wise — Ollama checks existing layers and only fetches what's
missing/changed.

**Exit codes:** `0` success · `1` the install script or `ollama pull` failed
(network error, invalid model name, disk full, ...) · `2` usage error
(missing/extra positional argument).

## `delete-model`

```
box-agent delete-model [-llm-url URL] <model>
```

Runs `ollama rm <model>`. `<model>` must match an installed name/tag exactly
(check with `ollama list` on the box if unsure).

**Exit codes:** `0` success · `1` `ollama rm` failed (e.g. model not found) ·
`2` usage error.

## `-llm-url`

Defaults to `http://localhost:11434`, matching the daemon's own `-llm-url`
default and box.agent's systemd unit. If you pass a non-default value, its
host:port (scheme stripped) is exported as `OLLAMA_HOST` for the `ollama`
subprocess, so both subcommands target the same server the daemon talks to.

## Automating a full rollout

A minimal script to bring a new model onto a fleet of boxes and point
customer traffic at it looks like:

```sh
#!/bin/sh
set -eu

model="$1"        # e.g. "llama3.1:70b"
provider="$2"     # matches an agents: key in router.agent's config.yaml

for host in $(cat boxes.txt); do
  ssh "root@$host" "box-agent install-model '$model'"
done

# box-agent's daemon doesn't need a restart for this: it forwards whatever
# model string the router sends with each chat request straight to the
# local backend (see backend.go's effectiveModel/-backend-model). Only
# update router.agent's config.yaml if you also want "$provider" (with no
# model specified) to default to the new model, and restart the router for
# that config change to take effect — see router.agent's config.yaml
# agents.<name>.default_model and its own deploy docs.
```

Each `ssh ... install-model` call above is independent and safe to re-run
(e.g. from a config-management tool like Ansible) — a box that already has
the model just gets a fast no-op pull.

## See also

- [`../README.md`](../README.md) — daemon mode, flags, systemd unit.
- [`BENCHMARK.md`](BENCHMARK.md) — the unrelated `"benchmark"` frame
  extension, for host/LLM diagnostics over the router connection.
- `ollama.go` — implementation (`ensureOllamaInstalled`, `pullModel`,
  `deleteModel`).
