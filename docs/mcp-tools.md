# MCP 工具参考

Termcp 通过 **SSE** 与 **Streamable HTTP** 两套对等传输暴露同一套 MCP 工具（同一监听端口）。

## 对接方式（传输）

| 传输 | 端点 | 配套 | 说明 |
|------|------|------|------|
| SSE | `GET /sse` | `POST /message` | 客户端只配置 `/sse`；SDK 自动用 `/message` 发 JSON-RPC |
| Streamable HTTP | `/stream` | — | 单路径；不要拼 `/sse` 或 `/message` |

- SSE：`http://<host>:18765/sse`（Claude：`--transport sse` / `type: "sse"`）
- Streamable HTTP：`http://<host>:18765/stream`（Claude：`--transport http` / `type: "http"`）

可复制配置与 HTTP API 速查：Web UI **`/api.html`**。完整客户端样例见 `README.md` / `README.zh.md`。同一批会话也可用实例自带的 Agent Skill 驱动（`/skills.md` 安装一次，配 `GET /api/resolve` 解析 `termcp://` 定位符，纯 curl）。

## Resources & Prompts

Besides tools, the server publishes its own HTTP API reference and curl skill as
MCP resources, and a `learn-api` prompt:

- Resources: `<origin>/api.md` (REST reference) and `<origin>/skills.md` (curl skill).
  URIs are the instance's real HTTP addresses, so the same string works for `curl`.
  (Tool arguments/results are described by the `tools/list` schemas themselves, so no
  separate tool reference document is served.)
- Prompt: `learn-api` (optional argument `task`) — primes an agent with `/api.md`
  and `/skills.md` before it scripts against Termcp over REST.

两个文档端点在开启鉴权后仍可**无凭据**获取（仅 GET/HEAD，纯静态、无数据）；其余所有面（REST/MCP/WS/Web UI）依旧要求 token。

## 工具懒加载（deferred tool loading）

Termcp 的 31 个工具按"热路径 / 低频面"分成两类。MCP 标准的 `defer_loading` 标记可以让客户端**按需加载**低频工具的 Schema，但这套机制**默认关闭**：

- **核心常驻**（永远不标记）：`session_start` / `session_list` / `session_info` / `session_terminate` / `session_delete` / `shell_open` / `shell_list` / `shell_close` / `shell_input` / `shell_key` / `shell_output` / `notify_user`。这些构成"开会话 → 打字 → 读输出"的主循环，若需先搜索才能用，每次交互都要多一个来回——**开启懒加载时它们依然立即可见**。
- **低频宽面**（19 个，`--mcp-defer-tools` 下才标记）：11 个 SFTP 文件工具、`forward`、`shell_resize` / `shell_detect` / `shell_notify` / `shell_reader_register` / `shell_reader_unregister`、`message`、`ssh_config`。这些工具参数面宽、调用频率低，客户端可按需检索后再拉取 Schema，节省每轮注入的上下文预算。

### 两种模式

| 模式 | `tools/list` 行为 |
|------|------------------|
| **默认** | 31 个工具全部返回完整 Schema，不打 `defer_loading` |
| **`--mcp-defer-tools`** | 12 个核心工具完整返回；19 个低频工具带 `defer_loading: true` |

两种模式都是**同样 31 个工具**，开关从不删除工具，只影响首次列表是否附带 Schema。

默认关闭的原因很实际：不认识 `defer_loading` 的客户端、或经网关转发而被丢弃标记的链路（实测 **Codex 0.155.1 经 AxonHub** 会把带标记的工具整个吞掉，模型侧看到"零工具"），会让这些工具**从模型视野里直接消失**，而不是"稍后能搜到"。只有确认客户端支持按需拉取（mcp-go 系、Claude Code）时才建议开启。

分类由 `internal/mcp/toolopts.go` 的 `deferredTools` 表定义，开关由 `Server.shouldDeferTool` 决定（`mcp.DeferTools()` option）；`TestDeferLoadingPolicy` 会分别校验两种模式，并拦住"新工具未分类"与"僵尸条目"。若要调整分类，改表即可，无需改各处注册代码。

**ID 规则（硬）：**

| 资源 | 参数名 | 谁用 |
|------|--------|------|
| Session（SSH 连接容器） | `session_id` | shell_open、forward、file_*、session_terminate、session_list、session_info |
| Shell（终端 channel） | `shell_id` | shell_input、shell_key、shell_output、shell_resize、shell_reader_register/unregister、shell_close |

`session_start` 返回 **两个不同** 的 id：`session_id` 与 `shell_id`（首个 shell 不与 session 共用 id）。

---

## 资源 URL 寻址（termcp://）

这些 URL 是**复制给 AI 用的定位符**：Web UI 各处的复制按钮（entry 卡片、session 卡片、终端标题、每个 shell 频道标签）一键复制后，直接粘进与 AI 的对话或任务描述中，AI 就能精确定位你说的是**哪个连接 / 哪个会话 / 会话里的第几个频道**，不用再费口舌描述。点击复制按钮不会触发连接、切换频道或关闭窗口。

| 层级 | URL 形式 | 例子 |
|------|----------|------|
| entry（连接配置） | `termcp://[entry名]` | `termcp://internal` |
| session（会话） | `termcp://#[会话id]`（**短形式，复制按钮统一输出此形式**） | `termcp://#ctf-1` |
| shell（频道） | `termcp://#[会话id]:[序号]` | `termcp://#ctf-1:2` |

- `[会话id]` 就是会话卡片上的等宽小字（不带 `session-` 前缀）；`[序号]` 是频道在该会话里的顺序，从 1 起，与频道标签 `shell-1`/`shell-2` 一致；无序号 = 首个 shell。
- **REST 侧也能解析**：`GET /api/resolve?url=termcp://...` 返回 `kind=entry|session|shell` 与对应 `ssh_config` / `session_id` / `shell_id`（纯 curl 的 agent 用；语法解析器与 MCP 共用 `internal/locator`）。
- **MCP 工具直接接受定位符**：`session_start(ssh_config="termcp://mac")`、`session_terminate(session_id="termcp://#ctf-1")`、`shell_input(shell_id="termcp://#ctf-1:2", ...)` 等都无需先解析成裸 id，一次调用直达。
- 兼容旧形式 `termcp://[entry名]#[会话id]`：entry 前缀被忽略（会话名与 entry 名无关），以会话 id 为准。
- **已关闭（DEAD）会话**：写操作工具（`shell_input` / `shell_key` / `shell_resize` 等）不接受定位到已关闭会话，会返回带提示的错误——已关闭会话是只读的，用 `shell_output`（`tail_lines` / `offset` 翻页）读取。定位符解析失败（如 `termcp://#sid:0`）返回 `invalid_argument` 并附具体原因。
- 通知通道专用格式：`shell_notify` 的 `channel="resource"` 广播的资源 uri 固定为 `termcp://shells/<shell_id>`（仅作事件载体，不是可用定位符）。

---

## 错误返回（错误码）

工具失败时返回 `isError=true`，其文本内容是一个带**专用错误码字段**的 JSON 对象，调用方据此分支处理，无需解析自然语言：

```json
{"error_code":"shell_not_found","error":"Shell '12d3e2f8-a15' not found"}
```

- `error_code`：稳定的 snake_case 错误码（仅失败结果携带；成功结果没有该字段）。
- `error`：人类可读的说明，仅用于展示/日志。

| 错误码 | 含义 | 典型处理 |
|--------|------|----------|
| `invalid_argument` | 参数缺失或非法 | 按提示修正参数后重试 |
| `session_not_found` | 无此 session_id，或会话已关闭（错误文本会提示用 `shell_output` 读取） | 用 `session_list` 复核 id |
| `shell_not_found` | 无此 shell_id（可能已 `shell_close` 删除） | 用 `shell_list` 复核 id |
| `session_not_running` | 会话已 DEAD（进程退出/显式关闭/断线/恢复），只读，不接受新操作 | 重新 `session_start` |
| `reader_not_registered` | `reader_id` 未在该 shell 注册 | 先 `shell_reader_register` |
| `forward_not_found` | 无此 forward_id | 用 `forward(action=list)` 复核 |
| `ssh_config_not_found` | 无此 ssh_config | 用 `ssh_config(action=list)` 复核 |
| `rule_not_found` | 无此通知规则 rule_id | 用 `shell_notify(action=list)` 复核 |
| `conflict` | 资源已存在 | 改用 edit/其它名称 |
| `not_configured` | 功能未启用/未初始化 | 检查服务启动参数 |
| `connection_failed` | SSH 拨号/连接失败 | 检查网络与目标地址 |
| `operation_failed` | 其它操作失败 | 查看 `error` 详情 |
| `internal_error` | 服务端内部错误 | 查看服务端日志 |

---

## 会话生命周期

```
ssh_config(action=list)
  → session_start → { session_id, shell_id, ... }
      → shell_input(shell_id, text)         # 只打字，不回车
      → shell_key(shell_id, key="enter")    # 只按键
      → shell_output(shell_id, timeout≤3)
      → shell_open(session_id) → { shell_id, session_id }   # 关掉全部 shell 后仍可新建
  → shell_close(shell_id)                  # 关掉 shell 不关会话：forward/file_* 照常
      → forward(session_id, action=local|remote|dynamic, ...)
      → file_*(session_id, ...)
  → session_terminate(session_id)           # 关闭会话（转入 DEAD：断连接+杀进程；force=true 强杀）
```

---

## 工具清单

### session_start

启动会话（连接容器）并创建一个主 shell 通道。

> **会话模式选择**：
> - **交互 shell（默认，省略 `command`/`args`）**：用于多步骤任务、带状态的操作（`cd`/环境变量/依赖后续步骤）以及通用 CLI 会话。在同一个会话中持续输入执行，保持工作目录与环境一致，形成连续的审计历史。
> - **专用单次程序（显式传入 `command`/`args`）**：**仅限**以下三种情况使用：
>   1. 交互式专用 REPL 或 TUI 工具（如 `python -i`、`mysql`、`htop`）；
>   2. 长期后台服务或守护进程（如 `npm run dev`、后端服务二进制）；
>   3. 需要进程原生退出码（ExitCode）的独立原子脚本。
> - **反模式**：切勿将多步骤任务拆解为多次 `session_start(command="bash", args=["-c", ...])` 执行。每一步都会丢失环境状态、产生多余 SSH 握手开销并割裂审计历史。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `command` | string | 否 | — | 可执行文件；空 = 按**命令优先级链**解析：profile 的 `default_shell` → （仅 pty）目标机自己的登录 shell。`pipe` + 空命令且 profile 无 `default_shell` 会被拒绝：pipe 通道没有登录 shell 可申请，客户端也不会拿本机 PATH 去猜目标机的 shell |
| `args` | string[] | 否 | `[]` | 命令行参数，仅 `command` 非空时有效 |
| `mode` | string | 否 | `"pty"` | **首个 shell** 的模式：`"pty"` 或 `"pipe"`。模式属于 shell，不属于会话；后续 shell 的 mode 由 `shell_open` 决定 |
| `name` | string | 否 | ssh_config | 会话显示名称 |
| `rows` | number | 否 | `24` | 初始 PTY 行数（1–1000） |
| `cols` | number | 否 | `80` | 初始 PTY 列数（1–1000） |
| `ssh_config` | string | **是** | — | profile 名称：`"internal"` = 本机 loopback，其他 = `ssh_configs/<name>/` 下的远端连接（可用 `ssh_config(action=list)` 查询） |

**返回**：`{ session_id, shell_id, pid, ssh_config }`

### shell_open

在已有会话连接上打开另一个 shell 通道（复用 SSH 传输）。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | 父会话 ID |
| `name` | string | 否 | — | 子通道显示名 |
| `command` | string | 否 | — | **可执行文件**（非命令行）；空 = 按**命令优先级链**解析：profile 的 `default_shell` → （仅 pty）目标机自己的登录 shell。`pipe` + 空命令且 profile 无 `default_shell` 会被拒绝。传 `"ls -la"` 会被当成单个文件名报 `executable file not found`，应传 `command="ls"` + `args=["-la"]` |
| `mode` | string | 否 | `"pty"` | 本 shell 通道的模式：`"pty"` 或 `"pipe"`（`"pipe"` = 无 TTY、逐行、运行即退出的命令）。模式是 shell 级属性，同一个会话可以同时挂 pty 与 pipe shell |
| `rows` | number | 否 | `24` | PTY 行数 |
| `cols` | number | 否 | `80` | PTY 列数 |

**返回**：`{ shell_id, session_id, name }`

### shell_list

列出某会话上的 shell 通道。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | 来自 `session_start` / `session_list` |

**返回**：`{ session_id, shells: [{id, name, status, ...}] }`

### shell_close

按 `shell_id` **删除**一个 shell 通道（手动关闭 = 删除，不是 DEAD；不会留下死态 tab）。不拆会话连接，不影响同会话其它 shell。internal 主 shell 关闭为 no-op（进程可存活于 tab 之外）。关掉最后一个 shell 也只是少了一个通道：容器保持 `running`，端口转发、SFTP、新建 shell 继续可用（shell 生命周期不决定容器生命周期）。彻底停止会话用 `session_terminate`。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | 来自 `session_start` / `shell_open` / `shell_list` |

### shell_input

向 shell stdin **只写文本**，不按回车、不执行命令。执行一行请再调 `shell_key(key="enter")`。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `text` | string | **是** | — | UTF-8 文本（不自动追加换行） |

### shell_key

向 shell 发送命名按键。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `key` | string | **是** | — | 见下方白名单 |
| `repeat` | number | 否 | `1` | 重复次数（上限约 20） |

**支持的 key：** `enter`, `tab`, `esc`, `up`, `down`, `left`, `right`, `backspace`, `delete`, `home`, `end`, `ctrl+c`, `ctrl+d`, `ctrl+z`, `ctrl+l`, `ctrl+u`, `ctrl+w`。

`enter`：PTY 下为 `\r`；pipe 下按 shell family 为 `\n` 或 `\r\n`。

#### 会话开了审阅时

会话可以开启**审阅**（Web UI 终端标题栏左侧标签条头部的锁图标，或它打开的面板里的开关；触屏同样可用）。开启后 `shell_input` / `shell_key` 不直接写字节：

```json
{ "ok": true, "approved": false, "review_pending": true }
```

- `shell_input` 先把文本**暂存**，不入队——人类要审的是**一条完整命令行**，而「文本」和「结束它的回车」是两次调用。
- `shell_key` 提交：把暂存的文本与这个键合成**一条**待审请求。没有暂存文本时，这个键自身就是一条（裸 `ctrl+c` 中断进程、裸 enter 执行空行，都该被人看到）。
- 返回里**没有 pending_id**：Agent 无权裁决自己的请求，给了 id 只会诱发重试循环。人类在 Web UI 点 Approve / Reject，一次批准即执行。
- 关闭或重开审阅时，**暂存但未提交的文本会被丢弃**（它从未成为可审对象）；已入队的请求随关闭而作废（fail-closed）。

完整语义（单一审阅者、一人一票、审计标记、裁决端点）见 [`docs/api.md` §12](./api.md)。

### shell_output

**统一输出读取工具**：活会话（内存缓冲）、已退出会话（保留缓冲）、已关闭/重启恢复的会话（磁盘消息流）全部用同一套字节流游标语义读取。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | shell_id 或 session_id 均可；已关闭会话也可用 shell_id 定位单个 shell 的输出流 |
| `strip_ansi` | boolean | 否 | `true` | 是否剥离 ANSI 转义码并压缩终端噪音 |
| `timeout` | number | 否 | `3` | 仅活会话：阻塞等待秒数（0–60）；0 = 非阻塞；多 shell 轮询建议 ≤3 |
| `offset` | number | 否 | `-1` | 无状态字节游标：从该原始字节位置向后读；-1 = 默认模式（见下） |
| `tail_lines` | number | 否 | `0` | 只返回流末尾最后 N 行（优先于 offset）；0 = 关闭 |
| `max_lines` | number | 否 | `0` | 最多返回 N 个完整行（窗口内裁切）；0 = 无限制 |
| `max_bytes` | number | 否 | `8192` | 单次返回最大原始字节数；0 = 无限制 |
| `reader_id` | number | 否 | `0` | 仅活会话流式游标；已关闭会话不支持 |

**读取模式（三选一）**：

1. **流式游标（默认，活会话）**：返回 reader 上次读取后的新输出，游标前移、不重复。`shell_input → shell_key(enter) → shell_output` 是驱动一条命令的完整循环（上一步的类型、回车、读取只是工具语义，不是回合边界）——应在同一轮内批处理：一个 `mcpScript` 里打字、回车、然后轮询到匹配或到期再返回。每次微步骤（打字/回车/读取各占一个模型回合）会让每个微步骤多付出 ~6–9s 的回合开销，而单次工具调用只要 2–10ms。
2. **`offset >= 0`（无状态绝对定位）**：读字节区间 `[offset, offset+max_bytes)`。任意时刻从头/任意位置翻页；每次调用显式传 `offset`（用返回的 `end_offset` 续读），服务器不保存状态，活会话与已关闭会话一视同仁。
3. **`tail_lines > 0`（末尾截取）**：反向取流末尾最后 N 行——只读最近输出，绝不拖入整段历史（token 友好）。无 `offset`/`tail_lines` 且目标是已关闭/死亡会话时，默认也取末尾最近一块（≤8 KiB），不会全量导出。

**返回**：`{ output, has_more, lines_returned, bytes_returned, start_offset, end_offset, total_bytes, source, session_id, shell_id, session_status, session_uptime_seconds? }`

- `start_offset` / `end_offset`：本次返回的原始字节区间；`total_bytes`：流总长；`has_more = end_offset < total_bytes`。
- `source`：`"live"`（内存缓冲）或 `"persisted"`（磁盘消息流）。
- 活会话流式读（模式 1）时 `start_offset`/`end_offset` 反映 reader 游标位置。

> 例：只读已关闭会话最后 10 行 → `shell_output(shell_id=已关闭session_id, tail_lines=10)`；从头翻页 → `shell_output(shell_id, offset=0, max_bytes=8000)` 后用 `end_offset` 续读。

### session_list

列出注册表中所有父会话。子 shell 不包含——用 `shell_list`。无参数。

**返回**：`{ sessions: [{id, name, status, ssh_endpoint, ...}] }`

### session_info

获取单个会话详细信息。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |

### session_terminate

终止并**关闭**会话：关闭全部 shell，并关闭 SSH 连接（级联清理 forwards / 通知规则）。会话**保留在注册表**中（状态 `exited` / DEAD），Web UI 上显示为灰色只读 tile，终端输出仍可用 `shell_output` 读取，重启后也会恢复。彻底删除用 `session_delete`。`force=true` 立即强杀；只关一个通道用 `shell_close`。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `force` | boolean | 否 | `false` | true = 跳过 grace_period 直接强杀 |
| `grace_period` | number | 否 | `5` | SIGTERM 后等待秒数（0–60） |


### shell_resize

调整 shell 的 PTY 行列数（会传播到远端 SSH）。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `rows` | number | 否 | `24` | 新行数 |
| `cols` | number | 否 | `80` | 新列数 |

### shell_reader_register

为 shell 注册独立 reader，返回新的 `reader_id`。新 reader 游标起点 = 当前缓冲末尾（**无历史 backlog**）。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | |

**返回**：`{ reader_id }`

### shell_reader_unregister

释放 reader。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | |
| `reader_id` | number | **是** | 非零 reader id（来自 `shell_reader_register`） |

### shell_notify

为某个 shell 注册**主动通知（push）**：Termcp 在事件发生时**主动通知 AI Agent** —— 进程退出 / 输出停顿 / 有新输出时即刻推送“醒来”信令，Agent 收到后再用 `shell_output` 拉取输出（通知**只带信令、不带终端内容**，避免污染上下文）。Agent 无需持续轮询，非常适合长任务挂起等待。

`action` 三选一：

| action | 参数 | 说明 |
|--------|------|------|
| `register` | `shell_id`（必填）、`channel`（必填）、`event`（可选，默认 `output`）、`silence_seconds`（可选，默认 3，仅 `silence` 生效，范围 1–300） | 新增规则，返回 `{ ok, rule_id, shell_id, channel, event }` |
| `unregister` | `rule_id`（必填） | 删除规则，返回 `{ ok, rule_id }`；规则不存在时 `error_code=rule_not_found` |
| `list` | `shell_id`（可选，过滤） | 返回 `{ rules: [...] }`，每条含 `rule_id`/`session_id`/`shell_id`/`channel`/`event`/`created_at` |

`channel`（下发通道）：

- `resource`：广播 MCP `notifications/resources/updated`，资源 uri = `termcp://shells/<shell_id>`。
- `sampling`：向注册该规则的 MCP 客户端发 `sampling/createMessage`（systemPrompt `termcp notification daemon`），直接唤起模型。

`event`（触发时机）：

| event | 触发 | 行为 |
|-------|------|------|
| `output`（默认） | 终端产生新输出 | **双沿**：立即发一次；输出停止 2s 后再兼底一次 |
| `exit` | 进程退出 / SSH 断开 | **一次性**：发一次后自动注销 |
| `silence` | 输出停止 N 秒（`silence_seconds`） | **一次性**：发一次后自动注销 |

**流控与生命周期**：所有下发共享一个全局 **1s 冷却阀**（防止风暴/刷屏）；shell 退出/`shell_close`/会话 `session_terminate` 或 `Delete` 时，该 shell/session 的规则**自动级联清理**，无定时器/协程泄漏。

**典型用法**（长任务编译）：

```jsonc
// 1) 编译命令跑起来后，注册退出通知
{ "action": "register", "shell_id": "<shell_id>", "channel": "sampling", "event": "exit" }
// → { "ok": true, "rule_id": "notif_...", "...": "..." }

// 2) 等 Termcp 主动唤醒（通知不带内容），再用 shell_output 拉取结果
{ "shell_id": "<shell_id>", "timeout": 0 }
```

> 进程还活但只是“输出停了”，用 `event="silence", silence_seconds=10`；需要持续跟踪输出变化用 `event="output"`。

### notify_user

向**人类用户**（而非 AI Agent）推送浏览器通知：在 Termcp Web UI 的**每个已打开页面**弹出彩色 toast，并尝试触发**浏览器系统通知**（需浏览器授权，页面在后台也能收到）；指定 `session_id` 时，该 session 的卡片会**高亮**（脉冲描边，滚到可视区；若其终端窗口已打开，窗口头部也会闪烁）。与 `shell_notify` 正相反 —— 后者是通知 AI Agent，本工具是 Agent 通知人。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `message` | string | **是** | 通知正文（最多 2000 字符） |
| `title` | string | 否 | 标题，默认 `termcp` |
| `level` | string | 否 | `info`（默认）/ `success` / `warn` / `error`，决定 toast 配色与左侧色条 |
| `duration_seconds` | number | 否 | toast 停留秒数（0–600）；`0` = 一直停留直到手动关闭，默认 `10` |
| `session_id` | string | 否 | 高亮该 session 的卡片；session 不存在时 `error_code=session_not_found` |

**返回**：`{ ok, delivered, title, level, session_id? }` —— `delivered` 是实际收到通知的已打开页面数（WebSocket 标签页）；为 `0` 表示当前没有页面打开，通知未展示（附 `hint` 说明）。

**什么时候用**：由 Agent 自行判断 —— 只要“人应该被提醒”就用，例如会话在等人（凭据、确认、MFA、交互式提问）、长任务结束、任务失败、需要人做决定。不限于固定场景清单。阻塞类提醒建议 `level=warn`/`error` + `duration_seconds=0`（不自动消失）+ `session_id`（高亮对应卡片）；同时要在回复里说同一件事，因为 `delivered=0` 说明没有页面打开、通知未展示。

**典型用法**：

```jsonc
{ "message": "构建已完成，耗时 2m31s", "level": "success", "session_id": "<session_id>" }
```

> 想通知 Agent 自己，用 `shell_notify`（MCP 信令通道）；想让页面上的用户看到提醒，用 `notify_user`（浏览器界面）。

### message（会话输出区段索引）

查看一个 shell 字节日志（`log.bin`）的区段索引：每段从哪里开始、什么时候开始、由什么产生。
**区段只描述位置，不携带内容** —— 字节全部在 `log.bin` 里，用 `shell_output` 按 offset 读取。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `action` | string | **是** | `"list"` |
| `session_id` | string | **是** | 会话 ID（省略 `shell_id` 时解析到该会话的 primary shell） |
| `shell_id` | string | 否 | 指定 shell 通道；省略则用会话的 primary shell |

- `action="list"`：返回 `{ "spans": [{ status, time, start, end }], "total_bytes": N, "session_id": "..." }`
- `status`：`"o"` = 输出，`"a"` = AI 输入（MCP），`"i"` = 接口输入（浏览器）
- `start`/`end`：该区段在 `log.bin` 中的字节区间；取内容用 `shell_output(shell_id, offset=start, max_bytes=end-start)`
- 输入是**零长度标记**（`start == end`）：按键已由终端回显进输出流，不重复写入

### session_delete（彻底删除会话）

**永久删除**一个会话：关闭仍存活的进程/传输，释放全部子资源（shell 通道、端口转发、通知规则、内存缓冲），从注册表移除（Web UI 上的 tile 随之消失），并删除磁盘上的会话目录（manifest + `log.bin` + `log.jsonl`）。**不可逆**。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | session_id 或 `termcp://` 定位符 |

> **close ≠ delete**：`session_terminate` 只**关闭**会话——断开连接、结束进程，但会话仍留在注册表中（状态 `exited`），Web UI 上显示为灰色只读 tile，终端输出仍可用 `shell_output` 读取，重启后也会恢复。只有 `session_delete` 才真正抹除。

---

## 服务端发现与配置

### shell_detect

探测 Termcp **宿主机**（不是 ssh_config 目标）的可用交互 shell。无参数。

**返回**：`{ path, family, hint }`

### ssh_config（统一入口）

SSH 连接 profile 管理。默认只暴露 `action=list`；write actions 需启动 Termcp 时加 `--mcp-manage-ssh-configs`。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `action` | string | **是** | `list` / `create` / `edit` / `copy` / `delete`（后四者需 flag） |
| `name` | string | 条件 | create/edit/delete：profile 名（`[A-Za-z0-9_-]`，最长 64） |
| `host` / `user` | string | 条件 | create 必填；edit 可选（仅更新传入字段） |
| `port` | number | 否 | 默认 22 |
| `password` / `private_key` / `key_passphrase` | string | 条件 | create 二选一；edit 省略保持原值 |
| `trust_unknown_host` | bool | 否 | 默认 false |
| `known_hosts` | string | 否 | 内容或路径 |
| `dial_timeout_seconds` | number | 否 | 默认 30 |
| `proxy` | string | 否 | SOCKS5 代理 URL |
| `description` / `default_shell` / `default_mode` | string | 否 | 新建会话的默认值。`default_shell` 是**命令优先级链**的第二级（调用方未给命令时生效，对该连接上的每个 shell 都有效）；`default_mode` 只作用于**首个 shell**，后续 shell 各自在 `shell_open` 里定 |
| `default_approval` | bool | 否 | 会话默认开启审阅（每次 AI 写入都等人裁决）。edit 时不传则保持原值，传 `false` 则关掉 |
| `jump_*` | — | 否 | 单层 bastion（ProxyJump）：`jump_host` / `jump_user` / `jump_port` / `jump_password` / `jump_private_key` / `jump_key_passphrase` / `jump_trust_unknown_host` / `jump_known_hosts` / `jump_dial_timeout_seconds` / `jump_proxy` |
| `source_name` / `target_name` | string | 条件 | copy：源与目标（目标须不存在） |

**注意**：password/private_key/key_passphrase/proxy 凭据**写入后不可读取**；不要在聊天中回显。write actions 未启用时调用返回错误提示启动 flag。

---

## 端口转发：forward（统一入口）

所有转发基于 SSH 通道，复用 `session_start` 建立的连接，参数均为 `session_id`。`action` 对应 OpenSSH 语义：

| action | OpenSSH | 语义 |
|--------|---------|------|
| `local` | `-L` | Termcp 侧监听本地端口，隧道到远端目标 |
| `remote` | `-R` | 远端监听端口，隧道回 Termcp 侧目标 |
| `dynamic` | `-D` | 本机 SOCKS5 代理 |
| `list` | — | 列出全部转发 |
| `close` | — | 按 `forward_id` 关闭 |

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `action` | string | **是** | — | `local` / `remote` / `dynamic` / `list` / `close` |
| `session_id` | string | 条件 | — | local/remote/dynamic 必填 |
| `remote_host` | string | 否 | `"localhost"` | local：目标主机（相对远端） |
| `remote_port` | number | 条件 | — | local：远端目标端口；remote：Termcp 侧目标端口 |
| `local_port` | number | 条件 | `0` | local/dynamic：本地监听端口（0=随机）；remote：远端监听端口（必填） |
| `local_host` | string | 否 | `"0.0.0.0"` | remote：远端监听绑定地址 |
| `forward_id` | string | 条件 | — | close：来自 `action=list` |

**返回**：local/dynamic → `{ local_port, forward_id }`；remote → `{ remote_port, forward_id }`；list → `{ forwards: [...] }`；close → `{ "success": true }`

---

## 文件（SFTP，session 级）

`file_*` 一律用 **`session_id`**（连接级，不绑 shell）。

### 高频独立入口

| 工具 | 说明 |
|------|------|
| `file_read` | 读文件（text/hex 或下载到 Termcp 主机） |
| `file_write` | 写文件（内联数据或从 host 文件流式写入） |
| `file_stat` | 文件/目录元信息（size、is_dir、children） |
| `file_delete` | 删除文件或空目录 |
| `file_rename` | 移动/重命名 |
| `file_mkdir` | 创建目录（含父目录） |
| `file_urls` | 获取 HTTP 下载/上传 URL |
| `file_getwd` | SFTP 工作目录（不是 shell 的 `pwd`） |

### 低频操作：分组入口（action 枚举）

低频文件操作合并为 3 个入口，用 `action` 参数区分具体操作，避免每个操作一个工具占用模型上下文。

**file_perm** — 权限/属主/时间戳

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `action` | string | **是** | `chmod` / `chown` / `chtimes` |
| `remote_path` | string | **是** | |
| `mode` | number | 条件 | chmod：十进制 Unix 权限（493 = 0755） |
| `uid` / `gid` | number | 条件 | chown：数字 uid/gid |
| `atime` / `mtime` | number | 条件 | chtimes：Unix 毫秒 |

**file_link** — 符号链接/硬链接

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `action` | string | **是** | `readlink` / `symlink` / `link` |
| `remote_path` | string | 条件 | readlink：要读取的链接路径 |
| `target` / `link_path` | string | 条件 | symlink：目标 + 新链接路径（`ln -s target link_path`） |
| `existing_path` / `new_path` | string | 条件 | link：已有文件 + 新硬链接路径 |

**file_fs** — 路径/文件系统

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `action` | string | **是** | `truncate` / `realpath` / `statvfs` |
| `remote_path` | string | **是** | |
| `size` | number | 条件 | truncate：新字节数 |

---

## 已移除（硬切换，无别名）

| 旧工具/参数 | 替代 |
|-------------|------|
| `send_and_read` | `shell_input` + `shell_key(enter)` + `shell_output` |
| `background_send` | `shell_input`（本身即立即返回） |
| `press_enter` 参数 | `shell_key(key="enter")` |
| `forward_port` | `forward(action=local)`（ssh -L） |
| 旧 `local_forward`（曾错误实现为 -R） | 现为 `forward(action=local)`；原 -R 能力见 `forward(action=remote)` |
| I/O 参数名 `session_id` | `shell_id`（shell_* 工具） |
| `shell_open` 的 `parent_session_id` | `session_id` |
| `initial_output` 返回字段 | 删除；用 `shell_output` |
| `file_chmod` / `file_chown` / `file_chtimes` | `file_perm(action=chmod\|chown\|chtimes)` |
| `file_readlink` / `file_symlink` / `file_link` | `file_link(action=readlink\|symlink\|link)` |
| `file_truncate` / `file_realpath` / `file_statvfs` | `file_fs(action=truncate\|realpath\|statvfs)` |
| `local_forward` / `remote_forward` / `dynamic_forward` / `list_forwards` / `close_forward` | `forward(action=local\|remote\|dynamic\|list\|close)` |
| `message_list` / `message_get` | `message(action=list\|get)` |
| ~~`history_*`~~ | **已移除**：`session_terminate` 只关闭会话（保留 DEAD 条目可读），不再归档；`shell_output` 统一读输出；彻底删除用 `session_delete` |
| `history(action=get_transcript)` | **已移除**；已关闭（DEAD）会话输出改用 `shell_output(shell_id=会话id或shell_id, tail_lines=N / offset)`，与活会话同一套游标语义 |
| `ssh_config_list` / `ssh_config_create` / `ssh_config_edit` / `ssh_config_copy` / `ssh_config_delete` | `ssh_config(action=list\|create\|edit\|copy\|delete)` |
