# box.agent, bundled into Vast.ai's own vLLM image (vastai/vllm, built from
# https://github.com/vast-ai/base-image, external/vllm) as an additional
# supervisor-managed service. vLLM itself is exactly what vastai/vllm
# already runs, unchanged - this only adds box-agent alongside it, to
# register the already-running server with the LLMOcean router/management
# API. box-agent must NOT redeploy vLLM itself (-deploy-vllm) on top of an
# image that already runs it as a native service - it would download the
# model a second time and launch a competing "vllm serve" fighting the
# existing one for the same VRAM (see ../install-supervisord.sh's header
# for the same reasoning, applied there over SSH after the fact instead of
# baked into an image).
#
# Configure entirely through environment variables at container/template
# launch (vast.ai's template editor "Environment Variables" field, or
# `docker run -e ...`) - same names deploy/install.sh itself accepts:
#
#   PROVIDER        required - e.g. "yourns/your-model"
#   TOKEN           an existing per-instance agent token, OR
#   API_TOKEN       an IAM/account-level token box-agent uses to
#                   auto-provision a new per-instance token on first boot
#   BACKEND_MODEL   defaults to $VLLM_MODEL (the model vLLM itself is
#                   already configured to serve) - only set this
#                   separately if it must differ
#   ROUTER, API_URL, LLM_URL, IS_PUBLIC, INPUT_PER_MILLION,
#   OUTPUT_PER_MILLION, CONTEXT_LENGTH, MAX_OUTPUT_LENGTH, QUANTIZATION,
#   INPUT_MODALITIES, OUTPUT_MODALITIES, SUPPORTED_FEATURES, LLM_API_KEY,
#   TOKEN_CACHE
#                   all optional, same names/defaults as install.sh
#
# box-agent self-skips (clean exit, no crash-loop - see box-agent.sh) if
# PROVIDER, a token, or a model isn't set, so this image behaves as a
# plain, ordinary vastai/vllm image when those aren't provided - nothing
# about normal vLLM template usage changes.
#
# Build (multi-arch; box-agent publishes both linux/amd64 and linux/arm64):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t <dockerhub-org>/greenference-box-vllm:latest --push .
#
# See README.md in this directory for the vast.ai template setup (ports,
# PORTAL_CONFIG) needed alongside the image itself.

ARG BASE_TAG=v0.28.0-cuda-13.0
FROM vastai/vllm:${BASE_TAG}

ARG BOX_AGENT_VERSION=latest
# Detected via uname rather than the BuildKit-only automatic $TARGETARCH arg
# (unset - and fatal under "set -eu" - on the classic, non-buildx builder).
# Also correct under a real multi-arch `docker buildx build --platform ...`:
# uname reports the emulated target arch inside a cross-arch RUN step, not
# the host's.
RUN set -eu; \
    arch="$(uname -m)"; \
    case "$arch" in \
      x86_64|amd64) goarch=amd64 ;; \
      aarch64|arm64) goarch=arm64 ;; \
      *) echo "error: unsupported arch '$arch'" >&2; exit 1 ;; \
    esac; \
    if [ "$BOX_AGENT_VERSION" = "latest" ]; then \
      url="https://github.com/LLMOcean/box.agent/releases/latest/download/box-agent-linux-${goarch}"; \
    else \
      url="https://github.com/LLMOcean/box.agent/releases/download/${BOX_AGENT_VERSION}/box-agent-linux-${goarch}"; \
    fi; \
    curl -fsSL "$url" -o /usr/local/bin/box-agent && \
    chmod +x /usr/local/bin/box-agent

COPY box-agent.sh /opt/supervisor-scripts/box-agent.sh
COPY box-agent.conf /etc/supervisor/conf.d/box-agent.conf
RUN chmod +x /opt/supervisor-scripts/box-agent.sh
