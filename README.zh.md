<div id="top">



<p align="center">
    <img src="./docs/assets/logo.png"></img>
  <h1 align="center">termcp</h1>
  <p align="center"><em>一个 AI Native 的终端平台：跨平台、可视化、人机协作。</em></p>
</p>



<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp/stargazers">
    <img src="https://img.shields.io/github/stars/open-mcp-ai/termcp?label=Stars&logo=github&style=for-the-badge" alt="Stars">
  </a>
  <a href="https://github.com/open-mcp-ai/termcp/forks">
    <img src="https://img.shields.io/github/forks/open-mcp-ai/termcp?label=Forks&logo=github&style=for-the-badge" alt="Forks">
  </a>
  <img src="https://img.shields.io/badge/平台-macOS%20%7C%20Linux%20%7C%20Windows-2786ff?style=for-the-badge" alt="平台">
  <img src="https://img.shields.io/badge/Go-Pure%20Go%20%7C%20No%20CGO-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Pure Go No CGO">
  <a href="./LICENSE">
    <img src="https://img.shields.io/badge/license-MIT-green?style=for-the-badge" alt="MIT License">
  </a>
</p>



<p align="center">
  <strong>中文</strong> | <a href="./README.md">English</a>
</p>



---

## 简介

`termcp` 是一款 AI Native 的终端平台：多主机、多会话同时管理，全程可视化。这些会话由人和 AI 共同管理、共同维护，随时可以互相接管与交接；连接配置由平台独立维护，Agent 无需读取凭据即可使用。

- **人** —— 浏览器实时查看、操作、接管任何会话；
- **AI Agent** —— 通过 MCP 或 SKILLS 驱动同一批终端；
- **脚本 / 程序** —— 通过 REST API 编程化接入。

平台层提供长驻会话与只读回放、多主机多会话并行编排、SSH 连接全生命周期管理，让整个过程**可观测、可编程、人机接力**。跨平台、云原生、纯 Go 无 CGO；单二进制、低开销、可长期驻留。

### 演示视频

https://github.com/user-attachments/assets/d06a3c36-250a-4eeb-aefa-e80d13d1551c

## 为什么选 termcp

### 多会话可视化管理

功能强大的 Web UI 把多主机、多会话集中到一个界面里管理：本地一条命令启动，或容器化部署到云端，浏览器访问的都是同一套操作界面。

- **多会话仪表盘**：所有运行中的会话按名称列出，随时切换、随时接管。
- **实时行为观测**：像操作本地终端一样，在浏览器里看 `htop` 的动态界面、`vim` 的编辑过程、安装程序的彩色提示。
- **标签化与平铺工作区**：一个 SSH 会话下可开多个 shell，各占一个标签；多个会话也可平铺展示，同时跟踪。
- **端口转发可视化**：会话相关的本地/远程端口与协议一目了然。
- **文件管理**：浏览目录、上传下载、重命名、建目录，都在管理界面里完成。
- **连接模板集中托管**：统一 SSH 配置管理；AI 开启会话时只指定配置名，读不到具体配置。
- **已关闭会话只读回放**：会话关闭、崩溃或重启后，完整输出仍可翻阅。

### AI Native 设计

无缝人机交互、结对操作：Agent 是终端的常驻用户，与你和脚本并列。

Agent 原生只能执行一次性命令，而真实工作大量是**多轮交互**（SSH 登录先输密码、Python REPL 逐行调试、回答安装程序的 `[Y/n]` 提示、驱动 `top`/`htop`/impacket）。`termcp` 把真实终端直接交给 Agent：会话持续复用，**TUI**、**REPL**、**GDB**、**msfconsole**、**vim** 都能像人一样被持续管理——走 MCP，或用实例自带的 [Agent Skill](#agent-skill纯-curl无需-mcp) 走纯 `curl`。

- **同一套会话层，平级入口。** MCP、SKILLS、REST/WebSocket 与 Web UI 同处一层，共用同一批真实会话。Agent 的每一步操作，你在浏览器里都看得见、随时能接管；反过来，Agent 需要时也可以停下来，把密码/MFA 提示交给你输入。
- **为 token 与轮次预算设计。** 工具 schema 紧凑、支持按需延迟加载（见 [`docs/mcp-tools.md`](./docs/mcp-tools.md)）；`shell_output` 用 tail/offset 游标分页，模型上下文只载入你真正需要的输出；`shell_notify` 只发唤醒信号；`message` 按需取回完整输出。
- **实例自描述。** 每个运行中的 termcp 都对外提供自己的 `/api.md` 与 `/skills.md`（免 token），并注册为 MCP resources 与 `learn-api` prompt；新 Agent 单单靠这两个文件就能驱动这个实例的当前版本。
- **密钥留在平台侧。** 经 `ssh_config` 写入的密码、私钥、口令仅保存在平台侧，MCP 的读取接口只返回配置名；SSH 配置写入工具默认关闭，需运维显式开启 `--mcp-manage-ssh-configs`。
- **失败可恢复。** 关闭、崩溃或重启过的会话仍以只读 DEAD 条目留在会话列表里，输出依旧可读，Agent（或你）可以接着中断前的状态继续；重连同一个 `termcp://<entry>` 即可开启下一段会话。
- **人始终保留中断权。** `notify_user` 可直接通知到你；需要提权的提示由你在 Web UI 里输入；同一 shell 的写入串行化，人与 Agent 的输入按序生效。

## 快速导航

- [功能特性](#功能特性)
- [快速开始](#快速开始)
- [使用](#使用)
- [Docker 部署](#docker-部署)
- [接入 AI 客户端（MCP）](#接入-ai-客户端mcp)
- [Agent Skill（纯 curl，无需 MCP）](#agent-skill纯-curl无需-mcp)
- [接入脚本 / 程序（REST API）](#接入脚本--程序rest-api)
- [示例](#示例)
- [工具参考](#工具参考)
- [已知限制与安全模型](#已知限制与安全模型)

## 功能特性

- **⚡ 一行安装，纯 Go 无 CGO** —— `go install github.com/open-mcp-ai/termcp@latest`；无 CGO 依赖（`CGO_ENABLED=0`），零系统动态库绑定，单静态二进制随处分发，原生完美跨平台（Windows ConPTY、macOS / Linux POSIX PTY 行为高度一致）。
- **🔌 一个端口，四个入口** —— Web UI（人）、MCP / SKILLS（Agent）、REST + WebSocket（脚本）共用同一端口。
- **🤝 人机接力** —— 人与 Agent 共用同一实时会话，你可随时接管或中断 Agent；遇到 `sudo` / 密码 / MFA 提示时 Agent 暂停，由你在 Web UI 输入；同一 shell 输入串行，互不打断。
- **🟦 多轮交互的真实终端** —— 进程持续运行，Agent 可跨对话轮次驱动 TUI、REPL、GDB、msfconsole、vim 等程序；完整 PTY（Windows 走 ConPTY），各平台行为一致。
- **🟫 本机 / 远程同一套流程** —— 零配置操作本机（`ssh_config="internal"`）或经 SSH profile 接入远程主机；命令、文件传输（SFTP + 可断点续传的 HTTP 直链）与端口转发（`-L` / `-R` / `-D`）都在同一条连接内完成。
- **🟧 内置可视化管理** —— 浏览器实时终端、多会话仪表盘、多标签频道、平铺工作区、已关闭会话只读回放、文件与转发面板；`/api.html` 提供 API / MCP / SKILLS 速查。
- **🟨 多 Agent 并行，断开不丢输出** —— 多个 Agent 同时读同一会话、各自游标互不抢占；会话关闭后（显式关闭、自然退出、断线或重启）仍以只读 DEAD tile 留在列表中，输出完整可回放、翻页或删除；断线后用同一个 entry（`termcp://<entry>`）新起一个会话即可接着干。
- **🟥 主动通知，免轮询** —— `shell_notify` 在进程退出 / 输出停顿 / 有新输出时主动唤醒 Agent，只发信令、不带内容（正文另行拉取）；`channel="sampling"` 时直接发送 `sampling/createMessage`。
- **🔒 凭据安全** —— 经 `ssh_config` 写入的密码、私钥、口令一律不可读回，明文凭据不进入 Agent 上下文；配置写入类工具默认关闭，需显式开启 `--mcp-manage-ssh-configs`。

## 快速开始

### 快速安装（需要 Go 环境）

最省事的方式 —— 一条命令搞定，无需克隆、无需编译：

```bash
go install github.com/open-mcp-ai/termcp@latest
```

`go install` 会通过 Go 模块代理拉取（中国大陆可用 `GOPROXY=https://goproxy.cn,direct`），把 `termcp` 二进制放到 `$(go env GOPATH)/bin`，请确保该目录在 `PATH` 中。termcp 用 Go 编写，安装方式就是 `go install` 或 Releases 预编译二进制，没有 npx/uvx 版本，也不需要 Node/Python 运行时。作为 Go module，它还支持**源码级集成**：可 `go get github.com/open-mcp-ai/termcp` 作为依赖引入，或 fork 源码构建定制版本。随后直接运行：

```bash
termcp
```

### 下载

前往 [Releases 页面](https://github.com/open-mcp-ai/termcp/releases)，下载对应平台的预编译二进制:

| 平台                  | 文件                       |
| :-------------------- | :------------------------- |
| Linux (x86_64)        | [termcp-linux-amd64](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-linux-amd64) |
| Linux (ARM64)         | [termcp-linux-arm64](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-linux-arm64) |
| macOS (Intel)         | [termcp-darwin-amd64](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-darwin-amd64) |
| macOS (Apple Silicon) | [termcp-darwin-arm64](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-darwin-arm64) |
| Windows (x86_64)      | [termcp-windows-amd64.exe](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-windows-amd64.exe) |
| Windows (ARM64)       | [termcp-windows-arm64.exe](https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-windows-arm64.exe) |

### 编译

```bash
# 克隆
git clone https://github.com/open-mcp-ai/termcp.git
cd termcp

# 编译（纯 Go，无需 CGO，支持任意平台交叉编译）
CGO_ENABLED=0 go build -o termcp .

# 运行（默认：loopback，端口 18765；数据存于 ~/.termcp）
./termcp
```

浏览器打开 `http://127.0.0.1:18765` 即可进入 **Web 界面**。

## 使用

### 命令行

```text
termcp [flags]
```

| Flag            | 默认值      | 说明                                                         |
| --------------- | ----------- | ------------------------------------------------------------ |
| `--host`        | `127.0.0.1` | HTTP 绑定地址。`0.0.0.0` 监听所有网卡。绑定非 loopback 地址时**必须**配置认证 Token/哈希，否则拒绝启动。 |
| `--port`        | `18765`     | HTTP 端口。Web UI、MCP SSE、MCP streamable HTTP、文档与 skill（`/api.md`、`/skills.md`）共用。 |
| `--data-dir`    | `~/.termcp` | 持久化目录（会话、消息、SSH 配置）。不存在则自动创建。默认值可用环境变量 `$TERMCP_DATA_DIR` 覆盖。 |
| `--log-level`   | `info`      | 日志级别：`debug` / `info` / `warn` / `error`。`debug` 显示全部 MCP 工具调用；失败的工具调用与会话创建错误始终以 `warn`/`error` 打印。 |
| `--no-internal` | `false`     | 禁用内建 loopback SSH profile。                                |
| `--mcp-manage-ssh-configs` | `false` | 允许 AI 通过 MCP 管理 SSH 配置（凭据永不暴露）。                |
| `--auth-token`  | *(未设置)*  | HTTP 认证静态 Token（或 `$TERMCP_AUTH_TOKEN`）。API、MCP、浏览器全部客户端都须携带。与 `--auth-hash` 互斥。 |
| `--auth-hash`   | *(未设置)*  | Token 的 salted SHA-256 哈希（`sha256-<salt_hex>-<digest_hex>`），服务端不保存明文（或 `$TERMCP_AUTH_HASH`）。用 `termcp --gen-auth-hash` 生成。与 `--auth-token` 互斥。 |
| `--gen-auth-hash` | *(action)* | 生成 token 的 salted SHA-256 哈希（供 `--auth-hash` 使用）后退出；token 取自参数，或不带参数时从终端 stdin 无回显读取。 |
| `--version`     | *(action)*  | 打印版本、commit 与构建时间后退出。版本自动跟随 git tag：release 构建通过 `-ldflags` 注入；直接 `go build` 或 `go install module@vX.Y.Z` 时回退到 Go 工具链嵌入的模块版本。 |

这些 flag 就是**能力门控**：`--no-internal` 把 Agent 收窄到只能连远程主机，`--mcp-manage-ssh-configs` 才放开 SSH 配置写入。按场景收紧或放开 Agent 能触达的面。认证详见下节[认证](#认证)。

### 示例

```bash
# 监听所有网卡
./termcp --host 0.0.0.0 --auth-token "your-long-random-token"

# 监听所有网卡，服务端只保存 salted 哈希
./termcp --host 0.0.0.0 --auth-hash "$(./termcp --gen-auth-hash)"

# 允许 AI Agent 管理 SSH 配置
./termcp --mcp-manage-ssh-configs

# 禁用内建 loopback profile（Agent 只能连远程主机）
./termcp --no-internal
```

### 认证

单一静态 Token 保护整个 HTTP 面——Web UI、REST API、MCP SSE、MCP Streamable HTTP 与浏览器 WebSocket（只读文档 `/api.md`、`/skills.md` 保持公开，供 agent 在拿到 token 前先读文档）。仅监听 loopback（`127.0.0.1`）时可保持零配置默认；绑定非 loopback 而未配置 Token 会直接启动失败。

```bash
# 明文方式：flag 或环境变量
./termcp --auth-token "your-long-random-token"
TERMCP_AUTH_TOKEN="your-long-random-token" ./termcp

# 哈希方式（推荐）：服务端只保存 sha256-<salt>-<digest>。
# `termcp --gen-auth-hash` 在终端下无回显地从 stdin 读取 Token，
# 不会进入 shell 历史：
./termcp --gen-auth-hash
TERMCP_AUTH_HASH='sha256-...' ./termcp
```

各客户端如何携带 Token：

| 客户端 | 凭据方式 |
|--------|---------|
| API / MCP / curl | `Authorization: Bearer <token>` 请求头 |
| 浏览器（Web UI） | 收到 `401` 时弹出原生登录框——用户名被忽略（留空即可），**密码填 Token**。认证成功后自动下发 `termcp_token` cookie，同源 WebSocket 握手随之通过。 |

行为说明：

- `--auth-token` 与 `--auth-hash` 互斥；同一配置项 flag 优先于环境变量。
- Token 含冒号也兼容：解码后的整个 `user:pass` 串与原 token 完全一致时同样放行，因此按首个冒号拆分的客户端（如 `curl -u user:pass`）也能通过；规范写法仍是 `curl -u :<token>`。
- 未配置 Token/哈希时，绑定任何非 loopback 地址（`0.0.0.0`、局域网 IP、非 `localhost` 的主机名）都会启动失败——被误暴露的实例不可能无认证运行。
- 浏览器走的是 HTTP Basic，只是 Base64 编码而非加密。对外提供服务时请在 termcp 前面用反向代理终止 TLS；此时仅当请求本身来自 TLS 时 `termcp_token` cookie 才会自动带上 `Secure` 标志。

### 连接远程主机

零配置：`ssh_config="internal"` 直接操作 termcp 本机。要连远程机器，在 Web UI 新建连接对话框创建 SSH profile（内置 TOML 模板与「测试连接」按钮），或通过 REST `PUT /api/connections/<name>` 提交 TOML：

```toml
kind = "remote"
host = "192.168.1.100"
user = "pi"
trust_unknown_host = true  # 首次连接未知主机

# 密码方式二选一：
password = "..."

# 或直接粘贴私钥 PEM 内容——写路径（如 "~/.ssh/id_ed25519"）是无效的：
private_key = """-----BEGIN OPENSSH PRIVATE KEY-----
<粘贴 ~/.ssh/id_ed25519 的完整内容>
-----END OPENSSH PRIVATE KEY-----"""
key_passphrase = "..."     # 仅当私钥带口令时填写

# 可选：跳板机（ProxyJump）
[jump]
host = "bastion.example.com"
user = "ops"
password = "..."
```

profile 存放在 `data-dir/ssh_configs/<name>/config.toml`，可用 `ssh_config(action=list)` 查询；按此方式写入的凭据一律不可读回。Agent 也能创建 profile，但仅在 termcp 以 `--mcp-manage-ssh-configs` 启动时可用。

## Docker 部署

### 运行官方镜像

官方镜像以非 root 用户 `termcp`（uid/gid 1000）运行，`/home/termcp` 声明为 `VOLUME`——全部状态（会话、SSH 配置、消息记录）默认存于 `~/.termcp`。镜像只携带二进制：不内置 entrypoint、不预声明端口，监听地址由运行命令决定。

```bash
docker run -d --name termcp -p 18765:18765 -v termcp-data:/home/termcp -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret ghcr.io/open-mcp-ai/termcp:latest --no-internal --host 0.0.0.0 --port 18765
```

> shell 示例均为单行：`\` 续行在 bash 里有效，但在 PowerShell 里是语法错误；单行命令可原样粘贴到 bash、zsh 与 PowerShell。

`--host 0.0.0.0` 使容器可被外部访问，因此必须提供认证 token。MCP 端点：`http://localhost:18765/stream`。改用 bind mount 时，先对宿主目录执行 `chown -R 1000:1000 /path/on/host`。

### 多阶段构建：添加到任意容器

把下面的 `Dockerfile` 放进应用项目：构建阶段用 `go install` 安装 termcp，再用 `COPY --from` 把二进制复制进目标镜像——目标容器不需要 Go 运行时。

```dockerfile
# syntax=docker/dockerfile:1

ARG GO_IMAGE=golang:1.25-alpine
FROM ${GO_IMAGE} AS termcp-build

# 模块代理；海外环境可换 https://proxy.golang.org,direct
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} GOBIN=/out CGO_ENABLED=0

# 生产环境建议固定版本，如 @vX.Y.Z
RUN go install github.com/open-mcp-ai/termcp@latest

# 任意目标基础镜像
FROM alpine
COPY --from=termcp-build /out/termcp /usr/local/bin/termcp
```

需要换模块代理或基础镜像源时，用 `--build-arg GOPROXY=...` / `--build-arg GO_IMAGE=...` 覆盖。

### 启动命令示例

容器内必须监听 `0.0.0.0`，非 loopback 监听**必须配置认证**（`TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH`），否则启动失败。

```bash
docker build --build-arg GOPROXY=https://goproxy.cn,direct -t my-app-with-termcp .
docker run -d --name my-app-termcp -p 18765:18765 -v termcp-data:/data -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret --entrypoint /usr/local/bin/termcp my-app-with-termcp --host 0.0.0.0 --port 18765 --data-dir /data
docker logs -f my-app-termcp
```

在启动参数后追加 `--mcp-manage-ssh-configs` 即可放开 SSH 配置写入工具。

必须与原应用共用同一容器时，从原有 entrypoint 或进程管理器启动 termcp；否则建议作为独立服务运行，通过 `http://termcp:18765/stream` 访问。

### Docker Compose 启动

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

## 接入 AI 客户端（MCP）

termcp 在**同一端口（18765）同时支持两种 MCP 传输**，按客户端能力二选一即可，工具面完全一致。

termcp 是常驻服务：同一端口同时服务 Web UI、任意数量的 MCP 客户端与会话持久化，因此只提供 **HTTP 传输**（Streamable HTTP / SSE），**不支持 stdio**（没有本地子进程模式）。

完全不想装 MCP 客户端？可以跳过本节，直接安装 [Agent Skill](#agent-skill纯-curl无需-mcp)：实例在 `/skills.md` 提供，装一次即可用 `curl` 驱动同一批会话。作为 AI 控制层，MCP 服务器只是它诸多能力面之一，可嵌入任意 MCP 宿主（Claude Code、Cursor、Codex、Open WebUI 或自研客户端）。

### 方式 A —— Streamable HTTP (`/stream`)

新一代 MCP 传输，单端点、无需单独的 message 路径。Claude Code、Open WebUI 及多数新客户端推荐使用。

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

- 同机：`http://127.0.0.1:18765/stream`。
- Open WebUI 在 Docker 内、termcp 在宿主机：`http://host.docker.internal:18765/stream`（macOS/Windows），或宿主机局域网 IP。
- 两者都在 Docker 内（同一网络，见 [Docker 部署](#docker-部署)）：`http://termcp:18765/stream`。

### 方式 B —— SSE (`/sse`)

传统传输方式。客户端**只配置 `/sse`**，SDK 会自动向 `/message` 发 JSON-RPC。

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

### 速查

- Streamable HTTP → `http://<host>:18765/stream`
- SSE → `http://<host>:18765/sse`（JSON-RPC 走 `POST /message`）

Web UI 的 **API / MCP / SKILLS** 页面（`/api.html`）提供两种传输的可复制配置，以及本实例的 Agent 文档与 skill 下载地址。

## Agent Skill（纯 curl，无需 MCP）

不想配 MCP 客户端？实例自带一份可安装的 **Agent Skill**，让任意 agent 只用
`curl` 就能驱动 termcp —— 包括识别用户从 Web UI 复制的 `termcp://` 定位符。

```bash
# 公开端点：下载文档本身不需要 token
curl -fsS http://<host>:18765/skills.md -o /tmp/termcp-SKILL.md

# Claude Code 读取 ~/.claude/skills/<名字>/SKILL.md
mkdir -p ~/.claude/skills/termcp && cp /tmp/termcp-SKILL.md ~/.claude/skills/termcp/SKILL.md

# 其他遵循共享约定的 agent 读取 ~/.agents/skills/<名字>/SKILL.md
mkdir -p ~/.agents/skills/termcp && cp /tmp/termcp-SKILL.md ~/.agents/skills/termcp/SKILL.md
```

安装后需重启 agent 会话（skill 在会话启动时加载）。Claude Code 没有单独的 skill 子命令：
安装 = 放进目录，卸载 = `rm -rf ~/.claude/skills/termcp`（以 plugin 形式分发时用
`claude plugin install/uninstall`）。

装好之后，一句“打开 termcp://rock64 并执行 `uname -a`”即可端到端完成：
skill 会先用 `GET /api/resolve?url=...` 解析定位符，用解析出的 `ssh_config` 建会话，
发送命令并轮询输出。同一份 skill 也注册为 MCP resource `<origin>/skills.md`，
`/api.html` 会给出当前实例的准确安装命令。

## 接入脚本 / 程序（REST API）

不通过 MCP 也能编程化使用同一套会话能力：完整的 REST API 与实时 WebSocket 通道。

```bash
# 列出会话（同样受 --auth-token 保护）
curl -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" http://127.0.0.1:18765/api/sessions

# 创建一个会话
curl -X POST http://127.0.0.1:18765/api/sessions -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" -H 'Content-Type: application/json' -d '{"ssh_config":"internal","command":"bash","mode":"pty"}'

# 读取会话输出 / 上传下载文件 / 端口转发，见 docs/api.md
```

终端实时 I/O 走 `WebSocket /api/ui/ws`；文件支持 HTTP 直链（Range 断点续传）。完整端点见 [`docs/api.md`](./docs/api.md)。

### 开启认证时的接入

服务端以 `--auth-token` / `--auth-hash` 启动后，所有 MCP 请求都要带 `Authorization: Bearer` 请求头：

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

不要把 Token 放进 URL，也不要贴进共享配置或截图。`curl` 与脚本使用相同请求头：

```bash
curl -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" http://your-server:18765/api/sessions
```

## 工具参考

termcp 共提供 31 个 MCP 工具。完整参数、返回结构与错误码请参见 [`docs/mcp-tools.md`](./docs/mcp-tools.md)。

| 分类 | 工具列表 |
|------|---------|
| 会话容器 | `session_start`, `session_list`, `session_info`, `session_terminate`（关闭，保留 DEAD 条目可读）、`session_delete`（彻底删除） |
| 终端通道 | `shell_open`, `shell_list`, `shell_close`, `shell_input`, `shell_key`, `shell_output`, `shell_resize`, `shell_reader_register`, `shell_reader_unregister` |
| 通知 | `shell_notify`（唤醒 AI Agent）、`notify_user`（弹窗提醒 Web UI 用户） |
| 连接配置 | `ssh_config`（`list`；启动带 `--mcp-manage-ssh-configs` 时支持 `create`/`edit`/`copy`/`delete`） |
| 端口转发 | `forward`（`-L` / `-R` / `-D` / 列表 / 关闭） |
| 文件操作（SFTP） | `file_read`, `file_write`, `file_stat`, `file_delete`, `file_rename`, `file_mkdir`, `file_urls`, `file_perm`, `file_link`, `file_fs`, `file_getwd` |
| 消息记录 | `message`（列表 / 获取） |
| 宿主探测 | `shell_detect` |

执行一行命令的标准做法为：`shell_input` 输入文本 + `shell_key(key="enter")` 按回车 + `shell_output` 读取输出。调用失败时返回带有 `error_code` 稳定错误码的结构化 JSON。

## 已知限制与安全模型

- **文件与转发操作需活跃连接**：在已关闭（DEAD）的会话上调用文件或转发工具将返回 `session_not_running` 错误码；终端输出仍可通过 `shell_output` 读取，且会话转入 DEAD 时其端口转发会自动级联关闭。
- **Basic 认证在局域网外需要 TLS**：浏览器登录框走 HTTP Basic，凭据只是 Base64 编码。把 termcp 暴露到可信局域网之外时，请在前面部署终止 TLS 的反向代理；静态 Token 本身不会被写入日志，也不会出现在 URL 中。

### 🚨 安全边界：termcp 不负责安全防范（它只是管道，不是杀软）

> **核心原则：termcp 是纯透明的终端字节管道（Byte Pipe），绝不是杀毒软件（Antivirus）、EDR 或应用防火墙；安全防线必须由调用方建立在 AI 输出端与业务网关。**

termcp 具备与系统真实终端完全一致的自由度与控制力。**作为底层管道，termcp 既无能力、也不可能替你判定执行内容的安全性**：

- **无法防范“上传并执行”恶意行为**：AI 可以通过 Base64 解码、分段追加写入文件、或调用系统现成的 `curl`/`wget` 从外部拉取脚本或二进制 Payload 并赋予执行权限。**termcp 是数据流通道，不是病毒查杀引擎**，它不可能去扫描流经管道的每个字节是不是木马。
- **无法通过简单正则断定命令意图**：危险指令可以通过各种方式混淆（变量切片拼接 `a="rm -"; b="rf /"; $a$b`、动态 `eval`、`printf` 展开、环境变量替换、甚至写入临时文件后执行）。在 PTY 视界中，一切输入都只是合法的键盘敲击序列，底层管道无法区分这是“混淆攻击”还是“正常的前端/运维脚本”。
- **安全防线必须前置在 AI 输出端**：
  - 调用方（宿主、Agent 框架）必须在 AI 触发 `shell_input`、`file_write` 等操作**之前**，于外部部署 Guardrails、敏感词审查、高危命令合规拦截或安全大模型。
  - **关键操作坚持人工在环（Human-in-the-loop）**：termcp 提供了 Web UI 实时同屏与一键接管机制。遇到 `sudo`、破坏性指令、格式化、不可逆数据修改等操作时，切勿在无人值守的生产环境完全信任 AI，请务必人工介入确认。

---

