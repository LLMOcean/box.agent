#!/usr/bin/env bash
# Installs box.agent as a persistent background service on this machine:
# downloads (or, if no release binary is published for this OS/arch,
# builds) the box-agent binary, installs it to survive reboots/crashes
# (systemd on Linux, launchd on macOS), and starts it. Must be run as
# root/sudo. See docs/INSTALL.md for the full walkthrough (and the manual
# steps this script automates).
#
# Usage (flags mirror box-agent's own -flag names; run with -h for all of
# them):
#
#   curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.sh \
#     | sudo bash -s -- \
#         -router wss://router.example.com \
#         -provider yourns/your-model \
#         -token "$REGISTRATION_TOKEN" \
#         -api-url https://api.example.com \
#         -api-token "$REGISTRATION_TOKEN" \
#         -llm-url http://localhost:11434
#
# Every flag can also be set via an identically-named (upper-cased, with
# "-" -> "_") environment variable instead, e.g. PROVIDER=... TOKEN=...
# ./install.sh - a flag always takes precedence over its env var.
#
# Installer-only flags/env vars (not forwarded to box-agent):
#   -version (VERSION)           - release tag to install (default: latest)
#   -install-dir (INSTALL_DIR)   - where to place the binary (default: /usr/local/bin)
#   -service-user (SERVICE_USER) - Linux only: system user to run the service
#                                   as (default: box-agent)

set -euo pipefail

REPO="LLMOcean/box.agent"

usage() {
  cat >&2 <<'EOF'
Usage: install.sh -provider NAME -token TOK -api-token TOK [options]

Required:
  -provider NAME           provider name, e.g. yourns/your-model
  -token TOKEN              registration token (or TOKEN/AGENT_TOKEN env var)
  -api-token TOKEN          registration token (or API_TOKEN env var)

box-agent flags (see docs/USAGE.md for details):
  -router URL               default: wss://router.example.com
  -api-url URL               default: https://api.example.com
  -llm-url URL               default: http://localhost:11434
  -llm-api-key KEY
  -backend-model NAME
  -context-length N
  -max-output-length N
  -quantization NAME
  -input-modalities LIST
  -output-modalities LIST
  -supported-features LIST

Installer-only flags:
  -version TAG               release tag to install (default: latest)
  -install-dir DIR            binary destination (default: /usr/local/bin)
  -service-user USER          Linux system user to run as (default: box-agent)
EOF
}

ROUTER="${ROUTER:-wss://router.example.com}"
PROVIDER="${PROVIDER:-}"
TOKEN="${TOKEN:-${AGENT_TOKEN:-}}"
API_URL="${API_URL:-https://api.example.com}"
API_TOKEN="${API_TOKEN:-}"
LLM_URL="${LLM_URL:-http://localhost:11434}"
LLM_API_KEY="${LLM_API_KEY:-}"
BACKEND_MODEL="${BACKEND_MODEL:-}"
CONTEXT_LENGTH="${CONTEXT_LENGTH:-}"
MAX_OUTPUT_LENGTH="${MAX_OUTPUT_LENGTH:-}"
QUANTIZATION="${QUANTIZATION:-}"
INPUT_MODALITIES="${INPUT_MODALITIES:-}"
OUTPUT_MODALITIES="${OUTPUT_MODALITIES:-}"
SUPPORTED_FEATURES="${SUPPORTED_FEATURES:-}"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
SERVICE_USER="${SERVICE_USER:-box-agent}"

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
    -llm-url) LLM_URL="$2"; shift 2 ;;
    -llm-url=*) LLM_URL="${1#*=}"; shift ;;
    -llm-api-key) LLM_API_KEY="$2"; shift 2 ;;
    -llm-api-key=*) LLM_API_KEY="${1#*=}"; shift ;;
    -backend-model) BACKEND_MODEL="$2"; shift 2 ;;
    -backend-model=*) BACKEND_MODEL="${1#*=}"; shift ;;
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
    -version) VERSION="$2"; shift 2 ;;
    -version=*) VERSION="${1#*=}"; shift ;;
    -install-dir) INSTALL_DIR="$2"; shift 2 ;;
    -install-dir=*) INSTALL_DIR="${1#*=}"; shift ;;
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
if [ -z "$PROVIDER" ] || [ -z "$TOKEN" ] || [ -z "$API_TOKEN" ]; then
  echo "error: -provider, -token, and -api-token are all required" >&2
  usage
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
  ( cd "$src_dir" && GOOS="$goos" GOARCH="$goarch" go build -o "$dest" . )
  chmod +x "$dest"
fi

EXTRA_ARGS=()
[ -n "$BACKEND_MODEL" ] && EXTRA_ARGS+=("-backend-model" "$BACKEND_MODEL")
[ -n "$CONTEXT_LENGTH" ] && EXTRA_ARGS+=("-context-length" "$CONTEXT_LENGTH")
[ -n "$MAX_OUTPUT_LENGTH" ] && EXTRA_ARGS+=("-max-output-length" "$MAX_OUTPUT_LENGTH")
[ -n "$QUANTIZATION" ] && EXTRA_ARGS+=("-quantization" "$QUANTIZATION")
[ -n "$INPUT_MODALITIES" ] && EXTRA_ARGS+=("-input-modalities" "$INPUT_MODALITIES")
[ -n "$OUTPUT_MODALITIES" ] && EXTRA_ARGS+=("-output-modalities" "$OUTPUT_MODALITIES")
[ -n "$SUPPORTED_FEATURES" ] && EXTRA_ARGS+=("-supported-features" "$SUPPORTED_FEATURES")

if [ "$goos" = "linux" ]; then
  UNIT_PATH="/etc/systemd/system/box-agent.service"

  if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    echo "Creating system user ${SERVICE_USER}" >&2
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
  fi

  extra_str="${EXTRA_ARGS[*]:-}"
  echo "Writing ${UNIT_PATH}" >&2
  {
    echo "[Unit]"
    echo "Description=router.agent remote WebSocket agent (box.agent)"
    echo "After=network.target"
    echo ""
    echo "[Service]"
    echo "ExecStart=${dest} -router ${ROUTER} -provider ${PROVIDER} -api-url ${API_URL} -llm-url ${LLM_URL}${extra_str:+ $extra_str}"
    echo "Environment=AGENT_TOKEN=${TOKEN}"
    echo "Environment=API_TOKEN=${API_TOKEN}"
    [ -n "$LLM_API_KEY" ] && echo "Environment=LLM_API_KEY=${LLM_API_KEY}"
    echo "Restart=always"
    echo "RestartSec=5"
    echo "User=${SERVICE_USER}"
    echo ""
    echo "[Install]"
    echo "WantedBy=multi-user.target"
  } > "$UNIT_PATH"

  # Unit file holds the registration token in plain text - keep it
  # root-only readable rather than the usual world-readable 644.
  chmod 600 "$UNIT_PATH"

  systemctl daemon-reload
  systemctl enable --now box-agent.service

  echo "box-agent installed and started." >&2
  echo "  systemctl status box-agent.service" >&2
  echo "  journalctl -u box-agent.service -f" >&2

elif [ "$goos" = "darwin" ]; then
  PLIST_LABEL="com.llmocean.box-agent"
  PLIST_PATH="/Library/LaunchDaemons/${PLIST_LABEL}.plist"
  LOG_PATH="/var/log/box-agent.log"

  xml_escape() {
    local s="$1"
    s="${s//&/&amp;}"
    s="${s//</&lt;}"
    s="${s//>/&gt;}"
    printf '%s' "$s"
  }

  PROGRAM_ARGS=("$dest" "-router" "$ROUTER" "-provider" "$PROVIDER" "-api-url" "$API_URL" "-llm-url" "$LLM_URL")
  PROGRAM_ARGS+=("${EXTRA_ARGS[@]}")

  echo "Writing ${PLIST_PATH}" >&2
  {
    echo '<?xml version="1.0" encoding="UTF-8"?>'
    echo '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
    echo '<plist version="1.0">'
    echo '<dict>'
    echo '  <key>Label</key>'
    echo "  <string>${PLIST_LABEL}</string>"
    echo '  <key>ProgramArguments</key>'
    echo '  <array>'
    for a in "${PROGRAM_ARGS[@]}"; do
      echo "    <string>$(xml_escape "$a")</string>"
    done
    echo '  </array>'
    echo '  <key>EnvironmentVariables</key>'
    echo '  <dict>'
    echo '    <key>AGENT_TOKEN</key>'
    echo "    <string>$(xml_escape "$TOKEN")</string>"
    echo '    <key>API_TOKEN</key>'
    echo "    <string>$(xml_escape "$API_TOKEN")</string>"
    if [ -n "$LLM_API_KEY" ]; then
      echo '    <key>LLM_API_KEY</key>'
      echo "    <string>$(xml_escape "$LLM_API_KEY")</string>"
    fi
    echo '  </dict>'
    echo '  <key>RunAtLoad</key>'
    echo '  <true/>'
    echo '  <key>KeepAlive</key>'
    echo '  <true/>'
    echo '  <key>StandardOutPath</key>'
    echo "  <string>${LOG_PATH}</string>"
    echo '  <key>StandardErrorPath</key>'
    echo "  <string>${LOG_PATH}</string>"
    echo '</dict>'
    echo '</plist>'
  } > "$PLIST_PATH"

  # Runs as root (no UserName key) - launchd's LaunchDaemons are
  # inherently root-installed anyway, and plist holds the registration
  # token in plain text, so keep it root-only readable.
  chown root:wheel "$PLIST_PATH"
  chmod 600 "$PLIST_PATH"

  launchctl unload -w "$PLIST_PATH" >/dev/null 2>&1 || true
  launchctl load -w "$PLIST_PATH"

  echo "box-agent installed and started." >&2
  echo "  sudo launchctl list | grep ${PLIST_LABEL}" >&2
  echo "  tail -f ${LOG_PATH}" >&2
fi
