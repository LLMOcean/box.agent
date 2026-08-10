#!/usr/bin/env bash
# One-time, non-persistent box.agent deployment: no systemd/launchd, no
# service user, no root required (beyond whatever Ollama's own installer
# needs, if you pass -install-model). Downloads the box-agent release
# binary for this OS/arch (or builds from source if none is published),
# then runs it directly in the foreground - Ctrl-C to stop; nothing
# persists across reboots or crashes. For a persistent, auto-restarting
# deployment instead, use install.sh.
#
# Any argument given to this script (other than -version/-install-dir,
# consumed by the installer itself) is passed straight through to
# box-agent as-is - see docs/USAGE.md for the full flag reference.
# -provider/-backend-model and one of -token/-api-token are required by
# box-agent itself. Add -install-model to also pull -backend-model via
# Ollama before registering, so this one command deploys the LLM, deploys
# box-agent, and registers it with the router:
#
#   curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install-and-run.sh \
#     | bash -s -- \
#         -provider yourns/your-model \
#         -backend-model "your-model-id" \
#         -token "$REGISTRATION_TOKEN" \
#         -install-model
#
# Env vars / flags (installer-only, not forwarded to box-agent):
#   -version (VERSION)         - release tag to install (default: latest)
#   -install-dir (INSTALL_DIR) - where to place the binary (default: current directory)

set -euo pipefail

REPO="LLMOcean/box.agent"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-.}"

BOXAGENT_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    -version) VERSION="$2"; shift 2 ;;
    -version=*) VERSION="${1#*=}"; shift ;;
    -install-dir) INSTALL_DIR="$2"; shift 2 ;;
    -install-dir=*) INSTALL_DIR="${1#*=}"; shift ;;
    -h|--help)
      cat <<'EOF'
Usage: install-and-run.sh [box-agent flags...] [-version TAG] [-install-dir DIR]

Downloads (or builds) box-agent and runs it directly in the foreground -
no systemd/launchd, no persistence, no root required. Every flag except
-version/-install-dir is forwarded to box-agent unchanged; run
"box-agent -h" (after this installs it) for the full list.
Required: -provider, -backend-model, and one of -token/-api-token.
Add -install-model to also pull -backend-model via Ollama first.
EOF
      exit 0
      ;;
    *) BOXAGENT_ARGS+=("$1"); shift ;;
  esac
done

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

# Ollama's own installer (run by box-agent's -install-model) downloads a
# .tar.zst archive - best-effort install zstd ahead of time so extraction
# never depends on whatever happens to already be on the image. Not fatal
# if it can't be installed (no root/sudo, unknown package manager, ...):
# print a warning and keep going, since box-agent doesn't require zstd
# itself and Ollama's installer may already bundle its own extraction path.
if [ "$goos" = "linux" ] && ! command -v zstd >/dev/null 2>&1; then
  SUDO=""
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null 2>&1 && SUDO="sudo"
  fi
  if [ "$(id -u)" -eq 0 ] || [ -n "$SUDO" ]; then
    echo "Installing zstd..." >&2
    # `|| true` on every branch: best-effort only, must never abort the
    # script under set -e - box-agent itself doesn't need zstd, and
    # Ollama's installer may not either.
    if command -v apt-get >/dev/null 2>&1; then
      ($SUDO apt-get update -qq && $SUDO apt-get install -y -qq zstd) || echo "warning: zstd install failed - continuing without it" >&2
    elif command -v dnf >/dev/null 2>&1; then
      $SUDO dnf install -y -q zstd || echo "warning: zstd install failed - continuing without it" >&2
    elif command -v yum >/dev/null 2>&1; then
      $SUDO yum install -y -q zstd || echo "warning: zstd install failed - continuing without it" >&2
    elif command -v apk >/dev/null 2>&1; then
      $SUDO apk add --no-cache zstd || echo "warning: zstd install failed - continuing without it" >&2
    elif command -v pacman >/dev/null 2>&1; then
      $SUDO pacman -Sy --noconfirm zstd || echo "warning: zstd install failed - continuing without it" >&2
    elif command -v zypper >/dev/null 2>&1; then
      $SUDO zypper install -y zstd || echo "warning: zstd install failed - continuing without it" >&2
    else
      echo "warning: zstd not found and no supported package manager detected - continuing without it" >&2
    fi
  else
    echo "warning: zstd not found and no root/sudo available to install it - continuing without it" >&2
  fi
fi

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
fi

# Default -token-cache to a location writable without root, since
# box-agent's own default (/var/lib/box-agent/token) assumes the systemd
# deployment's setup. Placed first so an explicit -token-cache in the
# forwarded args (later in argv) still wins.
echo "Starting box-agent (foreground - Ctrl-C to stop, nothing persists after this process exits)" >&2
exec "$dest" -token-cache "${INSTALL_DIR%/}/box-agent-token" "${BOXAGENT_ARGS[@]}"
