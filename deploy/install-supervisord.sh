#!/usr/bin/env bash
# Installs box.agent as a supervisord-managed program - for boxes where
# install.sh's systemd/launchd approach doesn't apply, most commonly a
# Docker container (e.g. a vast.ai instance) that has no init system of its
# own but already runs supervisord as PID 1 to manage sshd/jupyter/other
# services. Downloads (or, if no release binary is published for this
# OS/arch, builds) the box-agent binary, writes a supervisord program
# config, and tells the running supervisord to pick it up. Must be run as
# root (or with permission to write supervisord's conf.d and talk to
# supervisorctl). See docs/INSTALL.md for the full walkthrough; see
# install.sh instead if this box actually has systemd or launchd.
#
# Usage is identical to install.sh (same box-agent -flag names, same
# -provider/-backend-model/-token requirements, same -deploy-vllm support
# for self-hosting a model with vLLM) - run with -h for all of them.
#
#   curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install-supervisord.sh \
#     | bash -s -- \
#         -provider yourns/your-model \
#         -token "$REGISTRATION_TOKEN" \
#         -backend-model "meta-llama/Llama-3.1-8B-Instruct" \
#         -deploy-vllm -vllm-hf-token "$HF_TOKEN"
#
# Every flag can also be set via an identically-named (upper-cased, with
# "-" -> "_") environment variable instead, e.g. PROVIDER=... TOKEN=...
# ./install-supervisord.sh - a flag always takes precedence over its env var.
#
# Installer-only flags/env vars (not forwarded to box-agent):
#   -version (VERSION)           - release tag to install (default: latest)
#   -install-dir (INSTALL_DIR)   - where to place the binary (default: /usr/local/bin)
#   -conf-dir (CONF_DIR)         - supervisord conf.d directory to write into
#                                   (default: auto-detect /etc/supervisor/conf.d,
#                                   then /etc/supervisord.d)
#   -service-user (SERVICE_USER) - system user to run the program as (default:
#                                   empty - runs as whichever user this script
#                                   is run as, normally root; that's the norm
#                                   for a single-tenant container like a
#                                   vast.ai instance, unlike install.sh's
#                                   systemd path, which defaults to a
#                                   dedicated unprivileged user)

set -euo pipefail

REPO="LLMOcean/box.agent"

usage() {
  cat >&2 <<'EOF'
Usage: install-supervisord.sh -provider NAME -backend-model NAME (-token TOK | -api-token TOK) [options]

Required:
  -provider NAME           provider name, e.g. yourns/your-model
  -backend-model NAME       the model name - registered with the management
                            API and sent to the local LLM backend
  -token TOKEN              an existing per-instance token (or TOKEN/AGENT_TOKEN
                            env var); also used as -api-token unless that's set
                            separately. If omitted, -api-token is required instead.

box-agent flags (see docs/USAGE.md for details):
  -router URL               default: wss://llm.eu.greenference.com
  -api-url URL               default: https://api.greenference.com
  -api-token TOKEN           default: same as -token (or API_TOKEN env var).
                            If -token is omitted, this must be an IAM/account-
                            level token instead, used to auto-provision a new
                            agent instance/token on first run.
  -token-cache PATH          where an auto-provisioned token is cached across
                            restarts (default: /var/lib/box-agent/token);
                            only used when -token is omitted
  -llm-url URL               default: http://localhost:8000
  -llm-api-key KEY
  -is-public BOOL            default: true
  -input-per-million N       default: 0 (unreported)
  -output-per-million N      default: 0 (unreported)
  -context-length N
  -max-output-length N
  -quantization NAME
  -input-modalities LIST
  -output-modalities LIST
  -supported-features LIST

vLLM self-hosting (box-agent downloads/boots/supervises "vllm serve"
itself; requires vllm already on PATH - not installed by this script):
  -deploy-vllm                boot -backend-model with vllm serve instead of
                              expecting an already-running -llm-url backend
  -vllm-gpu-util N             default: 0.9
  -vllm-max-model-len N        default: vllm's own default
  -vllm-enable-sleep-mode BOOL default: true
  -vllm-enable-auto-tool-choice BOOL  required (with -vllm-tool-call-parser)
                              to advertise "tools" in -supported-features
  -vllm-tool-call-parser NAME  e.g. hermes, llama3_json, mistral, pythonic
  -vllm-reasoning-parser NAME  e.g. deepseek_r1, qwen3
  -vllm-chat-template PATH
  -vllm-extra-args "ARGS"      appended verbatim to vllm serve
  -vllm-hf-token TOKEN          for gated repos (or HF_TOKEN env)
  -vllm-hf-home DIR             HF cache dir (default: /var/lib/box-agent/hf-cache)
  -vllm-boot-timeout DURATION   default: 15m
  -vllm-log-file PATH           default: /var/log/box-agent-vllm.log

Installer-only flags:
  -version TAG               release tag to install (default: latest)
  -install-dir DIR            binary destination (default: /usr/local/bin)
  -conf-dir DIR                supervisord conf.d directory (default:
                              auto-detect /etc/supervisor/conf.d, then
                              /etc/supervisord.d)
  -service-user USER          system user to run the program as (default:
                              empty - run as whoever invokes this script)
EOF
}

ROUTER="${ROUTER:-wss://llm.eu.greenference.com}"
PROVIDER="${PROVIDER:-}"
TOKEN="${TOKEN:-${AGENT_TOKEN:-}}"
API_URL="${API_URL:-https://api.greenference.com}"
API_TOKEN="${API_TOKEN:-}"
TOKEN_CACHE="${TOKEN_CACHE:-/var/lib/box-agent/token}"
LLM_URL="${LLM_URL:-http://localhost:8000}"
LLM_API_KEY="${LLM_API_KEY:-}"
BACKEND_MODEL="${BACKEND_MODEL:-}"
IS_PUBLIC="${IS_PUBLIC:-}"
INPUT_PER_MILLION="${INPUT_PER_MILLION:-}"
OUTPUT_PER_MILLION="${OUTPUT_PER_MILLION:-}"
CONTEXT_LENGTH="${CONTEXT_LENGTH:-}"
MAX_OUTPUT_LENGTH="${MAX_OUTPUT_LENGTH:-}"
QUANTIZATION="${QUANTIZATION:-}"
INPUT_MODALITIES="${INPUT_MODALITIES:-}"
OUTPUT_MODALITIES="${OUTPUT_MODALITIES:-}"
SUPPORTED_FEATURES="${SUPPORTED_FEATURES:-}"
DEPLOY_VLLM="${DEPLOY_VLLM:-false}"
VLLM_GPU_UTIL="${VLLM_GPU_UTIL:-}"
VLLM_MAX_MODEL_LEN="${VLLM_MAX_MODEL_LEN:-}"
VLLM_ENABLE_SLEEP_MODE="${VLLM_ENABLE_SLEEP_MODE:-}"
VLLM_ENABLE_AUTO_TOOL_CHOICE="${VLLM_ENABLE_AUTO_TOOL_CHOICE:-}"
VLLM_TOOL_CALL_PARSER="${VLLM_TOOL_CALL_PARSER:-}"
VLLM_REASONING_PARSER="${VLLM_REASONING_PARSER:-}"
VLLM_CHAT_TEMPLATE="${VLLM_CHAT_TEMPLATE:-}"
VLLM_EXTRA_ARGS="${VLLM_EXTRA_ARGS:-}"
VLLM_HF_TOKEN="${VLLM_HF_TOKEN:-${HF_TOKEN:-}}"
VLLM_HF_HOME="${VLLM_HF_HOME:-}"
VLLM_BOOT_TIMEOUT="${VLLM_BOOT_TIMEOUT:-}"
VLLM_LOG_FILE="${VLLM_LOG_FILE:-}"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
CONF_DIR="${CONF_DIR:-}"
SERVICE_USER="${SERVICE_USER:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    -router) ROUTER="$2"; shift 2 ;;
    -router=*) ROUTER="${1#*=}"; shift ;;
    -provider) PROVIDER="$2"; shift 2 ;;
    -provider=*) PROVIDER="${1#*=}"; shift ;;
    -token) TOKEN="$2"; shift 2 ;;
    -token=*) TOKEN="${1#*=}"; shift ;;
    -api-url) API_URL="$2"; shift 2 ;;
    -api-url=*) API_URL="${1#*=}"; shift ;;
    -api-token) API_TOKEN="$2"; shift 2 ;;
    -api-token=*) API_TOKEN="${1#*=}"; shift ;;
    -token-cache) TOKEN_CACHE="$2"; shift 2 ;;
    -token-cache=*) TOKEN_CACHE="${1#*=}"; shift ;;
    -llm-url) LLM_URL="$2"; shift 2 ;;
    -llm-url=*) LLM_URL="${1#*=}"; shift ;;
    -llm-api-key) LLM_API_KEY="$2"; shift 2 ;;
    -llm-api-key=*) LLM_API_KEY="${1#*=}"; shift ;;
    -backend-model) BACKEND_MODEL="$2"; shift 2 ;;
    -backend-model=*) BACKEND_MODEL="${1#*=}"; shift ;;
    -is-public) IS_PUBLIC="$2"; shift 2 ;;
    -is-public=*) IS_PUBLIC="${1#*=}"; shift ;;
    -input-per-million) INPUT_PER_MILLION="$2"; shift 2 ;;
    -input-per-million=*) INPUT_PER_MILLION="${1#*=}"; shift ;;
    -output-per-million) OUTPUT_PER_MILLION="$2"; shift 2 ;;
    -output-per-million=*) OUTPUT_PER_MILLION="${1#*=}"; shift ;;
    -context-length) CONTEXT_LENGTH="$2"; shift 2 ;;
    -context-length=*) CONTEXT_LENGTH="${1#*=}"; shift ;;
    -max-output-length) MAX_OUTPUT_LENGTH="$2"; shift 2 ;;
    -max-output-length=*) MAX_OUTPUT_LENGTH="${1#*=}"; shift ;;
    -quantization) QUANTIZATION="$2"; shift 2 ;;
    -quantization=*) QUANTIZATION="${1#*=}"; shift ;;
    -input-modalities) INPUT_MODALITIES="$2"; shift 2 ;;
    -input-modalities=*) INPUT_MODALITIES="${1#*=}"; shift ;;
    -output-modalities) OUTPUT_MODALITIES="$2"; shift 2 ;;
    -output-modalities=*) OUTPUT_MODALITIES="${1#*=}"; shift ;;
    -supported-features) SUPPORTED_FEATURES="$2"; shift 2 ;;
    -supported-features=*) SUPPORTED_FEATURES="${1#*=}"; shift ;;
    -deploy-vllm) DEPLOY_VLLM="true"; shift ;;
    -vllm-gpu-util) VLLM_GPU_UTIL="$2"; shift 2 ;;
    -vllm-gpu-util=*) VLLM_GPU_UTIL="${1#*=}"; shift ;;
    -vllm-max-model-len) VLLM_MAX_MODEL_LEN="$2"; shift 2 ;;
    -vllm-max-model-len=*) VLLM_MAX_MODEL_LEN="${1#*=}"; shift ;;
    -vllm-enable-sleep-mode) VLLM_ENABLE_SLEEP_MODE="$2"; shift 2 ;;
    -vllm-enable-sleep-mode=*) VLLM_ENABLE_SLEEP_MODE="${1#*=}"; shift ;;
    -vllm-enable-auto-tool-choice) VLLM_ENABLE_AUTO_TOOL_CHOICE="$2"; shift 2 ;;
    -vllm-enable-auto-tool-choice=*) VLLM_ENABLE_AUTO_TOOL_CHOICE="${1#*=}"; shift ;;
    -vllm-tool-call-parser) VLLM_TOOL_CALL_PARSER="$2"; shift 2 ;;
    -vllm-tool-call-parser=*) VLLM_TOOL_CALL_PARSER="${1#*=}"; shift ;;
    -vllm-reasoning-parser) VLLM_REASONING_PARSER="$2"; shift 2 ;;
    -vllm-reasoning-parser=*) VLLM_REASONING_PARSER="${1#*=}"; shift ;;
    -vllm-chat-template) VLLM_CHAT_TEMPLATE="$2"; shift 2 ;;
    -vllm-chat-template=*) VLLM_CHAT_TEMPLATE="${1#*=}"; shift ;;
    -vllm-extra-args) VLLM_EXTRA_ARGS="$2"; shift 2 ;;
    -vllm-extra-args=*) VLLM_EXTRA_ARGS="${1#*=}"; shift ;;
    -vllm-hf-token) VLLM_HF_TOKEN="$2"; shift 2 ;;
    -vllm-hf-token=*) VLLM_HF_TOKEN="${1#*=}"; shift ;;
    -vllm-hf-home) VLLM_HF_HOME="$2"; shift 2 ;;
    -vllm-hf-home=*) VLLM_HF_HOME="${1#*=}"; shift ;;
    -vllm-boot-timeout) VLLM_BOOT_TIMEOUT="$2"; shift 2 ;;
    -vllm-boot-timeout=*) VLLM_BOOT_TIMEOUT="${1#*=}"; shift ;;
    -vllm-log-file) VLLM_LOG_FILE="$2"; shift 2 ;;
    -vllm-log-file=*) VLLM_LOG_FILE="${1#*=}"; shift ;;
    -version) VERSION="$2"; shift 2 ;;
    -version=*) VERSION="${1#*=}"; shift ;;
    -install-dir) INSTALL_DIR="$2"; shift 2 ;;
    -install-dir=*) INSTALL_DIR="${1#*=}"; shift ;;
    -conf-dir) CONF_DIR="$2"; shift 2 ;;
    -conf-dir=*) CONF_DIR="${1#*=}"; shift ;;
    -service-user) SERVICE_USER="$2"; shift 2 ;;
    -service-user=*) SERVICE_USER="${1#*=}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown flag '$1'" >&2; usage; exit 1 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "error: must be run as root (try: sudo $0 ...)" >&2
  exit 1
fi
if [ -z "$PROVIDER" ] || [ -z "$BACKEND_MODEL" ]; then
  echo "error: -provider and -backend-model are required - box-agent does not guess these" >&2
  usage
  exit 1
fi
if [ -z "$TOKEN" ] && [ -z "$API_TOKEN" ]; then
  echo "error: either -token (an existing agent token) or -api-token (an IAM/account token to auto-provision one) is required" >&2
  usage
  exit 1
fi
# -token and -api-token are the same registration token in practice - fall
# back so only one has to be passed, while still letting -api-token/
# API_TOKEN override it if they ever diverge. Only applies when -token was
# actually given; when it wasn't, -api-token is an IAM token for
# auto-provisioning instead, not a per-instance one.
if [ -n "$TOKEN" ] && [ -z "$API_TOKEN" ]; then
  API_TOKEN="$TOKEN"
fi

VLLM_BIN_DIR=""
if [ "$DEPLOY_VLLM" = "true" ]; then
  if ! command -v vllm >/dev/null 2>&1; then
    echo "error: -deploy-vllm requires vllm on PATH - install it first (pip install vllm, matched to this box's CUDA/driver), then rerun" >&2
    exit 1
  fi
  VLLM_BIN_DIR="$(dirname "$(command -v vllm)")"
  # Mirrors box-agent's own -deploy-vllm + -supported-features "tools" check
  # (main.go's containsCSV guard) - fail here, before the program is even
  # registered, rather than let it crash-loop with the same fatal log line.
  case ",$(echo "$SUPPORTED_FEATURES" | tr -d ' ')," in
    *,tools,*)
      if [ "$VLLM_ENABLE_AUTO_TOOL_CHOICE" != "true" ] || [ -z "$VLLM_TOOL_CALL_PARSER" ]; then
        echo "error: -supported-features declares \"tools\" but -vllm-enable-auto-tool-choice/-vllm-tool-call-parser aren't both set - either configure them or drop \"tools\" from -supported-features" >&2
        exit 1
      fi
      ;;
  esac
  # Defaulted here (rather than left to box-agent's own flag defaults) so
  # the directories created below match what's then explicitly forwarded.
  VLLM_LOG_FILE="${VLLM_LOG_FILE:-/var/log/box-agent-vllm.log}"
  VLLM_HF_HOME="${VLLM_HF_HOME:-/var/lib/box-agent/hf-cache}"
fi

uname_s="$(uname -s)"
uname_m="$(uname -m)"
case "$uname_m" in
  x86_64|amd64) goarch=amd64 ;;
  arm64|aarch64) goarch=arm64 ;;
  *) goarch="$uname_m" ;;
esac
if [ "$uname_s" != "Linux" ]; then
  echo "error: install-supervisord.sh targets Linux containers (got '$uname_s') - use install.sh instead" >&2
  exit 1
fi
goos=linux

# supervisord itself, not just box-agent - a standard, hardware-independent
# package (unlike vllm), so a best-effort auto-install is safe here, the
# same reasoning install-and-run.sh already applies to zstd.
if ! command -v supervisorctl >/dev/null 2>&1; then
  echo "supervisorctl not found - attempting to install supervisor..." >&2
  if command -v apt-get >/dev/null 2>&1; then
    (apt-get update -qq && apt-get install -y -qq supervisor) || true
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache supervisor || true
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q supervisor || true
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q supervisor || true
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm supervisor || true
  elif command -v zypper >/dev/null 2>&1; then
    zypper install -y supervisor || true
  elif command -v pip3 >/dev/null 2>&1; then
    pip3 install supervisor || true
  fi
  command -v supervisorctl >/dev/null 2>&1 || {
    echo "error: supervisorctl still not found - install supervisor yourself (e.g. apt-get install supervisor / pip install supervisor), then rerun" >&2
    exit 1
  }
fi

if [ -z "$CONF_DIR" ]; then
  if [ -d /etc/supervisor/conf.d ]; then
    CONF_DIR=/etc/supervisor/conf.d
  elif [ -d /etc/supervisord.d ]; then
    CONF_DIR=/etc/supervisord.d
  else
    echo "error: could not find a supervisord conf.d directory (looked in /etc/supervisor/conf.d, /etc/supervisord.d) - pass -conf-dir DIR explicitly" >&2
    exit 1
  fi
fi

# Most Docker images that run supervisord (vast.ai's included) already have
# it running as PID 1 or an early-boot service managing everything else in
# the container - "supervisorctl pid" only succeeds if that's reachable.
# Only try to start one ourselves as a fallback, for the less common case of
# supervisor freshly installed above but not yet running.
if ! supervisorctl pid >/dev/null 2>&1; then
  echo "supervisord doesn't appear to be running - starting it" >&2
  sd_conf=""
  for c in /etc/supervisor/supervisord.conf /etc/supervisord.conf; do
    [ -f "$c" ] && sd_conf="$c" && break
  done
  if [ -z "$sd_conf" ]; then
    echo "error: supervisord is not running and no config file found (looked in /etc/supervisor/supervisord.conf, /etc/supervisord.conf) - start it yourself, then rerun" >&2
    exit 1
  fi
  supervisord -c "$sd_conf"
  sleep 1
  supervisorctl pid >/dev/null 2>&1 || {
    echo "error: started supervisord but still can't reach it via supervisorctl" >&2
    exit 1
  }
fi

dest="${INSTALL_DIR%/}/box-agent"
asset="box-agent-${goos}-${goarch}"
if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

mkdir -p "$INSTALL_DIR"
echo "Downloading ${url} -> ${dest}" >&2
if curl -fsSL "$url" -o "$dest"; then
  chmod +x "$dest"
else
  echo "warning: no published '${asset}' release asset; building from source instead" >&2
  command -v git >/dev/null 2>&1 || { echo "error: git is required to build from source" >&2; exit 1; }
  command -v go >/dev/null 2>&1 || { echo "error: go is required to build from source (https://go.dev/dl/)" >&2; exit 1; }
  src_dir="$(mktemp -d)"
  trap 'rm -rf "$src_dir"' EXIT
  if [ "$VERSION" = "latest" ]; then
    git clone --depth 1 "https://github.com/${REPO}.git" "$src_dir"
  else
    git clone --depth 1 --branch "$VERSION" "https://github.com/${REPO}.git" "$src_dir"
  fi
  ( cd "$src_dir" && GOOS="$goos" GOARCH="$goarch" go build -ldflags "-X github.com/LLMOcean/box.agent/version.Version=${VERSION}" -o "$dest" . )
  chmod +x "$dest"
fi

EXTRA_ARGS=()
[ -n "$BACKEND_MODEL" ] && EXTRA_ARGS+=("-backend-model" "$BACKEND_MODEL")
# Only relevant to the auto-provisioning path (-token omitted), but harmless
# to always pass through - box-agent ignores it when -token is set directly.
[ -z "$TOKEN" ] && EXTRA_ARGS+=("-token-cache" "$TOKEN_CACHE")
[ -n "$IS_PUBLIC" ] && EXTRA_ARGS+=("-is-public" "$IS_PUBLIC")
[ -n "$INPUT_PER_MILLION" ] && EXTRA_ARGS+=("-input-per-million" "$INPUT_PER_MILLION")
[ -n "$OUTPUT_PER_MILLION" ] && EXTRA_ARGS+=("-output-per-million" "$OUTPUT_PER_MILLION")
[ -n "$CONTEXT_LENGTH" ] && EXTRA_ARGS+=("-context-length" "$CONTEXT_LENGTH")
[ -n "$MAX_OUTPUT_LENGTH" ] && EXTRA_ARGS+=("-max-output-length" "$MAX_OUTPUT_LENGTH")
[ -n "$QUANTIZATION" ] && EXTRA_ARGS+=("-quantization" "$QUANTIZATION")
[ -n "$INPUT_MODALITIES" ] && EXTRA_ARGS+=("-input-modalities" "$INPUT_MODALITIES")
[ -n "$OUTPUT_MODALITIES" ] && EXTRA_ARGS+=("-output-modalities" "$OUTPUT_MODALITIES")
[ -n "$SUPPORTED_FEATURES" ] && EXTRA_ARGS+=("-supported-features" "$SUPPORTED_FEATURES")
if [ "$DEPLOY_VLLM" = "true" ]; then
  EXTRA_ARGS+=("-deploy-vllm")
  [ -n "$VLLM_GPU_UTIL" ] && EXTRA_ARGS+=("-vllm-gpu-util" "$VLLM_GPU_UTIL")
  [ -n "$VLLM_MAX_MODEL_LEN" ] && EXTRA_ARGS+=("-vllm-max-model-len" "$VLLM_MAX_MODEL_LEN")
  [ -n "$VLLM_ENABLE_SLEEP_MODE" ] && EXTRA_ARGS+=("-vllm-enable-sleep-mode" "$VLLM_ENABLE_SLEEP_MODE")
  [ -n "$VLLM_ENABLE_AUTO_TOOL_CHOICE" ] && EXTRA_ARGS+=("-vllm-enable-auto-tool-choice" "$VLLM_ENABLE_AUTO_TOOL_CHOICE")
  [ -n "$VLLM_TOOL_CALL_PARSER" ] && EXTRA_ARGS+=("-vllm-tool-call-parser" "$VLLM_TOOL_CALL_PARSER")
  [ -n "$VLLM_REASONING_PARSER" ] && EXTRA_ARGS+=("-vllm-reasoning-parser" "$VLLM_REASONING_PARSER")
  [ -n "$VLLM_CHAT_TEMPLATE" ] && EXTRA_ARGS+=("-vllm-chat-template" "$VLLM_CHAT_TEMPLATE")
  [ -n "$VLLM_EXTRA_ARGS" ] && EXTRA_ARGS+=("-vllm-extra-args" "$VLLM_EXTRA_ARGS")
  [ -n "$VLLM_BOOT_TIMEOUT" ] && EXTRA_ARGS+=("-vllm-boot-timeout" "$VLLM_BOOT_TIMEOUT")
  # Always forwarded (not just when overridden) - VLLM_LOG_FILE was defaulted
  # above and the directory created below must match what box-agent is
  # actually told to write to.
  EXTRA_ARGS+=("-vllm-log-file" "$VLLM_LOG_FILE")
  # -vllm-hf-token/-vllm-hf-home are sent via environment= instead (below),
  # the same way AGENT_TOKEN/API_TOKEN already are, rather than as
  # plain-text command-line flags.
fi

if [ -n "$SERVICE_USER" ] && ! id "$SERVICE_USER" >/dev/null 2>&1; then
  echo "Creating system user ${SERVICE_USER}" >&2
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

# chown_if_service_user: directories below default to root-owned, which is
# fine when the program also runs as root (SERVICE_USER unset, the common
# case in a single-tenant container). Only need to hand ownership over when
# an unprivileged -service-user was explicitly requested.
chown_if_service_user() {
  [ -n "$SERVICE_USER" ] && chown "$SERVICE_USER" "$@"
  return 0
}

if [ -z "$TOKEN" ]; then
  cache_dir="$(dirname "$TOKEN_CACHE")"
  echo "Creating token cache dir ${cache_dir}" >&2
  mkdir -p "$cache_dir"
  chown_if_service_user "$cache_dir"
  [ -z "$SERVICE_USER" ] && chmod 700 "$cache_dir" || true
fi

if [ "$DEPLOY_VLLM" = "true" ]; then
  log_dir="$(dirname "$VLLM_LOG_FILE")"
  echo "Creating vllm log dir ${log_dir}" >&2
  mkdir -p "$log_dir"
  touch "$VLLM_LOG_FILE"
  chown_if_service_user "$log_dir" "$VLLM_LOG_FILE"

  echo "Creating HF cache dir ${VLLM_HF_HOME}" >&2
  mkdir -p "$VLLM_HF_HOME"
  chown_if_service_user "$VLLM_HF_HOME"
fi

# supervisord's "environment=" line is a comma-separated KEY="value" list
# where "%" needs doubling and embedded quotes need escaping - build it
# through a small escaper rather than interpolating tokens raw.
supervisor_escape() {
  local s="$1"
  s="${s//%/%%}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

ENV_PAIRS=()
[ -n "$TOKEN" ] && ENV_PAIRS+=("AGENT_TOKEN=\"$(supervisor_escape "$TOKEN")\"")
ENV_PAIRS+=("API_TOKEN=\"$(supervisor_escape "$API_TOKEN")\"")
[ -n "$LLM_API_KEY" ] && ENV_PAIRS+=("LLM_API_KEY=\"$(supervisor_escape "$LLM_API_KEY")\"")
if [ "$DEPLOY_VLLM" = "true" ]; then
  # supervisord programs get a minimal default PATH that won't include a
  # pip/venv install location - prepend vllm's own resolved bin dir so the
  # "vllm" child process box-agent execs actually resolves.
  ENV_PAIRS+=("PATH=\"$(supervisor_escape "${VLLM_BIN_DIR}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")\"")
  [ -n "$VLLM_HF_TOKEN" ] && ENV_PAIRS+=("HF_TOKEN=\"$(supervisor_escape "$VLLM_HF_TOKEN")\"")
  ENV_PAIRS+=("HF_HOME=\"$(supervisor_escape "$VLLM_HF_HOME")\"")
fi
env_str=""
for kv in "${ENV_PAIRS[@]}"; do
  env_str="${env_str:+${env_str},}${kv}"
done

extra_str="${EXTRA_ARGS[*]:-}"
conf_path="${CONF_DIR%/}/box-agent.conf"
echo "Writing ${conf_path}" >&2
{
  echo "[program:box-agent]"
  echo "command=${dest} -router ${ROUTER} -provider ${PROVIDER} -api-url ${API_URL} -llm-url ${LLM_URL}${extra_str:+ $extra_str}"
  echo "autostart=true"
  echo "autorestart=true"
  # startsecs=0: a fast failure (bad token rejected near-instantly, a
  # misconfigured flag, ...) can exit well under supervisord's default
  # startsecs=1 - three such quick exits get misclassified as a failed
  # launch (FATAL, "Exited too quickly") rather than a crash to retry, and
  # supervisord stops trying entirely until a manual `supervisorctl start`.
  # startsecs=0 makes any spawn count as "started" immediately regardless
  # of how fast it then exits, so autorestart=true above is what decides
  # whether it restarts - not exit timing (verified: a bad-token failure
  # against the real API happened to clear startsecs=1 on its own, purely
  # from network latency - not a guarantee for a faster local failure).
  echo "startsecs=0"
  echo "stopsignal=TERM"
  echo "stdout_logfile=/var/log/box-agent.out.log"
  echo "stdout_logfile_maxbytes=10MB"
  echo "stdout_logfile_backups=3"
  echo "redirect_stderr=true"
  [ -n "$env_str" ] && echo "environment=${env_str}"
  [ -n "$SERVICE_USER" ] && echo "user=${SERVICE_USER}"
} > "$conf_path"

# Conf file holds the registration token in plain text - keep it root-only
# readable rather than the usual world-readable 644, same reasoning as
# install.sh's systemd unit/launchd plist.
chmod 600 "$conf_path"

supervisorctl reread
supervisorctl update

echo "box-agent installed and started under supervisord." >&2
echo "  supervisorctl status box-agent" >&2
echo "  supervisorctl tail -f box-agent" >&2
