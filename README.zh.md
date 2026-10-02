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
    <img src="https://readme-typing-svg.demolab.com?font=Noto+Sans+SC&weight=700&size=20&pause=900&color=2786FF&center=true&vCenter=true&width=860&height=45&lines=%E4%B8%80%E4%B8%AA+AI+Native+%E7%9A%84%E7%BB%88%E7%AB%AF%E5%B9%B3%E5%8F%B0;%E8%B7%A8%E5%B9%B3%E5%8F%B0+%C2%B7+%E5%8F%AF%E8%A7%86%E5%8C%96+%C2%B7+%E4%BA%BA%E6%9C%BA%E5%8D%8F%E4%BD%9C;%E4%B8%80%E4%B8%AA%E7%AB%AF%E5%8F%A3%EF%BC%8C%E5%9B%9B%E4%B8%AA%E5%85%A5%E5%8F%A3;%E7%94%A8+MCP+%E4%B8%8E+SKILLS+%E9%A9%B1%E5%8A%A8%E7%9C%9F%E5%AE%9E%E7%BB%88%E7%AB%AF" alt="Termcp 标语">
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
  <img src="https://img.shields.io/badge/%E5%B9%B3%E5%8F%B0-macOS%20%7C%20Linux%20%7C%20Windows-2786ff?style=for-the-badge" alt="平台">
  <img src="https://img.shields.io/badge/Go-Pure%20Go%20%7C%20No%20CGO-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Pure Go No CGO">
  <a href="./LICENSE">
    <img src="https://img.shields.io/badge/License-MIT-22c55e?style=for-the-badge&logo=opensourceinitiative&logoColor=white" alt="MIT License">
  </a>
</p>

<p align="center">
  <strong>中文</strong> | <a href="./README.md">English</a>
</p>

<p align="center">
  <a href="#功能特性"><img src="https://img.shields.io/badge/%E5%8A%9F%E8%83%BD%E7%89%B9%E6%80%A7-2786ff?style=flat-square" alt="功能特性"></a>
  <a href="#快速开始"><img src="https://img.shields.io/badge/%E5%BF%AB%E9%80%9F%E5%BC%80%E5%A7%8B-2786ff?style=flat-square" alt="快速开始"></a>
  <a href="#使用"><img src="https://img.shields.io/badge/%E4%BD%BF%E7%94%A8-2786ff?style=flat-square" alt="使用"></a>
  <a href="#docker-部署"><img src="https://img.shields.io/badge/Docker%20%E9%83%A8%E7%BD%B2-2786ff?style=flat-square" alt="Docker 部署"></a>
  <a href="#接入-ai-客户端mcp"><img src="https://img.shields.io/badge/MCP-6E4AFF?style=flat-square" alt="MCP"></a>
  <a href="#agent-skill纯-curl无需-mcp"><img src="https://img.shields.io/badge/Skill-6E4AFF?style=flat-square" alt="Skill"></a>
  <a href="#接入脚本--程序rest-api"><img src="https://img.shields.io/badge/REST%20API-6E4AFF?style=flat-square" alt="REST API"></a>
  <a href="#工具参考"><img src="https://img.shields.io/badge/%E5%B7%A5%E5%85%B7%E5%8F%82%E8%80%83-00ADD8?style=flat-square" alt="工具参考"></a>
  <a href="#已知限制与安全模型"><img src="https://img.shields.io/badge/%E5%AE%89%E5%85%A8%E6%A8%A1%E5%9E%8B-FF69B4?style=flat-square" alt="安全模型"></a>
</p>

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=rect&color=0:2786FF,100:FF69B4&height=3&section=header" width="100%" alt="divider">
</p>

## 简介

Termcp 是一款 AI Native 的终端平台：多主机、多会话同时管理，全程可视化。这些会话由人和 AI 共同管理、共同维护，随时可以互相接管与交接；连接配置由平台独立维护，Agent 无需读取凭据即可使用。

- **人** —— 浏览器实时查看、操作、接管任何会话；
- **AI Agent** —— 通过 MCP 或 SKILLS 驱动同一批终端；
- **脚本 / 程序** —— 通过 REST API 编程化接入。

平台层提供长驻会话与只读回放、多主机多会话并行编排、SSH 连接全生命周期管理，让整个过程**可观测、可编程、人机接力**。跨平台、云原生、纯 Go 无 CGO；单二进制、低开销、可长期驻留。

### 演示视频

https://github.com/user-attachments/assets/d06a3c36-250a-4eeb-aefa-e80d13d1551c

## 为什么选 Termcp

### 多会话可视化管理

功能强大的 Web UI 把多主机、多会话集中到一个界面里管理：本地一条命令启动，或容器化部署到云端，浏览器访问的都是同一套操作界面。

![pic2_zh](https://github.com/user-attachments/assets/8f7a1c91-c75a-4717-a940-546029a77239)

- **多会话仪表盘**：所有运行中的会话按名称列出，随时切换、随时接管。
- **实时行为观测**：像操作本地终端一样，在浏览器里看 `htop` 的动态界面、`vim` 的编辑过程、安装程序的彩色提示。
- **标签化与平铺工作区**：一个 SSH 会话下可开多个 shell，各占一个标签；多个会话也可平铺展示，同时跟踪。
- **端口转发可视化**：会话相关的本地/远程端口与协议一目了然。
- **文件管理**：浏览目录、上传下载、重命名、建目录，都在管理界面里完成。
- **连接模板集中托管**：统一 SSH 配置管理；AI 开启会话时只指定配置名，读不到具体配置。
- **已关闭会话只读回放**：会话关闭、崩溃或重启后，完整输出仍可翻阅。

### AI Native 设计

无缝人机交互、结对操作：Agent 是终端的常驻用户，与你和脚本并列。

Agent 原生只能执行一次性命令，而真实工作大量是**多轮交互**（SSH 登录先输密码、Python REPL 逐行调试、回答安装程序的 `[Y/n]` 提示、驱动 `top`/`htop`/impacket）。Termcp 把真实终端直接交给 Agent：会话持续复用，**TUI**、**REPL**、**GDB**、**msfconsole**、**vim** 都能像人一样被持续管理——走 MCP，或用实例自带的 [Agent Skill](#agent-skill纯-curl无需-mcp) 走纯 `curl`。

![pic1_zh](https://github.com/user-attachments/assets/ae16e00f-e6cd-4ddc-867a-0bbe55c1b85f)

![1](https://github.com/user-attachments/assets/a0f4cfc1-4b73-4725-b8e8-97b85e19583b)

- **同一套会话层，平级入口。** MCP、SKILLS、REST/WebSocket 与 Web UI 同处一层，共用同一批真实会话。Agent 的每一步操作，你在浏览器里都看得见、随时能接管；反过来，Agent 需要时也可以停下来，把密码/MFA 提示交给你输入。
- **为 token 与轮次预算设计。** 工具 schema 紧凑、支持按需延迟加载（见 [`docs/mcp-tools.md`](./docs/mcp-tools.md)）；`shell_output` 用 tail/offset 游标分页，模型上下文只载入你真正需要的输出；`shell_notify` 只发唤醒信号。
- **实例自描述。** 每个运行中的 Termcp 都对外提供自己的 `/api.md` 与 `/skills.md`（免 token），并注册为 MCP resources 与 `learn-api` prompt；新 Agent 单单靠这两个文件就能驱动这个实例的当前版本。
- **密钥留在平台侧。** 经 `ssh_config` 写入的密码、私钥、口令仅保存在平台侧，MCP 的读取接口只返回配置名；SSH 配置写入工具默认关闭，需运维显式开启 `--mcp-manage-ssh-configs`。
- **失败可恢复。** 关闭、崩溃或重启过的会话仍以只读 DEAD 条目留在会话列表里，输出依旧可读，Agent（或你）可以接着中断前的状态继续；重连同一个 `termcp://<entry>` 即可开启下一段会话。
- **Agent 知道该把人指向哪里。** 客户端连进来的那个地址挂在 `notify_user` 的工具描述上，Agent 因此能报出确切的 Web UI 网址，而不是只说“打开 Web UI”让人自己去找。这是唯一保证送达的通道：客户端丢掉工具列表就根本调不了任何工具；而 `initialize` 的 instructions 在 MCP 里是可选的、很多客户端直接丢弃。地址取自请求本身——客户端连的那个主机名（或 `X-Forwarded-Host`），协议由 TLS 或 `X-Forwarded-Proto` 判定——所以局域网 IP、以及保留 `Host` 或设置转发头的反代都不会错；反代把 `Host` 改写成内网名时应改设 `X-Forwarded-Host`。`termcp stdio` 桥走回环，此时公布的地址也就是回环地址。
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
- **🌐 Web UI 多语言支持** —— 界面按浏览器语言自动选择，也可在标题栏手动切换；选择会被记住，切换语言不刷新页面、不重建已打开的终端。
- **🔍 可开启审阅模式** —— 审阅模式下 AI 的命令执行和文件改动需人工批准才会执行，用于生产环境。
- **🔒 凭据安全** —— 经 `ssh_config` 写入的密码、私钥、口令一律不可读回，明文凭据不进入 Agent 上下文；配置写入类工具默认关闭，需显式开启 `--mcp-manage-ssh-configs`。

## 快速开始

### 快速安装（需要 Go 环境）

最省事的方式 —— 一条命令搞定，无需克隆、无需编译：

```bash
go install github.com/open-mcp-ai/termcp@latest
```

`go install` 会通过 Go 模块代理拉取（中国大陆可用 `GOPROXY=https://goproxy.cn,direct`），把 `termcp` 二进制放到 `$(go env GOPATH)/bin`，请确保该目录在 `PATH` 中。Termcp 用 Go 编写，安装方式就是 `go install` 或 Releases 预编译二进制，不需要 Node/Python 等运行时。作为 Go module，它还支持**源码级集成**：可 `go get github.com/open-mcp-ai/termcp` 作为依赖引入，或 fork 源码构建定制版本。随后直接运行：

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

# 纯 API 编译（-tags no_webui）：只服务 REST / MCP / WebSocket。
# 不嵌入也不服务 Web 界面（GET / 返回 404）；/api.md、/skills.md 保留。
CGO_ENABLED=0 go build -tags no_webui -o termcp-api .

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
| `--assets`      | `~/.termcp/assets` | 外部静态资源目录（Web UI 与文档）。目录中存在的文件覆盖内嵌副本，缺失的文件回退内嵌；目录不存在属正常情况，行为不变。默认值可用 `$TERMCP_ASSETS_DIR` 覆盖。 |
| `--mcp-manage-ssh-configs` | `false` | 允许 AI 通过 MCP 管理 SSH 配置（凭据永不暴露）。                |
| `--auth-token`  | *(未设置)*  | HTTP 认证静态 Token（或 `$TERMCP_AUTH_TOKEN`）。API、MCP、浏览器全部客户端都须携带。与 `--auth-hash` 互斥。 |
| `--auth-hash`   | *(未设置)*  | Token 的 salted SHA-256 哈希（`sha256-<salt_hex>-<digest_hex>`），服务端不保存明文（或 `$TERMCP_AUTH_HASH`）。用 `termcp --gen-auth-hash` 生成。哈希配置的实例同时接受哈希串本身作为凭据——只留了哈希也能驱动 `termcp daemon` 与 `termcp stdio`——因此哈希须按机密对待。与 `--auth-token` 互斥。 |
| `--disable-auth` | `false` | **主动**关闭 HTTP 认证，非 loopback 绑定也放行（或 `$TERMCP_DISABLE_AUTH_TOKEN=1`）。建议同时把端口限定在 loopback，只让本机访问。与 `--auth-token`/`--auth-hash` 同时出现会直接报错，不会静默取其一。 |
| `--mcp-defer-tools` | `false` | 给低频工具（`file_*`、`forward`、`shell_resize` 等）打上 `defer_loading` 标记，让客户端按需拉取 schema，缩小首次 `tools/list`。默认关闭：不认识该标记的客户端、或被网关丢弃标记的链路（如 Codex 经 AxonHub），会干脆看不到这些工具。详见[工具懒加载](#工具懒加载)。 |
| `--idle-timeout` | `30s / 不限时` | 守护实例在无任何连接或请求后自行退出前的等待时长，如 `10m`；`0` 关闭。默认值：`termcp daemon stdio` 拉起的实例 30 秒，`termcp daemon start` 不限时。 |
| `--gen-auth-hash` | *(action)* | 生成 token 的 salted SHA-256 哈希（供 `--auth-hash` 使用）后退出；token 取自参数，或不带参数时从终端 stdin 无回显读取。 |
| `--version`     | *(action)*  | 打印版本、commit 与构建时间后退出。`make build` / release 构建通过 `-ldflags` 注入：HEAD 正好带 tag 时用该 tag，否则为 `dev-<commit>`，工作区有改动再缀 `-dirty`；直接 `go build` 或 `go install module@vX.Y.Z` 时回退到 Go 工具链嵌入的模块版本（即 release tag，或本地检出对应的伪版本）。`dev` 表示二进制里根本没有版本信息——`go run`、`-buildvcs=false`、解包后的源码包——不代表源码未打 tag。 |

除 flag 外，`termcp` 还提供以下子命令：

| 命令 | 作用 |
|---|---|
| `stdio` | 以 stdin/stdout 提供 MCP，把每条消息转发到 `--host`/`--port` 的 HTTP MCP 端点——一条独立命令，面向已在应答的实例（从不自己拉起实例）。可选端点值直接跟在 `stdio` 后：`sse`/`/sse` 走 SSE 传输，`stream`/`/stream` 走 streamable 传输（默认），完整的 `http(s)://` URL 指向任意 MCP HTTP 端点（路径以 `/sse` 结尾即 SSE 传输）。daemon 子命令的 `stdio` 动作则是组合形式：先确保实例在跑、再进桥。 |
| `daemon` | 管理在 `--host`/`--port` 上应答的实例；不带动作时列出可用动作，完整参数与细节用 `termcp daemon --help`。 |
| `daemon start` | 确保后台实例在跑：端点上有实例应答就复用（手动起的那个也算），没有就拉起一个分离的后台实例并等它就绪。实例会一直运行到被停止——不传 `--idle-timeout` 就没有空闲倒计时。 |
| `daemon stdio` | 和 `start` 一样确保实例在跑，然后本进程留在前台当 stdio 桥（可选端点值直接跟在动作后，如 `termcp daemon stdio sse`）。它拉起的实例在桥不在时开始倒计时。 |
| `daemon stop` | 请后台实例优雅停机；只有守护实例接受，手动起的实例会提示在原处停止。 |
| `daemon status` | 汇报实例的 pid / URL / 版本 / 日志路径。被动查询——它自己的请求被空闲倒计时豁免，所以“问一句”不会把被问的实例一直吊着。 |

子命令写在最前、flag 再跟在后面；daemon 的动作紧跟子命令（如 `termcp daemon start --port 9000`），两个 stdio 形式的可选端点值也写在名字后（`termcp stdio sse`、`termcp daemon stdio sse`）。daemon 的每个动作都只通过 HTTP 端点找实例，不查进程表——实例在哪里监听就能在哪里找到。用法见[接入 AI 客户端](#接入-ai-客户端mcp)的方式 C。

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

需要明确无认证运行（录屏、演示、单用户工作站）时用 `--disable-auth` 或 `TERMCP_DISABLE_AUTH_TOKEN=1`，它会放行非 loopback 绑定并把启动日志改为警告；但同时提供 Token/哈希会被视为矛盾配置直接报错。

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
- 浏览器走的是 HTTP Basic，只是 Base64 编码而非加密。对外提供服务时请在 Termcp 前面用反向代理终止 TLS；此时仅当请求本身来自 TLS 时 `termcp_token` cookie 才会自动带上 `Secure` 标志。

### 连接远程主机

零配置：`ssh_config="internal"` 直接操作 Termcp 本机。要连远程机器，在 Web UI 新建连接对话框创建 SSH profile（内置 TOML 模板与「测试连接」按钮），或通过 REST `PUT /api/connections/<name>` 提交 TOML：

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

profile 存放在 `data-dir/ssh_configs/<name>/config.toml`，可用 `ssh_config(action=list)` 查询；按此方式写入的凭据一律不可读回。Agent 也能创建 profile，但仅在 Termcp 以 `--mcp-manage-ssh-configs` 启动时可用。

## Docker 部署

### 运行官方镜像

官方镜像以非 root 用户 `termcp`（uid/gid 1000）运行，`/home/termcp` 声明为 `VOLUME`——全部状态（会话、SSH 配置、终端记录）默认存于 `~/.termcp`。镜像只携带二进制：不内置 entrypoint、不预声明端口，监听地址由运行命令决定。

```bash
docker run -d --name termcp -p 18765:18765 -v termcp-data:/home/termcp -e TERMCP_AUTH_TOKEN=change-me-to-a-long-random-secret ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765
```

> shell 示例均为单行：`\` 续行在 bash 里有效，但在 PowerShell 里是语法错误；单行命令可原样粘贴到 bash、zsh 与 PowerShell。

`--host 0.0.0.0` 使容器可被外部访问，因此必须提供认证 token。MCP 端点：`http://localhost:18765/stream`。改用 bind mount 时，先对宿主目录执行 `chown -R 1000:1000 /path/on/host`。

#### 不启用 Token 的 Docker 运行方式（仅限本机）

如果是临时演示、录屏、或单机自用，token 只是妨碍而没有任何保护价值。把端口只发布到**宿主 loopback**，并明确告知 Termcp 缺凭据是有意为之：

```bash
docker run -d --name termcp -p 127.0.0.1:18765:18765 -v termcp-data:/home/termcp ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765 --disable-auth
```

这里两个细节让它安全而不只是方便：`-p 127.0.0.1:18765:18765` 把发布端口绑在宿主 loopback 上，容器对本机可达、对局域网不可见（容器内仍必须监听 `0.0.0.0`，因为那是它网络命名空间之外唯一可路由的地址）；而 `--disable-auth` 之所以必需，正是因为 Termcp 拒绕在非 loopback 绑定上无认证启动——该 flag 就是运维主动承担责任，因此启动日志也从信息级降为警告级。等价的环变量写法是把 flag 换成 `-e TERMCP_DISABLE_AUTH_TOKEN=1`。

### 多阶段构建：添加到任意容器

把下面的 `Dockerfile` 放进应用项目：构建阶段用 `go install` 安装 Termcp，再用 `COPY --from` 把二进制复制进目标镜像——目标容器不需要 Go 运行时。

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

必须与原应用共用同一容器时，从原有 entrypoint 或进程管理器启动 Termcp；否则建议作为独立服务运行，通过 `http://termcp:18765/stream` 访问。

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

Termcp 在**同一端口（18765）同时支持两种 MCP 传输**，并为只能拉起本地子进程的客户端提供 **stdio 桥**（`termcp stdio`，见下方方式 C）；三种方式工具面完全一致。

Termcp 本身是常驻服务：同一端口同时服务 Web UI、任意数量的 MCP 客户端与会话持久化，没有本地 stdio *服务端*模式。对只认 stdio 的客户端，`termcp stdio` 是一条独立命令：进程的 stdin/stdout 承载 MCP JSON-RPC，每条消息转发到已在应答实例的 HTTP MCP 端点。`termcp daemon stdio` 则是组合命令——先确保分离的后台实例、再进桥；它拉起的实例空闲即自行退出，细节见下方方式 C。

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
- Open WebUI 在 Docker 内、Termcp 在宿主机：`http://host.docker.internal:18765/stream`（macOS/Windows），或宿主机局域网 IP。
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

### 方式 C —— stdio 桥（`termcp stdio`）

Claude Desktop 这类客户端只会拉起本地子进程。让子进程跑 `termcp daemon stdio`：一条命令先把后台实例拉起来，再进桥——桥把 stdin/stdout 上的 MCP 消息转发到实例的 HTTP 端点。

```json
{
  "mcpServers": {
    "termcp": {
      "command": "termcp",
      "args": ["daemon", "stdio"]
    }
  }
}
```

```bash
claude mcp add termcp -- termcp daemon stdio
```

这个动作是两半：先像 `daemon start` 一样确保实例在跑（见上文子命令表），再留在前台进桥。也可以分开：后台实例由你先安排好——`termcp daemon start`、直接 `termcp`、或交给服务管理器——客户端只跑 `termcp stdio`，桥接到已在应答的实例。（动作写错位置会被指回正确拼写：`termcp daemon start stdio` 会提示 `termcp daemon stdio`。）

桥接目标默认为 `http://<host>:<port>/stream`（streamable HTTP），由可选端点值改选：`termcp daemon stdio sse` 或 `termcp stdio sse`（`/sse` 同义）走 SSE 传输；完整的 `http(s)://` URL 指向任意 MCP HTTP 端点（路径以 `/sse` 结尾即 SSE）。只有落在 `--host`/`--port` 实例上的端点，才能这样和 `daemon stdio` 合成一条命令。

启动后：

- `daemon stdio`（与 `daemon start` 一样）先找 `--host`/`--port` 上应答的实例并复用——**包括你手动起的那个**（会提示它没有空闲倒计时、需在原处停止）——都没有才拉起一个分离的后台实例并等它就绪。
- 后台实例就是**完整 Termcp**：浏览器打开它的 URL（默认 `http://127.0.0.1:18765`）即可看到 agent 正在驱动的同一批会话——Web UI、REST 与 HTTP MCP 都可用，与桥并存。
- 桥在前台运行：stdin 按行读取，每条 MCP 消息转发到端点（默认 `/stream`；走 `/sse` 时先打开事件流、再 POST 到流里声明的消息地址），服务端回复（含通知）写回 stdout。状态信息全走 stderr，stdout 只承载 MCP 消息；管道批量输入后立即关闭（脚本场景，非交互客户端）也会先取回全部回复再退出。
- **它拉起的实例在无连接、无请求满 30 秒后自行退出**（`--idle-timeout 10m` 调整、`--idle-timeout 0` 关闭；直接 `termcp daemon start` 起来的实例例外——不传 `--idle-timeout` 就一直没有倒计时，运行到被停止为止）。桥在线就算活跃：MCP 会话建立前，桥以轻量心跳把倒计时一次次推迟；会话建立后由它打开的长连接接管——桥连着，实例就不退出，桥退出后重新开始倒计时。心跳间隔按实例**自己上报**的倒计时算（`GET /api/daemon` 带这个值），所以用 `--idle-timeout 6s` 之类短倒计时起来的旧实例同样吊得住，而不只是跑 30 秒默认值的那个。
- 日志在 `<data-dir>/termcp.log`（追加写，不轮转）。`termcp daemon status` 汇报 pid / URL / 版本 / 日志路径且不重置倒计时；`termcp daemon stop` 请守护实例优雅停机（`POST /api/daemon/stop`），手动起的实例需在原处停止。端点受认证保护时，管理命令要带上同一个 `--auth-token`。
- 认证贯穿所有命令：管理命令与桥按配置出示凭据——明文 token（`--auth-token` / `$TERMCP_AUTH_TOKEN`），或只保留了哈希时用哈希串本身（`--auth-hash` / `$TERMCP_AUTH_HASH`，哈希配置的实例接受它）。两种凭据都等同机密；拉起实例的动作（`termcp daemon start`、`termcp daemon stdio`）会把同一凭据交给后台实例。

### 速查

- Streamable HTTP → `http://<host>:18765/stream`
- SSE → `http://<host>:18765/sse`（JSON-RPC 走 `POST /message`）
- stdio（本地子进程）→ 还没实例时 `termcp daemon stdio`（顺带把实例拉起，桥退出后闲置即自行退出）；已有实例在应答时 `termcp stdio`；加 `sse`（`termcp daemon stdio sse`、`termcp stdio sse`）改走 SSE 传输（而非 `/stream`），或给完整 URL 指向任意 MCP HTTP 端点

Web UI 的 **API / MCP / SKILLS** 页面（`/api.html`）提供两种 HTTP 传输与 stdio 桥的可复制配置（地址随页面所在实例生成），以及本实例的 Agent 文档与 skill 下载地址。

## Agent Skill（纯 curl，无需 MCP）

不想配 MCP 客户端？实例自带一份可安装的 **Agent Skill**，让任意 agent 只用
`curl` 就能驱动 Termcp —— 包括识别用户从 Web UI 复制的 `termcp://` 定位符。

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

服务端以 `--auth-token` / `--auth-hash` 启动后，所有 MCP 请求都要带 `Authorization: Bearer` 请求头（哈希配置的服务端，同一请求头里放哈希串本身同样有效）：

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

Termcp 共提供 31 个 MCP 工具。完整参数、返回结构与错误码请参见 [`docs/mcp-tools.md`](./docs/mcp-tools.md)。

| 分类 | 工具列表 |
|------|---------|
| 会话容器 | `session_start`, `session_list`, `session_info`, `session_terminate`（关闭，保留 DEAD 条目可读）、`session_delete`（彻底删除） |
| 终端通道 | `shell_open`, `shell_list`, `shell_close`, `shell_input`, `shell_key`, `shell_output`, `shell_resize`, `shell_reader_register`, `shell_reader_unregister` |
| 通知 | `shell_notify`（唤醒 AI Agent）、`notify_user`（弹窗提醒 Web UI 用户） |
| 连接配置 | `ssh_config`（`list`；启动带 `--mcp-manage-ssh-configs` 时支持 `create`/`edit`/`copy`/`delete`） |
| 端口转发 | `forward`（`-L` / `-R` / `-D` / 列表 / 关闭） |
| 文件操作（SFTP） | `file_read`, `file_write`, `file_stat`, `file_delete`, `file_rename`, `file_mkdir`, `file_urls`, `file_perm`, `file_link`, `file_fs`, `file_getwd` |
| 输出区段索引 | `message`（列区段；字节用 `shell_output` 读） |
| 宿主探测 | `shell_detect` |

执行一行命令的标准做法为：`shell_input` 输入文本 + `shell_key(key="enter")` 按回车 + `shell_output` 读取输出。调用失败时返回带有 `error_code` 稳定错误码的结构化 JSON。

## 工具懒加载

MCP 客户端在 `tools/list` 时会拉取每个工具的 JSON Schema，工具多的服务就要为此付出上下文预算。MCP 规范留了一个口子：把低频工具标记为 `defer_loading`，客户端按需再拉 schema。Termcp 的 31 个工具分为热路径 **12 个**（会话生命周期 + 终端输入输出，永远立即可见）与低频宽面 **19 个**（11 个 SFTP `file_*`、`forward`、`shell_resize`/`shell_detect`/`shell_notify`、`shell_reader_register`/`shell_reader_unregister`、`message`、`ssh_config`）。

`--mcp-defer-tools` 才开启该标记，**默认关闭**：

- **默认**——全 31 个工具连同完整 schema 一次列出。这是所有不支持懒加载的客户端所需要的，包括经 AxonHub 这类网关访问 Termcp 的 Codex（网关可能丢掉 `defer_loading` 标记）。标记一旦丢失，这些工具无法再按需拉取，只会从模型视野里直接消失。
- **`--mcp-defer-tools`**——19 个低频工具带上 `defer_loading`；12 个核心工具保持立即可见，使 `session_start → shell_input → shell_output` 主循环永远不需要先搜工具。支持按需加载的客户端（mcp-go 系、Claude Code）只为自己真正用到的 schema 付费。

两种模式都是同样 31 个工具：开启开关从不删除工具，只影响首次列表是否附带 schema。

## 已知限制与安全模型

- **文件与转发操作需活跃连接**：在已关闭（DEAD）的会话上调用文件或转发工具将返回 `session_not_running` 错误码；终端输出仍可通过 `shell_output` 读取，且会话转入 DEAD 时其端口转发会自动级联关闭。
- **Basic 认证在局域网外需要 TLS**：浏览器登录框走 HTTP Basic，凭据只是 Base64 编码。把 Termcp 暴露到可信局域网之外时，请在前面部署终止 TLS 的反向代理；静态 Token 本身不会被写入日志，也不会出现在 URL 中。

### 🚨 安全边界

- **通常不要让 AI 介入生产环境；确需使用时，务必开启审阅模式。** 开启后 Agent 经 MCP 的每一次写入——终端输入、文件传输、端口转发——都要在 Web UI 里等人工批准或拒绝，强制每一次改动都人工在环（Human-in-the-loop）。
- **审阅模式不是万能安全。** 审阅模式需要人工确认，但脚本执行、文件上传都可能因复核者疏忽而被放行。

---

## Star 趋势

<p align="center">
  <a href="https://star-history.com/#open-mcp-ai/termcp&Date">
    <img src="https://api.star-history.com/svg?repos=open-mcp-ai/termcp&type=Date" alt="Star 趋势图" width="760">
  </a>
</p>

## 许可证

本项目基于 [MIT License](./LICENSE) 开源，可自由使用、修改与分发，只需保留版权与许可声明。感谢 [linux.do](https://linux.do/) 社区的支持与讨论。

---

<p align="right">
  <a href="#top">⬆️ 回到顶部</a>
</p>

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:2786FF,100:6E4AFF&height=110&section=footer" width="100%" alt="footer">
</p>
