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
  <a href="./README.md">English</a> | <strong>中文</strong>
</p>

<p align="center">
  <a href="#安装"><img src="https://img.shields.io/badge/安装-2786ff?style=flat-square" alt="安装"></a>
  <a href="#文档"><img src="https://img.shields.io/badge/文档-00ADD8?style=flat-square" alt="文档"></a>
  <a href="https://termcp.org" target="_blank" rel="noopener"><img src="https://img.shields.io/badge/官网-termcp.org-6E4AFF?style=flat-square" alt="官网"></a>
</p>

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=rect&color=0:2786FF,100:FF69B4&height=3&section=header" width="100%" alt="divider">
</p>

## Termcp：最终的 MCP | Terminus MCP

使用 Termcp：

- **AI 能动态安装、使用不限数量的 MCP。**
- **AI 可以自行开发 MCP 并热装载。**
- **AI 可以使用 SSH 管理主机，由人类审批，协作。**
- **AI 可以仅基于 SSH 跨机器安装和调用 MCP。**

**只暴露一个 tool。** 基于 [**Termcp 协议**](./docs/protocol.md)（Terminalization Model Context Protocol／终端化
上下文协议）实现：装载多个 MCP 和 skills 不会导致上下文增加。

**无依赖单文件（10M+）、跨平台，提供所见即所得的 WebUI**，人与 AI 共用终端、交互协作。

<p align="center">
  <a href="https://termcp.org" target="_blank" rel="noopener">
    <img src="https://termcp.org/ext-cn.jpg" width="860" alt="Termcp 扩展面板：装了哪些 MCP、装在哪台机器上、现在能不能用">
  </a>
</p>
<p align="center"><em>安装 80+ MCP server 与 skill。</em></p>

<p align="center">
  <a href="https://termcp.org" target="_blank" rel="noopener">
    <img src="https://termcp.org/cn.jpg" width="860" alt="Termcp 控制台：本地与 SSH 会话同屏，逐行标出你的输入、AI 输入与终端输出">
  </a>
</p>
<p align="center"><em>主机管理。</em></p>

## 安装

### 0 · AI 安装

```
把 npx -y @open-mcp-ai/termcp daemon stdio 装成名为 termcp 的 MCP。
```

复制给 AI。

### 1 · 一行命令接上你的 Agent

| Agent | 命令 |
| --- | --- |
| **Claude Code** | `claude mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio` |
| **Codex** | `codex mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio` |
| **Gemini CLI** | `gemini mcp add termcp npx -- -y @open-mcp-ai/termcp daemon stdio` |

### 2 · 配置文件

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
<summary><strong>其他安装方式</strong> —— npm / Go / Docker / 预编译二进制</summary>

| | 命令 |
| --- | --- |
| **npm** | `npm i -g @open-mcp-ai/termcp` —— 装出 `termcp` 命令；有了它，上面每处的 `npx -y @open-mcp-ai/termcp` 都换成 `termcp` 即可。包装器：[open-mcp-ai/Termcp-npm](https://github.com/open-mcp-ai/Termcp-npm)。 |
| **Go** | `go install github.com/open-mcp-ai/termcp@latest` —— 国内用 `GOPROXY=https://goproxy.cn,direct`；也可作为模块使用（`go get github.com/open-mcp-ai/termcp`）。 |
| **Docker** | `docker run -d --name termcp -p 18765:18765 -v termcp-data:/home/termcp -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765` |
| **二进制** | [预编译 Release](https://github.com/open-mcp-ai/termcp/releases/latest) —— `linux` / `darwin` / `windows` 的 `amd64` / `arm64`，各一个静态文件。 |

npm 包装器会下载对应平台的预编译二进制（可用 `TERMCP_VERSION`、`TERMCP_MIRROR`、`TERMCP_BIN`、`TERMCP_SKIP_DOWNLOAD` 覆盖）。完整 flag 见 [`docs/cli.md`](./docs/cli.md)；Docker 方案、Compose 与免 token 本机部署见 [`docs/deploy.md`](./docs/deploy.md)。

**HTTP 传输**，适用于已经有个实例在某处应答（`npx -y @open-mcp-ai/termcp`）：

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

客户端在 Docker 里可用 `http://host.docker.internal:18765/stream`（macOS/Windows）或宿主机的 LAN IP；同网络的另一个容器用 `http://termcp:18765/stream`。旧 SSE 用 `http://localhost:18765/sse`——只配 `/sse`，JSON-RPC 会自己 POST 到 `/message`。开了 `--auth-token` 就加 bearer 请求头：`"headers": {"Authorization": "Bearer <token>"}`。

</details>

## 文档

| | |
| --- | --- |
| [`docs/cli.md`](./docs/cli.md) | 全部 flag 与子命令、能力闸门、认证。 |
| [`docs/clients.md`](./docs/clients.md) | 四个入口的完整接法：MCP 传输、stdio 桥、认证、REST 示例，以及单工具面。 |
| [`docs/deploy.md`](./docs/deploy.md) | Docker、多阶段构建、Compose、纯 API 构建。 |
| [`docs/protocol.md`](./docs/protocol.md) | `TERM/1` 规格：线格式、状态码、标头、19 条指令、三个闸门。 |
| [`docs/capabilities.md`](./docs/capabilities.md) | 完整能力面与安全模型。 |
| [`docs/api.md`](./docs/api.md) | REST 端点、WebSocket 帧、定位符解析。 |
| [`docs/capabilities.md`](./docs/capabilities.md) | 完整能力面与安全模型。 |
| [`docs/architecture.md`](./docs/architecture.md) | 会话内核、四个入口、存储。 |
| [`docs/design/`](./docs/design) | 设计记录：Web UI、资源模型、会话存储、字体栈、移动端终端。 |
| [`CHANGELOG.md`](./CHANGELOG.md) | 版本记录。 |

## Benchmark

Comming Soon

## 功能安全性测试

Comming Soon

## Star 趋势

<p align="center">
  <a href="https://star-history.com/#open-mcp-ai/termcp&Date">
    <img src="https://api.star-history.com/svg?repos=open-mcp-ai/termcp&type=Date" alt="Star History Chart" width="760">
  </a>
</p>

## 许可证

基于 [MIT License](./LICENSE) 开源。
感谢 [linux.do](https://linux.do/) 社区朋友们的讨论、建议与支持。

---

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:2786FF,100:6E4AFF&height=110&section=footer" width="100%" alt="footer">
</p>

</div>
