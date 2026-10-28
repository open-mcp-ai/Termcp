# MCP 接入方式盘点与认证交互

状态：设计提案，认证改动尚未实现。日期：2026-10-27。核验对象：`dev@6a8af55` 及当前工作区；npm 启动行为另外核对相邻的 Termcp-npm 仓库。本次只修改设计文档。

本文细化 [身份与凭据签发机制](auth-issuance.md) 的接入交互。当前决策为 **OAuth 整体延期，简易静态 token 为 P1**。用户首次运行时设置 Admin，当前明文持久化，未来再配合加密系统；HTTP 和 daemon 使用同一默认 `.termcp` 数据目录。P1 中用户登录 WebUI、新增 Agent、取得 token 并自行填入 MCP 配置；stdio 读取 token 的能力与之同批交付。

本文第 3、5 节中的手动填 token 流程属于 P1；HTTP/stdio OAuth 均为未来计划。P1 未交付前，新 managed Agent 接入不具备完整能力，不能用匿名访问补齐。多桥协商、权限持久化、重启和失败语义的最新确认及遗漏项见 [多 stdio 与单 daemon 的协商](stdio-daemon-negotiation.md)。现有 Ensure 已支持目标 endpoint 的实例复用，本认证方案不新增 daemon 启动互斥机制。

## 1. 统计口径与当前入口

对宿主而言有 **3 类传输接入、4 种常用配置形态**。其中 stdio 是 HTTP 桥，服务端实际只实现两套 HTTP MCP 传输。npm、Docker、二进制安装是部署维度，不另计认证协议。

| 编号 | 常用配置形态 | 当前入口/命令 | 谁启动服务 | 最终到达哪里 |
| --- | --- | --- | --- | --- |
| A | Streamable HTTP 直连 | `/stream`，支持 POST/GET/DELETE | 管理员预先部署 | Termcp HTTP MCP handler |
| B | SSE 直连 | `GET /sse`，随后 `POST /message` | 管理员预先部署 | Termcp SSE MCP handler |
| C | stdio 连接已有实例 | `termcp stdio [endpoint]` | 管理员预先部署；桥不启动服务 | 默认 `/stream`，也可 SSE 或完整 HTTP(S) URL |
| D | stdio 自动确保实例 | `termcp daemon stdio [endpoint]` | 本机桥复用或拉起后台进程 | 指向 `--host/--port` 实例的 `/stream` 或 `/sse` |

C/D 可使用已安装的 `termcp`，也可把命令换成 `npx -y @open-mcp-ai/termcp`。正确顺序是 `daemon stdio`。`daemon start` 只部署服务，本身不是 MCP stdio 连接；裸 `termcp` 同样是启动 HTTP 服务。

源码及文档依据：

| 路径/符号 | 核验结果 |
| --- | --- |
| `main.go`，`runServer` | 注册 `GET /sse`、`POST /message`、`/stream`；REST `/api/term` 单独注册 |
| `internal/mcp/server.go`，三个 transport handler | 两套 HTTP MCP 实现；`/stream` 包含 GET/POST/DELETE |
| `subcommand.go`，`subcommands` | stdio、daemon 动作由同一 CLI 分发 |
| `daemoncli.go`，`resolveBridgeTarget` | endpoint 可省略、写 `stream`/`sse`（可带 `/`），或完整 HTTP(S) URL |
| `daemonfront.go`，`runDaemonFront` / `stdioBridge` | 普通 stdio 不启动实例；daemon stdio 先 Ensure；两者进入同一桥 |
| `internal/daemon/daemon.go`，`Ensure` | 复用正在应答的 Termcp，包括手动实例；只有未发现服务且端口可用时才尝试本机启动 |
| `internal/mcpbridge/bridge.go`，`Run` / `authorize` | stdio 消息转发到 HTTP，附加固定 Bearer/headers |
| npm `package.json` 与 `bin/termcp.js` 的启动段 | 包名为 `@open-mcp-ai/termcp`；包装器透传 argv、环境变量和标准流 |
| `docs/clients.md`、`docs/deploy.md`、`server.json` | 宿主配置、Docker 与 registry 对应上述传输，不构成额外认证入口 |

另有一条 **内部扩展桥**：`termcp-mcp-bridge <url> --header Name=Value`。它由 `internal/mcpbridge/builtin.go` 注册到内置 SSH server，供扩展经通道连接上游 MCP；`internal/extensions/reconciler.go:bridgeCommand` 负责生成参数。这不是对外分发的第五种宿主注册方式。若上游恰好是另一个 Termcp，仍按上游实例签发的 Agent token 认证；内置 SSH 的一次性通道凭据不能替代上游 HTTP 身份。

`POST /api/term`、浏览器 WebSocket 和 WebUI 不计为 MCP 接入。它们共享业务内核，但在本方案中属于人端业务面。Termcp 消费其他 MCP 扩展的能力也不能与“其他宿主接入 Termcp”混为一谈。

本次使用图谱搜索、调用链与路径 coverage，并以源码核对关键行为。npm 图谱排除了 `bin/`，其启动段直接读源验证；此盘点不是第三方 MCP 宿主兼容性认证。

## 2. 统一身份，保留不同配置载体

```text
HTTP/SSE 宿主 ── Authorization: Bearer ──────────────┐
                                                  ├─ 验证 Credential
stdio 宿主 ── env ── Termcp 桥 ── Bearer ────────────┘     ↓
                                                  Agent Principal
                                                        ↓
                                                  Grant 与当前策略

人的浏览器 ── human session Cookie ── Human Principal / Admin role
```

token 同时证明准入资格并引用服务端 Agent 身份；稳定身份是 `principal_id`。token 更新不改变 Principal。显示名、namespace、`clientInfo`、来源 IP 及“由 npx 启动”都不决定角色。

### 2.1 未来计划：OAuth（当前不实现）

本节仅保留后续接缝，HTTP 授权服务与 stdio OAuth 客户端均不进入当前实施范围，不阻塞 Admin 明文持久化或 P1 token。

支持 OAuth 的 HTTP/SSE 宿主添加实例 URL 后，通过授权发现打开 Termcp 授权页。用户登录，确认 Agent 名称和业务策略并批准；服务端创建或经用户明确选择关联 Agent Principal/Grant。宿主通过授权码与 PKCE 换取 access token，并在获准签发 refresh token 时负责保存和刷新。不要求用户预先创建静态 token 或复制 refresh token。

Admin 是批准者，MCP 凭据仍只代表 Agent 执行权限。OAuth `client_id` 标识客户端应用，不作为每个 Agent 的稳定 ID；同一客户端的多个接入可以有独立 Agent/Grant。访问与刷新凭据都必须关联原授权，不能因刷新而生成新身份或扩大权限。

OAuth 标准 token endpoint 与本文旧设计中的自定义 claim/renew 端点分开；不把设备配对、Agent Cookie 或自定义 renew 当作 OAuth 的必需组成。stdio OAuth 的登录 profile、凭据保存、刷新及同 profile 并发协调全部移入未来计划，当前不实现；P1 桥只读取显式 token，不负责替宿主授权或续期。

规范参考：[MCP HTTP 授权](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)、[通信与 PKCE 要求](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/security-considerations)。

### 2.2 P1：简易 token 兼容版本

只实现以下管理交互：WebUI 新增 Agent、一次显示 token、查看名称/状态/最近使用、撤销；更新时为原 Agent 重新生成 token，再由用户修改配置并重载。不要求 OAuth 客户端能力，不自动派发、不自动编辑第三方配置、不实现 refresh 或配对。

HTTP/SSE 客户端直接配置 Bearer；stdio 客户端通过下表提议的 env 交给桥。两种载体解析后仍落到同一个 Agent 身份模型。

| 配置项 | 当前行为 | managed 模式设计 |
| --- | --- | --- |
| HTTP `Authorization: Bearer` | 共享 token，全 HTTP 面同权 | 每 Agent 独立凭据，仅允许 Agent 业务及精确的控制端点 |
| `TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH` | 服务配置、daemon 管理和桥共用 | 留在显式旧模式；不能作为新模式 Agent 的配置模板 |
| `TERMCP_AGENT_TOKEN` | 尚不存在 | 新增桥专用 env，转换为 Bearer；不配置服务端 Admin，不授予 daemon stop |
| human session Cookie | 当前只有共享 `termcp_token` Cookie | 新的人端会话，仅供人端登录，不导入桥 |

兼容版本允许直接嵌入原始 token。HTTP 使用字面 header，stdio 使用宿主实际支持的 `env` 字段；不依赖通用占位符，也不要求外部 secret 管理。服务端仅保存 verifier；原始 token 在创建/替换凭据的响应中显示一次。

不能把客户端传来的 `TERMCP_AGENT_TOKEN` 自动登记成服务端的新 Agent，也不能拿它设置新服务的共享密码。服务端必须先有 Admin 签发记录；否则恶意客户端可以自行创建身份。daemon 子进程启动时须剥离桥专用 Agent env，服务从自己的持久化认证库加载授权。

身份隔离保护 Termcp 接口；有本机同一 OS 用户的任意 shell/文件权限的进程仍处于部署管理边界内。若需要阻止 Agent 读取或改写服务认证库，必须另用 OS 用户或容器隔离，不能声称一枚 Bearer token 能限制已经拥有本机管理权限的进程。

## 3. 简易 token 版本：按入口设计首次接入

### 3.1 已有服务：HTTP 或 SSE 直连

1. 用户打开 Termcp WebUI 登录，选择“连接 Agent”。
2. 输入名称、选择宿主及接入方式，创建 Agent。首期默认共享业务策略。
3. 页面显示一次 token，并立即生成含字面 Bearer header 的完整宿主配置或官方注册命令。
4. 用户粘贴配置并重载该 MCP；服务看到成功的认证 `initialize` 后显示“已连接”。

SSE 的同一凭据必须同时用于打开 `/sse` 和向 `/message` 发消息。只让 SSE GET 通过认证不足以建立可用连接。服务端还须验证消息请求属于该认证主体的 transport session。

HTTP 直连的配置示意如下；外层字段由宿主 adapter 渲染，不能声称这一份 JSON 通用于所有宿主：

```json
{
  "mcpServers": {
    "termcp": {
      "type": "http",
      "url": "http://127.0.0.1:18765/stream",
      "headers": {
        "Authorization": "Bearer <agent-token>"
      }
    }
  }
}
```

### 3.2 已有服务：stdio 桥

创建 Agent 的操作相同；页面输出带 `TERMCP_AGENT_TOKEN` 的 stdio 配置。桥读 env 并在每个上游请求中添加 Bearer。

连接本机已部署实例时，可以生成 `termcp stdio`；连接远端或反向代理时，应保留完整 URL，例如：

```json
{
  "mcpServers": {
    "termcp": {
      "command": "npx",
      "args": ["-y", "@open-mcp-ai/termcp", "stdio", "https://termcp.example.com/stream"],
      "env": {
        "TERMCP_AGENT_TOKEN": "<agent-token>"
      }
    }
  }
}
```

宿主启动本地桥并不等于能在远端启动服务。远端配置不应使用 `daemon stdio --host <远端地址>`。managed 模式的自动启动仅支持本机 loopback；显式远端 URL 只连接，不拉起进程。配置生成还要保留 HTTPS、代理路径前缀和非默认端口。

目前 `api.html` 一律按页面 hostname/port 生成 `daemon stdio`，且不保留 HTTPS/路径前缀。这是实施时需要一起替换的配置生成规则，不能直接在旧模板中补上一个 token 就算完成。

### 3.3 全新安装：npx / daemon stdio

首次没有服务、没有 Admin、没有 Agent token，必须先建立管理身份，再给宿主凭据。首期推荐明确的两阶段操作：

```text
用户在终端运行 npx -y @open-mcp-ai/termcp daemon start
  → 打开显示的 WebUI 地址，完成首个 Admin 初始化并登录
  → 创建 Agent，复制完整 MCP 配置或安装提示词
  → 宿主启动 npx … daemon stdio，并带上 Agent token
```

`daemon start` 当前默认没有空闲倒计时，适合首次初始化；不能用 `daemon stdio` 默认的 30 秒来要求用户赶在服务退出前完成操作。若之后继续复用这个常驻实例，它仍然没有倒计时；只有它退出后，后续 `daemon stdio` 新拉起的实例才采用该动作的空闲策略，界面应如实显示。

若用户先安装了不带 token 的 `npx … daemon stdio`：

- 服务仍可在本机启动，MCP 业务保持未认证；桥在 stderr 给出初始化/登录地址及“生成配置后重载”的提示，然后结束失败的 MCP 接入，不让宿主无限等待 `initialize`。
- 对新建但未完成初始化的后台实例，提议使用有上限的初始化宽限期（默认 10 分钟），独立于正常 30 秒空闲倒计时；宽限不能被匿名请求反复延长。已初始化实例不进入此状态。
- 这是失败恢复路径；宿主可能隐藏 stderr，不能保证模型会看到提示，也不能依赖先调用 `notify_user`。正常引导仍应放在 Termcp 安装文档和 WebUI 中。
- 配置修好后，宿主重新启动桥；只要服务认证库与签发时一致，重启不需要再次签发 token。

首次运行时设置 Admin，Admin 配置及凭据当前以明文保存在默认 `.termcp` 下；HTTP/daemon 重启后读取相同记录，后续配合加密系统迁移。初始化仅在未初始化状态开放，完成后关闭。远程部署可先在 loopback 启动，由管理员通过 SSH 本地转发完成设置；不能把普通未认证 MCP 请求当作设置 Admin 的授权。

没有可交互终端的 MCP 子进程不能在 stdin 请求管理员密码；首次设置通过用户可见的初始化入口完成。容器/服务器额外的离线 `termcp auth init` 命令保留为候选增强，当前不要求新增该命令或专用本机管理通道。Agent 仍在 Admin 设置并登录后的 WebUI 中创建。

普通重连、数据库丢失和“全新安装”必须区分：已有 token 找不到对应认证库时应报错，不自动重新登记或开放 bootstrap 来恢复它。

### 3.4 已配置的 daemon stdio：实例退出后恢复

1. MCP 宿主按原配置启动桥，进程 env 中仍是原 Agent token。
2. 已有实例则连接；本机实例未运行则以原 `data-dir` 拉起服务。
3. 服务加载持久化的 Principal、Grant 和 Credential verifier，桥携带 token 握手。
4. 凭据仍有效即恢复原身份；到期/撤销则报认证失败，进入 WebUI 更新配置。

不能让新实例把收到的 token 当作签发依据。更换 `data-dir`、丢失认证库或连接到另一个实例会导致原 token 无效；MCP 配置需要关联正确实例地址及必要的 data-dir。

## 4. 桥的探活与服务生命周期

当前桥在握手前会查询 `GET /api/daemon` 取得空闲期限；Streamable HTTP 桥在尚无 MCP session 时还通过 `GET /api/version` 心跳。前者见 `runDaemonFront`/`InspectEndpoint`，后者见 `internal/mcpbridge/pending.go:presenceURL`。只封住所有 `/api/*` 会破坏这两步。

提议两个精确注册的控制端点，P1 均要求有效 Agent 静态 Bearer，未来 OAuth 也可复用相同权限模型：

| 端点 | 返回/效果 | 不允许的能力 |
| --- | --- | --- |
| `GET /auth/agent/bridge-context` | 实例 ID、协议版本、该实例的空闲期限、当前凭据期限；只读 | 不返回 PID、日志路径、其他 Agent、主机或管理信息；不保活 |
| `POST /auth/agent/presence` | 只在 MCP session 建立前表示桥存活；计入 daemon 进程活动 | 不续签 token，不延长 Grant/认证期限，不产生人端管理权限 |

已有 MCP 连接建立后由 transport 连接负责存活计数。SSE 打开事件流前就认证；Streamable HTTP 的 presence 也必须认证。认证失败的探测/心跳不延长进程空闲计时。

当前 `main.go` 的 idle watcher 包在整个 HTTP handler 外，实施时必须让存活计数服从认证结果；只新增控制路由仍不足以阻止被拒绝的请求延长进程寿命。

这两个端点是“Agent 业务面只开放 MCP”的窄控制例外，不是放开 `/auth/*` 或 `/api/*`。`/api/daemon` 详情和 `/api/daemon/stop` 仍属于 Admin 管理面。桥只验证自己的 Agent token，不读取或索取人的登录凭据。

探测必须区分“服务在线但凭据无效”和“服务不在线”。401/403 不能触发启动另一个实例、换端口、关闭认证或改写签发库。利用公开的实例文档识别服务只能辅助在线判断，不能证明身份。首次新进程的就绪判定与 Agent 握手分开，不再把管理接口返回 PID 当成 Agent 接入前提。

认证之前不得把代理猜测的 MCP ping/initialize 注入宿主消息流；首期保留桥转发协议的职责，用独立控制端点完成探测。第三方上游 MCP 不应收到这些 Termcp 专用控制请求，只有已明确配置为 Termcp 的目标才启用它们。

## 5. 简易 token 版本：日常使用、替换与失败提示

| 事件 | HTTP/SSE 直连 | stdio / daemon stdio |
| --- | --- | --- |
| 新对话、transport 断开后重连 | 宿主继续携带同一 header | 桥继续携带同一 env 中的 token |
| 服务重启 | 宿主重新 initialize，原 token 验证通过后建立新 session | 初版由当前桥报告失效并退出，宿主新启动桥后重新 initialize；daemon stdio 在新桥启动时 Ensure |
| 用户替换 token | 更新 header 或宿主实际引用的环境变量，并重载 MCP | 更新 MCP 配置 `env`，重启桥进程；运行中的 env 不随文件变化 |
| 撤销/到期 | 401，已有长期连接也失效 | 将认证错误转成宿主可识别的失败，并在 stderr 提示更新配置 |
| 已认证但无对应操作权限 | 403/业务权限错误，不要求重新签发 | 同左；不能以重登绕过策略 |

替换由 Admin 在同一 Agent 页面创建新 Credential；新旧 token 短暂并存。向导按原接入方式生成替换配置，确认新 token 成功 `initialize` 后提示撤销旧 token。新 token 连上一次只证明有一个客户端更新成功；若同一凭据被多个进程使用，应展示旧 token 后续活动，由 Admin 决定何时撤销，不自动宣称全部迁移完成。疑似泄露可以立即撤销，无须等待新连接。

兼容版本不做周期自动换证。给静态配置设明确期限并允许人替换即可；不能一面保持纯静态 header/env，一面承诺服务端自动更新宿主。将配置交给另一个有 shell 的 Agent 执行属于用户选择的安装方式，那个 Agent 也需要宿主配置写权限，必要时仍由用户重载。

## 6. UI、实例定位与凭据边界

“连接 Agent”向导先选择目标实例及连接目的，再选择宿主：

| 用户选择 | 建议生成 |
| --- | --- |
| 宿主支持 OAuth，连接已部署服务 | 未来实现 OAuth 后再生成仅含 MCP URL 的授权配置；当前仍按 P1 token 接入 |
| 宿主支持 HTTP header，连接已部署服务 | 完整 `/stream` URL 与 Bearer header；SSE 作为兼容选项 |
| 宿主只支持 stdio，连接已部署服务 | `stdio <完整 URL>` 与 Agent env |
| 本机服务需要自动启动/退出 | `daemon stdio` 与 Agent env；必要时包含稳定 data-dir/非默认端口 |

浏览器页面在 loopback 不证明 MCP 宿主也在同一台机器，例如浏览器经 SSH 转发访问远端实例。远端容器里的 `127.0.0.1` 也不是宿主机。向导让用户确认“从 MCP 客户端可达的地址”，然后保留完整 scheme/host/port/path；不能仅凭页面 hostname 自动决定启动本机 daemon。

HTTP/SSE 所有请求绑定同一 Principal/Grant；Streamable HTTP 的 session ID 及 SSE 消息 session 不能换主体复用。token 不放 URL。桥遇到跨 origin 的 SSE 消息目标或重定向不得转发凭据；反向代理的同源路径前缀必须正确保留。

OAuth 安装配置不包含人工复制的访问/刷新凭据；静态兼容配置只包含 Agent token。不向宿主交付 Admin Cookie、密码、API token 或本机管理凭据。多个宿主需要独立审计时分别创建 Agent；同一静态 token 复制多份只能得到同一身份。所有人端管理记录还需保留 owner 用户，给多用户授权留出接口。

## 7. 分阶段实施与验收

1. 共用基础：首次运行设置 Admin，配置及凭据先明文持久化到默认 `.termcp`；HTTP/daemon 共用配置、权限、身份及撤销状态，后续配合加密系统。
2. 权限与桥适配：可信 Actor、权限过滤、MCP session 绑定与撤销，沿用既有 Ensure 复用机制；明确多桥保活、重启后初始化及结果不明时不重放。
3. **P1 简易 token**：WebUI 新增 Agent、一次性返回随机 token、列表及撤销；同批实现 stdio 读取 token。用户自行填写 header/env，支持人工重发和更新，不增加自动安装或换证依赖。
4. 同步配置模板和安装文档：HTTP/stdio 共用 P1 凭据模型，保留完整实例 URL；认证失败不自动降级成匿名访问。
5. **未来 OAuth**：标准 HTTP 授权服务与客户端兼容、stdio profile/授权/保存/刷新均延期，当前不实现。

OAuth 整体属于未来计划，不列入当前桥适配或 P1 的必需工作。多桥资源视图、后台任务是否阻止空闲退出等尚未确认的规则，继续按协商讨论稿标注待定。

未来 OAuth 再验收发现、PKCE、回调校验、resource/audience、刷新及撤销；本轮不执行或要求这些能力。P1 token 按下面的四种配置形态验收。

实施验收至少覆盖 A/B/C/D：有效/缺失/撤销 token、SSE GET 与 POST 主体一致性、Streamable GET/POST/DELETE、启动后认证恢复、桥初始化前保活，以及越权不能访问 daemon stop。额外验证非默认端口、HTTPS/路径前缀、远端 stdio 不启动本机实例、三 OS 环境继承与子进程剥离、同一 Agent 双 token 替换、不同 data-dir 误连及初始化超时。

当前认证源码仍是共享凭据模式，本次仅核对代码并设计交互，没有声称上述行为已经可用，也没有修改业务测试。

managed 模式还须修改现有启动验证：不能因未设置旧 `--auth-token` 就拒绝已经初始化的 managed 服务，也不能把 Agent env 当成服务端认证配置来满足旧验证。非 loopback 部署须先完成受控初始化和 HTTPS 部署；显式旧/open 模式与 managed 模式分开选择，不能请求失败后自动降级。
