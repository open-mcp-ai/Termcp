<div id="top">

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:2786FF,50:6E4AFF,100:FF69B4&height=150&section=header" width="100%" alt="header banner">
</p>

<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp">
    <img src="https://github.com/user-attachments/assets/3385c360-b8d1-48ad-91da-394a1305320a" width="150" alt="Termcp logo">
  </a>
</p>

<h1 align="center">⚡ Termcp</h1>

<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp">
    <img src="https://readme-typing-svg.demolab.com?font=JetBrains+Mono&weight=700&size=20&pause=900&color=2786FF&center=true&vCenter=true&width=860&height=45&lines=Terminus+MCP" alt="Termcp tagline">
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
  <a href="#install"><img src="https://img.shields.io/badge/Install-2786ff?style=flat-square" alt="Install"></a>
  <a href="#docs"><img src="https://img.shields.io/badge/Docs-00ADD8?style=flat-square" alt="Docs"></a>
  <a href="https://termcp.org" target="_blank" rel="noopener"><img src="https://img.shields.io/badge/Website-termcp.org-6E4AFF?style=flat-square" alt="Website"></a>
</p>

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=rect&color=0:2786FF,100:FF69B4&height=3&section=header" width="100%" alt="divider">
</p>

## Termcp: the final MCP | Terminus MCP

With Termcp:

- **An AI can install and use any number of MCP servers**, dynamically, at run time.
- **An AI can write its own MCP server and hot-load it.**
- **An AI can use SSH to manage hosts, approved and collaborated on by a human.**
- **An AI can install and call MCP servers across machines over nothing but SSH.**

**Exactly one tool is exposed.** Built on the [**Termcp protocol**](./docs/protocol.md) (*Terminalization Model Context
Protocol*): loading more MCP servers and skills does not grow the context.

**A single dependency-free binary (10 MB+), cross-platform, with a WYSIWYG web UI** where a human
and an AI share the same terminals and collaborate.

<p align="center">
  <a href="https://termcp.org" target="_blank" rel="noopener">
    <img src="https://termcp.org/ext-en.jpg" width="860" alt="Termcp extensions panel: which MCPs are installed, which host each one runs on, and whether it is available">
  </a>
</p>
<p align="center"><em>Install 80+ MCP servers and skills.</em></p>

<p align="center">
  <a href="https://termcp.org" target="_blank" rel="noopener">
    <img src="https://termcp.org/en.jpg" width="860" alt="Termcp console: local and SSH sessions on one screen, every line marked human / AI / shell output">
  </a>
</p>
<p align="center"><em>Host management.</em></p>

## Install

### 0 · AI install

```
Install npx -y @open-mcp-ai/termcp daemon stdio as an MCP named termcp.
```

Copy this to the AI.

### 1 · One command per agent

| Agent | Command |
| --- | --- |
| **Claude Code** | `claude mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio` |
| **Codex** | `codex mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio` |
| **Gemini CLI** | `gemini mcp add termcp npx -- -y @open-mcp-ai/termcp daemon stdio` |

### 2 · Config file

```json
{
  "mcpServers": {
    "termcp": {
      "command": "npx",
      "args": ["-y", "@open-mcp-ai/termcp", "daemon", "stdio"]
    }
  }
}
```

<details>
<summary><strong>Other install methods</strong> — npm / Go / Docker / prebuilt binary</summary>

| | Command |
| --- | --- |
| **npm** | `npm i -g @open-mcp-ai/termcp` — installs a `termcp` command; with it, drop `npx -y @open-mcp-ai/termcp` from every snippet above and use `termcp`. Wrapper: [open-mcp-ai/Termcp-npm](https://github.com/open-mcp-ai/Termcp-npm). |
| **Go** | `go install github.com/open-mcp-ai/termcp@latest` — `GOPROXY=https://goproxy.cn,direct` in mainland China; also usable as a module (`go get github.com/open-mcp-ai/termcp`). |
| **Docker** | `docker run -d --name termcp -p 18765:18765 -v termcp-data:/home/termcp -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765` |
| **Binary** | [Prebuilt releases](https://github.com/open-mcp-ai/termcp/releases/latest) — `linux` / `darwin` / `windows` on `amd64` / `arm64`, one static file each. |

The npm wrapper downloads the matching prebuilt binary (override with `TERMCP_VERSION`, `TERMCP_MIRROR`,
`TERMCP_BIN`, `TERMCP_SKIP_DOWNLOAD`). Flags: [`docs/cli.md`](./docs/cli.md). Docker recipes, Compose and a
token-free loopback setup: [`docs/deploy.md`](./docs/deploy.md).

**HTTP transport**, when an instance is already answering somewhere (`npx -y @open-mcp-ai/termcp`):

```json
{
  "mcpServers": {
    "termcp": {
      "type": "http",
      "url": "http://localhost:18765/stream"
    }
  }
}
```

Reachable from a client in Docker as `http://host.docker.internal:18765/stream` (macOS/Windows) or the host's
LAN IP; from another container on the same network as `http://termcp:18765/stream`. Legacy SSE uses
`http://localhost:18765/sse` — only `/sse`, JSON-RPC goes to `/message` by itself. With
`--auth-token`, add a bearer header: `"headers": {"Authorization": "Bearer <token>"}`.

</details>

## Docs

| | |
| --- | --- |
| [`docs/cli.md`](./docs/cli.md) | Every flag and subcommand, capability gates, authentication. |
| [`docs/clients.md`](./docs/clients.md) | All four entrances end to end: MCP transports, the stdio bridge, authentication, REST examples, and the one-tool surface. |
| [`docs/deploy.md`](./docs/deploy.md) | Docker, multi-stage builds, Compose, pure-API build. |
| [`docs/protocol.md`](./docs/protocol.md) | `TERM/1` specification: wire format, status codes, headers, the 19 directives, the three gates. |
| [`docs/capabilities.md`](./docs/capabilities.md) | The full capability surface and the security model. |
| [`docs/api.md`](./docs/api.md) | REST endpoints, WebSocket frames, locator resolution. |
| [`docs/capabilities.md`](./docs/capabilities.md) | Full capability surface and the security model. |
| [`docs/architecture.md`](./docs/architecture.md) | Session kernel, entrances, storage. |
| [`docs/design/`](./docs/design) | Design records: Web UI, resource model, session storage, font stack, mobile terminal. |
| [`CHANGELOG.md`](./CHANGELOG.md) | Release notes. |

## Benchmark

Comming Soon

## Functional security testing

Comming Soon

## Star History

<p align="center">
  <a href="https://star-history.com/#open-mcp-ai/termcp&Date">
    <img src="https://api.star-history.com/svg?repos=open-mcp-ai/termcp&type=Date" alt="Star History Chart" width="760">
  </a>
</p>

## License

Released under the [MIT License](./LICENSE).
Thanks to friends in the [linux.do](https://linux.do/) community for the discussions, feedback, and support.

---

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:2786FF,100:6E4AFF&height=110&section=footer" width="100%" alt="footer">
</p>

</div>
