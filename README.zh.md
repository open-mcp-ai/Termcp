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
    <img src="https://readme-typing-svg.demolab.com?font=JetBrains+Mono&weight=700&size=20&pause=900&color=2786FF&center=true&vCenter=true&width=860&height=45&lines=Let+AI+into+your+terminals" alt="Termcp tagline">
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
  <a href="#预览"><img src="https://img.shields.io/badge/预览-6E4AFF?style=flat-square" alt="预览"></a>
  <a href="#能力全景"><img src="https://img.shields.io/badge/能力-6E4AFF?style=flat-square" alt="能力"></a>
  <a href="#安全模型"><img src="https://img.shields.io/badge/安全-FF69B4?style=flat-square" alt="安全"></a>
  <a href="#文档"><img src="https://img.shields.io/badge/文档-00ADD8?style=flat-square" alt="文档"></a>
</p>

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=rect&color=0:2786FF,100:FF69B4&height=3&section=header" width="100%" alt="divider">
</p>

## 让 AI 进入你的诸多终端

轻量 AI Native 安全终端，可视化多界面，MCP+SKILLS+API

- **人机平等共驾** —— 双方写同一条终端，每一屏都实时看得见；接管、切换、协作。
- **细粒度会话控制** —— 文件管理、端口转发、多终端复用、读写终端窗口。
- **安装无依赖** —— 10m 左右单文件，多平台兼容，支持 npx 集成。

## 安装

### 1 · 一行命令接上你的 Agent

| Agent | 命令 |
| --- | --- |
| **Claude Code** | `claude mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio` |
| **Codex** | `codex mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio` |
| **Gemini CLI** | `gemini mcp add termcp npx -- -y @open-mcp-ai/termcp daemon stdio` |
| **Crush** | 写进 `~/.config/crush/crushrc`：`mcp add termcp --command npx --args -y --args @open-mcp-ai/termcp --args daemon --args stdio` |

### 2 · 或者这一个配置文件

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

这条命令就是完整的服务端：首次使用时它自己拉起后台实例，把 MCP 经 stdin/stdout 转发过去，实例空闲后自行退出。开箱即用。

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

## 预览

打开 **<http://127.0.0.1:18765>** —— 所有 Agent 的会话都在这里。

<p align="center">
  <img src="https://github.com/user-attachments/assets/622c5005-c293-46ef-a39f-e4c7863aac30" width="860" alt="webui">
</p>

每个 shell 的时间轴按来源标出每一行——绿色是 shell 输出、黄色是你的输入、粉色是 Agent 的。遇到 `sudo`、密码、MFA 或 `[Y/n]`，Agent 会调 `notify_user` 把提示交回给你；你在浏览器里输入密码，它留在模型上下文之外。

## 能力全景

| | |
| --- | --- |
| **会话** | 多条长驻 SSH 连接并存——连本机（`ssh_config="internal"`，零配置）或任意远程主机；每条会话持有多个 shell、转发与 SFTP。 |
| **终端** | 一条连接上并存完整的 `pty`（Windows 用 ConPTY）与逐行的 `pipe` 通道；任意行列数；输入、命名按键、resize。 |
| **输出** | 按字节游标读取（尾窗 / 偏移 / 实时读取器），从磁盘分页，重启后仍可回放；逐行时间轴标出每一行是谁写的。 |
| **文件** | SFTP 浏览、读取、写入、改名、删除、建目录、权限、链接，外加支持 Range 断点续传的 HTTP 直链。 |
| **转发** | `-L` 本地、`-R` 远程、`-D` SOCKS5，支持 ProxyJump 跳板链。 |
| **通知** | `shell_notify` 在退出 / 静默 / 新输出时叫醒 Agent；`notify_user` 带着实例 URL 通知人。 |
| **SSH 配置** | 中心化 TOML 存储，支持批量导入/导出、只存内存的临时主机、连接测试，以及编辑/复制/改名。 |
| **审阅模式** | 按会话开启的审批闸门：Agent 的每一次写入都等人裁决。 |
| **探测** | `shell_detect` 找出目标机自己的交互式 shell，而不是拿本机 `PATH` 去猜。 |
| **Agent Skill** | 实例自带 `/skills.md`，Agent 用纯 `curl` 即可驱动全部能力。 |

## 安全模型

- **凭据留在服务端。** 通过 `ssh_config` 写入的密码、私钥与口令只存在主机上，且不可读回；MCP 读接口只返回 profile 名。配置写工具默认关闭，除非设置 `--mcp-manage-ssh-configs`；`--no-internal` 把 Agent 限制在远程主机。
- **单个 token 保护所有面。** `--auth-token`，或用加盐的 `--auth-hash`（服务端不保存明文）——覆盖 Web UI、REST、两种 MCP 传输与 WebSocket。非 loopback 绑定**必须**配置。
- **Basic 认证是 Base64。** 把 Termcp 提供到自己机器之外时，请在前面用反向代理终止 TLS；token 从不进日志、也从不放进 URL。
- **🚨 不要把 Agent 独自放在生产主机上。必须如此时，打开审阅模式。** 它经 MCP 的每一次写入都会等人裁决。审阅模式是一道真实的闸门，不是安全保证：粗心的审阅者仍可能放行一条破坏性命令。
- **哪些操作需要活连接。** 在已关闭（DEAD）的会话上，文件与转发工具返回 `session_not_running`；输出读取照常可用，而该会话的端口转发会随它一起关闭。

## 文档

| | |
| --- | --- |
| [`docs/cli.md`](./docs/cli.md) | 全部 flag 与子命令、能力闸门、认证。 |
| [`docs/clients.md`](./docs/clients.md) | 四个入口的完整接法：MCP 传输、stdio 桥、Skill 安装、REST 示例。 |
| [`docs/deploy.md`](./docs/deploy.md) | Docker、多阶段构建、Compose、纯 API 构建。 |
| [`docs/mcp-tools.md`](./docs/mcp-tools.md) | 全部 31 个 MCP 工具：参数、返回形状、错误码。 |
| [`docs/api.md`](./docs/api.md) | REST 端点、WebSocket 帧、定位符解析。 |
| [`docs/architecture.md`](./docs/architecture.md) | 会话内核、四个入口、存储。 |
| [`docs/design/`](./docs/design) | 设计记录：Web UI、资源模型、会话存储、字体栈、移动端终端。 |
| [`CHANGELOG.md`](./CHANGELOG.md) | 版本记录。 |

## Star 趋势

<p align="center">
  <a href="https://star-history.com/#open-mcp-ai/termcp&Date">
    <img src="https://api.star-history.com/svg?repos=open-mcp-ai/termcp&type=Date" alt="Star History Chart" width="760">
  </a>
</p>

## 许可证

基于 [MIT License](./LICENSE) 发布。你可以自由使用、修改与分发，只需保留版权声明与许可声明。感谢 [linux.do](https://linux.do/) 社区的讨论与支持。

---

<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:2786FF,100:6E4AFF&height=110&section=footer" width="100%" alt="footer">
</p>

</div>
