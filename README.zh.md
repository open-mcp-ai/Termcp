<div id="top">



<p align="center">
    <img src="./docs/assets/logo.png"></img>
  <h1 align="center">termcp</h1>
  <p align="center"><em>让 AI Agent 拥有交互式终端能力。</em></p>
</p>



<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp/stargazers">
    <img src="https://img.shields.io/github/stars/open-mcp-ai/termcp?label=Stars&logo=github&style=for-the-badge" alt="Stars">
  </a>
  <a href="https://github.com/open-mcp-ai/termcp/forks">
    <img src="https://img.shields.io/github/forks/open-mcp-ai/termcp?label=Forks&logo=github&style=for-the-badge" alt="Forks">
  </a>
  <img src="https://img.shields.io/badge/平台-macOS%20%7C%20Linux%20%7C%20Windows-2786ff?style=for-the-badge" alt="平台">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go 1.25+">
  <a href="./LICENSE">
    <img src="https://img.shields.io/badge/license-MIT-green?style=for-the-badge" alt="MIT License">
  </a>
</p>



<p align="center">
  <strong>中文</strong> | <a href="./README.md">English</a>
</p>



---

## 简介

`termcp `是一个go语言编写的包含MCP服务器，以**SSH**会话的形式，让AI Agent能够持续地管理、调度交互式程序。此外，termcp还有专门用于管理这些会话的界面，使得用户能够完整地观测AI Agent的行为。同时用户能够像使用SSH一样，直接与受控机器进行交互或调整AI Agent行为。Go 编写、单二进制、低开销、可长期驻留；编译型 + goroutine 并发，高吞吐、低延迟。

### 演示视频

https://github.com/user-attachments/assets/d06a3c36-250a-4eeb-aefa-e80d13d1551c

## 为什么选 termcp

### 打破边界

Agent 原生只能执行一次性命令，运行完就返回。但现实中有大量工作是**多轮交互**的，例如：

- SSH 登录一台主机，先输密码，_再_执行命令。
- 在 Python REPL 里逐行调试代码。
- 回答安装程序里深埋的 `[Y/n]` 提示。
- 驱动 `top`、`htop`、或 impacket 这类终端依赖型工具。

这些场景里进程持续运行，Agent 必须在**多个对话轮次间读写进程的 I/O**。由此诞生了许多专门的MCP，但是为什么不直接赋予Agent双手，让他能够直接交互呢？`termcp`让 AI Agent打破了进程交互的边界，不再需要为每个交互工具安装编写单独的mcp，使其能够直接地持续管理、调度交互式程序，如**TUI**、**REPL**、**GDB**、**msfconsole**、**vim**等。

### 可视化管理

`termcp` 提供了一个会话管理界面，让你和 Agent 对进程里正在发生的一切一目了然:

- **多会话仪表盘**:所有正在运行的会话都在这里，以名称区分，随时切换，随时接管。
- **实时 AI Agent 行为观测**:像操作本地终端一样，直接在浏览器里看到 `htop` 的动态界面、`vim` 的编辑过程，或者安装程序弹出的彩色提示，不再对着"黑盒"猜测。
- **标签化管理**:一个 SSH 会话下可开多个操作shell，每个 shell 在 UI 里是独立标签页，Agent 在 A 标签调试、在 B 标签查日志，互不干扰。
- **端口转发可视化**：会话相关的所有端口转发等功能参数都列在面板里，本地/远程端口、协议一目了然。
- **文件管理**：在管理界面里直接浏览目录、上传下载、重命名、建目录。
- **连接模板集中托管**：提供统一的SSH配置管理，如果不想让Agent知道ssh具体配置，只需要告知Agent需要使用的ssh配置文件。

## 快速导航

- [功能特性](#功能特性)
- [快速开始](#快速开始)
- [使用](#使用)
- [Docker 部署](#docker-部署)
- [接入 MCP 客户端](#接入-mcp-客户端)
- [示例](#示例)
- [工具参考](#工具参考)
- [已知限制](#已知限制)

## 功能特性

- **⚡ 一行安装** —— `go install github.com/open-mcp-ai/termcp@latest`，无需克隆、无需编译，只需 Go 环境即可。
- **🔌 一行接入 MCP** —— 任何 MCP 客户端一行接入（`claude mcp add --transport http termcp http://localhost:18765/stream` 只是示例，Open WebUI 等其它客户端同样适用）；termcp 在同一端口同时提供 Streamable HTTP 与 SSE 两种传输，无需为每个工具单独集成。
- **🔗 资源 URL 寻址** —— 为配置、会话、频道定义了统一的 `termcp://` 寻址，Web UI 各处一键复制；把 URL 粘进与 AI 的对话，AI 即可准确定位并操作对应的连接、会话或频道。
- **🤝 人机接力，无缝切换** —— 人类与 Agent 共享同一个实时会话，可随时切换：你手动开好 root 权限的 shell，再交给 Agent 驱动；或 Agent 遇到 `sudo` / 密码 / MFA 提示时暂停，由你在 Web UI 里输入，Agent 继续执行。同一 shell 的输入串行写入，人机互不打断对方按键；凭据全程不由 Agent 猜测或回显；且你始终掌握控制权——Agent 出现不当操作时，可随时中断其操作。
- **🟦 支持多轮交互** —— 进程持续运行，Agent 可跨多个对话轮次驱动，而非一次性调用即返回。
- **🟪 真实终端环境** —— 完整模拟真实终端（PTY；Windows 下走 ConPTY），`vim`、`top`、`gdb` 等依赖终端特性的程序均可正常运行，跨平台兼容。
- **🟫 本机 / 远程任选入口** —— 既能零配置直接操作 termcp 所在本机（`ssh_config="internal"`），也能通过 SSH 接入任意远程主机，同一套工具流程通用。
- **🟧 内建可视化界面** —— 浏览器即可访问实时终端、会话列表、多 Shell 频道标签、平铺工作区、历史输出回放，单端口提供服务，无需额外部署。
- **🟨 多 Agent 并行不冲突** —— 多个 Agent 可同时读取同一会话，各自维护独立游标，输出互不抢占；统一的 `shell_output` 游标在活会话、死亡会话与归档会话上语义完全一致（字节 offset、`tail_lines`、翻页）。
- **🟩 远程操作一体集成** —— 单条 SSH 连接内完成命令执行、文件传输（完整 SFTP 套件 + 带 Range 断点续传的 HTTP 直链）、端口转发（`-L` / `-R` / `-D`），无需重复建立连接。
- **🟥 主动通知 AI Agent（推送，免轮询）** —— `shell_notify` 注册后由 termcp **主动通知 AI Agent**：进程退出 / 输出停顿 / 有新输出时即刻推送唤醒信令（仅信令、不带内容，避免污染上下文），无需 Agent 持续轮询；`channel="sampling"` 时还会主动发送 MCP `sampling/createMessage` 直接唤起模型。命令完成判定基于进程退出事件，而非固定超时。Web UI 可查看与拆下已注册规则。
- **🟨 断开 ≠ 删除，历史全保留** —— 正常退出或异常断线的会话自动归档，完整终端输出落盘保留，跨 termcp 重启仍可检索、重命名、打标签、渲染为 PNG 终端截图，仅显式删除才真正清理。
- **🔒 凭据安全设计** —— 通过 `ssh_config` 写入的密码、私钥、口令一律不可读回，明文凭据永不出现在 Agent 上下文中；配置写入类工具默认关闭，需显式开启 `--mcp-manage-ssh-configs`。
- **🛡️ 单一静态 Token 鉴权** —— 一个静态 Token 保护整个 HTTP 面：Web UI、REST API、MCP SSE、MCP Streamable HTTP 和 WebSocket。可配置 Token 本身（`--auth-token` / `TERMCP_AUTH_TOKEN`），也可只配置 salted SHA-256 哈希（`--auth-hash` / `TERMCP_AUTH_HASH`，用 `termcp --gen-auth-hash` 生成），服务端不保存明文配置。监听非 loopback 地址时未配置认证会拒绝启动。
- **🪶 上下文友好，省 Token** —— `shell_notify` 只推送唤醒信令（不带内容），终端正文通过 `shell_output` 按需拉取，避免原始输出灌满模型上下文。

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

# 编译
go build -o termcp .

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
| `--port`        | `18765`     | HTTP 端口。Web UI、MCP SSE、MCP streamable HTTP 共用。       |
| `--data-dir`    | `~/.termcp` | 持久化目录（会话、消息、SSH 配置）。不存在则自动创建。默认值可用环境变量 `$TERMCP_DATA_DIR` 覆盖。 |
| `--log-level`   | `info`      | 日志级别：`debug` / `info` / `warn` / `error`。`debug` 显示全部 MCP 工具调用；失败的工具调用与会话创建错误始终以 `warn`/`error` 打印。 |
| `--no-internal` | `false`     | 禁用内建 loopback SSH profile。                                |
| `--mcp-manage-ssh-configs` | `false` | 允许 AI 通过 MCP 管理 SSH 配置（凭据永不暴露）。                |
| `--auth-token`  | *(未设置)*  | HTTP 认证静态 Token（或 `$TERMCP_AUTH_TOKEN`）。API、MCP、浏览器全部客户端都须携带。与 `--auth-hash` 互斥。 |
| `--auth-hash`   | *(未设置)*  | Token 的 salted SHA-256 哈希（`sha256-<salt_hex>-<digest_hex>`），服务端不保存明文（或 `$TERMCP_AUTH_HASH`）。用 `termcp --gen-auth-hash` 生成。与 `--auth-token` 互斥。 |
| `--gen-auth-hash` | *(action)* | 生成 token 的 salted SHA-256 哈希（供 `--auth-hash` 使用）后退出；token 取自参数，或不带参数时从终端 stdin 无回显读取。 |

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

单一静态 Token 保护整个 HTTP 面——Web UI、REST API、MCP SSE、MCP Streamable HTTP 与浏览器 WebSocket。仅监听 loopback（`127.0.0.1`）时可保持零配置默认；绑定非 loopback 而未配置 Token 会直接启动失败。

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

### 多阶段构建：添加到任意容器

将下面的 `Dockerfile` 放到应用项目中。构建阶段通过 `go install` 安装 termcp，再用 `COPY --from` 把二进制文件复制到目标镜像；目标容器不需要安装 Go 运行时：

```dockerfile
# syntax=docker/dockerfile:1

# 可在构建时替换为可访问的 Go 基础镜像
ARG GO_IMAGE=golang:1.25-alpine
FROM ${GO_IMAGE} AS termcp-build

# Go 模块加速；海外环境可改为 https://proxy.golang.org,direct
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
ENV GOBIN=/out

# 生产环境建议将 latest 固定为具体版本，例如 @vX.Y.Z
RUN go install github.com/open-mcp-ai/termcp@latest

# 替换为任意目标基础镜像
FROM alpine
COPY --from=termcp-build /out/termcp /usr/local/bin/termcp
```

> `go install` 会从 Go 模块代理下载 termcp 及其依赖。`GOPROXY` 默认使用 `goproxy.cn`；也可以通过 `--build-arg GOPROXY=...` 替换。若 Docker Hub 访问较慢，可通过 `--build-arg GO_IMAGE=...` 指定可用的 Go 基础镜像镜像源。

### 启动命令示例

容器内必须监听 `0.0.0.0`，而非 loopback 监听**必须配置认证**；通过 `TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH` 或对应 flag 传入，否则启动失败。

```bash
# 构建包含 termcp 的应用镜像
# 也可以同时指定企业内网或其他可用的 GOPROXY / Go 基础镜像
docker build \
  --build-arg GOPROXY=https://goproxy.cn,direct \
  -t my-app-with-termcp .

# 以 termcp 作为容器主进程
# 数据目录挂载为持久化卷；通过环境变量配置认证 Token
docker run -d --name my-app-termcp \
  -p 18765:18765 \
  -v termcp-data:/data \
  -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret \
  --entrypoint /usr/local/bin/termcp \
  my-app-with-termcp \
  --host 0.0.0.0 --port 18765 --data-dir /data

# 开启 MCP SSH 配置写入工具（按需使用）
docker run -d --name my-app-termcp \
  -p 18765:18765 -v termcp-data:/data \
  -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret \
  --entrypoint /usr/local/bin/termcp \
  my-app-with-termcp \
  --host 0.0.0.0 --data-dir /data --mcp-manage-ssh-configs

# 查看日志
docker logs -f my-app-termcp
```

如果需要与原应用进程在同一个容器中同时运行，应在原有 entrypoint 或进程管理器中启动：

```bash
export TERMCP_AUTH_TOKEN="change-me-to-a-long-random-secret"
/usr/local/bin/termcp --host 0.0.0.0 --port 18765 --data-dir /data
```

Docker 容器通常只运行一个前台进程；若应用仍需作为主进程运行，建议将 termcp 放在同一 Docker 网络的独立服务中，并通过 `http://termcp:18765/stream` 访问。

### Docker Compose 启动

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

## 接入 MCP 客户端

termcp 在**同一端口（18765）同时支持两种 MCP 传输**，按客户端能力二选一即可，工具面完全一致。

termcp 是常驻服务：同一端口同时服务 Web UI、任意数量的 MCP 客户端与会话持久化，因此只提供 **HTTP 传输**（Streamable HTTP / SSE），**不支持 stdio**（没有本地子进程模式）。作为 MCP 服务器，它本身就是可嵌入任意 MCP 宿主（Claude Code、Cursor、Codex、Open WebUI 或自研客户端）的插件。

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

Web UI 的 **API / MCP** 页面（`/api.html`）提供两种传输的可复制配置。

### 开启认证时的接入

服务端以 `--auth-token` / `--auth-hash` 启动后，所有 MCP 请求都要带 `Authorization: Bearer` 请求头：

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

不要把 Token 放进 URL，也不要贴进共享配置或截图。`curl` 与脚本使用相同请求头：

```bash
curl -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" http://your-server:18765/api/sessions
```

## 工具参考

termcp 共提供 31 个 MCP 工具。完整参数、返回结构与错误码请参见 [`docs/mcp-tools.md`](./docs/mcp-tools.md)。

| 分类 | 工具列表 |
|------|---------|
| 会话容器 | `session_start`, `session_list`, `session_info`, `session_terminate` |
| 终端通道 | `shell_open`, `shell_list`, `shell_close`, `shell_input`, `shell_key`, `shell_output`, `shell_resize`, `shell_reader_register`, `shell_reader_unregister` |
| 通知 | `shell_notify`（唤醒 AI Agent）、`notify_user`（弹窗提醒 Web UI 用户） |
| 连接配置 | `ssh_config`（`list`；启动带 `--mcp-manage-ssh-configs` 时支持 `create`/`edit`/`copy`/`delete`） |
| 端口转发 | `forward`（`-L` / `-R` / `-D` / 列表 / 关闭） |
| 文件操作（SFTP） | `file_read`, `file_write`, `file_stat`, `file_delete`, `file_rename`, `file_mkdir`, `file_urls`, `file_perm`, `file_link`, `file_fs`, `file_getwd` |
| 历史与审计 | `history`（列表 / 消息搜索 / 重命名 / 备注标签 / 彻底清理 / 渲染截图）, `message`（列表 / 获取） |
| 宿主探测 | `shell_detect` |

执行一行命令的标准做法为：`shell_input` 输入文本 + `shell_key(key="enter")` 按回车 + `shell_output` 读取输出。调用失败时返回带有 `error_code` 稳定错误码的结构化 JSON。

## 已知限制

- **`history` 截图仅支持 ASCII 终端字符**：`history(action=screenshot)` 将持久化文本渲染为固定点阵终端图像，非 ASCII 字符可能无法高精度呈现。
- **文件与转发操作需活跃连接**：在已退出（DEAD）或归档的会话上调用文件或转发工具将返回 `session_not_running` 错误码；终端输出读取仍可通过 `shell_output` 进行。
- **断线不自动重连**：SSH 意外断线会被检测，会话置为 DEAD（`exited`）只读保留、输出不丢，但不会自动重连。需要时重新 `session_start`，或从历史中查看旧会话。
- **无命令白名单 / 目录限制**：termcp 不设命令白名单、路径限制或策略式风险分级。风险控制走**人工在环**：可在 Web UI 随时中断 AI 的操作；`sudo` / 密码 / MFA 提示默认交给你输入（Agent 遵循不猜测、不回显的约定，暂停等你输入），若你允许 Agent 代输也完全可以——termcp 不做禁止。
- **Basic 认证在局域网外需要 TLS**：浏览器登录框走 HTTP Basic，凭据只是 Base64 编码。把 termcp 暴露到可信局域网之外时，请在前面部署终止 TLS 的反向代理；静态 Token 本身不会被写入日志，也不会出现在 URL 中。

---

