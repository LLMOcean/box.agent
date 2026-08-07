# Installing box.agent on a remote machine

This guide walks through installing box.agent as a long-running service on a
remote GPU box (bare metal, VM, cloud instance) that's already running a
local OpenAI-compatible LLM server. For the control-panel walkthrough
(getting a token, running in the foreground, troubleshooting) see
[`USAGE.md`](USAGE.md). This doc focuses on the ops side: getting the binary
onto the box and keeping it running unattended.

box.agent only needs **outbound** access to the router — no inbound ports,
no public IP, and it works fine behind NAT.

```
Router  <--WebSocket tunnel--  box-agent  --HTTP-->  local LLM server
```

## 1. Prerequisites

- A Linux, macOS, or Windows host (release binaries are published for
  `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, and
  `windows/amd64` — on any other OS/arch the install scripts fall back to
  building from source, which needs `git` and `go` on `PATH`; see
  [§8](#8-other-platforms)).
- A local OpenAI-compatible LLM server already running and reachable from
  this host (Ollama, vLLM, llama.cpp-server, text-generation-inference),
  with a model loaded.
- Outbound HTTPS/WSS access from this host to the router and management API
  endpoints.
- A **registration token** and **provider name** from the control panel
  (see [`USAGE.md`](USAGE.md#1-prerequisites)).

## 2. Quick install (recommended)

[`deploy/install.sh`](../deploy/install.sh) (Linux and macOS — it detects
which) and [`deploy/install.ps1`](../deploy/install.ps1) (Windows) each do
the full job in one command: download the binary (or build it from source
if no release binary is published for your OS/arch), install it so it
survives reboots/crashes, and start it. Skip straight to
[§7](#7-upgrading) once you've run one of these — §3 through §5 below
describe what they automate, in case you want to do it by hand or
customize it.

`-router`, `-api-url`, and `-llm-url` all default to the platform already
(`llm.greenference.com`/`api.greenference.com`), and `-token` doubles as
`-api-token` unless given separately — so `-provider`/`-token` are normally
the only flags either script needs.

**Linux** (installs a systemd service; run as root):

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.sh \
  | sudo bash -s -- \
      -provider yourns/your-model \
      -token "$REGISTRATION_TOKEN"
```

**macOS** (installs a launchd daemon; run as root — it's the same script
as Linux, the OS is auto-detected):

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.sh \
  | sudo bash -s -- \
      -provider yourns/your-model \
      -token "$REGISTRATION_TOKEN"
```

**Windows** (installs a Scheduled Task running as SYSTEM; run from an
elevated PowerShell prompt):

```powershell
iex "& { $(irm https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install.ps1) } -Provider yourns/your-model -Token $env:REGISTRATION_TOKEN"
```

Pass `-router`/`-llm-url` (Linux/macOS) or `-Router`/`-LlmUrl` (Windows) to
point at a self-hosted router or local LLM server instead — see
[§4](#4-run-it-as-a-systemd-service) for a fully-explicit example.

`install.ps1` tries a Windows release asset first and, since none is
published today, falls back to building from source — which needs `git`
and `go` on `PATH`. `install.sh` does the same on macOS/non-amd64 Linux.
See [§8](#8-other-platforms) for details.

Every flag both scripts accept mirrors box-agent's own `-flag` names
(`-backend-model`, `-context-length`, etc. for `install.sh`; `-BackendModel`,
`-ContextLength`, etc. for `install.ps1`) — run `install.sh -h` or
`Get-Help .\install.ps1 -Full` to see them all. `install.sh` flags can also
be set via identically-named environment variables (`PROVIDER=...
TOKEN=...`) if you'd rather not pass a long argument list.

## 3. Download the binary manually

If you'd rather not use the installer, the same one-line download+run
(without installing a persistent service) is available via
[`deploy/install-and-run.sh`](../deploy/install-and-run.sh):

```bash
curl -fsSL https://raw.githubusercontent.com/LLMOcean/box.agent/main/deploy/install-and-run.sh | bash -s -- \
  -router wss://router.example.com \
  -provider yourns/your-model \
  -token "$REGISTRATION_TOKEN" \
  -api-url https://api.example.com \
  -api-token "$REGISTRATION_TOKEN" \
  -llm-url http://localhost:11434
```

- Set `VERSION=v1.0.1` (env var, before the pipe) to pin a specific release
  instead of `latest`.
- Set `INSTALL_DIR=/usr/local/bin` to place the binary somewhere other than
  the current directory.

For an unattended deployment (recommended — see [§4](#4-run-it-as-a-systemd-service)
(Linux) or [§5](#5-run-it-as-a-launchd-daemon-macos) (macOS) below),
download the binary without immediately running it. On Linux:

```bash
curl -fsSL -o /usr/local/bin/box-agent \
  https://github.com/LLMOcean/box.agent/releases/latest/download/box-agent-linux-amd64
chmod +x /usr/local/bin/box-agent
```

No macOS release binary is published today, so on macOS build from source
instead (requires Go 1.18+ — `brew install go` if you don't have it):

```bash
git clone https://github.com/LLMOcean/box.agent.git
cd box.agent
go build -o /usr/local/bin/box-agent .
```

## 4. Run it as a systemd service

Running box-agent directly in a terminal only lasts until you disconnect.
For a real deployment, install it as a systemd service so it survives
reboots and restarts on crash.

Create a dedicated, unprivileged user:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin box-agent
```

Copy and edit the unit file
([`deploy/systemd/box-agent.service`](../deploy/systemd/box-agent.service)):

```bash
sudo cp deploy/systemd/box-agent.service /etc/systemd/system/box-agent.service
sudo systemctl edit --full box-agent.service   # or edit the file directly
```

Fill in your real values:

```ini
[Unit]
Description=router.agent remote WebSocket agent (box.agent)
After=network.target

[Service]
ExecStart=/usr/local/bin/box-agent \
  -router wss://router.example.com \
  -provider yourns/your-model \
  -api-url https://api.example.com \
  -llm-url http://localhost:11434
Environment=AGENT_TOKEN=your-registration-token
Environment=API_TOKEN=your-registration-token
Restart=always
RestartSec=5
User=box-agent

[Install]
WantedBy=multi-user.target
```

Using `Environment=` for the tokens (rather than `-token`/`-api-token` flags)
keeps them out of `ps aux` output and shell history. Prefer an
`EnvironmentFile=/etc/box-agent.env` (mode `600`, owned by `box-agent`) over
inline `Environment=` lines if you'd rather not put secrets in a
world-readable unit file — systemd unit files under `/etc/systemd/system`
are typically root-readable only anyway, but this keeps the secret in one
place if you manage several units.

Enable and start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now box-agent.service
```

Check it came up cleanly:

```bash
sudo systemctl status box-agent.service
sudo journalctl -u box-agent.service -f
```

You're looking for the same two log lines described in
[`USAGE.md`](USAGE.md#3-run-it):

```
registered with API: model=your-model provider=yourns
connected: wss://router.example.com/v1/agents/connect?provider=yourns%2Fyour-model
```

## 5. Run it as a launchd daemon (macOS)

macOS has no systemd; the equivalent for a service that starts at boot and
restarts on crash is a `launchd` **LaunchDaemon**. Create
`/Library/LaunchDaemons/com.llmocean.box-agent.plist` (must be owned by
root, and root-only readable since it holds the tokens):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.llmocean.box-agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/box-agent</string>
    <string>-router</string>
    <string>wss://router.example.com</string>
    <string>-provider</string>
    <string>yourns/your-model</string>
    <string>-api-url</string>
    <string>https://api.example.com</string>
    <string>-llm-url</string>
    <string>http://localhost:11434</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>AGENT_TOKEN</key>
    <string>your-registration-token</string>
    <key>API_TOKEN</key>
    <string>your-registration-token</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/var/log/box-agent.log</string>
  <key>StandardErrorPath</key>
  <string>/var/log/box-agent.log</string>
</dict>
</plist>
```

Lock it down and load it:

```bash
sudo chown root:wheel /Library/LaunchDaemons/com.llmocean.box-agent.plist
sudo chmod 600 /Library/LaunchDaemons/com.llmocean.box-agent.plist
sudo launchctl load -w /Library/LaunchDaemons/com.llmocean.box-agent.plist
```

Check it came up cleanly:

```bash
sudo launchctl list | grep com.llmocean.box-agent
tail -f /var/log/box-agent.log
```

You're looking for the same two log lines described in
[`USAGE.md`](USAGE.md#3-run-it) (registered with API, then connected).

This LaunchDaemon runs as root (there's no `UserName` key above) — unlike
the dedicated unprivileged `box-agent` user the systemd path creates,
since scripting a new macOS system user via `dscl` is enough extra
ceremony that it's not done here. Add a `UserName` key yourself if you'd
rather run as a lower-privilege account you've already created.

To stop or remove it:

```bash
sudo launchctl unload -w /Library/LaunchDaemons/com.llmocean.box-agent.plist
```

## 6. Run it with Docker instead

Only worth it if your local LLM server is also containerized on the same
host/network — otherwise the systemd route above is simpler, since
box-agent still needs to reach the LLM server's HTTP port either way.

```bash
docker build -t box-agent .
docker run -d --name box-agent --restart unless-stopped \
  --network host \
  -e AGENT_TOKEN="$REGISTRATION_TOKEN" \
  -e API_TOKEN="$REGISTRATION_TOKEN" \
  box-agent \
  -router wss://router.example.com \
  -provider yourns/your-model \
  -api-url https://api.example.com \
  -llm-url http://localhost:11434
```

`--network host` is the easiest way to let the container reach an LLM server
running directly on the host (e.g. Ollama on `localhost:11434`); use a
shared user-defined bridge network instead if the LLM server is also in a
container.

## 7. Upgrading

**Linux (systemd)** — stop the service, replace the binary, restart:

```bash
sudo systemctl stop box-agent.service
curl -fsSL -o /usr/local/bin/box-agent \
  https://github.com/LLMOcean/box.agent/releases/latest/download/box-agent-linux-amd64
chmod +x /usr/local/bin/box-agent
sudo systemctl start box-agent.service
```

Or just re-run [`deploy/install.sh`](../deploy/install.sh) — it overwrites
the binary and rewrites the unit file in place.

**macOS (launchd)** — re-run [`deploy/install.sh`](../deploy/install.sh);
it overwrites the binary and the plist and reloads it. Or by hand: `sudo
launchctl unload -w ...plist`, rebuild/redownload the binary, `sudo
launchctl load -w ...plist`.

**Windows (Scheduled Task)** — re-run
[`deploy/install.ps1`](../deploy/install.ps1) with the same parameters; it
replaces the existing task and binary.

Pin a version (`-version`/`VERSION` for `install.sh`, `-Version` for
`install.ps1`) if you want to control exactly when you move to a new
release rather than always tracking `latest` — see
[§3](#3-download-the-binary-manually).

## 8. Other platforms

Release binaries are published for `linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`, and `windows/amd64`. For anything else,
[`deploy/install.sh`](../deploy/install.sh) and
[`deploy/install.ps1`](../deploy/install.ps1) both handle it automatically
by falling back to a source build (see
[§2](#2-quick-install-recommended)). Or build from source by hand
yourself — it's a single static Go binary with one dependency
([`gorilla/websocket`](https://github.com/gorilla/websocket)):

```bash
git clone https://github.com/LLMOcean/box.agent.git
cd box.agent
GOOS=<target-os> GOARCH=<target-arch> go build -o box-agent .
```

## 9. Verifying and troubleshooting

See [`USAGE.md` §5](USAGE.md#5-verify-youre-connected) (checking the
router's `/v1/agents/status` endpoint) and
[`USAGE.md` §6](USAGE.md#6-troubleshooting) (common startup/connection
errors) — both apply the same way whether box-agent is running under
systemd, launchd, a Windows Scheduled Task, Docker, or in a foreground
shell.
