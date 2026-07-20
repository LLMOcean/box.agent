#!/usr/bin/env bash
# Downloads the box.agent release binary from GitHub and runs it. Any
# arguments given to this script are passed straight through to box-agent
# as flags — see docs/USAGE.md for the full flag reference.
#
# Usage:
#   ./install-and-run.sh \
#     -router ws://chat.llmocean.com:8080 \
#     -provider plusclouds/qwen5.0-with-extras \
#     -token "$REGISTRATION_TOKEN" \
#     -api-url https://api.plusclouds.com \
#     -api-token "$REGISTRATION_TOKEN" \
#     -llm-url http://localhost:11434
#
# Env vars:
#   VERSION     - release tag to install, e.g. "v1.1.2" (default: latest)
#   INSTALL_DIR - where to place the binary (default: current directory)

set -euo pipefail

REPO="LLMOcean/box.agent"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-.}"
ASSET="box-agent-linux-amd64"

os="$(uname -s)"
arch="$(uname -m)"
if [ "$os" != "Linux" ] || [ "$arch" != "x86_64" ]; then
  echo "warning: only a linux/amd64 build is currently published; detected ${os}/${arch}" >&2
fi

if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${ASSET}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
fi

dest="${INSTALL_DIR%/}/box-agent"
echo "Downloading ${url} -> ${dest}" >&2
curl -fsSL "$url" -o "$dest"
chmod +x "$dest"

echo "Starting box-agent..." >&2
exec "$dest" "$@"
