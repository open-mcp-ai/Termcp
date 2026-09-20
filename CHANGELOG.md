# Changelog

## Unreleased

### Breaking

- **非 loopback 绑定必须配置认证**：监听 `0.0.0.0`、局域网 IP 或非 `localhost` 主机名时，若未提供 `--auth-token` / `--auth-hash` / `TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH`，启动直接失败（此前允许无认证运行）。loopback 绑定保持无认证默认行为不变。
- **`session_start` 必填 `ssh_config`**：此前未传 `ssh_config` 时会隐式回退到 `"internal"`（本机 loopback），导致当调用方意在连远端主机但漏传配置（如只传了 `name`）时会误打开宿主机终端。现在必须显式指定 `ssh_config`（连宿主请显式传 `"internal"`，连远端传 profile 名称）；未传或为空时直接返回 `invalid_argument`（`ssh_config is required`）。
- **`shell_output` 统一游标读取**：唯一输出读取入口，活会话（内存缓冲）、已退出会话（保留缓冲）、已关闭/重启恢复会话（磁盘消息流）全部同一套字节流游标语义。新增 `offset`（无状态字节定位，配合 `start_offset`/`end_offset`/`total_bytes`/`has_more` 翻页）与 `tail_lines`（只取末尾 N 行，token 友好）；返回体新增 `source`/`session_id`/`shell_id` 等游标元数据。
- **删除 `history(action=get_transcript)`**：已关闭（DEAD）会话输出读取并入 `shell_output`（`shell_id=会话id或shell_id`），不再提供全量转录导出，避免一次性把整个会话拖入 LLM 上下文。随归档子系统一并移除 WebUI 的 `GET /api/history/{id}/transcript` 导出，翻页改用 `/api/shells/{id}/output-range`。
- **低频工具合并为 action 枚举**：`local_forward` / `remote_forward` / `dynamic_forward` / `list_forwards` / `close_forward` → `forward(action=...)`；`message_list` / `message_get` → `message(action=...)`；7 个 `history_*` 工具 → `history(action=...)`；5 个 `ssh_config_*` 工具 → `ssh_config(action=...)`（写操作通过 `--mcp-manage-ssh-configs` 开关）。
- **删除 9 个低频文件工具**：`file_chmod` / `file_chown` / `file_chtimes` / `file_readlink` / `file_symlink` / `file_link` / `file_truncate` / `file_realpath` / `file_statvfs` → `file_perm` / `file_link` / `file_fs`（各带 `action` 枚举）。
- **工具总数 31**：`history(action=get_transcript)` 移除、新增 `session_delete`（彻底删除会话），`shell_output` 新增 `offset`/`tail_lines` 两参数。

### 新功能

- **单一静态 Token 的 HTTP 认证**：新增 `internal/auth` 包 + 共享 mux 外层中间件，一次覆盖 Web UI、REST、MCP SSE、`/stream`、WebSocket。配置 `--auth-token` / `TERMCP_AUTH_TOKEN`（明文）或 `--auth-hash` / `TERMCP_AUTH_HASH`（`sha256-<salt_hex>-<digest_hex>`，`digest = SHA256(salt || token)`，服务端不落明文），二者互斥、flag 优先于环境变量。凭据按序尝试 `Authorization: Bearer` → `Authorization: Basic`（密码字段即 token，用户名忽略）→ `termcp_token` cookie（Basic 成功后自动下发：HttpOnly、SameSite=Strict，TLS 下带 Secure；解决浏览器 WebSocket 握手无法自定义请求头的问题）；全部失败返回 `401` + `WWW-Authenticate: Basic`（浏览器原生登录框，无自定义登录页、无 `/auth/login`）。校验用 `crypto/subtle` 常数时间比对，token 不进日志、不进 URL。新增 `--gen-auth-hash` flag：生成 token 的 salted SHA-256 哈希（供 `--auth-hash` 使用）后退出；token 取自参数，或在不带参数时从 stdin 无回显读取（不进 shell 历史）。文档同步更新 README（中英）、`docs/api.md`、`docs/architecture.md`、Web UI `api.html` 与 Docker 示例。
- **反向唤醒通知 `shell_notify`**（信令与数据分离）：新增 `internal/notify` 统一通知内核，支持 `event=output`（双沿触发：立即 + 2s 尾沿兜底）/`exit`（一次性，进程退出/SSH 中断）/`silence`（N 秒无输出，一次性）；双通道 `resource`（广播 `notifications/resources/updated`，uri `termcp://shells/<id>`）与 `sampling`（向**注册规则的客户端 session** 发送 `sampling/createMessage`，systemPrompt `termcp notification daemon`）；全局 1s 冷却阀防刷屏；`register`/`unregister`/`list` 三个 action，shell 退出/关闭/会话删除自动级联反注册（零协程/定时器泄漏）。MCP 层用 `AddTerminateListener` 与 forward 清理共存（不再互相覆盖）；sampling 在注册时捕获 `ClientSession`，解决定时器 goroutine 中无 session 导致发送失败的问题。Web UI 终端窗口新增 **Notifications 标签页**：实时列出该会话已注册的通知规则（event/channel/silence 秒数），可一键拆除；新增 `GET /api/notifications`（支持 `shell_id`/`session_id` 过滤）与 `DELETE /api/notifications/{id}`，规则变更经 `notify.Manager` 回调广播实时刷新。文档：`docs/mcp-tools.md` 新增 `shell_notify` 章节，README 特性表补充。
- **资源 URL 寻址与复制按钮**：为 termcp 资源定义统一的 URL 寻址——entry `termcp://[entry_name]`（如 `termcp://internal`）；session `termcp://#[session_name]`；shell `termcp://#[session_name]:[shell_index]`。`[session_name]` 取**会话 id**（卡片上等宽小字，无 `session-` 前缀），`[shell_index]` 取会话内**频道顺序（1 起）**，与频道标签 `shell-1`/`shell-2` 一致，省略序号 = 首个 shell。session/shell 一律使用无 entry 前缀的**短格式**（会话名由用户命名、与 entry 名无对应关系，前缀不可靠）；旧的长格式 `termcp://[entry]#[session][:N]` 仍被接受以兼容既有链接，但 entry 部分被忽略、以会话 id 为准。`termcp://shells/<shell_id>` 是通知广播专用 URI，明确拒绝作为定位符。Web UI 新增/改造小复制按钮，统一复制短格式 URL：entry 卡片名字旁（新增，紧贴名字）、session 卡片（原复制 session id 改为 URL）、终端窗口标题栏与 Tools 面板、以及**每个底部 shell 频道标签**（新增，复制该频道的 `:index` URL）。点击复制按钮不触发连接/切换频道/关闭窗口。
- **MCP 工具直接接受 `termcp://` 定位符**（`internal/mcp/resource_url.go`）：新增定位符解析器并接入工具参数解析链，用户可直接把 Web UI 复制的 URL 粘进对话，无需先查询再换裸 id。`session_start(ssh_config="termcp://mac")` 解析为 entry profile；`session_terminate` / `session_info` 的 `session_id` 接受 `termcp://#<sid>`；`shell_input` / `shell_key` / `shell_output` 等 `shell_id` 参数接受 `termcp://#<sid>`（→ 首个 shell）与 `termcp://#<sid>:N`（→ 第 N 个频道），同时兼容传入裸 session id（自动落到主 shell）。已关闭（DEAD）会话的定位符或裸 id 返回带提示的 `session_not_found` 错误（引导改用 `shell_output` 读取），频道序号越界返回 `shell_not_found` 并附会话实际频道数，畸形定位符返回 `invalid_argument` 并附解析诊断。MCP instructions 与工具 schema 同步说明定位符用法。
- **AI 直读文档（HTTP + MCP resources）**：实例把自己的文档随二进制发布，同一 HTTP 端口直接提供 `GET /api.md`（全英文 REST + WebSocket 权威参考，`text/markdown` 显式声明、不依赖平台 mime 表）与 `/skills.md`（curl 技能包，下载后存为 `~/.agents/skills/termcp/SKILL.md` 即为可加载的 agent skill）；MCP 侧工具参数全部由 `tools/list` schema 自描述，不冗余提供工具文档——不装 MCP、不用打开 Web UI 也能让 AI 学会调用 HTTP API。MCP 侧把这两份文档注册为 resources（`resources/read` 的 URI 就是实例的 `<origin>/…` HTTP 地址，一个 URI 两条获取路径），新增 `learn-api` prompt 引导 agent 先读文档再动手；initialize instructions 增补第 10 条指向这批文档。Web UI 的 **API / MCP** 页（`/api.html`）新增 **2. Agent docs & skill** 区：技能下载地址（可点 Download / 一键 Copy）、`curl` 安装命令与两份文档的 URL 列表，不再只能靠手拼路径。仓库内 `docs/api.md` 为唯一真源，`make sync-assets` 同步到 `internal/webui/assets/`，`TestSyncedDocsMatchSource` 防漂移；同时修正 `WithResourceCapabilities(true,true)` 的虚假声明（mcp-go v0.50 无 subscribe 处理，改为不宣称 subscribe/listChanged）。
- **文档端点免认证（仅 GET/HEAD）**：`/api.md` 与 `/skills.md` 是纯静态、零数据、零机密的文档，配置 `--auth-token` 后也允许**无凭据**获取——否则"不装 MCP、先读文档再用 API"的流程在带 token 的实例上直接 401 死锁（agent 还没拿到 token 就没法学怎么用）。其余所有面（Web UI/REST/MCP/WS）依旧全部要求凭据；写方法与路径穿越均不放行（`internal/auth` 白名单精确匹配 + 测试覆盖）。
- **REST 定位符解析 `GET /api/resolve?url=termcp://...`**：把 Web UI 复制的定位符在 HTTP 侧一次性解析成具体 id，纯 REST/curl 的 agent 不再需要自己实现语法解析。返回 `{"kind":"entry"|"session"|"shell", ...}`：entry → `ssh_config`；session → `session_id`/`name`/`status`；shell → `session_id`+`shell_id`+`index`（1 起，与 `shell-1`/`shell-2` 标签一致）。已关闭（DEAD）会话返回 `"status":"exited"`（只读）；错误语义：400 畸形定位符、404 未知 profile/会话/序号越界、409 对已关闭会话用 shell 定位符。定位符解析器抽到共享包 `internal/locator`（MCP `internal/mcp/resource_url.go` 改为薄适配层，两边同一份语法与测试）。
- **skill 安装目录写清 Claude Code 与共享约定**：实测确认 Claude Code（2.1.x）只读 `~/.claude/skills/<名字>/SKILL.md`，**不读** `~/.agents/skills/`（后者是其他 agent 的共享约定目录），此前文档只写了 `~/.agents/skills/termcp/SKILL.md`，导致装完 Claude 侧根本没加载、自然"不知道 termcp:// 是啥"。README（中英）、`docs/api.md`、`/skills.md`、`/api.html` 统一改为：Claude Code 用 `~/.claude/skills/termcp/SKILL.md`，其他 agent 用 `~/.agents/skills/termcp/SKILL.md`，项目级为 `<project>/.claude/skills/termcp/SKILL.md`；并说明 skill 在会话启动时加载、需重启会话，Claude Code 无 per-skill 子命令（安装=写文件，卸载=删目录，插件形态用 `claude plugin install/uninstall`）。README 同步把"三种入口"扩为"四种入口"（新增 Agent Skill 行）、介绍段与快速导航加入 skill 条目。
- **Agent Skill 更名 `termcp-http` → `termcp` 并补齐定位符用法**：`/skills.md` 的 frontmatter `name: termcp`、安装目标 `~/.agents/skills/termcp/SKILL.md`（目录名即发现名，与 name 一致）。description 明确点名 `termcp://` 定位符，使 agent 在用户说“打开 termcp://rock64”时能命中该 skill；正文新增 **3. Resource locators (termcp://)** 一节：三种形式的语法表、`GET /api/resolve` 的 curl 用法与返回示例、“打开 entry 定位符 = `POST /api/sessions {ssh_config}`”的语义、已关闭会话只读与错误码，以及“别把定位符当命令/网页去打开”的硬规则。README（中英）新增 **Agent Skill** 独立小节并强化特性条目。
- **REST 终端 I/O（无 MCP 的 CLI 路径）**：新增 `POST /api/shells/{id}/input`（`{"text","press_enter"}`）、`POST /api/shells/{id}/key`（`{"key","repeat"}`，键名同 `shell_key`）、`POST /api/shells/{id}/resize`（`{"rows","cols"}`），把此前仅有 WebSocket 可写的输入/按键/尺寸能力开放给脚本与 curl；错误语义统一（404 未知 shell、409 非运行态、400 参数错误）。

### 改进

- **交互式 shell 裸启动，不再注入任何启动参数**：移除 `disableHistoryExpansion`（zsh `-o NO_BANG_HIST` / bash `+o histexpand`）。这些参数是 shell 私有选项，目标机没有 bash/zsh、`$SHELL` 或 `/bin/sh` 指向 dash/busybox ash 时会把参数当非法选项直接拒绝：`/bin/sh: illegal option +o histexpand`，交互会话根本起不来。现在 `$SHELL` 检测到什么就原样启动什么，不追加任何 flag；代价是 bash/zsh 交互会话恢复默认的 `!` 历史展开，含 `!` 的密码/URL 需自行 `set +H`（bash）/ `unsetopt banghist`（zsh）或加引号。
- **Web UI 仅在窗体打开时拦截离开页面**：离开页面保护（`beforeunload`）此前因统计了服务端后台运行的会话和端口转发，导致即使用户关掉了所有 session 终端窗体，关闭/刷新网页时仍会弹出“系统可能不会保存您所做的更改”。现在调整为仅在页面上实际存在打开的终端窗体（含浮动与平铺网格窗体）时才弹窗拦截；窗体全部关闭后直接离开，不打扰用户。
- **修复快速命令输出被截断**：SSH 会话在进程退出时立即上报 exit-status，此时末尾 stdout 可能仍在通道缓冲中未读，而旧的读取循环一看到进程退出就停止、随后立刻封存缓冲，导致 `echo`/`ls` 之类快速命令丢失最后几行。现在读取循环以通道 EOF 为准持续读取，封存缓冲前先等待该 shell 的输出管道排空（带超时兜底）。
- **修复 PTY 窗口改动被丢弃**：内部 SSH 会话（`pty` 模式）此前在服务端又开了一个 window-change 消费者，与库自身的 resize 处理竞争同一个通道，约一半的 resize 事件被丢弃——WebUI/MCP 调整窗口后子进程终端尺寸时大时小。现在统一交给库处理，客户端 resize 可靠地传到子进程（新增 `stty size` 回归测试）。
- **清理并发读写隐患**：内部 SSH 服务端不再跨 goroutine 直接读会话的 PTY 结构（改为在 session 请求 goroutine 上取一次再交接），PTY 的 fork/exec 与库的 PTY 关闭用同一把锁串行；`ExecSession` 的 stdin 写入与关闭也串行（x/crypto 的 `Write` 与 `CloseWrite` 并发会竞争通道 EOF 标志）。内存 SSH 传输 `duplexConn` 的 `Close`/`Write` 不再可能 `send on closed channel`。`go test -race ./...` 现全绿。
- **结构化工具错误码**：工具失败结果不再是纯文本消息，而是一个带专用字段的 JSON 对象 `{"error_code":"...","error":"..."}`（仅失败结果携带）。Agent 可像处理 `(value, error)` 一样按 `error_code` 分支，无需字符串匹配；`snake_case` 码包括 `invalid_argument` / `session_not_found` / `shell_not_found` / `session_not_running` / `reader_not_registered` / `history_not_found` / `forward_not_found` / `ssh_config_not_found` / `rule_not_found` / `conflict` / `not_configured` / `connection_failed` / `operation_failed`。`forward` 与 `ssh_config` 内部改用 sentinel error（`ErrNotFound`），经 `errors.Is` 映射为对应错误码；WARN 日志新增 `error_code` 属性。
- **修复 `file_read` 越界/超大读取导致服务端 Panic**：当 `offset` 超过文件大小且 `length` 省略（或为 0）时，旧的 `length = totalSize - offset` 会得到负数而触发 `makeslice: len out of range`。新增纯函数 `normalizeReadRange` 归一化窗口：不计算 `offset+length` 避免溢出，`offset` 钳制到 `[0, totalSize]`，`length` 不为负；text/hex 模式单次读入上限 8 MiB（超出置 `has_more` 并可用 `offset` 翻页），`mode=file` 下载仍不限流。

- **修复已退出/恢复会话调用文件及转发工具导致 Panic 的问题**：当对已退出（DEAD）或重启后从磁盘恢复的无活跃 SSH 连接的会话调用 `file_write`/`file_read` 等 SFTP 工具或端口转发工具时，`sftpClient` 与端口转发函数补充了 `SSHClient == nil` 的防御性校验，返回规范的 MCP 工具错误，避免了 `pkg/sftp.NewClient(nil)` 空指针解引用崩溃；同时在 `internal/sftp.NewClient` 与 Web UI 文件 API 中增加了对空连接的防守。
- **Web UI 窗口拖拽 resize 体验与 PTY 尺寸同步**：增大拖动手柄触发面积至 30×30px 并提升 z-index 防止被右下角滚动浮标遮挡；改用 Pointer Capture 杜绝甩出窗口时的鼠标丢帧；rAF 节流配合强制重排消除拖动滞后；修复松开鼠标（`onUp`）时活动 shell 通道未派发远程 PTY 尺寸同步的问题。
- **Web UI Sessions 列表批量选择与删除**：标题栏常驻三个纯图标按钮——全选/取消全选（复选框两态图标）、红色垃圾桶批量删除选中（无选中时置灰，气泡提示选中数量）、扫帚一键清理已退出 Dead 会话（弹窗确认后顺序批量删除）；标题栏左侧在选中数 N>0 时实时显示 `[N selected]`。卡片右上角叉号始终可单删；右下角复选框常驻，点击（`stopPropagation`）切换选中态，卡片主体点按仍打开/聚焦终端；选中卡片显示蓝色描边。动态刷新保留已选集合并与全选状态、计数双向联动。
- **引导 Agent 偏好长连接交互会话**：精简 instructions 第 2 条明确指出推荐单个交互会话（保持 cwd/env/审计历史），澄清 `shell_output` 返回的是新增字节（读空 ≠ 没输出，需继续轮询），警告 `session_start` 的 `command/args` 是 run-and-exit 单次程序，不应用于多次分拆 `bash -c`。
- **已关闭会话默认读取不再全量倾倒**：无 `offset`/`tail_lines` 的已关闭会话读取默认返回末尾最近一块（≤8 KiB，行对齐），配合 `has_more`/`end_offset` 翻页；`max_bytes` 缺省 8192 与 schema 一致（显式 `0` 仍表示不限）。
- **SSH 连接失败可见性**：会话创建失败（如 `ssh dial` 超时/拒绝）现在在 termcp 终端打出 `[ERROR] session create failed`（含目标地址、超时、模式，不含凭据）；MCP 工具错误结果从 Debug 升级为 `[WARN]` 并附带错误预览；Web UI "测试连接"失败同步打 `[WARN]`。连接类错误（超时/拒绝/不可达/重置）自动追加 `Hint:` 诊断提示，MCP 工具结果与 Web UI 响应同样携带。
- **拨号错误上下文**：直连失败错误信息包含目标地址与拨号超时，如 `ssh dial: connect 192.168.0.145:22 (timeout 30s): dial tcp ...`，不再只有裸的 `i/o timeout` / `connectex ...`。

- **`ReadOutput` timeout 修复**：接受 `timeout=0`（非阻塞轮询），下限从 0.1 改为 0。
- **`ssh_config` 写操作双保险**：schema 层面 `action` enum 默认仅 `list`；dispatcher 层面即使客户端绕过 schema 也会拒绝。
- **统一 dispatch 架构**：新增 `group_handlers.go`，4 个 action dispatcher 复用现有 handler。
- **指令精简**：12 条 → 7 条（3,054 B → 1,467 B）。

### 修复

- **已关闭（DEAD）会话的文件/转发类工具统一返回 `session_not_running`**：此前 `file_read`/`file_write`/`file_stat` 等拿到的是已关闭的 SSH 连接，报出底层 `SFTP: sftp: use of closed network connection`（错误码 `operation_failed`）；`forward` 更会在关闭的会话上成功创建一个永不工作的本地监听；`shell_open` 返回模糊的 `session failed`。现在四类入口（`file_*` 含 `file_urls`、`forward`、`shell_open`）统一以 `session_not_running` 拒绝，错误文本提示改用 `shell_output` 读取输出；REST 侧 `GET /api/sessions/{id}/files` 与 `POST /api/sessions/{id}/forwards` 同步改为 409。
- **会话转入 DEAD 时级联关闭其端口转发**：`session_terminate`、进程自然退出、连接断掉或服务重启（`MarkAllDead`）都会触发 `internal/session` 的 `SetOnDeadHook`，由 `ForwardManager.CloseBySession` 释放该会话的全部转发（此前监听一直残留为 active，直到 `session_delete` 才清理）。
- **MCP `initialize` 的 `serverInfo.version` 跟随构建版本**：此前硬编码 `0.0.4`，与 `termcp --version` 报告的真实版本不一致；现在 `main` 把同一个版本串传给 MCP server。
- **`GET /api/resolve` 对已关闭会话的 shell 定位符返回 409**（文档早已如此描述），会话定位符仍返回 200 + `status=exited` 只读提示；同时移除 `resolveResponse` 中从未赋值的 `archived` 字段。

### 文档

- **README（中英）Docker 示例改为单行并精简整节**：以 `\` 结尾的续行在 bash 可用但在 PowerShell 是语法错误，现在所有 shell 示例（`docker run`/`docker build`/`curl`/`claude mcp add`）均为单行；同时去掉 Dockerfile 注释与说明的重复、`--mcp-manage-ssh-configs` 单独占一条 run 示例、与示例重复的 `export` 代码块，Compose 去掉冗余 `build args`（仓库 Dockerfile 默认走 `proxy.golang.org`）。
- **Dockerfile 去掉 `EXPOSE` 与硬编码 `ENTRYPOINT`**：镜像只提供二进制，监听地址/端口由部署命令传入（示例：`docker run ... ghcr.io/open-mcp-ai/termcp:latest termcp --host 0.0.0.0 --port 18765`）；`release.yml` 与镜像构建补上 `-X main.version` 版本注入，使 `termcp --version` 与 MCP `serverInfo.version` 在发布产物中真的跟随 git tag。
- **清理归档（archive）残留措辞**：README.zh 简介、`docs/mcp-tools.md`、`internal/mcp/docs.go` 的 skills 资源描述、`/skills.md` frontmatter 不再提 “归档/transcripts/screenshots”；注释与错误文本统一为“已关闭（DEAD）会话”。
- **README 工具表去重**：`session_start`/`session_list`/`session_info`/`session_terminate` 不再出现两次，`session_delete` 并入会话容器行（31 个工具一一列出）。
- **README（中英）新增 “AI-native by design / AI Native 设计” 小节**：把“Agent 是常驻用户”的定位落到具体机制上——同层平级入口、token/轮次预算（延迟加载、tail/offset 游标、唤醒信号）、实例自描述（`/api.md`、`/skills.md`、`learn-api`）、密钥留在平台侧、DEAD 可恢复、人工保留中断权。
- **README（中英）标语与简介精简**：去掉“不仅是一个 MCP…更是一个平台”的句式，标语改为“一个 AI Native 的终端平台：跨平台、可视化、人机协作 / An AI-native terminal platform: cross-platform, visual, built for human–agent collaboration”（避免“跨平台…平台”叠字）；简介首段删掉 PTY/标签页/SFTP 与“接入本机/远程”等实现细节和 MCP 角色说明，改为“多主机、多会话同时管理、全程可视化”与“连接配置由平台独立维护，Agent 无需读取凭据即可使用”；结尾改为“跨平台、云原生、纯 Go 无 CGO；单二进制、低开销、可长期驻留”。
- **“为什么选 termcp”重排为两大板块**：“多会话可视化管理”（功能强大的 Web UI，本地一行命令或云端容器同一套界面）与“AI Native 设计”（无缝人机交互、结对操作），把原“打破边界”（跨轮次驱动 TUI/REPL 的能力）并进 AI Native 开头，删除与简介重复的四入口表格与独立的“可视化管理”小节。

---

## v0.1.12 — 2026-08-17

### Breaking

- **MCP 工具全面改名**：所有工具改为两级 `<group>_<action>` 命名（如 `session_start`、`shell_send_input`），与 resource model 对齐。旧名称无别名。
- **默认数据目录改为 `~/.termcp`**：优先级 `--data-dir` > `$TERMCP_DATA_DIR` > `~/.termcp`。目录权限收紧为 `0700`（SSH 配置属敏感数据）。

### 新功能

- **断开 ≠ 删除，会话保留只读历史**：会话异常退出后保留为只读 DEAD 磁贴（缓冲与输出不丢，重启后仍可恢复查看）；主动关闭的会话归档后不再残留 DEAD 磁贴。仅显式删除才会清理缓冲与消息文件。
- **会话历史工具**：新增 `list_history` / `get_transcript` / `search_messages` / `rename_session` / `update_session_meta` / `purge_session` / `screenshot` 七个 MCP 工具，及配套 `/api/history` REST 端点，支持检索、重命名、清理历史记录与终端截图。
- **Web UI 平铺工作区**：终端窗口可一键平铺为网格布局（iTerm2 风格），支持单窗格最大化、活动窗格高亮、自动/列/行/双列四种排布策略。
- **平铺/浮动单按钮切换**：切换按钮并入标签栏右侧，与 eye（隐藏全部）、排布策略下拉同行；平铺时按钮蓝色高亮提示当前状态。
- **SSH 连接测试按钮**：新建连接前可直接测试连通性。
- **离开页面保护**：关闭页面前提醒，防止误关正在运行的会话；终端新增"回到最新输出"按钮。

### 改进

- **构建**：新增平台感知 Makefile；默认按 release 模式构建（`-s -w -trimpath`）。
- 精简全部 MCP 工具描述与服务端 instructions（这些内容每轮注入模型上下文，显著降低 token 占用），并修正多处描述的歧义表述。

## v0.1.11 — 2026-08-02

### Breaking

- **删除 admin HTTP API**：移除 `--admin-host` / `--admin-port` / `--admin-token` 开关与独立 admin 端口。SSH 配置管理改为 MCP 工具（用 `--mcp-manage-ssh-configs` 启用）。
- **删除 `ssh-config init` / `ssh-config list` CLI 子命令**（从未出现在 `--help`）。SSH 配置改由 Web UI、`list_ssh_configs` 及 `--mcp-manage-ssh-configs` 的 MCP 工具管理。

### 新功能

- **MCP SSH 配置管理**（`--mcp-manage-ssh-configs`，默认关闭）：
  - `create_ssh_config`：结构化参数创建 remote SSH profile（host/user/password/private_key/jump）。
  - `edit_ssh_config`：增量修补已有 profile，省略字段保持原值（含凭据）。
  - `copy_ssh_config`：服务端复制 profile（含凭据），凭据不会经过 AI。
  - `delete_ssh_config`：按名称删除 profile。
  - 凭据写入后不可读取，日志不记录敏感字段。
- **WebUI 面板折叠**：Entries/Sessions 面板可折叠，状态持久化到 localStorage。
- **连接删除确认弹窗**：样式与整体 UI 统一。

### 修复

- 修正用户可见字符串中的配置文件名。

### 文档

- 新增 Docker 部署文档；README 全面重写；新增演示视频。

## v0.1.10 — 2026-07-20

### 修复

- **会话级联清理**：会话退出时级联回收子 shell 与转发资源，并对齐 shell API 行为。

## v0.1.9 — 2026-07-15

### 修复

- **转发生命周期**：端口转发创建时与会话绑定，UI 入口统一封装，不再出现孤儿转发。

## v0.1.8 — 2026-07-14

### 文档

- **Web UI `api.html`**：首页标题栏新增 **API / MCP** 入口；精简为 MCP 可复制配置 + HTTP/WebSocket API 速查表，含 `claude mcp add --transport http`、通用 `mcpServers` JSON 与 URL-only 说明；`/sse` 与 `/stream` 对等说明同步补齐（README / README.zh / `docs/mcp-tools.md` / `docs/api.md` / architecture）。

## v0.1.7 — 2026-07-11

### Breaking（MCP 重设计）

- **Session / Shell 双 ID**：`start_session` 返回互不相同的 `session_id`（连接容器）与 `shell_id`（终端通道）。首个 shell 不再与 session 共用 id。
- **I/O 只认 `shell_id`**：`send_input` / `press_key` / `read_output` / `resize_pty` / `register_reader` / `unregister_reader` / `close_shell` 参数改为 `shell_id`。连接级操作（forwards、files、terminate、delete、start_subshell）仍用 `session_id`。
- **删除工具**：`send_and_read`、`background_send`、`delete_session`（硬切换，无别名）。
- **删除参数**：`send_input.press_enter`。执行命令改为 `send_input` + `press_key(key="enter")`。
- **新增 `press_key`**：命名按键白名单（enter/tab/esc/方向键/backspace/delete/home/end/ctrl+c|d|z|l|u|w），可选 `repeat`。
- **转发 OpenSSH 命名**：`local_forward` = ssh `-L`；新增 `remote_forward` = ssh `-R`；删除 `forward_port`。旧版 `local_forward` 曾错误实现为 `-R`，现已纠正。
- **`start_subshell` / `list_subshells`**：参数 `parent_session_id` → `session_id`；返回字段对齐 `shell_id` / `shells`。
- **去掉恒空 `initial_output`**；`read_output` 默认 `timeout` 从 5 改为 3。
- **生命周期叙事**：MCP 只保留 `terminate_session`；`force=true` 立即强杀，HTTP `DELETE /api/sessions/{id}` 仍保留。

### 修复

- **`read_output.max_lines` 丢数据**：行数限制改为 buffer 层按换行截断游标；未返回行保留且 `has_more=true`（原先先消费再字符串截断）。
- **internal `close_shell` 误拆会话**：子 shell 主动关闭设 `deliberateClose`，退出 watcher 不再把 intentional channel close 当 SSH 断连。

### 文档

- 重写 MCP `instructions`、`docs/mcp-tools.md`、CLAUDE multi-session 规则；对齐 resource-model。

## v0.1.6 — 2026-07-11

### 新功能

- **SSH profile 改名**：配置文件同步迁移，引用不断链。
- **internal profile 虚拟化**：内置 loopback 连接不再落盘，且禁止编辑/删除。
- **`--no-internal` 开关**：禁用内建 loopback SSH profile。

## v0.1.5 — 2026-07-10

### 修复

- 密码框眼睛按钮纵向居中；密码/私钥字段自动 trim，防止首尾空格干扰认证。

## v0.1.4 — 2026-07-10

### 修复

- **`go install` 后 Web UI 资源缺失**：`vendor` 目录重命名为 `static`，避免 Go module zip 默认排除 `vendor` 路径导致 xterm.js 等静态资源丢失。

## v0.1.3 — 2026-07-09

### 修复

- 新建连接弹窗中 Key Passphrase 字段随 Auth 方式切换显隐，仅在 Private Key 时显示。

## v0.1.2 — 2026-07-03

### 新功能

- **SFTP 文件工具套件**：新增 10 个文件管理 MCP 工具（读写、列目录、删除、重命名、建目录等），统一走 SSH 路径。

### 改进

- **REST API 统一**：清理冗余端点，统一资源式设计。
- **转发/子 shell 变更实时推送**：创建与删除后通过 WebSocket 自动刷新 UI，无需手动刷新。
- Shell 平等化与会话层重构（sync.Map、SSH 断线检测）。

### 修复

- **exited session panic**：`SendTerminalBytes` 增加 nil 检查，会话退出后写入不再崩溃。

## v0.1.1 — 2026-07-01

### 新功能

- **SOCKS5 代理与跳板链**：SSH 配置支持 SOCKS5 代理与 ProxyJump 多级跳板。

### 修复

- **Enter 按目标系统发送**：换行符按目标 shell 所属平台（Windows CRLF / Unix LF）而非 termcp 本机 OS 决定，修复从 Windows 管理 Unix 主机时的输入异常。

## v0.1.0 — 2026-06-30

### 新功能

- **单会话多 shell 通道**：SSH shell 通道多路复用（统一 ChildShell 模型），一个会话内可开多个终端通道。
- **`close_shell` / `list_subshells` MCP 工具**：父子 shell 生命周期拆分，支持显式关闭单个 shell 与列出全部子 shell。
- **MCP 文件工具回归**：重新引入 SFTP 文件读写工具。

### 改进

- internal SSH 支持 SFTP subsystem；SSH 配置迁移至 TOML；包结构与命名重构、清理死代码。

### 修复

- **root shell 自然退出不再带垮 internal session**：主 shell 退出时正确区分连接关闭与整体断开。
- WebUI 连接加载指示器纵向居中，清理启动日志。

### 文档

- 文档与代码同步（31 个 MCP 工具清单、内存 SSH 架构说明）。

## v0.0.4 — 2026-05-23

### 新功能

- **Web UI**：浏览器端终端（xterm.js + WebSocket），支持实时会话列表（SSE）、全量输出回放、连接模板一键启动。单端口 18765 同时服务 MCP 和 Web UI。

- **服务端 SSH 配置**：SSH 连接信息存储在 `data/ssh_configs/<name>/config.json`，MCP 工具只需传配置名。支持 `internal`（loopback）和 `remote`（远端 SSH）两种类型，可选 `default_shell` 和 `default_mode`。新增 `ssh-config init` / `ssh-config list` CLI 子命令和可选 Admin HTTP API。

- **Streamable HTTP 传输**：新增 `/stream` 端点（MCP Streamable HTTP 规范），兼容 Open WebUI 等客户端。

- **read_output / send_and_read 增强**：返回 `session_status`、`session_uptime_seconds`；新增 `max_bytes` 参数（默认 8KB）配合 `has_more` 实现大输出分页。

- **MCP Agent 规则注入**：`initialize` 返回 `instructions`，引导 Agent 正确使用工具链（多步执行、crash-loop 检测、密码安全等）。

### 改进

- **PTY 标准终端模式**：SSH 客户端请求 PTY 时设置完整 termios（`ICANON`/`ICRNL`/`ONLCR`/`OPOST`/`ISIG`），修复 Python 3.13 pyrepl 崩溃等交互式程序异常。

- **输出缓冲区重构**：从 ring buffer 改为 append-only 多读者缓冲。每个 reader 独立游标，已消费前缀自动压缩，无固定容量覆盖。

- **TERM 环境变量传播**：PTY 请求的 `TERM` 值（如 `xterm-256color`）正确传递给子进程环境，修复 CI 中 `TERM=dumb` 导致的测试失败。

### 修复

- **Shell 历史展开干扰**：交互式 shell 启动时自动禁用 `!` 历史展开（zsh: `-o NO_BANG_HIST`，bash/sh: `+o histexpand`），防止含 `!` 的密码、URL 执行失败。

- **移除 SFTP 工具**：删除 `upload_file`/`download_file`/`list_files`，清理 sftp subsystem 残留，`trust_unknown_host` 默认 `false`。

- **Windows 跳过 TERM 测试**：`TestServer_PtyEnviron` 在 Windows 跳过，TERM 是 Unix 概念。

- **优化 .gitignore**：防范二进制文件误提交。

## v0.0.3 — 2026-05-12

### 新功能

- **detect_shell MCP 工具**：探测 termcp 主机上的可用交互 shell（bash/zsh/fish/pwsh/cmd），返回路径、family 和提示。跨平台混合环境中 Agent 可据此选择正确的命令语法。

### 改进

- **Shell 检测重构**：提取为可注入的 `Detector` 结构体，测试时可替换为固定实现，消除环境依赖。

- **Debug 日志增强**：MCP 工具调用时记录请求参数和输出预览，便于排查 Agent 行为。

### 修复

- **Kali sudo 密码提示泄漏**：修复 sudo 密码提示输出到父进程 TTY 的问题。

## v0.0.2 — 2026-05-07

### 新功能

- **Windows 平台支持**：全平台 Windows 支持，通过 ConPTY 运行 PowerShell PTY 会话。charmbracelet/ssh 提供原生伪终端分配，输入编码（CRLF）和输出规范化自动处理。

- **跨平台 CI**：测试 workflow 覆盖三平台 — `ubuntu-latest`、`macos-latest`、`windows-latest`，PR 和 push 到 `main`/`dev` 均触发。

- **多平台构建发布**：Release workflow 构建 6 个目标 — `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64`。

### 改进

- **SSH 库替换**：`gliderlabs/ssh` → `charmbracelet/ssh`。charmbracelet/ssh 内置 `AllocatePty()` 自动管理 PTY 生命周期，移除手工 `pty.StartWithSize`/`io.Copy`/`pty.Setsize` 代码。公共 API（`New`/`Start`/`Stop`/`Addr` 等）签名不变。

- **跨平台输入处理**：`SendInput` 在 `press_enter=true` 时自动选择平台换行符 — Windows 用 CRLF（`\r\n`），Unix 用 LF（`\n`）。

- **SFTP 路径规范化**：远程路径自动将反斜杠转为正斜杠，避免 Windows 路径分隔符导致 SSH 文件操作失败。

- **信号转发验证**：新增 `TestServer_SignalTerm` 和 `TestServer_SignalInterrupt` 验证 SIGTERM/SIGINT 通过 SSH 通道正确转发至目标进程。

### 修复

- **Windows PTY 交互输出为空**：ARM64 上 PowerShell/ConPTY 输出时序问题导致首次读取为空。改为 `Write-Output` 原生命令配合 marker 轮询读取（`testReadOutputUntil`）解决。

- **Windows 进程退出行为不确定**：PowerShell `-Command` 在 ConPTY 下完成命令后保持交互态，自然退出测试在 Windows 跳过。

- **Windows 跳过 POSIX signal 测试**：`SIGTERM`/`SIGINT` 测试在 Windows 跳过，Windows 不支持 POSIX 信号。

## v0.0.1 — 2026-05-05

### 初始发布

- termcp 首个版本：把交互式程序作为持久 SSH 会话暴露给 AI Agent 的 MCP server。
