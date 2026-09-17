<div id="top">



<p align="center">
    <img src="./docs/assets/logo.png"></img>
  <h1 align="center">termcp</h1>
  <p align="center"><em>A cross-platform terminal session platform — local & remote hosts, one session layer for humans, Agents, and scripts.</em></p>
</p>



<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp/stargazers">
    <img src="https://img.shields.io/github/stars/open-mcp-ai/termcp?label=Stars&logo=github&style=for-the-badge" alt="Stars">
  </a>
  <a href="https://github.com/open-mcp-ai/termcp/forks">
    <img src="https://img.shields.io/github/forks/open-mcp-ai/termcp?label=Forks&logo=github&style=for-the-badge" alt="Forks">
  </a>
  <img src="https://img.shields.io/badge/Platform-macOS%20%7C%20Linux%20%7C%20Windows-2786ff?style=for-the-badge" alt="Platform">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go 1.25+">
  <a href="./LICENSE">
    <img src="https://img.shields.io/badge/license-MIT-green?style=for-the-badge" alt="MIT License">
  </a>
</p>



<p align="center">
  <strong>English</strong> | <a href="./README.zh.md">中文</a>
</p>



---

## Introduction

`termcp` is a **cross-platform terminal session platform** written in Go. It treats the **terminal session** as its unifying primitive and connects two classes of machines: **the termcp host itself** (built-in loopback profile, zero config) and **any remote host** (SSH profiles with password / key / jump-host support). Every session is a real PTY channel — hosting multiple shell tabs, port forwards, and SFTP file transfer — opened simultaneously to three kinds of users:

- **You (human)** — a browser-based Web UI for live observation and instant takeover of any session;
- **AI Agents** — a built-in MCP server (Streamable HTTP and SSE on the same port) driving the same real terminals;
- **Scripts / programs** — a full REST API plus WebSocket channel for programmatic session, forward, and file operations.

termcp is not "an MCP tool": MCP is just one **interface layer** exposing its AI-control capabilities. The platform itself is a complete terminal service — suspendable/archivable sessions, parallel multi-session orchestration, a closed-loop SSH connection lifecycle — with a browser terminal, history replay, and a human-in-the-loop control model, forming an **observable, programmable, human-and-AI handoff** terminal platform.

Written in Go, it ships as a single lightweight binary that runs persistently with low overhead; compiled Go and goroutine concurrency keep it high-throughput and low-latency.

### Demo Video

https://github.com/user-attachments/assets/d06a3c36-250a-4eeb-aefa-e80d13d1551c

## Why termcp

### One platform, three entrances, one session layer

| Entrance | For | Form |
|----------|-----|------|
| **Web UI** (built-in) | Humans | Browser live terminals, session dashboard, tabs, history replay, file/forward panels |
| **MCP server** (built-in) | AI Agents | Sessions as persistent connections; Agents manage/drive interactive programs across turns |
| **REST API + WebSocket** (built-in) | Scripts | Programmatic session creation, terminal I/O, port forwarding, SFTP file operations |

### Breaking the Boundary

Agents can natively only execute one-shot commands — they run and return. But a huge amount of real-world work is **multi-turn interaction**, for example:

- SSH into a host: enter a password first, _then_ run commands.
- Debug code line by line in a Python REPL.
- Answer a `[Y/n]` prompt buried deep inside an installer.
- Drive terminal-dependent tools like `top`, `htop`, or impacket.

In these scenarios the process keeps running, and the Agent must **read and write the process's I/O across multiple conversation turns**. Plenty of specialized MCPs have sprung up to handle these — but why not just give the Agent hands so it can interact directly? `termcp` breaks that boundary for AI Agents: no more writing or installing a separate MCP for every interactive tool. The Agent can directly and continuously manage and drive interactive programs like **TUIs**, **REPLs**, **GDB**, **msfconsole**, **vim**, and more.

### Visual Management

`termcp` provides a session management UI that gives you and the Agent a clear view of everything happening inside the processes:

- **Multi-session dashboard**: every running session lives here, distinguished by name — switch between them or take over at any time.
- **Real-time Agent behavior observation**: just like a local terminal, watch `htop`'s live display, `vim`'s editing process, or an installer's colorful prompts right in the browser — no more guessing at a "black box".
- **Tab-based management**: under a single SSH session you can open multiple operating shells, each rendered as an independent tab in the UI. The Agent can debug in tab A and tail logs in tab B without interference.
- **Port forwarding at a glance**: every port-forwarding rule tied to a session is listed in the panel — local/remote ports and protocols, all visible at a glance.
- **File management**: browse directories, upload/download, rename, and create folders directly from the management UI.
- **Centralized connection templates**: a unified SSH config store. If you'd rather not expose the actual SSH credentials to the Agent, just tell it the name of the SSH config to use.

## Quick Navigation

- [Features](#features)
- [Quick Start](#quick-start)
- [Usage](#usage)
- [Docker Deployment](#docker-deployment)
- [Connecting AI Clients (MCP)](#connecting-ai-clients-mcp)
- [Connecting Scripts / Programs (REST API)](#connecting-scripts--programs-rest-api)
- [Examples](#examples)
- [Tool Reference](#tool-reference)
- [Known Limitations](#known-limitations)

## Features

- **⚡ One-command install** — `go install github.com/open-mcp-ai/termcp@latest`. No clone and no build; a single Go toolchain is all you need.
- **🔌 One port, many entrances** — The same port serves the browser Web UI, REST API, WebSocket, and MCP Streamable HTTP + SSE in parallel. Pick whichever fits: browsers for humans, MCP for Agents/clients, REST for scripts.
- **🔗 Resource URLs** — Every entry, session, and shell has a stable `termcp://` address with one-click copy buttons throughout the Web UI. Paste the URL into your chat with the Agent and it can precisely locate and operate that connection, session, or shell — `session_start` / `session_terminate` / `shell_input` accept locators directly, no lookups needed.
- **🤝 Human–AI relay, seamless handoff** — You and the Agent share the same live session and can switch at any moment: open a root-privileged shell yourself, then hand it to the Agent to drive; or let the Agent hit a `sudo` / password / MFA prompt and pause for you to type it into the Web UI, after which the Agent carries on. Input to each shell is serialized, so you and the Agent never garble each other's keystrokes, and credentials are never guessed or echoed by the Agent — and you stay in control: interrupt a misbehaving Agent at any time.
- **🟦 Multi-turn interaction** — The process keeps running; the Agent can drive it across multiple conversation turns instead of a one-shot call-and-return.
- **🟪 Real terminal environment** — A fully emulated real terminal (PTY; ConPTY on Windows), so programs that depend on terminal features like `vim`, `top`, `gdb` all run correctly, with cross-platform compatibility.
- **🟫 Local or remote, your choice** — Point an Agent at the termcp host itself with zero setup (`ssh_config="internal"`), or at any remote machine over SSH. The same tool workflow drives both.
- **🟧 Built-in visual UI** — Access live terminals, session lists, tabbed multi-shell windows, a tiling workspace, and output-history replay straight from a browser. Served from a single port, no extra deployment needed; `/api.html` provides API + MCP quick-reference configs.
- **🟦 REST API + WebSocket** — A complete HTTP surface: session CRUD, terminal I/O streaming, port forwarding, SFTP files, history search — for scripts and custom programs.
- **🟨 Multiple Agents, no conflicts** — Multiple Agents can read the same session simultaneously, each maintaining its own independent cursor, with no output stealing. The unified `shell_output` reader works identically on live, dead, and archived sessions (byte offsets, `tail_lines`, paging).
- **🟩 Remote operations, all integrated** — Command execution, file transfer (full SFTP suite plus direct HTTP download/upload URLs with resume), and port forwarding (`-L` / `-R` / `-D`) all over a single SSH connection, with no need to re-establish connections.
- **🟥 Proactive AI notifications (push, no polling)** — `shell_notify` actively notifies the AI Agent — no polling required. termcp pushes a wake-up signal the moment a process exits, output goes quiet, or new output arrives (signal only, no payload); with `channel="sampling"` it actively sends an MCP `sampling/createMessage` to wake the model. Completion is decided by the process-exit event, never a fixed timeout. The Web UI lists and can remove active rules.
- **🟨 Session history survives everything** — "Disconnect ≠ delete": sessions that exit or crash are archived with their full output, survive termcp restarts, and can be searched, annotated, renamed, rendered as screenshots, or permanently purged.
- **🔒 Credential-safe by design** — SSH passwords, private keys, and passphrases written through `ssh_config` are never readable back, so plaintext secrets never enter the Agent's context. Config-writing tools stay off unless you explicitly enable `--mcp-manage-ssh-configs`.
- **🛡️ Single-token HTTP authentication** — One static token guards the entire HTTP surface: Web UI, REST API, MCP SSE, MCP streamable HTTP, and the WebSocket. Configure the token itself (`--auth-token` / `TERMCP_AUTH_TOKEN`) or only its salted SHA-256 hash (`--auth-hash` / `TERMCP_AUTH_HASH`, generated by `termcp --gen-auth-hash`) so the server never stores the plaintext. Non-loopback binds refuse to start without one.
- **🪶 Context-friendly output** — `shell_notify` pushes only a wake-up signal (no payload), and terminal content is pulled on demand via `shell_output`; raw terminal output never floods the model context.

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

# Build
go build -o termcp .

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
| `--port`        | `18765`       | HTTP port. Shared by the Web UI, MCP SSE, and MCP streamable HTTP.       |
| `--data-dir`    | `~/.termcp`   | Persistence directory (sessions, messages, SSH configs). Auto-created. Default overridable via `$TERMCP_DATA_DIR`. |
| `--log-level`   | `info`        | Log level: `debug` / `info` / `warn` / `error`. `debug` shows all MCP tool calls; failed tool calls and session-create errors log at `warn`/`error` regardless. |
| `--no-internal` | `false`       | Disable the built-in loopback SSH profile.                                   |
| `--mcp-manage-ssh-configs` | `false` | Enable MCP tools to create/edit/delete SSH configs (secrets are never exposed). |
| `--auth-token`   | *(unset)*    | Static token for HTTP authentication (or `$TERMCP_AUTH_TOKEN`). Every client — API, MCP, browser — must present it. Mutually exclusive with `--auth-hash`. |
| `--auth-hash`    | *(unset)*    | Salted SHA-256 hash of the token (`sha256-<salt_hex>-<digest_hex>`) so the server never holds the plaintext (or `$TERMCP_AUTH_HASH`). Generate with `termcp --gen-auth-hash`. Mutually exclusive with `--auth-token`. |
| `--gen-auth-hash` | *(action)* | Generate the salted SHA-256 hash of a token for `--auth-hash`, then exit (token from an argument, or from stdin without echo on a terminal). |

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

A single static token protects the whole HTTP surface — the Web UI, REST API, MCP SSE, MCP streamable HTTP, and the browser WebSocket. Configuring it is optional for loopback-only binds (`127.0.0.1` keeps its no-setup default); exposing a non-loopback bind without a token is a startup error.

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

### Multi-stage build: add termcp to any container

Place the following `Dockerfile` in your application project. The build stage installs termcp with `go install`, then `COPY --from` copies the binary into the target image. The target container does not need the Go runtime:

```dockerfile
# syntax=docker/dockerfile:1

# Replace this at build time with an accessible Go base image if needed
ARG GO_IMAGE=golang:1.25-alpine
FROM ${GO_IMAGE} AS termcp-build

# Go module proxy; use https://proxy.golang.org,direct outside China if preferred
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
ENV GOBIN=/out

# Pin latest to a concrete version in production, for example @vX.Y.Z
RUN go install github.com/open-mcp-ai/termcp@latest

# Replace with any target base image
FROM alpine
COPY --from=termcp-build /out/termcp /usr/local/bin/termcp
```

> `go install` downloads termcp and its dependencies through the Go module proxy. `GOPROXY` defaults to `goproxy.cn` and can be replaced with `--build-arg GOPROXY=...`. If Docker Hub is slow or unavailable, use `--build-arg GO_IMAGE=...` to select an accessible Go base-image mirror.

### Startup command examples

Containers must bind to `0.0.0.0`, and a non-loopback bind **requires authentication** — pass the token (or its hash) via `TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH` or the matching flags, or startup fails.

```bash
# Build the application image with termcp included
# You can also pass an internal GOPROXY or Go base-image mirror
docker build \
  --build-arg GOPROXY=https://goproxy.cn,direct \
  -t my-app-with-termcp .

# Run termcp as the container's main process
# Persist the data directory as a volume; authenticate with a token via env
docker run -d --name my-app-termcp \
  -p 18765:18765 \
  -v termcp-data:/data \
  -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret \
  --entrypoint /usr/local/bin/termcp \
  my-app-with-termcp \
  --host 0.0.0.0 --port 18765 --data-dir /data

# Enable MCP tools that write SSH configurations when needed
docker run -d --name my-app-termcp \
  -p 18765:18765 -v termcp-data:/data \
  -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret \
  --entrypoint /usr/local/bin/termcp \
  my-app-with-termcp \
  --host 0.0.0.0 --data-dir /data --mcp-manage-ssh-configs

# Follow logs
docker logs -f my-app-termcp
```

If the original application must run in the same container, start termcp from the existing entrypoint or process manager:

```bash
export TERMCP_AUTH_TOKEN="change-me-to-a-long-random-secret"
/usr/local/bin/termcp --host 0.0.0.0 --port 18765 --data-dir /data
```

A container typically runs one foreground process. If the application must remain the main process, run termcp as a separate service on the same Docker network and connect to it at `http://termcp:18765/stream`.

### Docker Compose startup

```yaml
services:
  termcp:
    build:
      context: .
      args:
        GOPROXY: https://goproxy.cn,direct
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

termcp is a long-running service: the same port serves the Web UI, any number of MCP clients, and session persistence. It therefore offers **HTTP transports only** — Streamable HTTP and SSE — and does **not** support stdio (there is no local subprocess mode). The MCP server is one interface layer of the platform, embeddable into any MCP-capable host — Claude Code, Cursor, Codex, Open WebUI, or your own client.

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

The legacy transport. Configure **only** `/sse`; the SDK posts JSON-RPC to `/message` automatically.

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

The Web UI's **API / MCP** page (`/api.html`) offers copy-ready config for both transports.

## Connecting Scripts / Programs (REST API)

Skip MCP and use the same session layer programmatically: the full REST API and live WebSocket channel.

```bash
# List sessions (same --auth-token protection)
curl -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" http://127.0.0.1:18765/api/sessions

# Create a session
curl -X POST http://127.0.0.1:18765/api/sessions \
  -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"ssh_config":"internal","command":"bash","mode":"pty"}'

# Read output / upload files / port forwards — see docs/api.md
```

Live terminal I/O runs over `WebSocket /api/ui/ws`; files support direct HTTP URLs with Range resume. Full endpoint list in [`docs/api.md`](./docs/api.md).

### With authentication enabled

When the server runs with `--auth-token`/`--auth-hash`, every MCP request needs the token as an `Authorization: Bearer` header:

```bash
claude mcp add --transport http termcp http://your-server:18765/stream \
  --header "Authorization: Bearer $TERMCP_AUTH_TOKEN"
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
| Sessions (connection containers) | `session_start`, `session_list`, `session_info`, `session_terminate` |
| Shells (terminal channels) | `shell_open`, `shell_list`, `shell_close`, `shell_input`, `shell_key`, `shell_output`, `shell_resize`, `shell_reader_register`, `shell_reader_unregister` |
| Notifications | `shell_notify` (wakes the AI Agent), `notify_user` (toasts the human at the Web UI) |
| SSH profiles | `ssh_config` (`list`; `create`/`edit`/`copy`/`delete` with `--mcp-manage-ssh-configs`) |
| Port forwarding | `forward` (`-L` / `-R` / `-D` / list / close) |
| Files (SFTP) | `file_read`, `file_write`, `file_stat`, `file_delete`, `file_rename`, `file_mkdir`, `file_urls`, `file_perm`, `file_link`, `file_fs`, `file_getwd` |
| History & messages | `history` (list / search / rename / meta / purge / screenshot), `message` (list / get) |
| Host discovery | `shell_detect` |

Run a command as `shell_input` + `shell_key(key="enter")` + `shell_output`. Failed tools return `isError=true` with a JSON body carrying a stable `error_code`.

## Known Limitations

- **`history` screenshots are ASCII-only.** `history(action=screenshot)` renders the persisted text as a fixed-bitmap terminal image; it is not a pixel-accurate rendering of non-ASCII glyphs.
- **File and forward tools need a live connection.** On `exited`/archived sessions those tools return `session_not_running`; output reading still works via `shell_output`.
- **No auto-reconnect.** An unexpected SSH drop is detected and the session is marked DEAD (`exited`), kept read-only with its output retained — it is not reconnected automatically. Start a new session (`session_start`) or review the old one from history.
- **No command allowlisting or directory jail.** termcp does not enforce command whitelists, path restrictions, or policy-based risk tiers. Risk control is human-in-the-loop instead: interrupt the Agent from the Web UI at any time, and privileged prompts (`sudo` / password / MFA) are by default handed to you — Agents follow a no-guessing, no-echoing convention and pause for you to type. Whether the Agent may type them anyway is your call; termcp does not forbid it.
- **Basic authentication needs TLS outside localhost.** The browser login challenge uses HTTP Basic, whose credentials are only Base64-encoded. Put a TLS-terminating reverse proxy in front of termcp when exposing it beyond a trusted local network; the static token is still never logged or placed in a URL.

---

