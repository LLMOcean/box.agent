#!/usr/bin/env bash
# Multi-router variant of install-and-run.sh: downloads (or builds) a
# single box-agent binary, then launches one box-agent PROCESS PER LINE
# of an instances file, each connected to its own -router (and its own
# -provider/-token/-token-cache; typically the SAME -backend-model and
# -llm-url across all of them, since this is one local model registered
# under multiple routers, not a different model per router - box-agent
# itself only ever dials one router per process, see deploy/install.sh's
# comments and docs/USAGE.md). All instances run in the foreground of
# this script; Ctrl-C (or any instance exiting) stops all of them.
# Nothing persists across reboots - for that, run install.sh once per
# router instead, each with its own systemd unit.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install-and-run-multi.sh \
#     -o install-and-run-multi.sh
#   cat > routers.conf <<'CONF'
#   # one box-agent instance per line - full flag set, same syntax as
#   # box-agent itself (see docs/USAGE.md). Blank lines and lines
#   # starting with # are ignored. Same -backend-model/-llm-url on every
#   # line here since it's one local model, registered under two routers.
#   -router wss://llm.eu.greenference.com -provider yourns/your-model -backend-model "your-model-id" -token "$EU_TOKEN" -llm-url http://localhost:8000
#   -router wss://llm.na.greenference.com -provider yourns/your-model -backend-model "your-model-id" -token "$NA_TOKEN" -llm-url http://localhost:8000
#   CONF
#   bash install-and-run-multi.sh -config routers.conf -install-model "your-model-id"
#
# Note the config file is read locally, so (unlike install-and-run.sh)
# this script can't be run as a single `curl | bash` one-liner - fetch it
# first, write your routers.conf, then run it.
#
# Env vars / flags (installer-only):
#   -version (VERSION)         - release tag to install (default: latest)
#   -install-dir (INSTALL_DIR) - where to place the binary (default: current directory)
#   -config (CONFIG)           - instances file, one box-agent flag set per line (required)
#   -install-model (INSTALL_MODEL) - pull this model via Ollama (at -llm-url)
#                               once, before starting any instance - for the
#                               common case of one shared local model across
#                               all routers. Runs once, not once per line.
#   -llm-url (LLM_URL)         - where to pull -install-model, if given
#                               (default: http://localhost:8000)
#   -models (MODELS_FILE)      - for the N-different-models case (e.g. 5
#                               models on 5 local servers, each registered
#                               to 2 routers = 10 instances but only 5
#                               distinct pulls): a file with one "MODEL
#                               LLM_URL" pair per line, each pulled via
#                               Ollama exactly once before any instance
#                               starts. Blank lines and lines starting
#                               with # are ignored. Combine with -config
#                               lines that reference those same
#                               -backend-model/-llm-url values.

set -euo pipefail

REPO="LLMOcean/box.agent"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-.}"
CONFIG="${CONFIG:-}"
INSTALL_MODEL="${INSTALL_MODEL:-}"
LLM_URL="${LLM_URL:-http://localhost:8000}"
MODELS_FILE="${MODELS_FILE:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    -version) VERSION="$2"; shift 2 ;;
    -version=*) VERSION="${1#*=}"; shift ;;
    -install-dir) INSTALL_DIR="$2"; shift 2 ;;
    -install-dir=*) INSTALL_DIR="${1#*=}"; shift ;;
    -config) CONFIG="$2"; shift 2 ;;
    -config=*) CONFIG="${1#*=}"; shift ;;
    -install-model) INSTALL_MODEL="$2"; shift 2 ;;
    -install-model=*) INSTALL_MODEL="${1#*=}"; shift ;;
    -llm-url) LLM_URL="$2"; shift 2 ;;
    -llm-url=*) LLM_URL="${1#*=}"; shift ;;
    -models) MODELS_FILE="$2"; shift 2 ;;
    -models=*) MODELS_FILE="${1#*=}"; shift ;;
    -h|--help)
      cat <<'EOF'
Usage: install-and-run-multi.sh -config FILE [-install-model MODEL] [-llm-url URL] [-models FILE] [-version TAG] [-install-dir DIR]

Downloads (or builds) a single box-agent binary, then runs one box-agent
process per line of FILE, each line being a full box-agent flag set (its
own -router, -provider, -backend-model, -token, -llm-url, -token-cache,
...). Blank lines and lines starting with # are ignored. All processes
run in the foreground; Ctrl-C stops every instance together, and if any
one instance exits, the rest are stopped too.

Two ways to pre-pull models via Ollama once each, before any instance
starts (rather than redundantly per instance):
  -install-model MODEL [-llm-url URL]  - one shared model/server used by
                                          every instance (default URL:
                                          http://localhost:8000)
  -models FILE                         - N distinct models, e.g. 5 models
                                          on 5 local servers each
                                          registered to 2 routers (10
                                          instances, 5 pulls). FILE has
                                          one "MODEL LLM_URL" pair per
                                          line; blank/# lines ignored.

No systemd/launchd, no persistence, no root required. For a persistent,
auto-restarting deployment, run install.sh once per router instead.
EOF
      exit 0
      ;;
    *) echo "error: unrecognized argument '$1' (this script's flags are -config/-install-model/-llm-url/-models/-version/-install-dir; per-instance box-agent flags go in the config file, not on the command line)" >&2; exit 1 ;;
  esac
done

if [ -z "$CONFIG" ]; then
  echo "error: -config FILE is required - see -h for the expected format" >&2
  exit 1
fi
if [ ! -f "$CONFIG" ]; then
  echo "error: config file '$CONFIG' not found" >&2
  exit 1
fi

uname_s="$(uname -s)"
uname_m="$(uname -m)"
case "$uname_m" in
  x86_64|amd64) goarch=amd64 ;;
  arm64|aarch64) goarch=arm64 ;;
  *) goarch="$uname_m" ;;
esac
case "$uname_s" in
  Linux) goos=linux ;;
  Darwin) goos=darwin ;;
  *) echo "error: unsupported OS '$uname_s' - see docs/INSTALL.md #7 to build from source manually" >&2; exit 1 ;;
esac

mkdir -p "$INSTALL_DIR"
dest="${INSTALL_DIR%/}/box-agent"
asset="box-agent-${goos}-${goarch}"
if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

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
  trap - EXIT
  rm -rf "$src_dir"
fi

if [ -n "$INSTALL_MODEL" ]; then
  echo "Installing '${INSTALL_MODEL}' via Ollama at ${LLM_URL} (once, shared by all instances)..." >&2
  "$dest" install-model -llm-url "$LLM_URL" "$INSTALL_MODEL"
fi

if [ -n "$MODELS_FILE" ]; then
  [ -f "$MODELS_FILE" ] || { echo "error: models file '$MODELS_FILE' not found" >&2; exit 1; }
  while IFS= read -r mline || [ -n "$mline" ]; do
    mtrimmed="$(echo "$mline" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    [ -z "$mtrimmed" ] && continue
    case "$mtrimmed" in \#*) continue ;; esac
    read -r m_model m_url <<< "$mtrimmed"
    if [ -z "$m_model" ] || [ -z "$m_url" ]; then
      echo "error: bad line in '$MODELS_FILE' (expected 'MODEL LLM_URL'): $mtrimmed" >&2
      exit 1
    fi
    echo "Installing '${m_model}' via Ollama at ${m_url}..." >&2
    "$dest" install-model -llm-url "$m_url" "$m_model"
  done < "$MODELS_FILE"
fi

# Parse the config file into one argv array per instance, honoring quotes
# and $VAR/${VAR} expansion (so tokens can be kept out of the file itself)
# the same way a shell would, without eval'ing arbitrary shell syntax.
instance_lines=()
while IFS= read -r line || [ -n "$line" ]; do
  trimmed="$(echo "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  [ -z "$trimmed" ] && continue
  case "$trimmed" in
    \#*) continue ;;
  esac
  instance_lines+=("$trimmed")
done < "$CONFIG"

if [ "${#instance_lines[@]}" -eq 0 ]; then
  echo "error: '$CONFIG' has no instance lines (blank/comment-only)" >&2
  exit 1
fi

pids=()
cleaned_up=0
cleanup() {
  [ "$cleaned_up" = 1 ] && return
  cleaned_up=1
  echo "Stopping all ${#pids[@]} box-agent instance(s)..." >&2
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

i=0
for line in "${instance_lines[@]}"; do
  i=$((i + 1))
  # Default each instance's -token-cache to a per-instance, root-free path
  # so parallel instances relying on auto-provisioning don't collide on
  # box-agent's own default (/var/lib/box-agent/token) - placed first so
  # an explicit -token-cache later in the line still wins.
  eval "instance_args=($line)"
  echo "[instance $i] starting: box-agent -token-cache ${INSTALL_DIR%/}/box-agent-token-$i ${instance_args[*]}" >&2
  "$dest" -token-cache "${INSTALL_DIR%/}/box-agent-token-$i" "${instance_args[@]}" &
  pids+=("$!")
done

echo "${#pids[@]} box-agent instance(s) running (foreground - Ctrl-C to stop all, nothing persists after this process exits)" >&2

# Poll rather than `wait -n` (bash >=4.3 only) - macOS ships bash 3.2 by
# default, which lacks it. First instance to exit takes the rest down.
while :; do
  for idx in "${!pids[@]}"; do
    pid="${pids[$idx]}"
    if ! kill -0 "$pid" 2>/dev/null; then
      wait "$pid"
      exit_code=$?
      echo "[instance $((idx + 1))] exited (code $exit_code) - stopping the rest" >&2
      exit "$exit_code"
    fi
  done
  sleep 1
done
