# 接入 Termcp

Termcp 在同一个端口上同时提供四种入口，它们操作**同一批真实会话**：

| 入口 | 使用者 | 传输 |
| --- | --- | --- |
| Web UI | 人 | 浏览器（`/`） |
| MCP | AI Agent | Streamable HTTP（`/stream`）、SSE（`/sse`）、stdio 桥（本地子进程） |
| REST | AI Agent（纯 curl，无需 MCP） | `POST /api/term`（与 MCP `term` 同一套指令） |
| REST + WebSocket | 脚本 / 程序 | `/api/*`、`/api/ui/ws` |

四者共享一套会话内核，所以你在浏览器里看到的、Agent 正在驱动的、脚本在读的是同一条终端。指令空间由实例自述（`help`、`help <directive>`、空 `cmd` 返回引导），端点见 [`api.md`](./api.md)。

## Managed MCP tokens

首次运行先打开 `/login` 设置本机的登录密码与解密密码；在 `/permissions` 权限管理页选择或新建权限组，配置动作和主机范围，再添加主体并签发 MCP token；管理员也可以为自己签发 token。页面提供带 token 的配置片段和只读 DSL 权限预览。下文不含凭据的示例需补充相应 header/env，或用于显式旧共享认证/开放模式。

- HTTP `/stream` 和 SSE `/sse`：每个请求携带 `Authorization: Bearer <token>`（SSE 的 `/message` 请求也需要）。
- stdio / daemon stdio：MCP 配置的 `env` 写入 `TERMCP_MCP_TOKEN`。它是客户端凭据，不改变 daemon 的认证模式，也不会传给新启动的 daemon。
- 远端实例使用 `stdio <完整 HTTP(S) URL>`。`daemon stdio` 用于本机实例；自定义数据目录时，请补充相同的 `--data-dir`。生成片段采用常见 `mcpServers` JSON 格式；不同宿主的外层配置键可能需要调整，不依赖环境变量插值。
- 服务端仅保存摘要，不能再次查看原 token。轮换时为同一主体增发、更新客户端、确认成功后撤销旧 token。撤销一枚 token 不影响其他 token；停用主体或权限组会断开受影响 token 的连接。
- daemon 重启后需要先输入登录密码，再用解密密码解锁配置；客户端 token 不解锁 Main Key。OAuth 不在此版本实现。

## 按客户端接入

### 一条命令

| 客户端 | 命令 |
| --- | --- |
| **Claude Code** | `claude mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio` |
| **Codex** | `codex mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio` |
| **Gemini CLI** | `gemini mcp add termcp npx -- -y @open-mcp-ai/termcp daemon stdio`（`--` 必须，否则 `-y` 会被当成 gemini 自己的选项） |
| **Crush** | 写进 `~/.config/crush/crushrc`：`mcp add termcp --command npx --args -y --args @open-mcp-ai/termcp --args daemon --args stdio`（`mcp add` 是 crushrc 里的配置命令，不是 `crush` 的子命令） |

### 粘贴配置文件

| 配置文件 | 客户端 |
| --- | --- |
| `claude_desktop_config.json` —— `%APPDATA%\Claude\`（Windows）· `~/Library/Application Support/Claude/`（macOS） | Claude Desktop |
| `~/.cursor/mcp.json`（或项目内的 `.cursor/mcp.json`） | Cursor |
| `mcp.json` —— 命令 `MCP: Open User Configuration`，或 `.vscode/mcp.json` | VS Code / Copilot |
| `~/.config/opencode/opencode.json` | opencode |
| `<你的客户端的 MCP 配置>` | 其他任何说 MCP 的客户端 |

**前四个装的是同一段**（`npx` 意味着什么都不用先装，这就是整个服务端）：

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

装了二进制就把 `command` 换成 `termcp`、`args` 收成 `["daemon", "stdio"]`。

**VS Code** 外层键是 `servers`：

```json
{
  "servers": {
    "termcp": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@open-mcp-ai/termcp", "daemon", "stdio"]
    }
  }
}
```

**opencode** 外层键是 `mcp`，命令写成数组：

```json
{
  "mcp": {
    "termcp": {
      "type": "local",
      "command": ["npx", "-y", "@open-mcp-ai/termcp", "daemon", "stdio"],
      "enabled": true
    }
  }
}
```

`npx … daemon stdio` 就是完整的服务端：首次使用时它自己拉起后台实例，把 MCP 经 stdin/stdout 转发过去，实例空闲后自行退出。不需要先跑任何实例。

### 用 URL 而不是 stdio

需要有个实例在应答（跑一次 `npx -y @open-mcp-ai/termcp`）：

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

- 同一台机器 → `http://127.0.0.1:18765/stream`
- Termcp 在宿主机、客户端在 Docker → `http://host.docker.internal:18765/stream`（macOS/Windows），或宿主机的 LAN IP
- 两者都在 Docker 同一网络 → `http://termcp:18765/stream`
- 旧 SSE → `http://localhost:18765/sse`（**只**配 `/sse`；JSON-RPC 会自己 POST 到 `/message`）
- Gemini CLI 由它自己的 CLI 写入 `"url"` + `"type": "http"`
- opencode 的 URL 形式用 `"type": "remote"` + `"url"`

> **关于 `dsh`**：它是一个 profile launcher（`dsh --profile <name>`），自己没有 mcp 子命令——
> MCP 配置属于它启动的那个应用（如 `dsh-web-app`），所以按该应用的文档配（多数是上面那个 `mcpServers` 形状）。

## 认证

默认使用 managed 本机密码认证：第一次在本机访问 `/login` 只设置登录密码与解密密码，不再输入用户名，之后登录获得 HttpOnly JWT Cookie。顶部“登录与安全”入口 `/auth` 可进入登录设置和权限管理。登录校验记录、keyring、identity 和 EDK 位于 `<data-dir>/local-state/state.json`，加密正文按对象身份和代际保存在 `local-state/objects/`。浏览器会话不跨重启恢复。

登录密码只认证浏览器会话；保险库锁定时，登录后再在 `/auth` 输入解密密码。Agent token 不承担解锁，423 表示需要完成保险库解锁或中断重封续跑。“登录与安全”页改密码只改变登录密码并撤销浏览器登录；配置加密页改密码只重封 EDK，不改变登录密码、正文或 MCP token。旧布局验证后迁移并清理源文件，旧明文主机仍由用户选择是否转换。

托管模式写请求使用 `/auth/status` 返回的 CSRF 值，通过 `X-Termcp-CSRF` 发送；内置
WebUI 自动处理。首次设置只接受 loopback 连接及 loopback Host；远程部署可先通过
SSH 本地转发设置。`/api.md` 仍公开。API-only 构建同样提供必要的登录/设置入口。

当前已实现本机密码登录、角色入口权限、权限组/主体/凭据生命周期和逐操作 MCP 策略。权限管理页支持预置模板、三种效果（禁止/允许/需审批）、动作与参数条件、主机范围、主体绑定、token 签发/撤销、只读 DSL 预览和结构化审计。Cookie 写请求需要 CSRF，显式 MCP Bearer 不需要；Agent bearer 只能进入 MCP。OAuth、多个权限组继承和专属词汇尚未实现。无凭据 MCP 连接不能访问托管实例。未配置的 stdio daemon 有 10 分钟的初始化宽限，桥会报告设置地址并结束失败的接入，不在 MCP stdin 读取密码。手动 `daemon start` 默认常驻。

显式传入 `--auth-token` / `--auth-hash` 时，继续使用旧共享凭据模式，行为如下：

```bash
# 明文：flag 或环境变量
./termcp --auth-token "your-long-random-token"
TERMCP_AUTH_TOKEN="your-long-random-token" ./termcp

# 哈希（推荐）：服务端只保存 sha256-<salt>-<digest>。
# `termcp --gen-auth-hash` 在终端下无回显地从 stdin 读取 token，
# 因此它不会进入 shell 历史：
./termcp --gen-auth-hash
TERMCP_AUTH_HASH='sha256-...' ./termcp
```

各客户端如何出示 token：

| 客户端 | 凭据 |
| --- | --- |
| API / MCP / curl | `Authorization: Bearer <token>` 请求头 |
| 浏览器（Web UI） | `401` 时弹原生登录框——用户名被忽略（留空即可），**token 就是密码**。随后自动下发 `termcp_token` cookie，使同源的 WebSocket 握手也能认证。 |

行为说明：

- `--auth-token` 与 `--auth-hash` 互斥；同名设置的 flag 值覆盖环境变量。
- token 里含冒号没问题：服务端在解码后的整个 `user:pass` 串等于 token 时也接受，因此在第一个冒号处拆分的客户端（如 `curl -u user:pass`）照样通过。规范写法仍是 `curl -u :<token>`。
- 没有 token/hash 时选择托管认证，包括非 loopback 监听；未设置 Admin 或未登录的请求不能执行业务。
- `--disable-auth`（或 `TERMCP_DISABLE_AUTH_TOKEN=1`）显式解除该要求。它是 loopback 专用场景的逃生口——演示视频、录屏、单用户工作站——那里 token 保护不了什么。因为它是一次刻意的覆盖，与 `--auth-token`/`--auth-hash` 同时给出是启动错误而不是某个参数静默胜出，启动日志也从信息行改为警告。
- 浏览器用的是 HTTP Basic，它是 Base64 而非加密。把 Termcp 提供到自己机器之外时，请在它前面用反向代理终止 TLS——那时 `termcp_token` cookie 才会自动带上 `Secure` 标志（仅当请求经 TLS 到达）。

## 方式 A —— Streamable HTTP（`/stream`）

现代 MCP 传输；单一端点，没有单独的消息路径。Claude Code、Open WebUI 与大多数当前客户端用这个。

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

- 同一台机器：`http://127.0.0.1:18765/stream`。
- Open WebUI 在 Docker 里、Termcp 在宿主机：`http://host.docker.internal:18765/stream`（macOS/Windows），或宿主机的 LAN IP。
- 两者都在 Docker 同一网络：`http://termcp:18765/stream`。

## 方式 B —— SSE（`/sse`）

传统 SSE 传输。**只**配置 `/sse`；SDK 会自动把 JSON-RPC POST 到 `/message`。

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

## 方式 C —— stdio 桥（`termcp stdio`）

Claude Desktop 这类客户端只能拉起本地子进程。把它们指向 `termcp daemon stdio`：一条命令先确保后台实例起来，再进入桥——在 stdin/stdout 与实例的 HTTP 端点之间转发 MCP 消息。

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

这个动作是两半：像 `daemon start` 一样确保实例（见 [`cli.md`](./cli.md#子命令)），然后变成桥。两者也可以分开：你自己安排实例——`termcp daemon start`、裸 `termcp`，或一个服务管理器——再让客户端对一个已在应答的实例跑裸 `termcp stdio`。

桥的目标默认是 `http://<host>:<port>/stream`（streamable HTTP）；可选端点值另选：`termcp daemon stdio sse`，或 `termcp stdio sse`（`/sse` 也一样）走 SSE 传输；完整 `http(s)://` URL 指向任意 MCP HTTP 端点（路径以 `/sse` 结尾 = SSE）。只有落在 `--host`/`--port` 实例上的端点才能折叠进一条命令的 `daemon stdio` 形式。

启动之后：

- `daemon stdio`（与 `daemon start` 一样）复用 `--host`/`--port` 上应答的东西——**包括你手动启动的实例**（它会指出那个实例没有空闲倒计时、必须在它被启动的地方停止）——只在没有时才拉起一个**分离的后台实例**并等它就绪。
- 后台实例是**完整的 Termcp**：在浏览器打开它的 URL（默认 `http://127.0.0.1:18765`），就能旁观 Agent 正在驱动的同一批会话——Web UI、REST 与 HTTP MCP 都与桥并行可用。
- 桥在前台运行：逐行读 stdin，把每条 MCP 消息转发到端点（默认 `/stream`；走 `/sse` 时桥打开事件流并向流中给出的消息端点 POST），服务端回复（含通知）写回 stdout。状态行走 stderr；stdout 只承载 MCP 消息。整批写入后立刻关闭 stdin（脚本，而不是交互式客户端）也能在退出前取回全部回复。
- **它拉起的实例在空闲 30 秒后自行退出**——无连接、无请求（`--idle-timeout 10m` 调整，`--idle-timeout 0` 关闭；裸 `termcp daemon start` 起的实例不受此限：除非你传 `--idle-timeout`，它运行到被停止）。正在运行的桥算活动：在它的 MCP 会话存在之前，桥用轻量心跳把倒计时往外推；会话建立后由它建立的那条长连接接管——桥附着期间实例保持存活，桥退出后倒计时重新开始。心跳间隔跟随实例**上报**的倒计时（`GET /api/daemon` 带该值），所以一个先前以 `--idle-timeout 6s` 启动的实例也能被它顶住，而不只是跑 30 秒默认值的那种。
- 它的日志是 `<data-dir>/termcp.log`（追加写，不轮转）。`termcp daemon status` 汇报 pid / URL / 版本 / 日志路径且不重置倒计时；`termcp daemon stop` 请求守护实例优雅停机（`POST /api/daemon/stop`）——手动实例必须在它被启动的地方停止。面对旧共享认证端点时，给管理命令传同一凭据。
- 认证按入口区分：managed 模式的 MCP token 只进入 MCP，不能查看或停止 daemon；`daemon status` / `daemon stop` 会在交互终端中无回显询问本机登录密码，金库锁定时再询问解密密码。显式旧共享模式继续使用明文 token（`--auth-token` / `$TERMCP_AUTH_TOKEN`）或哈希串（`--auth-hash` / `$TERMCP_AUTH_HASH`）管理 daemon，桥也接受这些旧凭据。拉起动作会把旧共享凭据交给后台实例，但绝不会把 `TERMCP_MCP_TOKEN` 写成服务端认证。

## 速查

- Streamable HTTP → `http://<host>:18765/stream`
- SSE → `http://<host>:18765/sse`（JSON-RPC 发到 `POST /message`）
- stdio（本地子进程）→ `termcp daemon stdio`（什么都还没跑；它拉起实例，实例随后空闲退出），或对一个已在应答的实例跑 `termcp stdio`；加 `sse`（`termcp daemon stdio sse`、`termcp stdio sse`）改走 SSE 传输而不是 `/stream`，或给一个完整 URL 指向任意 MCP HTTP 端点
- npm 包装器（`@open-mcp-ai/termcp`）也可当 stdio 命令：`npx -y @open-mcp-ai/termcp daemon stdio`

Web UI 的 **API / MCP** 页面（`/api.html`）给出两种 HTTP 传输与 stdio 桥的可复制配置——地址跟随页面所在实例——外加本实例的 Agent 文档地址。

## 纯 curl 驱动（无需 MCP）

不想配 MCP 客户端时，用 REST 入口——**一个端点，一条指令行**，与 MCP 的 `term`
工具是同一套指令语言、同一个信封：

```bash
# 公开端点：读文档本身不需要 token
curl -fsS http://<host>:18765/api.md

# 一条命令：与 MCP 的 term(cmd:"ls") 完全等价
curl -sS -X POST http://<host>:18765/api/term -d '{"cmd":"ls"}'
```

指令空间由实例自述，不需要另写教程：

```bash
curl -sS -X POST http://<host>:18765/api/term -d '{}'              # 引导：这里有什么、我能做什么
curl -sS -X POST http://<host>:18765/api/term -d '{"cmd":"help"}'  # 全部指令
curl -sS -X POST http://<host>:18765/api/term -d '{"cmd":"help run"}'  # 单条指令的形状与坑
```

这三条回复都由与 MCP 工具同一张表生成，因此不会出现「文档说过时了」的情况。
定位符（`termcp://#<会话>:2`）与短句柄（`<会话>:2`）都能直接当目标用。命令失败是
**slot 里的失败**（HTTP 仍是 2xx），按 slot 读 `code`。

端到端一句「打开 termcp://rock64 并执行 `uname -a`」即可完成：`{"cmd":"pty
termcp://rock64"}` 建会话，然后 `{"cmd":"run <handle> $1","args":["uname -a"]}`
发命令并读回输出——**命令行走 `args` 值，不写进 `cmd`**（`run` 的 payload 只接一个变量引用）。

## 接入脚本 / 程序（REST API）

不通过 MCP 也能编程化使用同一套会话能力：完整的 REST API 与实时 WebSocket 通道。

```bash
# 列出会话（同样受 --auth-token 保护）
curl -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" http://127.0.0.1:18765/api/sessions

# 创建一个会话
curl -X POST http://127.0.0.1:18765/api/sessions -H "Authorization: Bearer $TERMCP_AUTH_TOKEN" -H 'Content-Type: application/json' -d '{"ssh_config":"internal","command":"bash","mode":"pty"}'

# 读取会话输出 / 上传下载文件 / 端口转发，见 docs/api.md
```

终端实时 I/O 走 `WebSocket /api/ui/ws`；文件支持 HTTP 直链（Range 断点续传）。完整端点见 [`api.md`](./api.md)。

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

不要把 Token 放进 URL，也不要贴进共享配置或截图。

## 工具面：一个入口

`tools/list` 只有一个工具 `term`（约 2.5 KB），参数是 `cmd` 加十二个窗口与替换参数。与上下文预算相关的三件事因此都不再是问题：

- **不再需要 `defer_loading`。** 那个机制解决的是「工具太多，先别给 schema」；一个工具没有可延后加载的部分。`--mcp-defer-tools` 与分类表已删除。
- **客户端不必理解任何标记。** 丢掉未知字段的网关（实测某些 Codex 链路会丢掉 `defer_loading`）只会丢掉一个不存在的字段，不会丢工具。
- **能力不随工具数增长。** 装一个 MCP server、一条技能扩展、十条别名之后，`tools/list` 逐字节不变；差异只出现在 `cmd` 的返回内容里（`ls ext` / `ls` 不同），不在工具面。
