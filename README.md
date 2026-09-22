<div id="top">

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:2786FF,50:6E4AFF,100:FF69B4&height=150&section=header" width="100%" alt="header banner">
</p>

<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp">
    <img src="./docs/assets/logo.png" width="150" alt="termcp logo">
  </a>
</p>

<h1 align="center">⚡ termcp</h1>

<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp">
    <img src="https://readme-typing-svg.demolab.com?font=JetBrains+Mono&weight=700&size=20&pause=900&color=2786FF&center=true&vCenter=true&width=860&height=45&lines=An+AI-native+terminal+platform;Cross-platform+%C2%B7+Visual+%C2%B7+Built+for+Human%E2%80%93Agent+Collaboration;One+port%2C+four+entrances;Drive+real+terminals+over+MCP+and+SKILLS" alt="termcp tagline">
  </a>
</p>

<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp/stargazers">
    <img src="https://img.shields.io/github/stars/open-mcp-ai/termcp?label=Stars&logo=github&style=for-the-badge&color=2786ff" alt="Stars">
  </a>
  <a href="https://github.com/open-mcp-ai/termcp/forks">
    <img src="https://img.shields.io/github/forks/open-mcp-ai/termcp?label=Forks&logo=github&style=for-the-badge&color=2786ff" alt="Forks">
  </a>
  <a href="https://github.com/open-mcp-ai/termcp/releases">
    <img src="https://img.shields.io/github/v/release/open-mcp-ai/termcp?label=Release&logo=github&style=for-the-badge&color=2786ff" alt="Release">
  </a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Platform-macOS%20%7C%20Linux%20%7C%20Windows-2786ff?style=for-the-badge" alt="Platform">
  <img src="https://img.shields.io/badge/Go-Pure%20Go%20%7C%20No%20CGO-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Pure Go, No CGO">
  <a href="./LICENSE">
    <img src="https://img.shields.io/badge/License-MIT-22c55e?style=for-the-badge&logo=opensourceinitiative&logoColor=white" alt="MIT License">
  </a>
</p>

<p align="center">
  <strong>English</strong> | <a href="./README.zh.md">中文</a>
</p>

<p align="center">
  <a href="#features"><img src="https://img.shields.io/badge/Features-2786ff?style=flat-square" alt="Features"></a>
  <a href="#quick-start"><img src="https://img.shields.io/badge/Quick%20Start-2786ff?style=flat-square" alt="Quick Start"></a>
  <a href="#usage"><img src="https://img.shields.io/badge/Usage-2786ff?style=flat-square" alt="Usage"></a>
  <a href="#docker-deployment"><img src="https://img.shields.io/badge/Docker-2786ff?style=flat-square" alt="Docker"></a>
  <a href="#connecting-ai-clients-mcp"><img src="https://img.shields.io/badge/MCP-6E4AFF?style=flat-square" alt="MCP"></a>
  <a href="#agent-skill-curl-only-no-mcp"><img src="https://img.shields.io/badge/Skill-6E4AFF?style=flat-square" alt="Skill"></a>
  <a href="#connecting-scripts--programs-rest-api"><img src="https://img.shields.io/badge/REST%20API-6E4AFF?style=flat-square" alt="REST API"></a>
  <a href="#tool-reference"><img src="https://img.shields.io/badge/Tools-00ADD8?style=flat-square" alt="Tools"></a>
  <a href="#known-limitations--security-model"><img src="https://img.shields.io/badge/Security-FF69B4?style=flat-square" alt="Security"></a>
</p>

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=rect&color=0:2786FF,100:FF69B4&height=3&section=header" width="100%" alt="divider">
</p>

## Introduction

`termcp` is an AI-native terminal platform: many hosts and many sessions at once, fully visualized. Those sessions are managed and maintained by humans and AI together, each side free to take over from or hand back to the other at any time; connection profiles are maintained independently by the platform, so an Agent can use a connection without ever reading its credentials.

- **You (human)** — a browser-based Web UI for live observation and instant takeover of any session;
- **AI Agents** — drive the same real terminals through **MCP** or **SKILLS**;
- **Scripts / programs** — a full REST API plus WebSocket channel for programmatic session, forward, and file operations.

On the platform layer, long-lived sessions with read-only replay, parallel multi-host / multi-session orchestration, and a full SSH connection lifecycle keep the whole loop **observable, programmable, and easy to hand off between human and AI**. Cross-platform and cloud-native, written in pure Go with no CGO: it ships as a single lightweight binary that runs persistently with low overhead, and goroutine concurrency keeps it high-throughput and low-latency.

### Demo Video

https://github.com/user-attachments/assets/d06a3c36-250a-4eeb-aefa-e80d13d1551c

## Why termcp

### Multi-session visual management

A powerful Web UI manages many hosts and many sessions in one place: start it locally with a single command or deploy the container to the cloud — the browser gets the same interface either way.

![pic2](docs/assets/pic2.png)

- **Multi-session dashboard**: every running session listed by name, switch or take over at any time.
- **Real-time observation**: watch `htop`'s live display, `vim`'s editing process, or an installer's prompts in the browser, just like a local terminal.
- **Tabs and tiling workspace**: one SSH session can open several shells, each its own tab; sessions can also be tiled side by side and tracked together.
- **Port forwarding at a glance**: local/remote ports and protocols for every session, all in one panel.
- **File management**: browse, upload, download, rename and create directories from the UI.
- **Centralized connection templates**: a unified SSH config store; the Agent opens sessions by profile name and never reads the config itself.
- **Read-only replay of closed sessions**: output stays browsable after a session closes, crashes, or survives a restart.

### AI-native by design

Seamless human–agent interaction and pair operation: the Agent is a standing user of the terminal, alongside you and your scripts.

An Agent natively runs only one-shot commands, while real work is largely **multi-turn interaction** — SSH login needs a password first, a Python REPL is debugged line by line, an installer asks `[Y/n]`, tools like `top`/`htop`/impacket need a terminal. `termcp` hands the Agent a real terminal: one session stays alive and gets reused, so **TUIs**, **REPLs**, **GDB**, **msfconsole** and **vim** can be driven continuously the way a human would — through MCP or through the instance's own [Agent Skill](#agent-skill-curl-only-no-mcp) over plain `curl`.

![pic1](docs/assets/pic1.png)

- **One session layer, peer entrances.** MCP, SKILLS and REST/WebSocket sit at the same level as the Web UI, sharing the same real sessions. You can watch every Agent step in the browser and take over at any time; the Agent in turn can pause and hand a password/MFA prompt to you.
- **Built for token and turn budgets.** Tool schemas are compact and can be deferred-loaded (see [`docs/mcp-tools.md`](./docs/mcp-tools.md)); `shell_output` pages by tail/offset cursors so only the slices you ask for ever enter the context window; `shell_notify` sends a bare wake-up signal.
- **Self-describing instances.** Every running termcp serves its own `/api.md` and `/skills.md` (no token needed) and registers them as MCP resources plus a `learn-api` prompt, so a fresh Agent can drive this exact instance straight away, using only these two files.
- **Secrets stay server-side.** Passwords, private keys and passphrases written through `ssh_config` are stored only on the host, and the MCP read interface returns profile names only; the SSH-config write tools stay off unless the operator opts in with `--mcp-manage-ssh-configs`.
- **Failure-tolerant, resumable work.** A closed, crashed or restarted session stays in the session list as a read-only DEAD tile with its output readable, so an Agent (or you) can pick up from the interrupted state; reconnecting the same `termcp://<entry>` starts a fresh session.
- **Humans always keep the option to step in.** `notify_user` reaches you directly, privileged prompts are meant to be typed by you in the Web UI, and writes to one shell are serialized, so a human and an Agent can type on the same terminal with their inputs applied in order.

## Quick Navigation

- [Features](#features)
- [Quick Start](#quick-start)
- [Usage](#usage)
- [Docker Deployment](#docker-deployment)
- [Connecting AI Clients (MCP)](#connecting-ai-clients-mcp)
- [Agent Skill (curl-only, no MCP)](#agent-skill-curl-only-no-mcp)
- [Connecting Scripts / Programs (REST API)](#connecting-scripts--programs-rest-api)
- [Examples](#examples)
- [Tool Reference](#tool-reference)
- [Known Limitations & Security Model](#known-limitations--security-model)

## Features

- **⚡ One-command install, pure Go, no CGO** — `go install github.com/open-mcp-ai/termcp@latest`; builds with `CGO_ENABLED=0` and binds no system shared libraries, so one static binary runs anywhere and cross-compiles natively (ConPTY on Windows, POSIX PTY on macOS / Linux — same behaviour everywhere).
- **🔌 One port, four entrances** — Web UI (humans), MCP / SKILLS (Agents), and REST + WebSocket (scripts) share one port.
- **🤝 Human–AI relay** — You and the Agent share one live session and you can take over or interrupt at any time; the Agent pauses at `sudo` / password / MFA prompts for you to type in the Web UI; input is serialized so keystrokes never collide.
- **🟦 Multi-turn interaction on a real terminal** — The process keeps running, so an Agent drives TUIs, REPLs, GDB, msfconsole, or vim across conversation turns; a full PTY (ConPTY on Windows) behaves the same on every platform.
- **🟫 Local or remote, one workflow** — Zero-config access to the termcp host (`ssh_config="internal"`) or any remote machine over SSH profiles; commands, file transfer (SFTP plus resumable HTTP URLs), and port forwarding (`-L` / `-R` / `-D`) all run over that single connection.
- **🟧 Built-in visual management** — Browser live terminals, session dashboard, tabbed shells, tiling workspace, read-only replay of closed sessions, file and forward panels; `/api.html` holds the API / MCP / SKILLS cheat sheet.
- **🟨 Multiple Agents, no lost output** — Parallel readers of one session keep independent cursors; a closed session (explicit close, exit, crash, or restart) stays in the registry as a read-only DEAD tile with its full output intact, so you can still replay, page through, or delete it whenever you like. After a drop, open a fresh session from the same entry (`termcp://<entry>`) and carry on.
- **🟥 Proactive notifications, no polling** — `shell_notify` wakes the Agent on process exit, silence, or new output — signal only, no payload (pull the text when needed); `channel="sampling"` sends `sampling/createMessage` directly.
- **🔒 Credential-safe by design** — Passwords, private keys, and passphrases written through `ssh_config` are never readable back, so plaintext never enters the Agent's context; config-writing tools stay off unless `--mcp-manage-ssh-configs` is set.

## Quick Start

### Quick Install (Go toolchain required)

The fastest way to install — one command, no clone, no build:

```bash
go install github.com/open-mcp-ai/termcp@latest
```

`go install` resolves the module through the Go proxy (use `GOPROXY=https://goproxy.cn,direct` in mainland China) and drops the `termcp` binary into `$(go env GOPATH)/bin` — make sure that directory is on your `PATH`. termcp is written in Go, so install is `go install` or a prebuilt Release binary: there is no `npx`/`uvx` variant, and it needs no Node or Python runtime. Being a Go module, it also supports source-level integration: `go get github.com/open-mcp-ai/termcp` to bring it in as a dependency, or fork and build a customized binary from source. Then run:

```bash
termcp
```

### Download

Head to the [Releases page](https://github.com/open-mcp-ai/termcp/releases) and download the pre-built binary for your platform:

| Platform            | File                       |
| :------------------ | :------------------------- |
| Linux (x86_64)      | [termcp-linux-amd64](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-linux-amd64) |
| Linux (ARM64)       | [termcp-linux-arm64](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-linux-arm64) |
| macOS (Intel)       | [termcp-darwin-amd64](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-darwin-amd64) |
| macOS (Apple Silicon) | [termcp-darwin-arm64](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-darwin-arm64) |
| Windows (x86_64)    | [termcp-windows-amd64.exe](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-windows-amd64.exe) |
| Windows (ARM64)     | [termcp-windows-arm64.exe](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-windows-arm64.exe) |

### Build

```bash
# Clone
git clone https://github.com/open-mcp-ai/termcp.git
cd termcp

# Build (pure Go — no CGO needed, cross-compiles to any platform)
CGO_ENABLED=0 go build -o termcp .

# Run (defaults: loopback, port 18765; data goes to ~/.termcp)
./termcp
```

Open `http://127.0.0.1:18765` in your browser to enter the **Web UI**.

## Usage

### Command Line

```text
termcp [flags]
```

| Flag            | Default       | Description                                                              |
| --------------- | ------------- | ------------------------------------------------------------------------ |
| `--host`        | `127.0.0.1`   | HTTP bind address. `0.0.0.0` listens on all interfaces. A non-loopback bind **requires** an auth token/hash (startup fails otherwise). |
| `--port`        | `18765`       | HTTP port. Shared by the Web UI, MCP SSE, MCP streamable HTTP, and the docs/skill endpoints (`/api.md`, `/skills.md`). |
| `--data-dir`    | `~/.termcp`   | Persistence directory (sessions, SSH configs). Auto-created. Default overridable via `$TERMCP_DATA_DIR`. |
| `--log-level`   | `info`        | Log level: `debug` / `info` / `warn` / `error`. `debug` shows all MCP tool calls; failed tool calls and session-create errors log at `warn`/`error` regardless. |
| `--no-internal` | `false`       | Disable the built-in loopback SSH profile.                                   |
| `--mcp-manage-ssh-configs` | `false` | Enable MCP tools to create/edit/delete SSH configs (secrets are never exposed). |
| `--auth-token`   | *(unset)*    | Static token for HTTP authentication (or `$TERMCP_AUTH_TOKEN`). Every client — API, MCP, browser — must present it. Mutually exclusive with `--auth-hash`. |
| `--auth-hash`    | *(unset)*    | Salted SHA-256 hash of the token (`sha256-<salt_hex>-<digest_hex>`) so the server never holds the plaintext (or `$TERMCP_AUTH_HASH`). Generate with `termcp --gen-auth-hash`. Mutually exclusive with `--auth-token`. |
| `--disable-auth` | `false`      | Turn HTTP authentication off **on purpose**, including on a non-loopback bind (or `$TERMCP_DISABLE_AUTH_TOKEN=1`). Pair it with a loopback port so only local callers can reach the port. Combining it with `--auth-token`/`--auth-hash` is an error rather than a silently-won argument. |
| `--mcp-defer-tools` | `false`   | Tag low-frequency MCP tools (`file_*`, `forward`, `shell_resize`, …) with `defer_loading` so clients fetch their schemas on demand, shrinking the initial `tools/list`. Off by default: clients that ignore the marker — or talk to termcp through a gateway that drops it — would otherwise never see those tools. See [Deferred tool loading](#deferred-tool-loading). |
| `--gen-auth-hash` | *(action)* | Generate the salted SHA-256 hash of a token for `--auth-hash`, then exit (token from an argument, or from stdin without echo on a terminal). |
| `--version`      | *(action)*   | Print version, commit, and build date, then exit. The version follows the git tag automatically (release builds inject it via `-ldflags`; plain `go build` / `go install module@vX.Y.Z` falls back to the module version embedded by the Go toolchain). |

These flags are your **capability gates**: `--no-internal` narrows Agents to remote hosts only, and `--mcp-manage-ssh-configs` is what opens SSH-config write access. Tighten or loosen what Agents can touch per scenario. See [Authentication](#authentication) below.

### Examples

```bash
# Listen on all interfaces
./termcp --host 0.0.0.0 --auth-token "your-long-random-token"

# Listen on all interfaces with only a salted hash stored server-side
./termcp --host 0.0.0.0 --auth-hash "$(./termcp --gen-auth-hash)"

# Allow AI agents to manage SSH configs
./termcp --mcp-manage-ssh-configs

# Disable the built-in loopback profile (agents may only reach remote hosts)
./termcp --no-internal
```

### Authentication

A single static token protects the whole HTTP surface — the Web UI, REST API, MCP SSE, MCP streamable HTTP, and the browser WebSocket. (The read-only docs `/api.md` and `/skills.md` stay public, so an agent can fetch them before it has a token.) Configuring it is optional for loopback-only binds (`127.0.0.1` keeps its no-setup default); exposing a non-loopback bind without a token is a startup error.

```bash
# Plaintext: flag or env var
./termcp --auth-token "your-long-random-token"
TERMCP_AUTH_TOKEN="your-long-random-token" ./termcp

# Hashed (recommended): the server keeps only sha256-<salt>-<digest>.
# `termcp --gen-auth-hash` reads the token from stdin without echo on a terminal,
# so it never lands in shell history:
./termcp --gen-auth-hash
TERMCP_AUTH_HASH='sha256-...' ./termcp
```

How each client presents the token:

| Client | Credential |
|--------|------------|
| API / MCP / curl | `Authorization: Bearer <token>` header |
| Browser (Web UI) | Native login prompt on `401` — the username is ignored (leave it empty), the **token is the password**. A `termcp_token` cookie is then set automatically so same-origin WebSocket handshakes authenticate too. |

Behavior notes:

- `--auth-token` and `--auth-hash` are mutually exclusive; a flag value overrides the environment variable of the same setting.
- A colon inside the token is fine: the server also accepts the whole decoded `user:pass` string when it equals the token, so clients that split at the first colon (e.g. `curl -u user:pass`) still authenticate. `curl -u :<token>` remains the canonical form.
- Without a token or hash, startup fails on any non-loopback host (`0.0.0.0`, a LAN IP, or a hostname other than `localhost`), so an accidentally exposed instance can never run unauthenticated.
- `--disable-auth` (or `TERMCP_DISABLE_AUTH_TOKEN=1`) explicitly lifts that requirement. It is the escape hatch for loopback-only setups — demo videos, screen recordings, single-user workstations — where the token protects nothing. Because it is a deliberate override, combining it with `--auth-token`/`--auth-hash` is a startup error rather than a silently-won argument, and the startup log switches from the informational auth line to a warning.
- Browsers use HTTP Basic, which is Base64, not encryption. When serving termcp beyond your own machine, terminate TLS in a reverse proxy in front of it — the `termcp_token` cookie then gets the `Secure` flag automatically only when the request arrived over TLS.

### Connecting to Remote Hosts

Zero setup: `ssh_config="internal"` drives the termcp host itself. To reach a remote machine, create an SSH profile — in the Web UI's new-connection dialog (it ships a TOML template and a **Test connection** button), or via the REST API `PUT /api/connections/<name>` with a TOML body:

```toml
kind = "remote"
host = "192.168.1.100"
user = "pi"
trust_unknown_host = true  # first connect to an unknown host

# EITHER a password:
password = "..."

# OR the private key's PEM content itself — a path like "~/.ssh/id_ed25519" will NOT work:
private_key = """-----BEGIN OPENSSH PRIVATE KEY-----
<paste the full content of ~/.ssh/id_ed25519>
-----END OPENSSH PRIVATE KEY-----"""
key_passphrase = "..."     # only if the key is passphrase-protected

# Optional bastion (ProxyJump) hop:
[jump]
host = "bastion.example.com"
user = "ops"
password = "..."
```

Profiles live in `data-dir/ssh_configs/<name>/config.toml`; list them with `ssh_config(action=list)`. Credentials written this way are never readable back. Agents can create profiles too, but only when termcp was started with `--mcp-manage-ssh-configs`.

## Docker Deployment

### Run the official image

The registry image runs as non-root `termcp` (uid/gid 1000) with `/home/termcp` declared a `VOLUME` — all state (sessions, SSH configs, transcripts) defaults to `~/.termcp`. It carries only the binary: no baked-in entrypoint or exposed port, so the run command decides the bind address.

```bash
docker run -d --name termcp -p 18765:18765 -v termcp-data:/home/termcp -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765
```

> Shell examples are single-line on purpose: a `\` continuation is valid bash but a syntax error in PowerShell, so every command pastes as-is into bash, zsh, and PowerShell.

`--host 0.0.0.0` is reachable from outside the container, so an auth token is required. MCP endpoint: `http://localhost:18765/stream`. With a bind mount instead of a named volume, chown the host directory first: `chown -R 1000:1000 /path/on/host`.

#### Docker without a token (loopback only)

For a throwaway demo, a screen recording, or a single-user workstation, the token is friction with no benefit. Publish the port on the **host loopback only** and tell termcp explicitly that the missing credentials are intentional:

```bash
docker run -d --name termcp -p 127.0.0.1:18765:18765 -v termcp-data:/home/termcp ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765 --disable-auth
```

Two details make this safe rather than merely convenient. `-p 127.0.0.1:18765:18765` binds the published port to the host's loopback, so the container stays reachable to this machine and invisible to the LAN — the container itself still listens on `0.0.0.0` because that is the only address routable from outside its network namespace. And `--disable-auth` is required precisely because termcp refuses to start unauthenticated on a non-loopback bind: the flag is the operator taking responsibility, which is why it also downgrades the startup log to a warning. The equivalent environment form is `-e TERMCP_DISABLE_AUTH_TOKEN=1` instead of the flag.

### Multi-stage build: add termcp to any container

Drop this `Dockerfile` into your application project: the build stage installs termcp with `go install`, then `COPY --from` copies the binary into the target image — no Go runtime needed there.

```dockerfile
# syntax=docker/dockerfile:1

ARG GO_IMAGE=golang:1.25-alpine
FROM ${GO_IMAGE} AS termcp-build

# Module proxy; use https://proxy.golang.org,direct outside China
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} GOBIN=/out CGO_ENABLED=0

# Pin to a concrete version in production, e.g. @vX.Y.Z
RUN go install github.com/open-mcp-ai/termcp@latest

# Any target base image
FROM alpine
COPY --from=termcp-build /out/termcp /usr/local/bin/termcp
```

Swap `GOPROXY` or `GO_IMAGE` with `--build-arg` if you need another module proxy or base-image mirror.

### Startup command examples

Containers must bind `0.0.0.0`, and a non-loopback bind **requires authentication** (`TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH`) or startup fails.

```bash
docker build --build-arg GOPROXY=https://goproxy.cn,direct -t my-app-with-termcp .
docker run -d --name my-app-termcp -p 18765:18765 -v termcp-data:/data -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret --entrypoint /usr/local/bin/termcp my-app-with-termcp --host 0.0.0.0 --port 18765 --data-dir /data
docker logs -f my-app-termcp
```

Append `--mcp-manage-ssh-configs` to open the SSH-config write tools to Agents.

If termcp must share a container with another main process, start it from the existing entrypoint or process manager; otherwise run it as a separate service and reach it at `http://termcp:18765/stream`.

### Docker Compose startup

```yaml
services:
  termcp:
    build: .
    entrypoint: ["/usr/local/bin/termcp"]
    command: ["--host", "0.0.0.0", "--port", "18765", "--data-dir", "/data"]
    environment:
      - TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret
    ports:
      - "18765:18765"
    volumes:
      - termcp-data:/data

volumes:
  termcp-data:
```

```bash
docker compose up -d --build
```

## Connecting AI Clients (MCP)

termcp speaks **both MCP transports** on the same port (18765). Choose whichever your client supports — the tool surface is identical.

termcp is a long-running service: the same port serves the Web UI, any number of MCP clients, and session persistence. It therefore offers **HTTP transports only** — Streamable HTTP and SSE — and does **not** support stdio (there is no local subprocess mode).

Alternative: the [Agent Skill](#agent-skill-curl-only-no-mcp) drives the same sessions over plain `curl` — the instance serves it at `/skills.md`. The MCP server is one interface layer of the platform, embeddable into any MCP-capable host — Claude Code, Cursor, Codex, Open WebUI, or your own client.

### Option A — Streamable HTTP (`/stream`)

The modern MCP transport; a single endpoint, no separate message path. Use this for Claude Code, Open WebUI, and most current clients.

```json
{
  "mcpServers": {
    "termcp": {
      "type": "http",
      "url": "http://your-server:18765/stream"
    }
  }
}
```

```bash
claude mcp add --transport http termcp http://localhost:18765/stream
```

- Same machine: `http://127.0.0.1:18765/stream`.
- Open WebUI in Docker, termcp on the host: `http://host.docker.internal:18765/stream` (macOS/Windows), or the host's LAN IP.
- Both in Docker on the same network (see [Docker Deployment](#docker-deployment)): `http://termcp:18765/stream`.

### Option B — SSE (`/sse`)

The legacy SSE transport. Configure **only** `/sse`; the SDK posts JSON-RPC to `/message` automatically.

```json
{
  "mcpServers": {
    "termcp": {
      "type": "sse",
      "url": "http://your-server:18765/sse"
    }
  }
}
```

```bash
claude mcp add --transport sse termcp http://localhost:18765/sse
```

### Cheat sheet

- Streamable HTTP → `http://<host>:18765/stream`
- SSE → `http://<host>:18765/sse` (JSON-RPC goes to `POST /message`)

The Web UI's **API / MCP / SKILLS** page (`/api.html`) offers copy-ready config for both transports, plus the Agent-docs and skill-download addresses for this instance.

## Agent Skill (curl-only, no MCP)

Don't want to configure an MCP client? The instance ships an installable
**Agent Skill** that teaches any agent to drive termcp with `curl` alone —
including the `termcp://` locators users paste from the Web UI.

```bash
# Public endpoint: no token needed for the download itself
curl -fsS http://<host>:18765/skills.md -o /tmp/termcp-SKILL.md

# Claude Code reads ~/.claude/skills/<name>/SKILL.md
mkdir -p ~/.claude/skills/termcp && cp /tmp/termcp-SKILL.md ~/.claude/skills/termcp/SKILL.md

# Other agents that follow the shared convention read ~/.agents/skills/<name>/SKILL.md
mkdir -p ~/.agents/skills/termcp && cp /tmp/termcp-SKILL.md ~/.agents/skills/termcp/SKILL.md
```

Restart the agent session after installing (skills are loaded at session start).
Claude Code has no per-skill CLI command — adding is "drop the file in", removing
is `rm -rf ~/.claude/skills/termcp` (or `claude plugin install/uninstall` when the
skill ships as a plugin).

Once installed, a request as simple as *"open termcp://rock64 and run `uname -a`"*
works end to end: the skill resolves the locator via
`GET /api/resolve?url=...`, creates the session with that `ssh_config`, sends the
command, and polls the output. The same skill is registered as the MCP resource
`<origin>/skills.md`, and `/api.html` shows the exact install command for the
instance you are looking at.

## Connecting Scripts / Programs (REST API)

Skip MCP and use the same session layer programmatically: the full REST API and live WebSocket channel.

```bash
# List sessions (same --auth-token protection)
curl -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" http://127.0.0.1:18765/api/sessions

# Create a session
curl -X POST http://127.0.0.1:18765/api/sessions -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" -H 'Content-Type: application/json' -d '{"ssh_config":"internal","command":"bash","mode":"pty"}'

# Read output / upload files / port forwards — see docs/api.md
```

Live terminal I/O runs over `WebSocket /api/ui/ws`; files support direct HTTP URLs with Range resume. Full endpoint list in [`docs/api.md`](./docs/api.md).

### With authentication enabled

When the server runs with `--auth-token`/`--auth-hash`, every MCP request needs the token as an `Authorization: Bearer` header:

```bash
claude mcp add --transport http termcp http://your-server:18765/stream --header "Authorization: Bearer $TERMCP_AUTH_TOKEN"
```

```json
{
  "mcpServers": {
    "termcp": {
      "type": "http",
      "url": "http://your-server:18765/stream",
      "headers": { "Authorization": "Bearer <your-token>" }
    }
  }
}
```

Keep the token out of URLs and out of shared configs/screenshots. `curl` and scripts use the same header:

```bash
curl -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" http://your-server:18765/api/sessions
```

## Tool Reference

termcp exposes 31 MCP tools. Full parameters, return shapes, and error codes live in [`docs/mcp-tools.md`](./docs/mcp-tools.md).

| Area | Tools |
|------|-------|
| Sessions (connection containers) | `session_start`, `session_list`, `session_info`, `session_terminate` (close; keeps the DEAD entry readable), `session_delete` (permanent) |
| Shells (terminal channels) | `shell_open`, `shell_list`, `shell_close`, `shell_input`, `shell_key`, `shell_output`, `shell_resize`, `shell_reader_register`, `shell_reader_unregister` |
| Notifications | `shell_notify` (wakes the AI Agent), `notify_user` (toasts the human at the Web UI) |
| SSH profiles | `ssh_config` (`list`; `create`/`edit`/`copy`/`delete` with `--mcp-manage-ssh-configs`) |
| Port forwarding | `forward` (`-L` / `-R` / `-D` / list / close) |
| Files (SFTP) | `file_read`, `file_write`, `file_stat`, `file_delete`, `file_rename`, `file_mkdir`, `file_urls`, `file_perm`, `file_link`, `file_fs`, `file_getwd` |
| Transcript index | `message` (span list; bytes via `shell_output`) |
| Host discovery | `shell_detect` |

Run a command as `shell_input` + `shell_key(key="enter")` + `shell_output`. Failed tools return `isError=true` with a JSON body carrying a stable `error_code`.

## Deferred tool loading

An MCP client fetches every tool's JSON schema in `tools/list`, so tool-heavy servers pay for that in context budget. The MCP spec offers an escape hatch: mark low-frequency tools with `defer_loading`, and a client loads their schema on demand. termcp's 31 tools split into a hot path of **12** (session lifecycle + shell input/output — always listed) and **19** wide, low-frequency surfaces (the 11 SFTP `file_*` tools, `forward`, `shell_resize`/`shell_detect`/`shell_notify`, `shell_reader_register`/`shell_reader_unregister`, `message`, `ssh_config`).

`--mcp-defer-tools` turns the marker on and is **off by default**, so:

- **Default** — all 31 tools are listed eagerly with full schema. This is what every client that does not implement deferred loading needs — including any Codex that talks to termcp through a gateway such as AxonHub, which can drop the `defer_loading` marker. With the marker lost, those tools are not reloadable on demand and would simply vanish from the model's view.
- **`--mcp-defer-tools`** — the 19 low-frequency tools carry `defer_loading`; the 12 core tools stay eager so the `session_start → shell_input → shell_output` loop never requires a search round trip. Clients that support on-demand loading (mcp-go based clients, Claude Code) pay only for the schemas they actually use.

Same 31 tools either way: enabling the flag never removes tools, it only withholds schemas from the initial listing.

## Known Limitations & Security Model

- **File and forward tools need a live connection.** On a closed (DEAD) session they return `session_not_running`; output reading still works via `shell_output`, and a session's port forwards are closed automatically when it goes DEAD.
- **Basic authentication needs TLS outside localhost.** The browser login challenge uses HTTP Basic, whose credentials are only Base64-encoded. Put a TLS-terminating reverse proxy in front of termcp when exposing it beyond a trusted local network; the static token is still never logged or placed in a URL.

### 🚨 Security boundary: termcp does not enforce security (it is a pipe, not an antivirus)

> **The defence line belongs at the AI's output side and your gateway — not in the terminal pipe. termcp is NOT an antivirus, EDR, or WAF.**

termcp is a **transparent real-terminal and multiplexed-session pipe** (PTY transport) with the same freedom and power as the machine's own terminal. It therefore **cannot and should not judge the intent of what it carries**:

1. **Why a terminal pipe cannot detect malicious intent.**
   - **Upload-and-execute cannot be stopped here.** Malicious content arrives Base64-decoded through a pipe, written in fragments, or fetched by legitimate tools (`curl` / `wget`) in multiple stages and then chmod'ed and run. termcp is a **data pipe, not a malware scanner**: inspecting every streaming byte for a trojan is simply not something a byte transport can do.
   - **Obfuscation and concatenation are undecidable at the byte layer.** An AI can split a dangerous command into string fragments (`a="rm -"; b="rf /"; $a$b`), rebuild it through variable renaming, dynamic `eval`, `printf` injection, environment-variable stitching, or by writing several partial files and executing them. To the PTY every character is a legal keystroke; the transport cannot tell "obfuscated payload" from "ordinary development script".
2. **Security must be enforced upstream.**
   - **The caller (host application / Agent harness) must guard the AI's output before the tool call.** Wrap `shell_input` / `file_write` with output guardrails, an instruction-compliance policy layer, sensitive-content filters, or a safety model that inspects the generated command **before** it reaches termcp. termcp does not enforce command allowlists, path jails, or policy-based risk tiers.
   - **Keep humans in the loop for privileged or destructive steps.** The Web UI shows every session live and lets you take over or interrupt at any time. Treat `sudo`, destructive, or irreversible commands as human-approval events — and never hand unattended high-privilege terminal access to a production host that is not sandboxed.

---

## Star History

<p align="center">
  <a href="https://star-history.com/#open-mcp-ai/termcp&Date">
    <img src="https://api.star-history.com/svg?repos=open-mcp-ai/termcp&type=Date" alt="Star History Chart" width="760">
  </a>
</p>

## License

Released under the [MIT License](./LICENSE). You are free to use, modify, and distribute it, provided the copyright notice and permission notice are retained. Thanks to the [linux.do](https://linux.do/) community for the discussions and support.

---




<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:2786FF,100:6E4AFF&height=110&section=footer" width="100%" alt="footer">
</p>
