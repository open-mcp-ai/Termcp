# Termcp 终端进程通信流程架构

## 零、HTTP 入口与认证中间件（可选）

启用 `--auth-token` / `--auth-hash` 后，共享 `http.ServeMux` 外层包一层 `internal/auth` 中间件，一次覆盖所有入口（Web UI、REST、MCP SSE、`/stream`、WebSocket）：

```
浏览器 / AI Agent / curl / 脚本
        │
        ▼
┌──────────────────────────────┐
│  internal/auth（中间件）        │  credentials: Bearer → Basic → cookie
│  常数时间比对（crypto/subtle）  │  失败: 401 + WWW-Authenticate: Basic
│  Basic 成功: 下发 termcp_token │  成功: 放行 next
└──────────────┬───────────────┘
               ▼
     ┌─────────────────────┐
     │  共享 http.ServeMux   │
     └───┬─────┬─────┬─────┘
         │     │     │
 /sse /message /stream ────> internal/mcp（工具面不变）
 /api/*（REST） ───────────> internal/webui
 /api/ui/ws（WebSocket） ──> internal/webui
```

- 凭据按序尝试：`Authorization: Bearer` → `Authorization: Basic`（密码字段即 token，用户名忽略）→ `termcp_token` cookie；任一通过即放行，全部失败返回 401。Basic 若带冒号 token，解码后的整个 `user:pass` 串与原 token 完全一致时也放行（兼容 `curl -u user:pass` 这类在客户端自行拆分密码的用法）。
- Basic 认证成功时中间件设置 `termcp_token` cookie（HttpOnly、SameSite=Strict、Path=/），使无法携带自定义请求头的浏览器 WebSocket 握手也能完成认证；无服务端 session 存储，cookie 值即 token 本身（TLS 下自动附 `Secure`）。
- 哈希格式 `sha256-<salt_hex>-<digest_hex>`，其中 `digest = SHA256(salt || token)`，由 `termcp --gen-auth-hash` 生成；服务端可只保存哈希、不落明文。
- 启动期校验：`--auth-token` 与 `--auth-hash` 互斥；绑定非 loopback 且未配置认证直接拒绝启动；loopback 绑定保持无认证默认行为。

## 一、整体分层

### 可被外部 import 的三层

仓库里有一个**对外契约层**，它在 `internal/` 之外，因此别的 Go 项目可以直接引用：

```
termcp/term     TERM/1 协议本体：DSL 解析、指令注册表、信封、地址。
                纯函数层——不认识会话，不开 socket，不读文件，不看时钟。
                go list -deps ./term 里没有一条 internal/ 路径。
termcp/term/server
                协议的传输层：HTTP 入口与信封的线上形态。它把 term 变成一个
                能对外服务的服务器，但仍然只认 Handler 接口，不认识会话。
                自定义词表 + 一个 Handler + 这一层 = 一个 TERM/1 服务器。
termcp/core     运行时装配：会话、shell、存储、转发、扩展、MCP 服务面、
                可选 Web UI。core.New(core.Options{...}) 起一个实例。
termcp/config   实例设置（Config），命令行与库调用共用同一份。
根包 main      只剩 flag/env → Config → core.New 的装配。
```

`term` 与 `term/server` 分两层是有原因的：**它们回答不同的问题**。前者是"这行文本是什么意思"，
后者是"怎么把它送到网络上"。一个只想加自己动词的调用方需要后者但不需要 core 的会话、
存储与转发；反过来，一个只用协议做解析的调用方（`termcp_tool` 声明的校验就是）连后者都不该拖进来。

`termcp/core` 有两种用法，区别只在**调哪几个方法**，不在一个开关：

```go
// 二进制走这条：Start 装配，Listen 绑端口并阻塞到关停。
srv, _ := core.New(core.Options{Config: cfg})
defer srv.Close("done")
srv.Start()
srv.Listen()

// 嵌入方走这条：只 Start，不 Listen。不绑端口，但 MCP 面与 REST 面都在。
cfg.Headless = true
srv, _ := core.New(core.Options{Config: cfg})
defer srv.Close("done")
srv.Start()
reply := srv.Term(ctx, "ls session", nil)   // 拿到 TERM/1 信封
// 或者把 srv.Handler() 挂到自己的 mux 上
```

**为什么不是 `NoListener: true`。** 那曾经是一个布尔，把 `Start` 切成两半，而切错了地方：它名义上只说"不要开 socket"，实际把 `mcp.Start` 一起跳过了——那个调用注册 agent 读的 `learn-api` 文档提示并解析 baseURL，和端口毫无关系。于是最需要自我描述的形态（无端口、无 UI、进程内驱动）恰恰是唯一拿不到文档的形态。

一个问题一个方法，就不会切错：`Start` 把所有面（SSH、MCP、扩展、`ready`）拉起来并返回；`Listen` 绑定并阻塞。"不监听"＝不调那一步，不是一个字段在替所有人决定跳过 `Start` 的哪一半。端口校验也跟着挪进了 `Listen`——没绑过端口的实例不该因为一个用不到的端口号被拒绝。

`Headless` 是 `config.Config` 的字段，不是 `core.Options` 的：它描述的是这个实例是什么，和 `Host`、`DataDir` 同一类，所以设置它的地方只有一处。`core.New` 读它，同时也在这里应用 `AssetsDir` 与 `ThemesDir`——调用方不需要、也不应该去动 `webui` 的包级变量。它省掉浏览器 UI（静态资源、`/api/sessions`、WebSocket），保留 MCP 工具面与 `POST /api/term`——这正是 agent 需要的全部。它与 `no_webui` 构建标签方向一致：那个标签在编译期就不装 UI，`Headless` 在运行期不装配它。

于是有五种组合，各有各的用途：

| 形态 | 怎么得到 | 适用 |
|---|---|---|
| 完整版 | `make build` | 人 + agent 共用一台实例 |
| 运行时无 UI | `termcp --headless` | 一个二进制既能给 web 也能只给 agent |
| 编译期无 UI（termcp-headless） | `make build-api` → `dist/termcp-api-*` | 镜像要小、永远不给 web 用 |
| 库（SDK） | `import "termcp/core"`，只 `Start()` 不 `Listen()` | 嵌进别的 Go 程序，连端口都不要 |
| 自定义协议服务器 | `import "termcp/term"` + `termcp/term/server` | 词表是自己的，只写动词 |

第三种就是 issue 里说的 `termcp-headless`：产物名带 `api` 后缀，可与完整版并存于 `dist/`。它和 `--headless` 不重复——一个减的是**二进制**，一个减的是**装配**。

协议层的稳定性承诺跟着协议版本走，不跟模块版本走：改一行的含义、改状态码、改 locator 指代什么是 breaking change；加一条指令、一个 flag、一个 helper 不是。

### 内部运行时

Termcp 是平台型架构：**一个会话内核（session/message/sshclient/sshserver）支撑四个平行入口**——Web UI（人）、AI Agent 的两种接入方式——MCP 与 REST（两者是同一套 `term` 指令空间的两种传输，实例自带 `{}` 引导与 `help` 表，不需要另写教程）、WebSocket（实时终端 I/O）。它们操作同一批会话，互不冲突。

```
┌──────────────────────────────────────────────────────────────┐
│  ┌────────────────┐   ┌──────────────────────┐   ┌─────────┐  │
│  │   人 (浏览器)    │   │   AI Agent (MCP)     │   │ 脚本/程序 │  │
│  │  Web UI /api   │   │ SSE | /stream        │   │ REST API │  │
│  │  WebSocket     │   │ 同一工具面 / JSON-RPC │   │ WebSocket│  │
│  └───────┬────────┘   └───────────┬──────────┘   └────┬────┘  │
└──────────┼────────────────────────┼─────────────────────┼─────┘
           ▼                        ▼                     ▼
┌────────────────────────────────────────────────────────────────┐
│                   共享 http.ServeMux（单端口 18765）            │
│  /                → Web UI（internal/webui）                    │
│  /api/*           → REST（internal/webui/handler.go）            │
│  /api/ui/ws       → WebSocket 终端 I/O                          │
│  /api.md → agent 文档（无 MCP 也能读）                          │
│  /sse /message/stream → internal/mcp（工具面不变）               │
└──────┬──────────────────────────────────┬──────────────────────┘
       │                                  │
       ▼                                  ▼
┌──────────────────────┐      ┌──────────────────────────┐
│ internal/mcp/        │      │ internal/webui/          │
│ server.go 工具注册    │      │ 会话卡片/终端窗口/历史     │
│ term 单工具 (TERM/1)  │      │ handler.go REST+WS        │
│ logging.go 结构化日志  │      └───────────┬──────────────┘
└──────┬───────────────┘                  │
       │                                  │
       └──────────┬───────────────────────┘
                  │ session.Manager (注册表) │ message.Manager (持久化)
                  ▼                          ▼
    ┌──────────────────────┐    ┌──────────────────────────┐
    │  internal/session/    │    │  internal/message/        │
    │  manager.go          │    │  message.go              │
    │  Create/Get/Delete/  │    │  AppendOutput/Marks       │
    │  Terminate/ListAll   │    │  每 session 独立 mutex     │
    └──────────┬───────────┘    └──────────┬───────────────┘
               │ session.go                │
               │ New/SendInput/ReadOutput  │
               │ Terminate/ResizePty       │
               │ RegisterReader/Upload...  │
               └──────────┬───────────────┘
                          │
          ┌───────────────┼───────────────┐
          ▼               ▼               ▼
   ┌─────────────┐ ┌────────────┐ ┌──────────────┐
   │ sshclient/  │ │ buffer/    │ │ storage/     │
   │ ExecSession │ │ Buffer     │ │ Store        │
   │ ChildShell  │ │ 缓冲区     │ │ 持久化       │
   └──────┬──────┘ └────────────┘ └──────────────┘
          │ SSH (x/crypto/ssh) over in-memory net.Conn
          ▼
   ┌──────────────────────────────────────────┐
   │        internal/sshserver/                │
   │        charmbracelet/ssh Server           │
   │        (in-process, 无 TCP 监听)          │
   │                                          │
   │  ┌─────────────────────────────────┐     │
   │  │ PTY 分支: charmbracelet/ssh     │     │
   │  │   Pty.Start(cmd) (底层 creack/  │     │
   │  │   pty；Windows 走 ConPTY)       │     │
   │  │   io.Copy(pty, session)         │     │
   │  │   WindowChange → pty.Setsize    │     │
   │  ├─────────────────────────────────┤     │
   │  │ Pipe 分支: cmd.Stdin/Out/Err    │     │
   │  │   直接连接 SSH session          │     │
   │  └─────────────────────────────────┘     │
   │                                          │
   │  sftp subsystem → pkg/sftp (file_*)     │
   │  sshSignalToOSSig: TERM→SIGTERM, ...    │
   └──────────────────┬───────────────────────┘
                      │ exec.Command
                      ▼
             ┌─────────────────┐
             │  实际进程        │
             │  bash/zsh/pwsh  │
             │  python/node/.. │
             └─────────────────┘
```

## 二、运行时路径

这里曾经是六张分步时序图：启动、输入、输出、终止、resize、多 Agent 共享。它们画的是
**单工具化之前**的接口——`session_start`、`shell_output`、`shell_reader_register`——以及
当时点名的函数（`Session.SendInput`、`Session.ResizePty`）。工具名和函数名现在都不存在了，
图却仍然整齐，于是读的人会在源码里搜不到其中任何一个名字。

一条会骗人的箭头比没有箭头更糟，所以图换成了路径本身。要细节时，代码是权威：

1. **语言层** — `term/directive` 把一行解析成语句，`term/execute` 按 `&&` / `||` / `&`
   排定顺序，每条语句交给 Handler 一次。某个动词接受什么形状由词表生成，问运行中的实例：
   `{"cmd":"help <directive>"}`，不会过期。
2. **能力层** — `internal/mcp` 把语句翻译成对会话、shell、文件、转发的调用。评审闸门在
   这一层（`internal/capability`），闸门的选择见 [`protocol.md` §6](./protocol.md#6-three-way-capability-gate)。
3. **会话内核** — `internal/session` 持有连接容器（Session）与它的通道（`internal/session/shell.go`、
   `shell_output.go`）。`internal/sshclient` 用一对**内存 `net.Conn`** 连到同进程里的
   `internal/sshserver`：没有 TCP、没有端口、没有第二个二进制。所以"连本机"和"连远端"
   走的是同一条代码路径，差异只在拨号器。
4. **进程** — `internal/sshserver` 在 ConPTY（Windows）或 POSIX PTY（Linux/macOS）里启动
   真实命令；pipe 通道直接把 `cmd.Stdin/Stdout` 接到 SSH 会话上。
5. **输出** — 读到的字节进入 `internal/message` 的按会话日志，`read` 以字节游标从磁盘分页，
   所以会话结束后仍可回放。

改动这条路径时不需要同步本文：它说明的是为什么长成这个形状，不是每一步调了哪个函数。

## 十、会话生命周期状态机

Session 的 `status` 描述的是连接容器，而 Shell 有自己独立的 `status`。当前状态的含义如下：

- `running`：Session 未关闭，SSH transport 可用；这是 Session 的正常在线状态。Shell 可以是 `running`，也可以是自然退出后仍保留在 channel 列表中的 `exited`。
- `exited`（代码/界面常称 DEAD）：Session 已结束（显式 terminate、SSH transport 异常断线、server shutdown）。Shell 的存亡与它无关：Shell 自然退出、手动关闭、甚至全部关掉，容器都保持 `running`（transport 仍然可用，端口转发/SFTP/新建 shell 继续工作）。Session 仍保留在 registry，缓冲区、字节流日志和自然退出 shell 快照用于只读查看，直到显式 `DELETE`。
- `error`：保留给启动失败等错误；当前启动失败直接返回错误，不会创建 Session。

```
                          connect + open (pty)
                                   │
                                   ▼
                           ┌──────────────┐
                           │   running    │
                           │  (在线容器)  │
                           └──┬─────┬─────┘
                              │     │
           Session terminate/ │     │  传输异常断线
           disconnect/shutdown│     │
                              │     │
                              └──┬──┘
                                 ▼
                           ┌──────────────┐
                           │   exited     │
                           │ DEAD / 只读  │
                           └──────┬───────┘
                                  │ DELETE
                                  ▼
                               [gone]

      running ── shell 自然退出 ──> running（shell 留在列表供 drain）
      running ── shell 手动关闭 ──> running（shell 直接删除；关掉最后一个也一样）
      running ── 所有 shell 关闭/退出 ──> running（transport 仍可用：转发/SFTP/open 照常）
```

**关键不变量：**

1. Session 未关闭时，`GET /api/sessions` 显示 `status: "running"`；不要因为某个 shell 退出或手动关闭（哪怕一个不剩）就把仍可用的 Session 标成 DEAD —— 容器持有的是 SSH transport，端口转发、SFTP 与 `open` 都只依赖它。
2. Shell 的关闭有两种语义：
   - **归档并保留记录（`ArchiveChildShell`，`POST /api/shells/{id}/archive`）**：关闭通道并标记 `archived`，退出码与最后日志保留在 `shellHistory` 快照中，UI 移除活动标签但可在归档列表和只读视图回溯。
   - **彻底删除（`CloseChildShell`，`DELETE /api/shells/{id}`）**：从 live map、shell history snapshot 中彻底删除，不留记录。
3. 只有自然退出、归档或 transport 异常断线的 shell，才会保留元数据供 DEAD/只读/归档视图使用。
4. Session 转为 `exited` 后不能创建新 shell；显式 `DELETE` 才释放对象、buffer、transport 并从 registry 移除。`exited` 只由 terminate/disconnect/transport 断线/shutdown 产生，永远不由 shell 生命周期产生。
5. **传输隔离与连接池**：
   - 普通会话（Session）各拥有独立的 SSH 传输，互不共享 client；同一主机的会话断线不会影响其他会话。
   - `Manager.AcquireTransport`（`TransportPool`）仅为无会话的一次性指令借用（如 `run use:<host>`、`read use:<host>`）。
   - 扩展层（Extensions）由 `Reconciler` 基于 `groupKey`（目标主机 + 权限白名单）单例共享 Session，在内部按需分配独立 channel。

**exitOnce / closeOnce 保证**：无论是进程自然退出还是 terminate/手动关闭触发，状态转换和 Done channel 都只执行一次；手动关闭与自然退出并发时，关闭标记优先，避免 shell 被重新写回历史快照。

### 资源作用域：创建点挂载，级联释放

Session 持有一个 `ResourceScope`（`internal/session/scope.go`），等价于一次性的 `context.Context` 加 LIFO 清理栈：

```
finalize()  ──►  scope.Release()  ──►  逆序执行所有已挂载的清理函数
                    ▲
      ┌─────────────┼───────────────────────────┐
      │             │                           │
  端口转发      通知规则                消息管理器会话状态
  (创建时挂载)  (注册时挂载)             (会话创建时挂载)
```

- **创建点负责挂载**：任何绑定到某个 Session 的子资源（端口转发、通知规则、订阅句柄、临时凭据等），在它被创建的那一刻调用 `sess.AttachCleanup(fn)` / `sess.AttachCloser(c)` 挂到该 Session 上。
- **唯一级联点**：`finalize()` → `scope.Release()` 是唯一的释放入口；子系统之间互不感知，也不需要任何全局注册表或"记得在 main 里注册 listener"的隐式约定。
- **幂等与并发安全**：`Release()` 可重复调用（`sync.Once` 语义），清理函数 panic 不会阻断其余释放；若挂载发生在 scope 已释放之后（会话已删除与资源创建并发），清理函数立即同步执行，因此不存在"挂到一个死会话上"的泄漏窗口。
- **LIFO 顺序**：后创建的资源先释放，符合依赖方向（例如先关转发，再关底层连接）。
- **DEAD 不释放 scope，但释放传输资源**：`terminate` / 断线只把 Session 标记为 `exited`，不触发 `Release()`（保留缓冲与消息供只读回放）；但绑定在已死传输上的资源——端口转发——会经 `Manager.SetOnDeadHook` → `ForwardManager.CloseBySession` 立即关闭，否则本地监听会残留成 active 的死端点。只有 `DELETE`（`Manager.Delete`）才跑 `finalize()` → `scope.Release()`。

---

## 十一、跨平台设计与安全边界模型

### 1. 跨平台与纯 Go 无 CGO 设计

Termcp 采用 **100% 纯 Go 实现（`CGO_ENABLED=0`）**：

- **零系统 C 库绑定**：不引入 libc / glibc / musl 动态链接约束，单个二进制文件静态自包含，可在 Alpine、CentOS、Debian、Ubuntu、macOS、Windows 等任意环境下即拷即用，无环境依赖地支持交叉编译。
- **全平台一致的真实 PTY**：
  - **Windows**：直接对接操作系统原生 **ConPTY**（Pseudo Console API），规避传统 winpty 依赖 C++ runtime 与不稳定注入的问题，与 PowerShell / CMD 具有完全一致的 ANSI 转义序列与 VT 仿真体验。
  - **Linux / macOS**：采用 POSIX PTY，结合标准 termios 控制字符与行规程（line discipline）。
- **进程生命周期统一抽象**：无论是本机回环还是远程 SSH，会话容器层以统一的 PTY 游标与字节流呈现，跨平台差异在内部各探测器和适配层彻底抹平。

### 2. 安全边界：责任自负与 AI 输出端前置过滤

> **核心原则：安全防线必须建立在 AI 模型的输出端（Output Guardrails / Tool Call Validator）与应用网关，而非底层终端管道。**

Termcp 本质上是**透明的真实终端与多路交互管道**（PTY Transport），不包含任何业务意图审计；期望底层终端识别或拦截恶意意图是不切实际且不可行的：

1. **为什么终端管道无法防范恶意行为？**
   - **语义混淆在底层不可判定**：AI 可以将破坏性指令切片拼接（如 `a="rm -"; b="rf /"; $a$b`）、通过变量重组、`eval` 注入、`printf` 格式化拼接、环境变量构造执行、或拆解为多步分片写入后执行。在 PTY 看来，这些输入完全是合法的键盘按键与字符流，底层通道绝无可能在不破坏正常开发操作的前提下区分“混淆攻击”与“正常开发脚本”。
   - **Payload 交付方式千变万化**：攻击性行为可通过 Base64 管道还原、分段追加写入、乃至使用合法的 `curl`/`wget`/`ssh` 远程拉取多阶段执行体。终端与 SFTP 只是原始字节输送管，无法也不应该扮演杀毒引擎或 EDR 的角色。
2. **责任共担模型（Shared Responsibility）**：
   - **调用方自负防线**：宿主程序、Agent Harness 或集成框架必须在模型发起 `input` 或 `write` 调用**之前**完成安全过滤——包括模型输出敏感指令检测、正则阻断、参数合规审计与指令沙箱隔离。
   - **人机接力作为最后屏障**：Termcp 原生提供 Web UI 实时同屏监视与强制接管能力。对高风险命令、权限跃迁（`sudo`、MFA 等）默认暂停等待人工确认，切忌在缺乏隔离的真实生产机器上完全放任 AI 无人值守执行。
