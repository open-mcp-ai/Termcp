# dev 对外接口与操作清单

基线：`dev`，提交 `76b320976ae671721f3370983f5b0d6b412685b1`，整理日期：2026-10-25。

本文件是后续接口鉴权、DSL 操作授权的现状底表，不是新的实时 API 使用手册。运行时 `help` / `help <verb> --all` 为 DSL 的实时参考；传输参数见 [HTTP API](../api.md)。后续代码变化需要重新核对本快照。

基于此清单的后续方案见 [Agent 权限机制设计](agent-permissions.md)，其中的操作 ID 与管理 API 均为提案，不是本快照已经实现的接口。

## 1. 范围与已确认方向

- 主机连接配置按条独立存储；加密实现采用 [逐条信封方案](config-encryption.md) 的 Main Key → DEK → 正文层级，不恢复 `encryption-test` 的 `hosts.enc` 整库文件，也不采用 age。接口鉴权与管理路由仍未实现。
- 暂不纳入会话输入输出及历史记录加密。
- 后续先按接口和 DSL 的资源、操作划分权限，再逐步细化为可供人配置的 Agent 权限；本文的“当前检查”只描述现状，不是已经实施的 token scope。
- 明文凭据不能随摘要或普通读取轻易返回。保留用户明确执行的默认明文导出，也保留用户自选密码的加密导出；导出格式与导出权限是两个维度。
- 审批、修改审阅规则、管理扩展和停止实例均纳入盘点，不能因为请求来自 loopback 就认定调用者是人。

覆盖所有由当前程序注册的 HTTP 入口、兼容路径、WebSocket 消息、Termcp 自己发布的 MCP 对象、DSL 动词及资源子操作，并补充 CLI/内部桥接入口。内部 Go 函数、测试服务和第三方远端服务的全部 API 不作为 Termcp 自身 API 数量统计；远端任意方法的透传入口会明确列出。

## 2. 数量与现有鉴权模型

| 项目 | 数量 / 范围 |
| --- | --- |
| HTTP 注册模式 | 73：68 条 `/api/*`（含 WS 升级、配置加密管理和 9 条兼容路径）、3 条 MCP 传输、2 条互斥静态资源注册 |
| 应用 REST 注册模式 | 67，等于 68 条 `/api/*` 去掉 WS 升级；包含 `/api/term`、配置加密管理和兼容路径 |
| DSL 正式动词 | 19；按资源和子操作继续展开，不能只做 19 个开关 |
| term 顶层参数 | 13 |
| capability 表现有 ID | 45；有 ID 不代表所有对应入口都已检查 |
| MCP 应用对象 | 1 个工具 `term`、1 个提示词 `learn-api`、0 个自定义资源 |
| WebSocket 应用消息 | 客户端 4 种；服务端 6 种，另有协议 ping/pong/close |

当前 `internal/auth` 使用一把可选的部署 token/hash，接受 Bearer、Basic 和 `termcp_token` cookie。配置认证后中间件包住整棵 HTTP 路由；`GET/HEAD /api.md` 是免认证例外。未配置时 loopback 可无认证运行；显式禁用认证另有启动配置。哈希配置本身也被接受为登录凭据。

当前没有 #130 的 human/agent Principal、scoped token 表、token 签发吊销 API，也没有 storage/vault 解锁接口。认证中间件不区分调用者能做哪些操作。扩展会话的 `allow` / `paths` 是资源侧限制，不是调用者的权限配置；普通会话默认不带这些限制。

`X-Termcp-Namespace` 在 MCP/DSL 路径上决定命名空间视图，由调用者声明，尚未与认证身份绑定，不能当作可信的 Agent 身份。专用 REST 和 Web UI 路径不能据此推定已拥有相同的命名空间隔离。

下表“当前 capability 检查”为 `—` 表示在所列 handler 及其相关路径未发现对应的资源白名单检查，仍受全局 HTTP 认证配置影响；不是“无论何种配置都可匿名访问”。`+ paths` 表示同时执行资源路径限制。REST/WS 的模板白名单检查不会自动增加 MCP 审阅策略。

## 3. HTTP 完整清单

参数列给出权限梳理所需的关键输入；完整字段及响应结构继续以 handler 和 `docs/api.md` 为准。所有显式 GET 路由的注册模式也匹配 HEAD；handler 和 SDK 对具体方法的响应仍由自身实现决定。静态资源表中的 `*` 是注册未限 method，不代表所有 method 都有业务功能。

### MCP 传输

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /sse` | 建立旧式 SSE 连接；MCP 客户端参数；X-Termcp-Namespace | — | 承载 DSL 结果与通知 | `main.go:395` |
| `POST /message` | 提交 SSE 会话的 JSON-RPC 请求；sessionId；JSON-RPC 请求体；X-Termcp-Namespace | — | 承载 DSL 参数 | `main.go:396` |
| `SDK /stream` | MCP Streamable HTTP；GET/POST/DELETE 由 SDK 分派；Mcp-Session-Id、MCP-Protocol-Version、X-Termcp-Namespace | — | 承载 DSL 参数与结果；路由注册不指定 method；不意味着接受所有方法 | `main.go:397` |

### 命令入口

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `POST /api/term` | 执行 DSL 批次；13 个顶层参数；X-Termcp-Namespace | 按实际 DSL 操作 | 命令、配置、文件与协议帧均可能含秘密；REST 命令入口；不是仅授权 /api/term 就完成操作授权 | `main.go:405` |

### 实例与管理

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/version` | 读取版本；无 | — | 无直接凭据返回 | `internal/webui/handler_session.go:19` |
| `GET /api/clients` | 列出 MCP 客户端和浏览器连接者；无 | — | 连接者地址、客户端信息 | `internal/webui/viewers.go:175` |
| `GET /api/activity` | 读取跨会话活动；limit、since_ms、session_id | — | 活动与命令内容可能含秘密 | `internal/webui/activity.go:91` |
| `GET /api/daemon` | 读取 daemon 状态与实例信息；无 | — | 实例与运行路径元数据 | `internal/webui/daemon.go:17` |
| `POST /api/daemon/stop` | 停止实例；无 | — | 无直接凭据返回；进程级操作 | `internal/webui/daemon.go:31` |

### 连接配置

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/connections` | 列出连接摘要；无 | — | 地址、用户名；摘要不含密码 | `internal/webui/handler_conn.go:37` |
| `GET /api/connections/batch` | 批量导出连接原文；names；X-Termcp-Password 请求头 | — | 默认明文 TOML；非空导出密码时加密；当前 dev 的导出实现还会包含临时配置；属于现状记录，后续是否调整单独决策 | `internal/webui/handler_conn.go:164` |
| `POST /api/connections/batch` | 批量导入持久或临时配置；temporary；TOML 或加密文件；X-Termcp-Password | — | 接收凭据；加密文件密码与服务登录凭据独立 | `internal/webui/handler_conn.go:219` |
| `GET /api/connections/{name}` | 读取单条原始 TOML；name | — | 原文可含密码、私钥、口令、跳板及代理凭据 | `internal/webui/handler_conn.go:81` |
| `PUT /api/connections/{name}` | 创建、覆盖、改名或切换临时状态；name；from、temporary；TOML 请求体 | — | 接收凭据；一个 PUT 同时承载多种操作；internal 覆盖另有约束 | `internal/webui/handler_conn.go:101` |
| `DELETE /api/connections/{name}` | 删除配置；name（支持逗号批量） | — | 无直接凭据返回；同时影响后续能否连接 | `internal/webui/handler_conn.go:297` |
| `POST /api/connections/test` | 测试 SSH 连通与认证；原始 TOML 请求体（完整连接配置） | — | 接收并使用测试凭据；会发起真实 SSH 连接 | `internal/webui/handler_conn.go:331` |

### 扩展配置

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/extensions` | 读取扩展摘要与运行状态；无 | — | 连接、启动命令和运行信息 | `internal/webui/handler_extension.go:109` |
| `POST /api/extensions` | 创建扩展并交给协调器运行；JSON 或 TOML；name、kind、target、command/args、url/headers、allow、paths、forwards、alias | — | 接收认证头和启动内容；可引入新进程、远端工具与网络访问 | `internal/webui/handler_extension.go:265` |
| `GET /api/extensions/batch` | 导出扩展模板原文；names | — | 原始模板可能含认证 headers 或命令秘密 | `internal/webui/handler_extension.go:422` |
| `POST /api/extensions/batch` | 批量导入扩展模板；TOML 请求体 | — | 接收认证头和启动内容 | `internal/webui/handler_extension.go:449` |
| `GET /api/extensions/{name}` | 读取扩展模板；name；format=toml 可读原文 | — | 模板原文和 headers 等字段可能含凭据 | `internal/webui/handler_extension.go:121` |
| `PUT /api/extensions/{name}` | 更新或启停扩展；name；JSON 或 TOML | — | 接收认证头和启动内容 | `internal/webui/handler_extension.go:329` |
| `DELETE /api/extensions/{name}` | 删除扩展并释放关联资源；name（支持逗号批量） | — | 无直接凭据返回；受内置、只读模板约束 | `internal/webui/handler_extension.go:363` |
| `POST /api/extensions/{name}/retry` | 重试或重建扩展运行实例；name | — | 无直接凭据返回；可终止旧实例并重新启动 | `internal/webui/handler_extension.go:474` |

### 会话

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/sessions` | 列出所有会话；无 | — | 会话、目标与进程元数据 | `internal/webui/handler_session.go:23` |
| `GET /api/resolve` | 解析 termcp 定位符；url | — | 配置、会话、通道定位信息 | `internal/webui/resolve.go:33` |
| `POST /api/sessions` | 按连接配置建立 SSH 会话；JSON：ssh_config、command、args、mode、rows、cols、name、on_exit、parent_session_id | — | 使用凭据；命令参数可能含秘密；对应 session.start，但 REST handler 未接模板 capability 检查 | `internal/webui/handler_session.go:40` |
| `GET /api/sessions/{id}` | 读取会话详情；id | — | 会话、通道、目标元数据 | `internal/webui/handler_session.go:130` |
| `PATCH /api/sessions/{id}` | 修改会话显示名；id；JSON name | — | 无直接凭据返回 | `internal/webui/handler_session.go:232` |
| `DELETE /api/sessions/{id}` | 终止并删除会话与历史记录；id（支持逗号批量） | session.delete | 无直接凭据返回；删除记录；与 terminate/disconnect 保留记录不同 | `internal/webui/handler_session.go:206` |

### 审批

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/sessions/{id}/approval` | 读取或修改会话审阅开关；id；PATCH：enabled、need、timeout_seconds、scopes | — | 无直接凭据返回；读取与改变安全设置共用 handler；尚无独立审批管理身份 | `internal/webui/approval.go:50` |
| `PATCH /api/sessions/{id}/approval` | 读取或修改会话审阅开关；id；PATCH：enabled、need、timeout_seconds、scopes | — | 无直接凭据返回；读取与改变安全设置共用 handler；尚无独立审批管理身份 | `internal/webui/approval.go:50` |
| `GET /api/approvals` | 列出审批请求；session_id | — | 待审命令、文件操作及配置内容可能含秘密 | `internal/webui/approval.go:144` |
| `POST /api/approvals/{id}/approve` | 批准并执行，或拒绝请求；id；可选 JSON reason | — | 操作内容可能含秘密；批准会触发实际执行；尚无独立审批者权限 | `internal/webui/approval.go:173` |
| `POST /api/approvals/{id}/reject` | 批准并执行，或拒绝请求；id；可选 JSON reason | — | 操作内容可能含秘密；批准会触发实际执行；尚无独立审批者权限 | `internal/webui/approval.go:173` |

### 终端与输出

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/sessions/{id}/output-range` | 读取主通道或指定通道输出字节窗口；id；start 或 tail=1；max、stream | shell.output | 终端原文可能含秘密；兼容路径；id 可解析为 shell_id 或 session_id | `internal/webui/handler_shell.go:87` |
| `POST /api/sessions/{id}/shells` | 在已有会话中新建通道；id；JSON command、args、mode、rows、cols、name | shell.open | 命令参数可能含秘密 | `internal/webui/handler_shell.go:18` |
| `GET /api/sessions/{id}/shells` | 列出会话通道；id | — | 通道与进程元数据 | `internal/webui/handler_shell.go:197` |
| `GET /api/shells/{id}/output-range` | 读取通道输出字节窗口；id；start 或 tail=1；max、stream | shell.output | 终端原文可能含秘密 | `internal/webui/handler_shell.go:81` |
| `GET /api/shells/{id}/marks` | 读取状态和输入输出标记；id；start、end 字节窗口 | session.message | 终端活动及关联输入信息 | `internal/webui/marks.go:52` |
| `GET /api/shells/{id}/lines` | 按逻辑行读取历史；id；from、count、content | shell.output | 终端原文可能含秘密 | `internal/webui/handler_lines.go:44` |
| `GET /api/shells/{id}/rail` | 按终端宽度读取行布局；id；cols、top、count、height | shell.output | 输出字节范围与活动标记 | `internal/webui/shellrail.go:108` |
| `DELETE /api/shells/{id}` | 关闭并删除通道元数据；id（支持逗号批量） | shell.close | 无直接凭据返回；与 archive 保留可寻址记录不同 | `internal/webui/handler_shell.go:215` |
| `POST /api/shells/{id}/archive` | 停止通道并保留记录；id（支持逗号批量） | shell.close | 无直接凭据返回 | `internal/webui/handler_shell.go:256` |
| `POST /api/shells/{id}/input` | 向通道输入文字并可选回车；id；JSON text、press_enter | shell.input | 接收任意终端输入；REST 作为操作者输入，不经过 AI 行级审批 | `internal/webui/shellio.go:33` |
| `POST /api/shells/{id}/key` | 向通道发送按键；id；JSON key、repeat | shell.key | 无直接凭据返回；REST 不经过 AI 行级审批 | `internal/webui/shellio.go:61` |
| `POST /api/shells/{id}/resize` | 调整 PTY 尺寸；id；JSON rows、cols | shell.resize | 无直接凭据返回 | `internal/webui/shellio.go:89` |

### 转发

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/forwards` | 列出全部转发；无 | — | 监听地址与网络拓扑 | `internal/webui/handler_forward.go:14` |
| `DELETE /api/forwards/{id}` | 关闭指定转发；id | forward.close | 无直接凭据返回 | `internal/webui/handler_forward.go:22` |
| `GET /api/sessions/{id}/forwards` | 列出会话转发；id | — | 监听地址与网络拓扑 | `internal/webui/handler_forward.go:161` |
| `POST /api/sessions/{id}/forwards` | 建立 local/remote/dynamic 转发；路径 id 优先，兼容 body session_id；direction、remote_host/port、bind_host、local_host/port | forward.open | 无直接凭据返回；三种方向共用操作；监听与目标地址应是后续资源限制维度 | `internal/webui/handler_forward.go:55` |

### 文件

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/sessions/{id}/files` | 读取文件元数据或目录列表；id；path | file.stat + paths | 文件与目录元数据 | `internal/webui/handler_file.go:58` |
| `GET /api/sessions/{id}/files/download` | 下载文件或字节范围；id；path、h.<Name>；Range 请求头 | file.read + paths | 任意获准路径内容可能含凭据 | `internal/webui/handler_file.go:118` |
| `PUT /api/sessions/{id}/files` | 移动或重命名文件；id；from、to | file.rename + paths | 无直接凭据返回 | `internal/webui/handler_file.go:414` |
| `POST /api/sessions/{id}/files/upload` | 上传、覆盖或分块续写文件；id；path、offset；Content-Range；原始或 multipart 请求体 | file.write + paths | 接收文件内容 | `internal/webui/handler_file.go:298` |
| `DELETE /api/sessions/{id}/files` | 删除文件或目录；id；path | file.delete + paths | 无直接凭据返回 | `internal/webui/handler_file.go:380` |
| `POST /api/sessions/{id}/files/dir` | 创建目录；id；path | file.mkdir + paths | 无直接凭据返回 | `internal/webui/handler_file.go:453` |

### WebSocket

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `GET /api/ui/ws` | 升级为浏览器实时连接；WebSocket Upgrade；cookie 或 Authorization | — | 握手后推送会话、配置摘要、扩展、连接者、通知与审批事件；消息级操作见单独清单；握手不等于消息级权限 | `internal/webui/ws.go:120` |

### 兼容路由

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `POST /api/sessions/start` | 按连接配置建立 SSH 会话；JSON：ssh_config、command、args、mode、rows、cols、name、on_exit、parent_session_id | — | 使用凭据；命令参数可能含秘密；对应 session.start，但 REST handler 未接模板 capability 检查 | `internal/webui/handler_session.go:40` |
| `GET /api/sessions/{id}/child-shells` | 重定向到通道列表；id | — | 无直接凭据返回；301；不是独立能力 | `internal/webui/handler_shell.go:75` |
| `POST /api/sessions/{id}/terminate` | 断开会话，保留历史记录；id（支持逗号批量） | session.terminate | 无直接凭据返回；与 DELETE /api/sessions/{id} 的清除语义不同 | `internal/webui/handler_session.go:174` |
| `POST /api/sessions/{id}/disconnect` | 断开会话，保留历史记录；id（支持逗号批量） | session.terminate | 无直接凭据返回；与 DELETE /api/sessions/{id} 的清除语义不同 | `internal/webui/handler_session.go:174` |
| `POST /api/sessions/{id}/close-shell` | 关闭会话主通道；id=session_id | shell.close | 无直接凭据返回 | `internal/webui/handler_shell.go:289` |
| `POST /api/forwards` | 建立 local/remote/dynamic 转发；路径 id 优先，兼容 body session_id；direction、remote_host/port、bind_host、local_host/port | forward.open | 无直接凭据返回；三种方向共用操作；监听与目标地址应是后续资源限制维度 | `internal/webui/handler_forward.go:55` |
| `DELETE /api/sessions/{id}/files/delete` | 删除文件或目录；id；path | file.delete + paths | 无直接凭据返回 | `internal/webui/handler_file.go:380` |
| `POST /api/sessions/{id}/files/rename` | 移动或重命名文件；id；from、to | file.rename + paths | 无直接凭据返回 | `internal/webui/handler_file.go:414` |
| `POST /api/sessions/{id}/files/mkdir` | 创建目录；id；path | file.mkdir + paths | 无直接凭据返回 | `internal/webui/handler_file.go:453` |

### 静态资源

| 方法与路径 | 操作与关键输入 | 当前 capability 检查 | 敏感内容 / 语义 | 实现位置 |
| --- | --- | --- | --- | --- |
| `* /` | 静态 UI 与资源；资源路径 | — | 文档与静态资源；两条为互斥构建分支；普通构建通过 / 提供 /api.md；仅 GET/HEAD /api.md 可免认证 | `internal/webui/handler.go:254` |
| `* /api.md` | 纯 API 构建的文档路由；资源路径 | — | 文档与静态资源；两条为互斥构建分支；普通构建通过 / 提供 /api.md；仅 GET/HEAD /api.md 可免认证 | `internal/webui/handler.go:262` |

### 需要防止混淆的当前语义

- `POST /api/sessions/{id}/terminate` 和 `disconnect` 只断开连接并保留记录；`DELETE /api/sessions/{id}` 还会删除记录。当前 `docs/api.md` 的兼容路由表把前两者直接写成 DELETE 的映射，语义不准确，本清单以 handler 为准。
- `POST /api/shells/{id}/archive` 保留可寻址的通道记录；DELETE 通道的语义不同，但当前两者同用 `shell.close` 检查。
- `PUT /api/connections/{name}` 同时包含创建、覆盖、改名和临时状态变化；以后不能只按 PUT 方法给一项模糊权限。
- `GET /api/connections` 是摘要；`GET /api/connections/{name}` 是原始 TOML。两者不能共用一个可以返回凭据的“读”权限。
- `GET /api/extensions/{name}?format=toml` 以及扩展导出会读取原文；DSL `get ext` 会返回 headers 等配置；REST JSON 详情会返回 command/args 等内容。扩展读取同样需要区分摘要和秘密。

## 4. WebSocket 消息级接口

入口为 `GET /api/ui/ws`。全局 HTTP 认证只在握手时执行；当前 `CheckOrigin` 返回 true，WS 实现没有调用者 Principal 或按身份的 scope。此处逐项记录消息，避免日后只保护 Upgrade 路由。

| 方向 | type | 字段 | 作用 | 当前检查 | 敏感内容 |
| --- | --- | --- | --- | --- | --- |
| 客户端 → 服务端 | watch_add | id=shell_id | 订阅通道输出；会注册 reader | 未见 shell.output 白名单检查 | 终端原文 |
| 客户端 → 服务端 | watch_remove | id=shell_id | 取消此连接上的输出订阅 | 连接内 watch 管理，无独立 capability | 无直接凭据返回 |
| 客户端 → 服务端 | input | id、d、nl | 原始文字输入，可附加换行 | 资源模板 shell.input；不经 AI 行级审批 | 输入可含秘密 |
| 客户端 → 服务端 | resize | id、rows、cols | 修改 PTY 尺寸 | 资源模板 shell.resize | 无直接凭据返回 |
| 服务端 → 客户端 | sessions | sessions、connections、clients、mcp_clients、extensions | 初始及状态变化时推送综合列表 | 全量列表，未见按调用者权限过滤 | 配置摘要、运行与连接者元数据 |
| 服务端 → 客户端 | terminal | id、d | 输出原文流 | 由 watch_add 的订阅驱动 | 输出可含秘密 |
| 服务端 → 客户端 | terminal_done | id | 通道输出结束 | 由 watch_add 的订阅驱动 | 无直接凭据返回 |
| 服务端 → 客户端 | shell_activity | shell_id、src、submit | 输入来源及提交状态 | 广播；未见按调用者 scope 过滤 | 跨会话活动 |
| 服务端 → 客户端 | ui_notify | title、message、level、duration_seconds、session_id | 用户通知 | 广播；未见按调用者 scope 过滤 | 通知内容 |
| 服务端 → 客户端 | approval | session_id、request 与审阅状态 | 审批状态变化 | 广播；未见独立审批者权限过滤 | 待审命令或操作参数 |

协议控制帧 ping/pong 用于存活检测，close 用于连接关闭；它们不是会话能力。WS 连接持有的输出订阅、列表推送、通知和审批广播都需要后续独立纳入授权/吊销语义。

## 5. MCP 与 CLI/桥接入口

Termcp 通过 `/sse` + `/message` 或 `/stream` 承载 MCP，stdio 是 HTTP 桥，不是另一套操作实现。Termcp 自己发布的应用对象如下；SDK 的协议生命周期不等于新的业务权限。

| 协议方法 / 对象 | 作用 | 权限梳理含义 |
| --- | --- | --- |
| initialize / notifications/initialized / ping | MCP 初始化与存活 | SDK 协议生命周期 |
| tools/list | 枚举唯一工具 term | 当前没有按调用者 scope 裁剪 DSL 能力 |
| tools/call：term | 运行 DSL | 必须按解析并展开后的实际操作授权 |
| prompts/list | 列出 learn-api | 提示词发现 |
| prompts/get：learn-api | 读取 API 学习提示词，可选 task 参数 | 返回本实例 API 文档地址与学习指引 |
| sampling/createMessage（服务端 → 客户端） | 将子 MCP 的采样请求转发给宿主 Agent 并返回结果 | 客户端需声明 sampling；当前透传不检查提示词内容；参数/响应及授权应单独盘点 |

当前未启用的 SDK 方法族包括 `logging/setLevel`、`resources/list`、`resources/templates/list`、`resources/read`、`tasks/get`、`tasks/list`、`tasks/result`、`tasks/cancel`、`completion/complete`；这些由 SDK 按 capability 拒绝，不是当前应用能力。DSL 的 `ls/get/close task` 是 Termcp 自己的传输任务，与 MCP SDK tasks 方法族无关。

不发布自定义 MCP resource，也不把 `session_start`、`shell_input`、`ssh_config`、`extension` 等内部 handler 名视为额外公开 MCP 工具。它们是 DSL 内部调用和旧权限词汇，不会出现在当前 `tools/list` 中。

| 本地入口 | 实际作用 | 对鉴权盘点的意义 |
| --- | --- | --- |
| `termcp [flags]` | 前台运行实例 | 本地启动与配置控制；不是 HTTP 路由 |
| `termcp daemon start` | 探测或启动后台实例 | 复用实例管理 HTTP；本地启动权还受 OS 权限约束 |
| `termcp daemon status` | 读取实例状态 | 对应实例管理读取 |
| `termcp daemon stop` | 请求停止实例 | 对应管理停止权限 |
| `termcp stdio [endpoint]` | stdin/stdout 桥接 MCP HTTP | 转发部署凭据与可选 namespace，不额外产生操作权限 |
| `termcp daemon stdio` | 确保实例启动并桥接 | 同时包含本地启动与 MCP 访问 |
| `--gen-auth-hash` | 本地生成登录凭据 verifier | 本地辅助命令，无 token 创建/吊销语义 |
| `--help`、`--version`、裸 `daemon` | 参数、版本与动作说明 | 元数据读取 |
| 内置 SSH/SFTP | 进程内 SSH，运行 shell、exec、文件与 TCP 转发 | 不额外暴露公网 TCP 监听；由会话/文件/转发入口进入 |
| 内置 `termcp-mcp-bridge` 桥命令 | 在扩展通道内桥接远端 MCP | 远端 URL、headers、目标和工具名是后续授权维度 |

出处：`subcommand.go`、`main.go`、`daemonfront.go`、`internal/mcpbridge`、`internal/sshserver`。当前没有 `termcp token` 或 `termcp vault` CLI。

## 6. DSL 顶层参数（13 项）

MCP `term` 与 `POST /api/term` 共用参数声明和解析。参数改变传输、值替换或输出窗口，不应被当作独立业务权限；值替换和 alias 展开后仍需检查实际动作。

| 参数 | 类型 / 默认值 | 含义 |
| --- | --- | --- |
| cmd | string，默认空 | DSL 批次；空值返回 guide |
| timeout | number，默认 2 秒，>=0 | 等输出的窗口；60 秒是建议值而非硬上限 |
| bytes_limit | number，默认 8192 | 每 slot 输出字节预算；0 不返回输出字节，否则至少 4 |
| lines_limit | number，默认 200，>=0 | 每 slot 行数预算；0 不限行数 |
| total_bytes_limit | number，默认 32768 | 整批回复字节预算 |
| on_time_overflow | truncate / fold，默认 truncate | 时间窗口到达时的输出处理 |
| on_bytes_overflow | truncate / fold，默认 truncate | 字节预算到达时的输出处理 |
| on_lines_overflow | truncate / fold，默认 truncate | 行数预算到达时的输出处理 |
| learn_mode | boolean，默认 false | 附带动词用法 |
| structured_content | boolean，默认 false | 额外返回结构化投影 |
| args | string[] | 位置值 $1、${10}；当前会记录值 |
| vars | map[string]string | 具名值 $NAME；当前会记录值 |
| secrets | map[string]string | 具名秘密值；日志不写值；不能保证终端回显或下游不会输出 |

`secrets` 是显式的日志记录控制，不是会话加密，也不是通用输出打码。字面写入 `cmd` 的密码、放入 `args` / `vars` 的值仍可能进入日志。

## 7. DSL 的 19 个动词

从本提交编译独立实例，通过 `/api/term` 读取 `help` 与每个 `help <verb> --all`，并与 `internal/directive` 表逐项对照。表中 flags 是声明全集；具体资源种类会再限制合法组合。`read` / `get` 的 `--op` 有写操作分支，不能根据动词英文判断只读。

| 动词 | 含义 | 声明的资源 kind | 声明 flags |
| --- | --- | --- | --- |
| `connect` | 由 host 新建 SSH 连接及首通道 | 按目标地址解析 | --name, --argv, --rows:number, --cols:number, --size:geometry, --on-exit:enum=keep,close |
| `open` | 在会话内开通道、转发、协议 peer 或附着浏览器 target | forward, mcp, cdp, jrpc | --pipe, --pty, --argv, --name, --on-exit:enum=keep,close, --rows:number, --cols:number, --size:geometry, --bind-host, --remote-host, --remote-port:number, --local-port:number, --local-host, --dynamic, --remote, --target |
| `ls` | 按资源种类列举 | forward, file, host, ext, alias, cdp, mcp, jrpc, task | --all |
| `get` | 读取详情、配置、任务或文件子操作 | file, host, ext, protocol, cdp, mcp, jrpc, task | --op |
| `close` | 停止会话/通道/转发/peer，或取消任务 | forward, mcp, cdp, jrpc, file, task | --grace:number, --forward-id |
| `rm` | 删除会话记录、文件、host、ext 或 alias | file, host, ext, alias | --force, --dry-run |
| `run` | 终端输入、回车并读取输出窗口 | 按目标地址解析 | --tail:number, --grep, --grep-limit:number, --encoding |
| `input` | 输入原文，不自动提交 | 按目标地址解析 | 无 |
| `key` | 发送命名按键 | 按目标地址解析 | --repeat:number |
| `resize` | 修改终端尺寸 | 按目标地址解析 | 无 |
| `span` | 读取通道状态跨度 | 按目标地址解析 | 无 |
| `reader` | 创建或注销输出 reader | 按目标地址解析 | --drop |
| `read` | 读取终端、文件、协议帧，或启动复制管道 | file, cdp, mcp, jrpc | --cursor:number, --since:number, --at:number, --offset:number, --bytes:number, --from-line:number, --tail:number, --stream:enum=o,e,all, --raw, --grep, --grep-limit:number, --frame, --encoding |
| `write` | 写文件、替换内容及文件属性子操作 | file | --data, --mode, --local-path, --offset:number, --length:number, --size:number, --old-text, --new-text, --op, --uid:number, --gid:number, --atime:number, --mtime:number |
| `call` | 发送/请求协议帧，或调用远端 MCP 工具 | cdp, mcp, jrpc | --send, --json |
| `create` | 创建 host、ext、alias、目录或链接 | host, ext, alias, file | --name, --host, --user, --port, --password, --private-key, --key-passphrase, --trust-unknown-host, --known-hosts, --dial-timeout-seconds, --proxy, --description, --default-shell, --default-mode, --default-approval, --jump-host, --jump-user, --jump-port, --jump-password, --jump-private-key, --jump-key-passphrase, --source, --kind, --target, --command, --args, --url, --headers, --allow, --paths, --notes, --enabled, --forwards, --link-path, --mode, --op |
| `edit` | 编辑配置、别名，或重启 ext | host, ext, alias | --name, --host, --user, --port, --password, --private-key, --key-passphrase, --trust-unknown-host, --known-hosts, --dial-timeout-seconds, --proxy, --description, --default-shell, --default-mode, --default-approval, --jump-host, --jump-user, --jump-port, --jump-password, --jump-private-key, --jump-key-passphrase, --kind, --target, --command, --args, --url, --headers, --allow, --paths, --notes, --enabled, --forwards, --restart |
| `notify` | 发用户通知 | 按目标地址解析 | --level, --sticky, --on, --title, --for |
| `help` | guide、动词/资源帮助与完整细节 | 按目标地址解析 | --all |

## 8. DSL 按资源、子操作展开

下表“对应 capability”表示当前映射/可复用的 ID，不表示已经具备调用者级授权。带“缺独立 ID”或“直接管理器”的路径是下一步权限梳理需要补齐的项。主机、扩展、任务与 alias 等顶层资源常没有现成会话可供 `Restriction` 检查，不能因为存在 `refuseAction` 就视为已隔离。

### 会话与通道

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `connect <host>` | 建立连接及首个通道 | session.start | 使用保存的凭据；--argv 可指定启动命令；当前不因 session.start 自动入审；使用凭据与启动参数 |
| `open <session>` | 在已有连接创建 PTY 或 pipe 通道 | shell.open | --argv、--pty、--pipe、--name、--on-exit、尺寸 |
| `ls [<session>] [--all]` | 列举会话、通道及 peer | session.list / shell.list（概念映射） | 由 directivePS/列表生成路径直接读取；默认折叠已结束通道，--all 显示；运行与目标元数据 |
| `get <session>` | 读取会话详情 | session.info（概念映射） | direct manager 路径；不能假定已检查 session.info |
| `get <channel>` | 读取通道详情 | session.info / shell.output（缺独立通道详情 ID） | 通道存在性检查与 session 详情分开 |
| `close <session> [--grace N]` | 终止会话并保留记录 | session.terminate |  |
| `close <channel>` | 停止通道并保留记录 | shell.close | 内部调用 shell_archive |
| `rm <session> [--force] [--dry-run]` | 删除会话和记录 | session.delete | dry-run 与实际删除应分别记录实际效果 |

### 终端输入输出

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `run <channel> $1` | 输入命令、提交并读取窗口 | shell.input + shell.key + shell.output | 多个底层动作；run 的 --tail/--grep/--grep-limit/--encoding 控制本次读窗；输入与输出均可能含秘密 |
| `input <channel> $1` | 输入文字，不自动回车 | shell.input | AI 来源走已有行级审阅路径；输入可能含秘密 |
| `key <channel> <key> [--repeat N]` | 发送按键 | shell.key | Enter 可完成一行并触发审批；Ctrl-C 等也有实际副作用 |
| `resize <channel> <rows> <cols>` | 调整 PTY | shell.resize |  |
| `read <channel>` | 读取输出窗口 | shell.output | --cursor、--from-line、--tail、--stream、--raw、--grep、--grep-limit、--encoding；终端原文可能含秘密 |
| `span <channel>` | 读取状态跨度与标记 | session.message | 内部调用 message/list 读取跨度；与 shell.output 是不同的现有 ID |
| `reader <channel>` | 注册独立输出 reader | shell.reader.register | 改变游标/资源状态，虽然不改远端文件 |
| `reader <channel> --drop <id>` | 注销输出 reader | shell.reader.unregister |  |

### 主机配置

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `ls host` | 列举连接配置 | profile.list | 顶层资源，无现成 session Restriction 可约束；目标元数据 |
| `get host <name>` | 读取连接字段 | profile.list（无独立 get ID） | password/key_passphrase 只回 has_*，但 private_key、proxy 字段仍直接返回；不可笼统标为无秘密；private_key、代理配置、跳板配置可能含凭据 |
| `create host <name>` | 创建连接配置 | profile.create | 需要 --mcp-manage-ssh-configs；可以接收密码、私钥及跳板字段；接收凭据 |
| `create host <name> --source <other>` | 复制连接配置 | profile.copy | 复制也会复制原有凭据；需要管理配置开关；使用并复制凭据 |
| `edit host <name>` | 编辑连接配置 | profile.edit | 需要管理配置开关；允许修改连通与默认审阅设置；接收凭据 |
| `rm host <name>` | 删除连接配置 | profile.delete | 需要管理配置开关 |

### 扩展

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `ls ext` | 列举扩展状态 | ext.list | 顶层资源；启动命令/配置需要作为敏感元数据处理；模板与运行信息 |
| `get ext <name>` | 读取单条模板 | ext.get | 读取 headers、command、args、alias、allow、paths 等内容；headers 和启动内容可能含凭据 |
| `create ext <name>` | 安装扩展模板 | ext.create | 包括 SSH 通道、远端 MCP、CDP、JSON-RPC 等 kind，能扩大可达能力；接收认证头和启动内容 |
| `edit ext <name>` | 更新模板、启停和资源边界 | ext.update | --enabled、--allow、--paths、--forwards 不能都混为普通描述修改；接收认证头和启动内容 |
| `edit ext <name> --restart` | 重建扩展运行实例 | ext.retry | 与配置修改分开的动作；对应 REST retry |
| `rm ext <name>` | 删除扩展及关联运行资源 | ext.delete |  |

### 转发

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `ls <session> forward` | 列出转发 | forward.list | 网络拓扑信息 |
| `open <session> forward` | 创建 local 转发 | forward.open | 限制监听地址与远端目标比单一 capability 更细 |
| `open <session> forward --remote` | 创建 remote 转发 | forward.open | 改变远端监听及回连目标 |
| `open <session> forward --dynamic` | 创建 SOCKS 转发 | forward.open | 动态代理的目的地址不完全在创建时确定 |
| `close <session> forward <id>` | 关闭转发 | forward.close | 也支持 --forward-id <id> 形式 |

### 文件

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `ls <session> file <path>` | 读取目录和文件元数据 | file.stat + paths |  |
| `get <session> file <path> [--op stat]` | 读取文件元数据 | file.stat + paths |  |
| `read <session> file <path>` | 读取文件内容 | file.read + paths | 输出窗口与文本/hex 编码；文件可能含凭据 |
| `write <session> file <path> --data <value>` | 写入、追加或按偏移更新内容 | file.write + paths | --offset、--mode、--length 参与动作 |
| `write <session> file <path> --old-text <old> --new-text <new>` | 替换文件内容 | file.write + paths | 与普通写共用 capability |
| `write <session> file <path> --local-path <file>` | 上传服务端本地文件到目标 | file.write + paths | 可异步返回 task；local-path 是 Termcp 进程所在主机的路径；本地文件可能含凭据 |
| `create <session> file <path>` | 创建目录 | file.mkdir + paths | 此形式不是创建空普通文件 |
| `create <session> file <link> --target <target>` | 创建符号链接 | file.link + paths | --op symlink 为链接种类 |
| `create <session> file <new> --target <existing> --op link` | 创建硬链接 | file.link + paths |  |
| `rm <session> file <path>` | 删除文件或目录 | file.delete + paths |  |
| `get/write <session> file <path> --op chmod` | 修改权限 | file.perm + paths | write 仅给 --mode 且无内容也会进入 chmod；get --op 也可进入此写操作 |
| `get/write <session> file <path> --op chown` | 修改所有者 | file.perm + paths | --uid、--gid 是 write 的参数；get 别因动词只读就放行 |
| `get/write <session> file <path> --op chtimes` | 修改时间 | file.perm + paths | --atime、--mtime |
| `get/write <session> file <path> --op readlink` | 读取符号链接目标 | file.link + paths | 与创建链接共用一项 capability，应记录读写混合 |
| `get/write <session> file <path> --op truncate` | 截断文件 | file.fs + paths | --size 是 write 的参数；归属与 realpath/statvfs 相同 |
| `get/write <session> file <path> --op realpath` | 解析绝对路径 | file.fs + paths | 与 truncate 共用 ID，不能直接整组当只读 |
| `get/write <session> file <path> --op statvfs` | 读取文件系统统计 | file.fs + paths | 与 truncate 共用 ID |
| `get/write <session> file <path> --op getwd` | 读取远端当前目录 | file.getwd | 分派器接受此子操作；仅请求路径不等于实际路径约束 |
| `get/write <session> file <path> --op urls` | 生成文件上传下载 URL | file.urls + paths | URL 不应成为绕过对应文件读写权限的凭据；生成下载/上传入口 |
| `get/write <session> file <path> --op stat` | 读取文件元数据 | file.stat + paths | write 动词也可进入读取子操作 |

### 文件与任务

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `read <source-channel> \| write <session> file <path>` | 将输出/帧复制成文件 | 来源读权限 + file.write（需逐端核对实际检查） | 异步 task；跨会话/跨资源边界，授权不能只检查管道一端；复制内容可能含秘密 |

### 任务

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `ls task` | 列举当前进程传输任务 | 缺独立 ID；直接 tasks 管理器 | 当前不是 MCP SDK 的 tasks/list |
| `get task <id>` | 查看或等待任务结果 | 缺独立 ID；直接 tasks 管理器 | 使用 timeout 等待；任务结束后保留回执一段时间 |
| `close task <id>` | 取消传输并清理部分文件 | 缺独立 ID；直接 tasks 管理器 | 取消具有文件副作用 |

### 协议 peer

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `open <session> mcp <url>` | 经会话打开远端 MCP | mcp.open | 当前检查会话模板；源码明确没有在此执行 review；URL/headers 或远端工具内容可能含秘密 |
| `open <session> cdp <url>` | 经会话打开浏览器调试 peer | cdp.open | 当前检查会话模板；可连接调试入口 |
| `open <session> jrpc <url>` | 经会话打开通用 JSON-RPC peer | jrpc.open | 当前检查会话模板；远端方法不由 Termcp 静态定义 |
| `ls <session> cdp/mcp/jrpc` | 列出该类 peer | 缺独立 peer.list ID | CDP peer 子层还可列出 targets/pages |
| `get <session> cdp/mcp/jrpc[:N]` | 读取 peer 或附着 target 详情 | 缺独立 peer.get ID |  |
| `get <session> cdp[:N] protocol [Domain]` | 读取浏览器协议定义 | 缺独立协议读取 ID | 定义来自实际浏览器，方法集合动态变化 |
| `get <session> mcp[:N] tools [tool]` | 读取远端工具列表或完整 schema | 缺独立远端工具发现 ID | 不是本机 tools/list；工具说明与 schema |
| `open <session> cdp:N --target <targetId>` | 附着指定浏览器 target | 缺独立 target.attach ID | 与创建初始 CDP peer 不是同一个动作 |
| `call <peer-or-channel> <frame>` | 请求 JSON-RPC/CDP 并读取匹配回复 | 缺独立 peer.call / method 权限 | 可执行任意远端方法；不能只检查最初的 *.open；帧参数与结果可能含秘密 |
| `call <peer-or-channel> --send <frame>` | 仅发送帧 | 缺独立 peer.send ID | 与同步 call 共用入口；帧可能含秘密 |
| `call <session> mcp:N <tool> [<json-args>]` | 按名称调用远端 MCP 工具 | 缺独立远端 tool 授权 | 远端工具名、参数、目标是后续权限维度；工具参数和结果可能含秘密 |
| `read <peer-or-channel> [--frame]` | 读取下一帧 | 缺独立 peer.read ID；通道帧读取需结合 shell.output | 网络 peer 与本地 pipe 两种来源；帧可能含秘密 |
| `read <session> cdp/mcp/jrpc[:N] --since N` | 按序列读帧日志/事件 | 缺独立 peer.events ID | 支持 target 与 grep 过滤；帧日志可能含秘密 |
| `read <session> cdp/mcp/jrpc[:N] --at N --offset B --bytes N` | 分页读取大帧 | 缺独立 frame.read ID | --since 与 --at 是不同维度；帧内容可能含秘密 |
| `close <session> cdp/mcp/jrpc` | 关闭协议 peer | 缺独立 peer.close ID |  |
| `close <session> cdp:N` | 解除附着 target | 缺独立 target.detach ID | 与断开整个 CDP peer 分开 |

### 别名

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `ls alias` | 读取 alias 表 | 缺独立 alias.list ID；进程内表 | 包含扩展安装的 alias 与运行时 alias |
| `create alias <name> <expansion>` | 创建运行时 alias | 缺独立 alias.create ID | 影响后续命令展开；重启不保留；展开内容可能含秘密 |
| `edit alias <name> <expansion>` | 修改运行时 alias | 缺独立 alias.edit ID | 不能把 alias 名作为最终授权对象 |
| `rm alias <name>` | 删除运行时 alias | 缺独立 alias.delete ID |  |

### 通知与帮助

| DSL 形态 | 实际动作 | 对应 capability / 当前边界 | 备注与敏感内容 |
| --- | --- | --- | --- |
| `notify [--on <session>] <body>` | 通知浏览器用户 | user.notify | --level、--title、--sticky 或 --for；通知正文 |
| `help [verb-or-node] [--all] / 空 cmd` | 读取动态帮助 | 缺独立 help ID；元数据操作 | 资源节点帮助与动词帮助均由运行时生成 |

### DSL 组合语法也必须保留权限边界

- `;` 批量：每个实际操作都需判定，不能只检查首条。
- `&&` / `||`：按实际执行分支检查，不能借条件组合绕过限制。
- `|`：当前为输出到文件的复制语义，读取端与写入端都要检查。
- 逗号目标批量：每个 session/channel 都需检查资源范围，不能只取首个目标。
- `$1` / `$NAME`、`args` / `vars` / `secrets`：替换后解析的动作与资源才是授权对象；秘密值不需要出现在权限记录中。
- alias 与扩展 alias：展开后继续核对真实动作、资源和远端方法；编辑 alias 本身也有控制面意义。
- `<session>`、`<channel>`、`termcp://` 定位符和 profile 名需要先规范化为同一资源身份，再判断资源权限。
- 现有 `file.perm`、`file.link`、`file.fs` 将不同子操作合在同一项中；`get --op chmod` 这类有效分派说明“按动词给读写权限”不充分。部分参数组合会被后续 handler 拒绝，权限判定仍应按最终子操作。

## 9. 现有 45 项 capability 原始底表

出处：`internal/capability/capability.go`。`GateOperator` / `GateDevice` / `GateReview` 是当前操作分类，不是已经认证的 human/agent 角色。`GateReview` 或非空审批 kind 也不保证所有入口都会排队：还需要经过实际 admission/操作路径。特别是当前 peer open 的源码明确只做模板白名单检查，审批钩子尚不在此执行。

| capability ID | Gate 分类 | 审批 kind | 路径参数 | 行级暂存 Staged |
| --- | --- | --- | --- | --- |
| `session.start` | GateDevice | 无 | 无 | 否 |
| `session.list` | GateOperator | 无 | 无 | 否 |
| `session.info` | GateOperator | 无 | 无 | 否 |
| `session.terminate` | GateDevice | 无 | 无 | 否 |
| `session.delete` | GateDevice | 无 | 无 | 否 |
| `shell.open` | GateDevice | 无 | 无 | 否 |
| `shell.list` | GateOperator | 无 | 无 | 否 |
| `shell.close` | GateDevice | 无 | 无 | 否 |
| `shell.input` | GateReview | KindShellInput | 无 | 是 |
| `shell.key` | GateReview | KindShellInput | 无 | 是 |
| `shell.output` | GateOperator | 无 | 无 | 否 |
| `shell.resize` | GateDevice | 无 | 无 | 否 |
| `shell.detect` | GateOperator | 无 | 无 | 否 |
| `shell.reader.register` | GateOperator | 无 | 无 | 否 |
| `shell.reader.unregister` | GateOperator | 无 | 无 | 否 |
| `session.message` | GateOperator | 无 | 无 | 否 |
| `user.notify` | GateOperator | 无 | 无 | 否 |
| `forward.open` | GateReview | KindForwardOpen | 无 | 否 |
| `forward.close` | GateReview | KindForwardClose | 无 | 否 |
| `forward.list` | GateOperator | 无 | 无 | 否 |
| `file.read` | GateOperator | 无 | remote_path, local_path | 否 |
| `file.write` | GateReview | KindFileWrite | remote_path, local_path | 否 |
| `file.stat` | GateOperator | 无 | remote_path | 否 |
| `file.delete` | GateReview | KindFileDelete | remote_path | 否 |
| `file.rename` | GateReview | KindFileRename | from_path, to_path | 否 |
| `file.mkdir` | GateReview | KindFileMkdir | remote_path | 否 |
| `file.urls` | GateOperator | 无 | remote_path | 否 |
| `file.perm` | GateReview | KindFilePerm | remote_path | 否 |
| `file.link` | GateReview | KindFileLink | remote_path, target, link_path, existing_path, new_path | 否 |
| `file.fs` | GateReview | KindFileTruncate | remote_path | 否 |
| `file.getwd` | GateOperator | 无 | 无 | 否 |
| `profile.list` | GateOperator | 无 | 无 | 否 |
| `profile.create` | GateReview | KindProfileCreate | 无 | 否 |
| `profile.edit` | GateReview | KindProfileEdit | 无 | 否 |
| `profile.copy` | GateReview | KindProfileCreate | 无 | 否 |
| `profile.delete` | GateDevice | 无 | 无 | 否 |
| `ext.list` | GateOperator | 无 | 无 | 否 |
| `ext.get` | GateOperator | 无 | 无 | 否 |
| `ext.create` | GateReview | KindExtensionCreate | 无 | 否 |
| `ext.update` | GateReview | KindExtensionUpdate | 无 | 否 |
| `ext.delete` | GateDevice | 无 | 无 | 否 |
| `ext.retry` | GateDevice | 无 | 无 | 否 |
| `jrpc.open` | GateReview | KindExtensionCreate | 无 | 否 |
| `mcp.open` | GateReview | KindExtensionCreate | 无 | 否 |
| `cdp.open` | GateReview | KindExtensionCreate | 无 | 否 |

`session.message` 当前用于 REST marks 和 DSL span；`shell.detect` 仍在表中但不能据此认定有独立公开工具。当前唯一公开 MCP 工具为 `term`，内部 handler 名不是另一个外部工具入口。相反，当前实例管理、审批决定、任务、alias、peer 调用与 target 附着等对外操作没有各自完整的 capability ID。

## 10. 凭据与权限划分的重点边界

以下只是待细化维度，不是本次已实施的权限名称或默认策略。

| 资源 | 至少要分开的动作 | 当前需要记录的原因 |
| --- | --- | --- |
| 主机配置 | 摘要、使用凭据连接、原文读取、导出、导入、创建、覆盖、改名、删除、连接测试 | REST 单条读取是原文；DSL get host 仍直接返回 private_key/proxy；不能把两者都当安全摘要 |
| 凭据导出 | 获准导出、明文格式、用户自选密码加密格式 | 默认明文导出可以保留；应先授权“能否导出”，再选择格式；加密文件密码不是访问权限 |
| 扩展 | 摘要、模板读取、秘密字段读取、安装、编辑、启停、重启、删除、导入导出 | headers、URL、command/args 和 alias 都可能承载认证或扩大能力 |
| 会话与通道 | 可见范围、连接、新通道、输入、按键、输出、关闭、删除记录 | 当前资源白名单与调用者权限不是同一层；任意 shell 执行也可访问该 OS 身份可读文件 |
| 文件 | 内容读取、下载、上传、覆盖、替换、删除、改名、目录、权限、所有者、时间、链接、截断、统计 | 需要路径和读写方向；local-path 涉及 Termcp 服务端本地文件 |
| 转发 | local、remote、dynamic、列表、关闭；监听地址与可达目标 | 一个 forward.open 无法表达三种网络权限边界 |
| 协议 peer | 连接、目标附着、工具发现、具体远端方法/工具调用、发送、读取、订阅、关闭 | 远端工具与 CDP 方法不是固定集合；仅允许 open 会授予一个可继续调用的入口 |
| 审批 | 查看待审项、批准、拒绝、启用审阅、关闭或放宽审阅规则 | 申请操作权限不应自动带上自我批准或放宽审阅的权限 |
| 管理面 | 版本、运行状态、连接者、跨会话活动、停止实例、未来的凭据与 token 管理 | 全局管理权限不能因 loopback 或使用同一 token 自动继承 |
| 推送与长连接 | 初始快照、输出订阅、通知、审批事件、每次写入与吊销 | 不能只在建立 WS/SSE 时认证一次就把所有后续资源全部推送 |

当前 dev 的 `/api/connections/{name}`、连接导出、扩展原文与导出是显式的秘密读取路径。除此之外，DSL `get host` 的密码/口令是 has_* 标记，但 `private_key` 和 `proxy` 未统一隐藏，不能把“未返回 password 字段”当作没有凭据暴露。文件读取、服务端本地文件上传、终端执行、远端 peer 方法也可能间接获得秘密；后续权限边界须与它们能到达的 OS/网络资源一致。

`secrets` 的日志控制与凭据加密、接口授权各自独立。本轮不加入会话加密；也不把接口梳理扩大为命令内容过滤策略实现。

## 11. 核对方式与局限

1. 读取当前 HEAD 并确认与 `origin/dev` 一致，固定提交记录在文件首行。
2. 扫描非测试源码中的路由注册，逐项覆盖 `main.go` 与 `Handler.Register` 的 69 个注册模式，包括 9 个兼容路径及 2 个互斥静态注册。
3. 从该提交构建临时二进制，在独立临时数据目录和临时 loopback 端口启动，不加载真实 profile、不连接真实 SSH、不传入真实凭据；读取 guide、help 及 19 个动词的完整帮助后关闭实例。
4. 将运行时动词集合与源码、文档的 19 项对照，将 13 个参数与声明表对照，将 45 个 capability ID 与源码对照；再实际调用 MCP `initialize`、`tools/list`、`prompts/list`、`resources/list`，确认只发布 `term` 和 `learn-api`、工具 schema 的 13 个参数与源码一致、未启用的资源接口返回协议错误。
5. 追踪 REST 的直接和公共 helper 检查（输出、文件、终端 I/O、转发关闭、批量停止/删除），区分“有 capability 名称”和“入口实际调用了检查”。
6. CSV 与 Markdown 对照全部 HTTP 注册、DSL 子操作及 WS/MCP 应用入口；`git diff --check` 检查文档格式。

本轮仅新增现状文档与 CSV，不改变运行时鉴权、存储格式、导出行为或审批逻辑。没有运行完整业务回归测试；动态核对只覆盖自描述接口，不是对所有有副作用操作的执行测试。

配套 [CSV 清单](interface-inventory.csv) 用于按入口、操作、资源、当前检查、关键参数与敏感内容筛选。后续可以在此底表上补充拟定权限 ID、调用者类型、默认策略和资源范围。
