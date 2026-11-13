# Termcp 权限配置清单

> 2026-11-05：权限管理的唯一需求来源是 [#252](https://github.com/open-mcp-ai/Termcp/issues/252)。当前正文的 45 项是代码快照，不是最终目录；目标为修正后的 46 项能力。在 #252 对齐前，不按本表数量扩展实现。范围决定见 [认证与权限范围收敛](design/auth-scope-2026-11-05.md)。

> 本表对应当前 `authorization.Catalog()`（catalog version 4，共 45 项），并同步列出现有五个预置权限组。权限界面中未选择动作按禁止处理；本文不替代服务端校验。

## 权限效果

| 界面名称 | 存储值 | 含义 |
| --- | --- | --- |
| 禁止 | `deny`，或没有该 action | 请求直接拒绝。当前 UI 删除 action 条目来表示禁止。 |
| 允许 | `allow` | 满足主机范围和参数条件后直接执行。 |
| 需审批 | `ask` | 完整请求先入审批队列；批准时按当前凭据、主体、权限组、主机和参数重新校验。 |

## 主机范围

| 模式 | 存储值 | 含义 |
| --- | --- | --- |
| 全部主机 | `all` | 当前和以后新增的已保存主机；共享配置修改也要求此模式。 |
| 指定主机 | `selected` | 只允许 `hosts` 中列出的稳定主机身份。空列表不授予主机。 |
| 无主机 | `none` | 不授予任何主机资源。 |

风险级别只用于治理提示，不替代 allow/deny/ask 和参数约束。

## 总览

- 动作总数：45
- 风险分布：标准 27，敏感 2，高风险 16
- 权限族：会话 6，终端与通道 10，文件 11，端口转发 3，外部协议 3，主机配置 5，扩展 6，通知 1
- 全局动作：7；修改共享主机配置或扩展定义必须选择“全部主机”。

### 会话（6）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| List sessions | `session.list` | 标准 | 允许的主机范围 | DSL: `ls` / `ps`<br>工具: `session_list`<br>WebUI: `/api/sessions GET` | — | 未提供说明 |
| Inspect sessions | `session.info` | 标准 | 允许的主机范围 | DSL: `info` / `get`<br>工具: `session_info`<br>WebUI: `/api/sessions/{id} GET` | — | 未提供说明 |
| Start sessions | `session.start` | 标准 | 允许的主机范围 | DSL: `connect`<br>工具: `session_start`<br>WebUI: `/api/sessions POST` | — | 未提供说明 |
| Stop sessions | `session.terminate` | 标准 | 允许的主机范围 | DSL: `rm session`<br>工具: `session_terminate`<br>WebUI: `/api/sessions/{id} DELETE` | — | 未提供说明 |
| Delete sessions | `session.delete` | 敏感 | 允许的主机范围 | DSL: `rm session --force`<br>工具: `session_delete`<br>WebUI: `/api/sessions/{id}/purge DELETE` | — | Delete a Termcp session record after it has ended. |
| Read session messages | `session.message` | 标准 | 允许的主机范围 | DSL: `span`<br>工具: `message` | — | 未提供说明 |

### 终端与通道（10）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| List channels | `shell.list` | 标准 | 允许的主机范围 | DSL: `ls`<br>工具: `shell_list` | — | 未提供说明 |
| Open channels | `shell.open` | 标准 | 允许的主机范围 | DSL: `open`<br>工具: `shell_open`<br>WebUI: `/api/sessions/{id}/shells POST` | — | 未提供说明 |
| Close channels | `shell.close` | 标准 | 允许的主机范围 | DSL: `close`<br>工具: `shell_close` / `shell_archive`<br>WebUI: `/api/shells/{id} DELETE` | — | 未提供说明 |
| Resize channels | `shell.resize` | 标准 | 允许的主机范围 | DSL: `resize`<br>工具: `shell_resize`<br>WebUI: `/api/shells/{id}/resize POST` / `/api/ui/ws resize` | — | 未提供说明 |
| Read output | `shell.output` | 标准 | 允许的主机范围 | DSL: `read` / `run`<br>工具: `shell_output`<br>WebUI: `/api/shells/{id}/lines GET` | — | 未提供说明 |
| Type input | `shell.input` | 标准 | 允许的主机范围 | DSL: `input` / `run`<br>工具: `shell_input`<br>WebUI: `/api/shells/{id}/input POST` / `/api/ui/ws input` | — | Type text into an existing terminal. The target OS account still decides what that text can do. |
| Press keys | `shell.key` | 标准 | 允许的主机范围 | DSL: `key` / `run`<br>工具: `shell_key`<br>WebUI: `/api/shells/{id}/key POST` | keys：Effective key names after aliases; values are open-ended.<br>repeat_max：Maximum requested repeat count. | 未提供说明 |
| Detect environment | `shell.detect` | 标准 | 允许的主机范围 | 工具: `shell_detect` | — | Detect the local shell environment used by this Termcp process; it does not inspect the remote target. |
| Register readers | `shell.reader.register` | 标准 | 允许的主机范围 | DSL: `reader`<br>工具: `shell_reader_register` | — | 未提供说明 |
| Unregister readers | `shell.reader.unregister` | 标准 | 允许的主机范围 | DSL: `reader --drop`<br>工具: `shell_reader_unregister` | — | 未提供说明 |

### 通知（1）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| Notify the operator | `user.notify` | 标准 | 允许的主机范围 | DSL: `notify`<br>工具: `notify_user`<br>WebUI: `/api/notify POST` | — | Send an operator-facing UI notification and optional session reference. |

### 端口转发（3）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| List forwards | `forward.list` | 标准 | 允许的主机范围 | DSL: `ls forward`<br>工具: `forward`<br>WebUI: `/api/forwards GET` | — | 未提供说明 |
| Open forwards | `forward.open` | 高风险 | 允许的主机范围 | DSL: `fwd` / `open forward`<br>工具: `forward`<br>WebUI: `/api/forwards POST` | — | 未提供说明 |
| Close forwards | `forward.close` | 标准 | 允许的主机范围 | DSL: `close forward` / `fwd --action close`<br>工具: `forward`<br>WebUI: `/api/forwards/{id} DELETE` | — | 未提供说明 |

### 文件（11）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| Read files | `file.read` | 标准 | 允许的主机范围 | DSL: `read file` / `get file --op readlink` / `copy source`<br>工具: `file_read`<br>WebUI: `/api/files GET` | forms：local requires a positive length; inline is capped at 8 MiB.<br>max_bytes：Requires a proven positive transfer length or the existing inline cap. | 未提供说明 |
| Stat files | `file.stat` | 标准 | 允许的主机范围 | DSL: `ls file` / `get file --op stat`<br>工具: `file_stat`<br>WebUI: `/api/files/stat GET` | — | 未提供说明 |
| Read working directory | `file.getwd` | 标准 | 允许的主机范围 | DSL: `get file --op getwd`<br>工具: `file_getwd` | — | 未提供说明 |
| Get transfer URLs | `file.urls` | 标准 | 允许的主机范围 | DSL: `get file --op urls`<br>工具: `file_urls`<br>WebUI: `/api/files/urls GET` | — | 未提供说明 |
| Write files | `file.write` | 敏感 | 允许的主机范围 | DSL: `write file` / `copy destination`<br>工具: `file_write`<br>WebUI: `/api/files PUT` / `/api/files POST` | forms：Inline counts decoded bytes; local requires a positive length.<br>max_bytes：Requires decoded inline size or a proven transfer length. | Write inline data or transfer from a local path with an explicit byte bound. |
| Delete files | `file.delete` | 高风险 | 允许的主机范围 | DSL: `rm file`<br>工具: `file_delete`<br>WebUI: `/api/files DELETE` | — | Delete a file or directory in the target account's namespace. |
| Create directories | `file.mkdir` | 标准 | 允许的主机范围 | DSL: `mkdir`<br>工具: `file_mkdir`<br>WebUI: `/api/files/mkdir POST` | — | 未提供说明 |
| Rename files | `file.rename` | 高风险 | 允许的主机范围 | 工具: `file_rename`<br>WebUI: `/api/files/patch` | — | 未提供说明 |
| Change file permissions | `file.perm` | 高风险 | 允许的主机范围 | DSL: `write file --mode` / `write file --owner` / `write file --time`<br>工具: `file_perm`<br>WebUI: `/api/files/patch` | — | 未提供说明 |
| Manage links | `file.link` | 高风险 | 允许的主机范围 | DSL: `get file --op readlink` / `write file --op symlink` / `write file --op link`<br>工具: `file_link`<br>WebUI: `/api/files/links` | — | Create a hard link on the target. |
| Manage filesystem objects | `file.fs` | 高风险 | 允许的主机范围 | DSL: `get file --op realpath` / `get file --op statvfs` / `write file --op truncate`<br>工具: `file_fs`<br>WebUI: `/api/files/fs` | — | 未提供说明 |

### 主机配置（5）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| List profiles | `profile.list` | 标准 | 允许的主机范围 | DSL: `ls host` / `get host`<br>工具: `ssh_config`<br>WebUI: `/api/connections GET` / `/api/connections/{name} GET` | — | 未提供说明 |
| Create profiles | `profile.create` | 高风险 | 全部主机（全局） | DSL: `create host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/{name} PUT` | — | Create a saved host profile and its connection material. |
| Edit profiles | `profile.edit` | 高风险 | 全部主机（全局） | DSL: `edit host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/{name} PUT` | — | Change a saved profile's destination or credential material. |
| Copy profiles | `profile.copy` | 高风险 | 全部主机（全局） | DSL: `copy host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/batch GET` | — | 未提供说明 |
| Delete profiles | `profile.delete` | 高风险 | 全部主机（全局） | DSL: `rm host`<br>工具: `ssh_config`<br>WebUI: `/api/connections/{name} DELETE` | — | Delete a saved host profile. |

### 扩展（6）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| List extensions | `ext.list` | 标准 | 允许的主机范围 | DSL: `ls ext`<br>工具: `extension`<br>WebUI: `/api/extensions GET` | — | 未提供说明 |
| View extensions | `ext.get` | 标准 | 允许的主机范围 | DSL: `get ext` / `open ext` / `close ext`<br>工具: `extension`<br>WebUI: `/api/extensions/{name} GET` | — | 未提供说明 |
| Retry extensions | `ext.retry` | 标准 | 允许的主机范围 | DSL: `edit ext --restart`<br>工具: `extension`<br>WebUI: `/api/extensions/{name}/retry POST` | — | 未提供说明 |
| Create extensions | `ext.create` | 高风险 | 全部主机（全局） | DSL: `create ext`<br>工具: `extension`<br>WebUI: `/api/extensions POST` | — | Create an executable extension definition. |
| Update extensions | `ext.update` | 高风险 | 全部主机（全局） | DSL: `edit ext`<br>工具: `extension`<br>WebUI: `/api/extensions/{name} PUT` | — | Change an extension executable command, endpoint, allowlist or target. |
| Delete extensions | `ext.delete` | 高风险 | 全部主机（全局） | DSL: `rm ext`<br>工具: `extension`<br>WebUI: `/api/extensions/{name} DELETE` | — | Delete an extension definition and stop its managed instance. |

### 外部协议（3）

| 权限项 | Operation ID | 风险 | 主机范围 | 可达入口与实际接口 | 参数条件 / 依赖 | 说明 |
| --- | --- | --- | --- | --- | --- | --- |
| Open JSON-RPC peers | `jrpc.open` | 高风险 | 允许的主机范围 | DSL: `open jrpc` / `ls jrpc` / `get jrpc` / `close jrpc` / `read jrpc` / `call jrpc`<br>工具: `term` | methods：Normalized JSON-RPC method name. | Open or control a JSON-RPC peer; method constraints identify the operations that may run. |
| Open MCP peers | `mcp.open` | 高风险 | 允许的主机范围 | DSL: `open mcp` / `open session mcp` / `ls mcp` / `get mcp` / `close mcp` / `read mcp` / `call mcp`<br>工具: `term` | tool_names：Normalized remote MCP tool name. | Open or control an MCP peer; tool constraints identify the tools that may run. |
| Open CDP peers | `cdp.open` | 高风险 | 允许的主机范围 | DSL: `open cdp` / `ls cdp` / `get cdp` / `close cdp` / `read cdp` / `call cdp`<br>工具: `term` | methods：Normalized CDP method name. | Open or control a CDP peer; method constraints identify the operations that may run. |

## 组合依赖

一次组合请求必须同时获得下列全部动作权限。

| Recipe | 名称 | 必需动作 | 可选动作 | 说明 |
| --- | --- | --- | --- | --- |
| `dsl.run` | Run a command line | `shell.input` / `shell.key` / `shell.output` | — | Types a line, presses Enter, and reads the requested output window. |
| `dsl.connect` | Connect to a host | `session.start` / `shell.open` | — | Starts a session and opens its first channel. |
| `dsl.file-copy` | Copy file data | `file.read` / `file.write` | — | Reads one endpoint and writes the other. |

## 预置权限组

| 模板 | 名称 | 版本 | 允许动作数 | 主机范围 | 说明 |
| --- | --- | ---: | ---: | --- | --- |
| `no-access` | 未授权 / No access | 5 | 0 | `all` | 业务动作默认禁止；主机范围本身不授予操作权限。 |
| `viewer` | 资源观察 / Viewer | 5 | 5 | `all` | 查看安全摘要和状态，不读取终端输出或文件正文。 |
| `diagnostics` | 只读诊断 / Diagnostics reader | 5 | 11 | `all` | 读取已分配资源的输出和文件内容，需要已有的可达会话。 |
| `file-maintainer` | 文件维护 / File maintainer | 5 | 13 | `all` | 允许文件读写和目录维护；不授予终端执行、权限/链接/文件系统变更、网络转发或全局配置管理。 |
| `host-operator` | 主机运维 / Host operator | 5 | 33 | `all` | 允许连接主机、操作终端、常用文件、显式属性修改、转发和已配置扩展；不授予凭据配置。 |

### 未授权 / No access (`no-access`)

- 不授予任何业务动作；`host_mode=all` 只表示没有额外主机范围限制。

### 资源观察 / Viewer (`viewer`)

- `session.list`（allow）
- `session.info`（allow）
- `shell.list`（allow）
- `profile.list`（allow）
- `ext.list`（allow）

### 只读诊断 / Diagnostics reader (`diagnostics`)

- `session.list`（allow）
- `session.info`（allow）
- `shell.list`（allow）
- `shell.output`（allow）
- `session.message`（allow）
- `file.read`（allow；forms=inline）
- `file.stat`（allow）
- `file.getwd`（allow）
- `file.urls`（allow）
- `profile.list`（allow）
- `ext.list`（allow）

### 文件维护 / File maintainer (`file-maintainer`)

- `session.list`（allow）
- `session.info`（allow）
- `shell.list`（allow）
- `shell.output`（allow）
- `session.message`（allow）
- `file.read`（allow；forms=inline）
- `file.stat`（allow）
- `file.getwd`（allow）
- `file.urls`（allow）
- `file.write`（allow；forms=inline）
- `file.mkdir`（allow）
- `profile.list`（allow）
- `ext.list`（allow）

### 主机运维 / Host operator (`host-operator`)

- `session.list`（allow）
- `session.info`（allow）
- `session.start`（allow）
- `session.terminate`（allow）
- `session.delete`（allow）
- `shell.list`（allow）
- `shell.open`（allow）
- `shell.close`（allow）
- `shell.input`（allow）
- `shell.key`（allow）
- `shell.resize`（allow）
- `shell.detect`（allow）
- `shell.reader.register`（allow）
- `shell.reader.unregister`（allow）
- `shell.output`（allow）
- `session.message`（allow）
- `file.read`（allow；forms=inline,local,pipe）
- `file.stat`（allow）
- `file.getwd`（allow）
- `file.urls`（allow）
- `file.write`（allow；forms=inline,local,pipe）
- `file.delete`（allow）
- `file.mkdir`（allow）
- `file.rename`（allow）
- `file.perm`（allow）
- `forward.list`（allow）
- `forward.open`（allow）
- `forward.close`（allow）
- `profile.list`（allow）
- `ext.list`（allow）
- `ext.get`（allow）
- `ext.retry`（allow）
- `user.notify`（allow）

## 常用权限预览

权限页面提供以下只读预览入口；预览不会执行命令、创建会话或启动传输。

| 名称 | DSL 模板 |
| --- | --- |
| 列出会话 | `ls` |
| 查看主机 | `get host <已保存主机名>` |
| 建立连接 | `connect <已保存主机名>` |
| 读取终端 | `read <session>:<channel>` |
| 运行命令 | `run <session>:<channel> $1` |
| 读取文件 | `read <session>:<channel> file <remote-path>` |
| 写入文件 | `write <session> file <remote-path> --data <text>` |
| 列出转发 | `ls forward` |

## 说明

- `shell.detect` 检测 Termcp 本机使用的 shell，不探测远端主机。
- `shell.input`、`shell.key`、`session.start`、`shell.open` 等终端基础动作仍需显式授权；预设“主机运维”会授予它们。
- 终端动作本身不是操作系统沙箱。获得 shell 权限后，远端命令仍可能绕开单独关闭的文件操作。
- `mcp.open`、`cdp.open`、`jrpc.open` 可通过工具名或方法 allowlist 缩小到指定调用；这些约束由服务端核验。
- `POST /ext/{name}/stream` 对 Agent 在任何字节写入前核验 `ext.get`、`shell.input`、`shell.key(enter)`、需要回复时的 `shell.output` 和扩展目标主机，并使用授权时冻结的模板/主机绑定启动。首次响应返回 `Mcp-Session-Id`；后续帧必须由同一 verified principal + credential 使用该 session。DELETE 只释放该 session 的独立 channel。
- 文件 `forms`、`max_bytes`，按键 `keys`、`repeat_max`，以及工具/方法 allowlist 都是服务端可验证条件；无法证明的约束应保守拒绝。
- 预设组只提供初始策略快照。管理员可以在组级策略中修改。

## 权限变化与 MCP 会话

权限组、动作、约束和主机范围属于业务授权，不是登录身份。普通修改完成后，现有静态
Token、OAuth grant、Agent owner 和 MCP session 保持不变；下一次工具调用、组合动作、
审批重放或后台任务后续受控步骤重新读取当前策略。组停用、删除或把 Agent 移入
“未授权”同样允许 MCP 握手和协议帧，但业务动作返回 403。已经失去所需动作或主机范围的
后台传输按它登记的 operation/host authority 被取消；与此次变化无关的传输继续运行。

只有 Agent 停用、登录用户停用、OAuth grant/client/credential 撤销、静态 Token 撤销或
替换、配置保险库锁定这类身份或凭据失效事件才注销 transport session 并取消该凭据的请求
和后台工作。OAuth refresh 轮换只改变 opaque access token，不改变 Agent、owner、group、
grant、client 或 resource，因此可以继续已有 Streamable HTTP session。

会话丢失不是撤权：有效 Bearer 携带未知 session 时返回 404，客户端可重新
`initialize`；真实归属不匹配返回 403；无效 Bearer 返回 401。自管 stdio bridge 对
Streamable HTTP 与 SSE 都做有界、可取消的重握手，但不会重放结果不明的命令、按键或文件
操作；这类请求以 `termcp_session_replaced` / `result=unknown` / `request_was_replayed=false`
返回。
