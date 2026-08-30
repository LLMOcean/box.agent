#!/bin/bash
# Supervisor-managed entrypoint for box-agent inside the vastai/vllm-derived
# image - see ../Dockerfile's header comment for the full picture. Follows
# the same wrapper-script pattern vast.ai's own base image documents
# (/etc/vast_agents/base.md §7): source the shared utils, log to the
# portal, run the real program in the foreground.
utils=/opt/supervisor-scripts/utils
. "${utils}/logging.sh"
. "${utils}/environment.sh"

# Self-skip (clean exit 0 - autorestart=unexpected in box-agent.conf means
# supervisord treats a 0 exit as intentional and does not restart it) when
# the instance wasn't actually configured to register with the router. This
# keeps the image usable as a plain, ordinary vastai/vllm image - nothing
# about normal vLLM template usage changes if these env vars are left unset.
if [ -z "${PROVIDER:-}" ]; then
  echo "box-agent: PROVIDER not set - skipping (this instance will just serve vLLM normally, unregistered)"
  exit 0
fi
if [ -z "${TOKEN:-}" ] && [ -z "${API_TOKEN:-}" ]; then
  echo "box-agent: neither TOKEN nor API_TOKEN set - skipping (this instance will just serve vLLM normally, unregistered)"
  exit 0
fi

# Defaults to whatever vLLM's own service is already configured to serve
# (vastai/vllm's VLLM_MODEL) - only set BACKEND_MODEL separately if it must
# differ, since box-agent registers/reports this model string but never
# controls what vLLM itself is actually running in this image.
MODEL="${BACKEND_MODEL:-${VLLM_MODEL:-}}"
if [ -z "$MODEL" ]; then
  echo "box-agent: no model set (BACKEND_MODEL or VLLM_MODEL) - skipping"
  exit 0
fi

ARGS=(
  -router "${ROUTER:-wss://llm.eu.greenference.com}"
  -api-url "${API_URL:-https://api.greenference.com}"
  -llm-url "${LLM_URL:-http://127.0.0.1:18000}"
  -provider "$PROVIDER"
  -backend-model "$MODEL"
)
# Only relevant to the auto-provisioning path (TOKEN unset), but harmless to
# always pass through - box-agent ignores it once AGENT_TOKEN is set.
[ -z "${TOKEN:-}" ] && ARGS+=(-token-cache "${TOKEN_CACHE:-/var/lib/box-agent/token}")
[ -n "${IS_PUBLIC:-}" ] && ARGS+=(-is-public "$IS_PUBLIC")
[ -n "${INPUT_PER_MILLION:-}" ] && ARGS+=(-input-per-million "$INPUT_PER_MILLION")
[ -n "${OUTPUT_PER_MILLION:-}" ] && ARGS+=(-output-per-million "$OUTPUT_PER_MILLION")
[ -n "${CONTEXT_LENGTH:-}" ] && ARGS+=(-context-length "$CONTEXT_LENGTH")
[ -n "${MAX_OUTPUT_LENGTH:-}" ] && ARGS+=(-max-output-length "$MAX_OUTPUT_LENGTH")
[ -n "${QUANTIZATION:-}" ] && ARGS+=(-quantization "$QUANTIZATION")
[ -n "${INPUT_MODALITIES:-}" ] && ARGS+=(-input-modalities "$INPUT_MODALITIES")
[ -n "${OUTPUT_MODALITIES:-}" ] && ARGS+=(-output-modalities "$OUTPUT_MODALITIES")
[ -n "${SUPPORTED_FEATURES:-}" ] && ARGS+=(-supported-features "$SUPPORTED_FEATURES")

export AGENT_TOKEN="${TOKEN:-}"
export API_TOKEN="${API_TOKEN:-}"
[ -n "${LLM_API_KEY:-}" ] && export LLM_API_KEY

# Deliberately not wrapped in `pty` (unlike other supervisor scripts in this
# image) - `pty`'s `unbuffer -p` is for giving progress-bar tools (pip, apt,
# huggingface downloads) a fake tty; box-agent's own logging is plain
# line-buffered output that needs no such thing, and unbuffer -p was
# measured to swallow the wrapped command's real exit code entirely (e.g.
# `unbuffer -p false` exits 0) - which would silently break
# box-agent.conf's autorestart=unexpected: every crash would look like a
# clean, intentional exit and never restart. Running it directly (and via
# `exec`, since this really is an executable on PATH, unlike the `pty`
# shell function above) preserves the real exit code.
exec /usr/local/bin/box-agent "${ARGS[@]}"
