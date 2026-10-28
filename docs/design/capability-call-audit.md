# WebUI / MCP / DSL 能力调用连接点审计

基线提交：`76b320976ae671721f3370983f5b0d6b412685b1`。核对日期：2026-10-27。当前工作区业务源码与该提交一致；本报告新增文档，不改变任何能力或权限行为。

## 1. 汇合结论

**两条入口没有全部落到同一个强制能力点。当前是共享部分业务函数、共享部分底层实现，同时存在独立实现、直接管理器/协议调用和审批回放路径。只 Hook callTool 或 checkAdmission 不足以覆盖 WebUI 与所有 DSL 操作。**

这里分开回答两个问题：

1. 执行点是否相同：用具体接收者类型、函数定义和两边逐跳调用位置验证，不能把“同一个 Store/Manager”或“同一份文件”当作相同能力。
2. 授权点是否相同：同一个业务函数之前可能经过不同检查；共享业务函数不代表共享 MCP 的 checkAdmission。

共核对 45 个现有 capability ID，并纳入目录之外的管理、任务和协议操作，合计 98 项语义/分支对照。统计如下：

| 逐项验证结果 | 项数 |
| --- | --- |
| 同一项目业务执行点 | 36 |
| 执行路径不同 | 1 |
| 更低层项目执行点汇合 | 3 |
| 仅 WebUI/REST | 21 |
| 当前两入口均无公开映射 | 1 |
| 仅 DSL/MCP | 29 |
| 仅第三方 SFTP 执行点汇合 | 1 |
| 项目内执行路径不同 | 5 |
| 不同业务读取点与结果 | 1 |

同一业务函数的结论只针对表中匹配的分支，不保证参数、来源标记、列表过滤和返回投影相同。对于 internal 文件分支、归档/删除和原文/字段投影等差异，表中分别列出，不用相似名称填“已汇合”。

## 2. 完整产物与覆盖范围

| 产物 | 内容 |
| --- | --- |
| [完整连接点 Markdown](capability-connection-points.md) | 714 个连接点，逐行列出文件、函数、行号、方向、完整源码实参、旁路类别和局部 guard 证据 |
| [连接点 CSV](capability-connections.csv) | 同一完整清单，增加列号、接收者静态类型、被调函数定义与三平台位置 |
| [逐能力汇合 CSV](capability-convergence.csv) | 98 项，保存双方完整逐跳路径、实参、DSL 分发绑定、浏览器引用和限定条件 |
| [全部 Go 调用表达式](capability-call-edges.csv) | 11247 个三平台去重调用位置，包含调用面内部连接及业务层下游调用；不省略普通辅助调用 |
| [函数/闭包/初始化节点定义](capability-function-definitions.csv) | 2236 个定义位置，区分构建平台 |
| [函数值与回调绑定引用](capability-function-references.csv) | 320 个函数值引用；不把引用本身伪装成已执行调用 |
| [实参构造与赋值](capability-argument-bindings.csv) | 3887 个调用面 Go 赋值与浏览器请求构造相关绑定，保留源码表达式，不伪造运行值 |
| [浏览器网络/API 连接点](capability-webui-client-calls.csv) | 112 处网络调用、地址引用和请求 wrapper，含文件、函数、行号、实参和同函数前置绑定 |
| [入口注册覆盖](capability-entrypoints.csv) | 69 个 HTTP 注册模式、4 个 WS 入站消息和 19 个 DSL 动词，合计 92 项 |
| [源文件哈希](capability-source-hashes.csv) | 固定所有直接参与提取的源码文件内容，供复核报告是否仍匹配代码 |

“能力层”在当前代码中不是单一 package：本报告同时盘点 capability/ns/approval 等检查层、session/sshconfig/extensions/forward/sftp/transcript/jrpc/task 等业务层，以及适配器直接访问的 OS/IO。Go 全量清单覆盖项目的 34 个生产 package 的函数体及顶层初始化表达式；初始化节点明确以 $init 标记。普通错误格式化、HTTP 响应写入等不属于能力执行连接点，不列入连接点表，但不从全量调用 CSV 删除。不包含测试文件、第三方库内部或 OS 内核实现。浏览器侧覆盖应用 static/js 和 HTML 内联脚本，不解析第三方 vendor/xterm 库作为 Termcp 自身代码。

源码实参表示调用位置实际写入的表达式，例如 req.Text、callerInputSource(ctx) 或完整 session.Config；它不是运行时值。对于 interface 调用、函数变量和回调，清单保留真实静态类型或绑定来源，不根据名字猜接收者。分支列记录 AST 所处 if/case 容器，不等于已计算的路径条件，尤其不能把处于 if 的 else 内误写成条件为真。

macOS、Linux、Windows 构建配置均先 go list -deps -export，再用 Go parser/go types 解析调用与接收者；三套类型检查错误数均为 0。JavaScript 使用 Acorn 解析，语法错误数为 0。类型解析不等于三平台运行测试；本报告没有执行生产连接、删除或其他有副作用能力。调用清单是源码静态连接，不把存在一条分支当作所有输入必经，也不模拟 defer/异步调度的发生时间。

## 3. 现有入口与准入连接

### 3.1 MCP/DSL 常规路径

外部 MCP 只发布 term。handleTerm 进入 DSL，runDirective 再进入各 directive；需要旧 handler 的路径经 callTool → withLogging → checkAdmission → handlerFor 选出的 handler。文件/profile/ext 等复用 group handler 的 action 分派。完整的 34 个 handlerFor 函数值绑定如下：

| 内部 operation 名 | 返回 handler | 绑定位置 |
| --- | --- | --- |
| term | internal/mcp.(*Server).handleTerm | internal/mcp/call.go:40 |
| session_start | internal/mcp.(*Server).handleStartSession | internal/mcp/call.go:42 |
| session_list | internal/mcp.(*Server).handleListSessions | internal/mcp/call.go:44 |
| session_info | internal/mcp.(*Server).handleGetSessionInfo | internal/mcp/call.go:46 |
| session_terminate | internal/mcp.(*Server).handleTerminateSession | internal/mcp/call.go:48 |
| session_delete | internal/mcp.(*Server).handleDeleteSession | internal/mcp/call.go:50 |
| shell_open | internal/mcp.(*Server).handleStartSubShell | internal/mcp/call.go:52 |
| shell_list | internal/mcp.(*Server).handleListSubshells | internal/mcp/call.go:54 |
| shell_close | internal/mcp.(*Server).handleCloseShell | internal/mcp/call.go:56 |
| shell_archive | internal/mcp.(*Server).handleArchiveShell | internal/mcp/call.go:58 |
| shell_input | internal/mcp.(*Server).handleSendInput | internal/mcp/call.go:60 |
| shell_key | internal/mcp.(*Server).handlePressKey | internal/mcp/call.go:62 |
| shell_output | internal/mcp.(*Server).handleReadOutput | internal/mcp/call.go:64 |
| shell_resize | internal/mcp.(*Server).handleResizePty | internal/mcp/call.go:66 |
| shell_detect | internal/mcp.(*Server).handleDetectShell | internal/mcp/call.go:68 |
| shell_reader_register | internal/mcp.(*Server).handleRegisterReader | internal/mcp/call.go:70 |
| shell_reader_unregister | internal/mcp.(*Server).handleUnregisterReader | internal/mcp/call.go:72 |
| message | internal/mcp.(*Server).handleMessageOps | internal/mcp/call.go:74 |
| notify_user | internal/mcp.(*Server).handleNotifyUser | internal/mcp/call.go:76 |
| forward | internal/mcp.(*Server).handleForwardOps | internal/mcp/call.go:78 |
| file_read | internal/mcp.(*Server).handleFileRead | internal/mcp/call.go:80 |
| file_write | internal/mcp.(*Server).handleFileWrite | internal/mcp/call.go:82 |
| file_replace | internal/mcp.(*Server).handleFileReplace | internal/mcp/call.go:84 |
| file_stat | internal/mcp.(*Server).handleFileStat | internal/mcp/call.go:86 |
| file_delete | internal/mcp.(*Server).handleFileDelete | internal/mcp/call.go:88 |
| file_rename | internal/mcp.(*Server).handleFileRename | internal/mcp/call.go:90 |
| file_mkdir | internal/mcp.(*Server).handleFileMakeDir | internal/mcp/call.go:92 |
| file_urls | internal/mcp.(*Server).handleGetFileURLs | internal/mcp/call.go:94 |
| file_perm | internal/mcp.(*Server).handleFilePerm | internal/mcp/call.go:96 |
| file_link | internal/mcp.(*Server).handleFileLinkOp | internal/mcp/call.go:98 |
| file_fs | internal/mcp.(*Server).handleFileFsOp | internal/mcp/call.go:100 |
| file_getwd | internal/mcp.(*Server).handleFileGetwd | internal/mcp/call.go:102 |
| ssh_config | internal/mcp.(*Server).handleSSHConfigOps | internal/mcp/call.go:104 |
| extension | internal/mcp.(*Server).handleExtensionOps | internal/mcp/call.go:106 |

这张映射不是公开工具列表，也不是每个 handler 都能从当前 DSL 触达。shell_detect 没有当前公开 DSL 入口；file_rename 虽有内部 handler 和 fileCapability 映射，当前词面分支没有产生 rename action，不能仅凭内部函数存在宣称已被 MCP 使用。

### 3.2 WebUI 路径

浏览器发 REST 或 WS；Handler.Register 和 readPump 分派到 WebUI handler。它们直接调用业务函数，部分先经 restRefuseTool、getShellForIO、resolveFileSession、marksTarget 等独立 helper。WebUI 不经过 MCP 的 callTool/checkAdmission。

例如浏览器 onData 的 WS input 与 MCP 非审阅输入会在 ChildShell.SendTerminalBytesFrom 汇合，但前者是 InputFromAPI，后者来源由 callerInputSource(ctx) 决定；MCP 审阅开启时首先 StageForApproval，不能说它已经即时走到相同写入点。

### 3.3 部分资源守卫的低层汇合

下列共享判断函数确有双方路径，但不是所有入口都调用它们，更不是一个对所有操作强制执行的统一门禁：

| 判断 | WebUI 路径（逐跳调用及实参） | MCP 路径（逐跳调用及实参） |
| --- | --- | --- |
| 能力白名单 | internal/webui.(*Handler).getShellForIO @ internal/webui/shellio.go:130 → internal/webui.restRefuseTool(w, h.Sessions.Get(cs.ParentSessionID()), tool) → internal/webui.restRefuseTool @ internal/webui/restriction.go:44 → internal/session.(Restriction).Allows(capID) | internal/mcp.(*Server).checkAdmission @ internal/mcp/admission.go:44 → internal/mcp.(*Server).admission(ctx, toolName, g, request) → internal/mcp.(*Server).admission @ internal/mcp/admission.go:70 → internal/mcp.(toolGuard).check(ctx, toolName, g, request) → internal/mcp.(toolGuard).check @ internal/mcp/guard.go:95 → internal/mcp.(toolGuard).checkSession(sess, toolName, c, args) → internal/mcp.(toolGuard).checkSession @ internal/mcp/guard.go:224 → internal/mcp.refuseSession(sess, "\x00unknown", toolName) → internal/mcp.refuseSession @ internal/mcp/admission.go:100 → internal/session.(Restriction).Allows(capID) |
| 文件路径白名单 | internal/webui.(*Handler).resolveFileSession @ internal/webui/handler_file.go:41 → internal/webui.restRefusePaths(w, sess, paths) → internal/webui.restRefusePaths @ internal/webui/restriction.go:119 → internal/session.(Restriction).AllowsPath(p[1]) | internal/mcp.(*Server).checkAdmission @ internal/mcp/admission.go:44 → internal/mcp.(*Server).admission(ctx, toolName, g, request) → internal/mcp.(*Server).admission @ internal/mcp/admission.go:70 → internal/mcp.(toolGuard).check(ctx, toolName, g, request) → internal/mcp.(toolGuard).check @ internal/mcp/guard.go:95 → internal/mcp.(toolGuard).checkSession(sess, toolName, c, args) → internal/mcp.(toolGuard).checkSession @ internal/mcp/guard.go:237 → internal/session.(Restriction).AllowsPath(path) |

## 4. 同一实例的构造证据

不是只比较方法名：main.go 把同一组 manager/store 变量交给 MCP Server 和 WebUI Handler。以下为实际构造/赋值表达式；MCP 侧字段接收关系还在 internal/mcp/register.go 的 New 初始化中。

| 位置 | 调用/赋值 | 完整实参/右值 |
| --- | --- | --- |
| main.go:362 | session.NewManager | ["msgMgr", "store", "sshSrv"] |
| main.go:367 | sshconfig.NewStore | ["cfg.DataDir"] |
| main.go:379 | extensions.NewReconciler | ["extensions.NewStore(cfg.DataDir)", "sessMgr", "msgMgr", "sshStore", "forwardMgr"] |
| main.go:389 | mcpmod.New | ["sessMgr", "msgMgr", "sshStore", "forwardMgr", "versionString()", "mcpOpts"] |
| main.go:424 | mcpSrv.SetUINotifier | ["webuiH.BroadcastUINotify"] |
| main.go:406 | webuiH | &webui.Handler{Sessions: sessMgr, MsgMgr: msgMgr, SSH: sshStore, ForwardMgr: forwardMgr, Extensions: extReconciler, NoInternal: cfg.NoInternal, Version: versionString(), Daemon: isDaemonChild, StartedAt: time.Now().UTC().Format(time.RFC3339)} |
| main.go:420 | webuiH.ExecuteOperation | mcpSrv.ExecuteApprovedOperation |
| main.go:428 | webuiH.MCPConnections | func() []any {<br>	conns := mcpSrv.Connections()<br>	out := make([]any, len(conns))<br>	for i, c := range conns {<br>		out[i] = c<br>	}<br>	return out<br>} |

## 5. 跨面回调与旁路

### 5.1 审批回放

main.go:420 明确赋值 webuiH.ExecuteOperation = mcpSrv.ExecuteApprovedOperation。WebUI executeApproved 在 approval.go:263 调用 h.ExecuteOperation(req)。MCP ExecuteApprovedOperation 在 approval_gate.go:204 用 handlerFor(p.Tool) 取 handler，并在 :224 直接调用 handler(withApproved(context.Background()), call)。这条回放不调用 callTool/withLogging/checkAdmission。

这是同一业务 handler 的另一条可证实入口，不是凭空假设的可攻击路径；本报告只记录控制流差异。行级输入另由 WebUI executeApproved 按 Source 写入 ChildShell，亦不通过 MCP checkAdmission。实际 Payload、Source、SessionID、背景 context 和调用实参均见全量边及赋值清单。

### 5.2 通知、连接者与生命周期

main.go:424 通过 SetUINotifier 把 WebUI BroadcastUINotify 交给 MCP；handleNotifyUser 的 uiNotify 函数值回调是 DSL 到 UI 的单向通知，不是 UI 也调用了 notify 能力。main.go:428 的 MCPConnections 闭包向 WebUI/WS 提供 MCP 连接者列表。

internal/mcp/register.go:32/:38 把 ForwardManager.CloseBySession 和 closePeersBySession 挂到 session DEAD 生命周期；Handler.Register 把 SSH/forward/extension 的 onChange 挂到 UI 列表广播。注册引用与实际函数值调用分别保留在 references、bindings 和 call-edges 中，不能把注册时传递函数值写成当时已经执行。

### 5.3 绕过边界的定义

- WebUI 直接到业务/OS 的连接绕过当前 MCP 能力包装；不等于没有 HTTP 认证或没有 REST Restriction。
- DSL 的 ps/info、协议 peer、alias/task 与 copy 等直接路径，不因为位于 term 内就自动获得内层 checkAdmission；独立的 namespace/refuseAction/refuseDoor 检查按真实调用点记录。
- span 是一个反例：当前 directiveSpan 在 term_generated.go:488 调用 callTool("message")，然后进入 handleMessageOps/handleListMessages。runDirective 上将 span 归为“直接读取”的注释不能代替实际代码。
- 调用面中的 exec/IO helper、handler 内的业务调用与审批回放需分开看。连接表对不能仅凭单条边确定上游的项明确注明执行路径依赖，不用“没有旁路”填表。

## 6. 浏览器与路由核对发现

浏览器引用与后台路由分开核对。REST /key、mkdir 等存在不代表浏览器已经调用它们；实际键盘和 resize 使用 WS。主机/扩展导出经 downloadTOML wrapper 取文件；证据不只包含 apiPath 字符串。

存在一条确认的路径不匹配：sessions.js:149 在无 shells 的归档导出分支调用 fetchAllSessionLines，后者 :296 请求 GET /api/sessions/{id}/lines。当前 Handler.Register 没有这个注册模式，仅有 GET /api/shells/{id}/lines（handler.go:210）。因此这条前端连接不能标为落到 handleShellLines；源码确有调用分支，但本报告未在浏览器中触发该状态。此处只报告，不修复。

## 7. 逐能力/语义分支验证结果

每行的具体完整调用路径和实参在 convergence CSV 中；这里列出结论、实际执行点、来源及限定条件，不将相似能力合并填“已汇合”。

| 编号 / 能力 | 语义分支 | WebUI backend 函数 | DSL/MCP 函数 | 汇合结论 / 验证点 | 限定条件 |
| --- | --- | --- | --- | --- | --- |
| C001 session.start | 建会话 | internal/webui.(*Handler).handleCreateSession | internal/mcp.(*Server).handleStartSession | 同一项目业务执行点：internal/session.(*Manager).Create | 相同 Create，UI Config 无 caller namespace；MCP Config 设置 Namespace: callerNamespace(ctx)；参数并非完全相同。 |
| C002 session.list | 会话列表（REST） | internal/webui.(*Handler).handleListSessions | internal/mcp.(*Server).directivePS | 同一项目业务执行点：internal/session.(*Manager).ListAll | DSL ls 在 directivePS 直接 ListAll，不走旧 session_list handler；后续过滤只看源码，不能据此声称列表完全相同。 |
| C003 session.list | 会话列表（浏览器 WS） | internal/webui.(*uiWS).enqueueSessions | internal/mcp.(*Server).directivePS | 同一项目业务执行点：internal/session.(*Manager).ListAll | WS sessions 推送与 DSL ls 共用存储读取，但 WS 列表未经过 MCP 命名空间过滤。 |
| C004 session.info | 会话详情 | internal/webui.(*Handler).handleGetSession | internal/mcp.(*Server).directiveInfo | 同一项目业务执行点：internal/session.(*Session).Info | UI 后台 GET 详情和 DSL get；浏览器主要经 sessions WS 获取状态，不把 REST 存在等同于独立浏览器请求。 |
| C005 session.terminate | 断开并保留会话记录 | internal/webui.(*Handler).handleDeleteSession | internal/mcp.(*Server).handleTerminateSession | 同一项目业务执行点：internal/session.(*Manager).Terminate | UI force=true, grace=0；MCP 可按参数传入 force/grace。 |
| C006 session.delete | 删除会话和记录 | internal/webui.(*Handler).handlePurgeSession | internal/mcp.(*Server).handleDeleteSession | 同一项目业务执行点：internal/session.(*Manager).Delete |  |
| C007 shell.open | 已有连接中新建通道 | internal/webui.(*Handler).handleCreateShell | internal/mcp.(*Server).handleStartSubShell | 同一项目业务执行点：internal/session.(*Session).CreateChildShell |  |
| C008 shell.list | 列通道 | internal/webui.(*Handler).handleListShells | internal/mcp.(*Server).directivePS | 同一项目业务执行点：internal/session.(*Session).ListChildShells | DSL 默认折叠已结束通道；--all 与 UI 返回集合需区分，尽管读取函数相同。 |
| C009 shell.close | 浏览器关闭通道保留记录 | internal/webui.(*Handler).handleArchiveShell | internal/mcp.(*Server).handleArchiveShell | 同一项目业务执行点：internal/session.(*Manager).ArchiveChildShell |  |
| C010 shell.close | REST DELETE 与 DSL close 不能当作相同语义 | internal/webui.(*Handler).handleCloseShell | internal/mcp.(*Server).handleArchiveShell | 执行路径不同 | 当前 capability ID 相同，但一个删除通道元数据，一个归档保留记录；未在同一公开业务方法汇合。 |
| C011 shell.input | REST 输入与 MCP 非审阅写入 | internal/webui.(*Handler).handleShellInput | internal/mcp.(*Server).writeShellInput | 同一项目业务执行点：internal/session.(*ChildShell).SendTerminalBytesFrom | 仅 MCP reviewHolds=false 分支；UI 经 SendTerminalBytes 固定 InputFromAPI；MCP source 来自 callerInputSource(ctx)。review=true 则先 StageForApproval，不当作同一次立即写入。 |
| C012 shell.input | 浏览器 WS 输入与 MCP 非审阅写入 | internal/webui.(*uiWS).handleWSInput | internal/mcp.(*Server).writeShellInput | 同一项目业务执行点：internal/session.(*ChildShell).SendTerminalBytesFrom | WS 输入经过自己的 ShellInput Restriction 检查，不经 MCP 准入或 AI staging。 |
| C013 shell.key | REST 命名按键与 MCP 非审阅按键 | internal/webui.(*Handler).handleShellKey | internal/mcp.(*Server).pressShellKey | 同一项目业务执行点：internal/session.(*ChildShell).PressKeyFrom | 浏览器实际键盘通过 WS input 发原始字节，未发现它调用 REST /key；本行是后台 REST 能力。review=true 则 CommitStagedForApproval。 |
| C014 shell.output | REST 输出字节窗口 | internal/webui.(*Handler).writeOutputRange | internal/mcp.(*Server).handleReadOutput | 同一项目业务执行点：internal/transcript.Read | 同一 transcript.Read，UI StripAnsi=false 和显式窗口；DSL 从 args 解析窗口并维护自己的游标。 |
| C015 shell.output | 浏览器 WS 实时输出 | internal/webui.(*uiWS).runWatch | internal/mcp.(*Server).handleReadOutput | 更低层项目执行点汇合：internal/session.(*ChildShell).ReadTerminalStream | WS 直接读 ChildShell，MCP 经 transcript.Read；WS 不调用 transcript.Read，不共享输出窗口/审批入口。 |
| C016 shell.output | REST 逻辑行与 DSL from-line | internal/webui.(*Handler).handleShellLines | internal/mcp.(*Server).handleReadOutput | 更低层项目执行点汇合：internal/message.(*Manager).OutputByteRange | UI 先 LineSpans，MCP from_line 先 LineStartOffset；未共用 LineSpans，只在原始字节区间读取汇合。 |
| C017 shell.output | WebUI rail 布局 | internal/webui.(*Handler).handleShellRail | 无对应公开 DSL 入口 | 仅 WebUI/REST | 没有等价 DSL rail 布局命令，不能把读取同一份输出字节当作布局能力汇合。 |
| C018 shell.resize | 浏览器 WS PTY 尺寸 | internal/webui.(*uiWS).handleWSResize | internal/mcp.(*Server).handleResizePty | 同一项目业务执行点：internal/session.(*ChildShell).ResizePty |  |
| C019 shell.resize | REST PTY 尺寸 | internal/webui.(*Handler).handleShellResize | internal/mcp.(*Server).handleResizePty | 同一项目业务执行点：internal/session.(*ChildShell).ResizePty | 后台 REST 存在，但浏览器源码使用 WS resize。 |
| C020 shell.detect | 旧 shell_detect handler | 无对应业务入口 | 无对应公开 DSL 入口 | 当前两入口均无公开映射 | handlerFor 存在 shell_detect，但当前 19 个 DSL 动词无公开映射，callTool 实参/动态 fileCapability 映射也没有此入口；没有 WebUI 路由。 |
| C021 shell.reader.register | 输出 reader 注册 | internal/webui.(*uiWS).addWatch | internal/mcp.(*Server).handleRegisterReader | 同一项目业务执行点：internal/session.(*ChildShell).RegisterReader | WS watch_add 自动注册，无 ShellReaderRegister 检查；DSL 显式 reader 经 callTool。 |
| C022 shell.reader.unregister | reader 注销/订阅退出清理 | internal/webui.(*uiWS).endWatch | internal/mcp.(*Server).handleUnregisterReader | 同一项目业务执行点：internal/session.(*ChildShell).UnregisterReader | WebUI 是订阅生命周期清理，不是让浏览器任意提供 reader_id 的等价接口。 |
| C023 session.message | 完整 marks 与 DSL span | internal/webui.(*Handler).handleShellMarks | internal/mcp.(*Server).handleListMessages | 同一项目业务执行点：internal/session.(*Manager).Marks | WebUI 不传 start/end 的分支；span 确认经过 callTool(message) → handleMessageOps → handleListMessages。旧 runDirective 注释不足以证明 span 旁路。 |
| C024 session.message | 窗口 marks | internal/webui.(*Handler).handleShellMarks | 无对应公开 DSL 入口 | 仅 WebUI/REST | start/end 窗口分支；DSL span 没有等价窗口接口。 |
| C025 user.notify | DSL 通知到 WebUI 回调 | 无对应业务入口 | internal/mcp.(*Server).handleNotifyUser | 仅 DSL/MCP | 不是两条入口共同调用业务函数；是 MCP uiNotify 函数值经 main.go 显式绑定到 WebUI BroadcastUINotify。单列回调连接证明。 |
| C026 forward.open | 建立 local 转发 | internal/webui.(*Handler).handleCreateForward | internal/mcp.(*Server).handleLocalForward | 同一项目业务执行点：internal/forward.(*ForwardManager).CreateLocal | 只比较匹配 direction/action 的分支；actual parameters 完整保留，UI sshConfig 与 MCP sess.Info().Name 也不混同。 |
| C027 forward.open | 建立 remote 转发 | internal/webui.(*Handler).handleCreateForward | internal/mcp.(*Server).handleRemoteForward | 同一项目业务执行点：internal/forward.(*ForwardManager).CreateRemote | 只比较匹配 direction/action 的分支；actual parameters 完整保留，UI sshConfig 与 MCP sess.Info().Name 也不混同。 |
| C028 forward.open | 建立 dynamic 转发 | internal/webui.(*Handler).handleCreateForward | internal/mcp.(*Server).handleDynamicForward | 同一项目业务执行点：internal/forward.(*ForwardManager).CreateDynamic | 只比较匹配 direction/action 的分支；actual parameters 完整保留，UI sshConfig 与 MCP sess.Info().Name 也不混同。 |
| C029 forward.close | 关闭转发 | internal/webui.(*Handler).handleDeleteForward | internal/mcp.(*Server).handleCloseForward | 同一项目业务执行点：internal/forward.(*ForwardManager).Close |  |
| C030 forward.list | 转发列表 | internal/webui.(*Handler).handleListForwards | internal/mcp.(*Server).handleListForwards | 同一项目业务执行点：internal/forward.(*ForwardManager).List |  |
| C031 file.read | 远程文件下载/读取 | internal/webui.serveRemoteFile | internal/mcp.(*Server).handleFileRead | 仅第三方 SFTP 执行点汇合：github.com/pkg/sftp.(*Client).Open | UI StreamReadTo 与 MCP ReadFile/ReadToLocalProgress 是不同项目函数；仅在下层 pkg/sftp.Open 汇合。MCP 可能异步落本地文件，UI 流向 HTTP 响应。 |
| C032 file.read | internal 本机文件读取 | internal/webui.serveLocalFile | internal/mcp.(*Server).handleFileRead | 项目内执行路径不同 | UI 本机分支直接 OS；MCP 即使 internal 也先 sshClientForSession 再 internal/sftp.Client。未展开第三方服务端文件处理器，不声称最终 OS 调用完全相同。 |
| C033 file.write | 远程文件上传/写入 | internal/webui.(*Handler).handleUploadFile | internal/mcp.(*Server).handleFileWrite | 更低层项目执行点汇合：internal/sftp.(*Client).openForWrite | UI StreamWriteFrom；MCP WriteFile 或异步 WriteFromLocalProgress，共用 openForWrite；内容来源和任务/返回语义不同。 |
| C034 file.write | internal 本机文件上传 | internal/webui.(*Handler).handleUploadFile | internal/mcp.(*Server).handleFileWrite | 项目内执行路径不同 | UI 本机直接 OS，MCP 的目标文件经 SFTP；模式/offset 分支见源码实参。 |
| C035 file.write | 文件文本替换 | 无对应业务入口 | internal/mcp.(*Server).handleFileReplace | 仅 DSL/MCP | 没有等价 WebUI/REST replace 路由。 |
| C036 file.stat | 远程目录与文件 stat | internal/webui.(*Handler).handleListFiles | internal/mcp.(*Server).handleFileStat | 同一项目业务执行点：internal/sftp.(*Client).StatFile |  |
| C037 file.stat | internal 本机目录与 stat | internal/webui.(*Handler).handleListFiles | internal/mcp.(*Server).handleFileStat | 项目内执行路径不同 | UI os.Stat/os.ReadDir；MCP 经 SFTP。 |
| C038 file.delete | 远程文件删除 | internal/webui.(*Handler).handleDeleteFile | internal/mcp.(*Server).handleFileDelete | 同一项目业务执行点：internal/sftp.(*Client).RemoveFile |  |
| C039 file.delete | internal 本机删除 | internal/webui.(*Handler).handleDeleteFile | internal/mcp.(*Server).handleFileDelete | 项目内执行路径不同 |  |
| C040 file.rename | 文件重命名 | internal/webui.(*Handler).handleRenameFile | 无对应公开 DSL 入口 | 仅 WebUI/REST | 仅确认 WebUI 远程执行点；MCP 有 file_rename handler 和 fileCapability 映射，但当前公开 DSL 分支没有产生 action=rename，不把内部方法存在当作可调用。internal 分支 os.Rename。 |
| C041 file.mkdir | 远程建目录 | internal/webui.(*Handler).handleMakeDir | internal/mcp.(*Server).handleFileMakeDir | 同一项目业务执行点：internal/sftp.(*Client).MakeDir | WebUI backend 路由存在，未发现本版本浏览器发起 mkdir 请求。 |
| C042 file.mkdir | internal 本机建目录 | internal/webui.(*Handler).handleMakeDir | internal/mcp.(*Server).handleFileMakeDir | 项目内执行路径不同 |  |
| C043 file.urls | 生成文件上传下载 URL | 无对应业务入口 | internal/mcp.(*Server).handleGetFileURLs | 仅 DSL/MCP | 浏览器自行构造下载/上传 URL，不调用 DSL 的 URL 生成 handler；不能当作相同能力点。 |
| C044 file.perm | ChmodFile | 无对应业务入口 | internal/mcp.(*Server).handleFilePerm | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C045 file.perm | ChownFile | 无对应业务入口 | internal/mcp.(*Server).handleFilePerm | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C046 file.perm | ChtimesFile | 无对应业务入口 | internal/mcp.(*Server).handleFilePerm | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C047 file.link | ReadLink | 无对应业务入口 | internal/mcp.(*Server).handleFileLinkOp | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C048 file.link | SymlinkFile | 无对应业务入口 | internal/mcp.(*Server).handleFileLinkOp | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C049 file.link | LinkFile | 无对应业务入口 | internal/mcp.(*Server).handleFileLinkOp | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C050 file.fs | TruncateFile | 无对应业务入口 | internal/mcp.(*Server).handleFileFsOp | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C051 file.fs | RealPath | 无对应业务入口 | internal/mcp.(*Server).handleFileFsOp | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C052 file.fs | StatVFS | 无对应业务入口 | internal/mcp.(*Server).handleFileFsOp | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C053 file.getwd | Getwd | 无对应业务入口 | internal/mcp.(*Server).handleFileGetwd | 仅 DSL/MCP | 未注册对应 WebUI 业务路由；按 handler 的具体 op 分支验证到该方法，不合并读写子动作。 |
| C054 profile.list | 连接摘要列表 | internal/webui.(*Handler).handleListConnections | internal/mcp.(*Server).handleListSSHConfigs | 同一项目业务执行点：internal/sshconfig.(*Store).List | 共同 List/Load，不表示摘要字段与过滤一致。 |
| C055 profile.list | 单条连接读取：原文 vs 投影 | internal/webui.(*Handler).handleGetConnection | internal/mcp.(*Server).handleGetSSHConfig | 不同业务读取点与结果 | UI ReadRaw 返回 TOML；MCP Load 后组装字段。不能以同一个 Store 对象认定是同一个读取能力。 |
| C056 profile.create | 创建连接配置 | internal/webui.(*Handler).handlePutConnection | internal/mcp.(*Server).handleCreateSSHConfig | 同一项目业务执行点：internal/sshconfig.(*Store).SaveWithOptions | MCP Save 是临时标记 false 的包装；UI 传 actual temporary 值。 |
| C057 profile.edit | 编辑连接配置 | internal/webui.(*Handler).handlePutConnection | internal/mcp.(*Server).handleEditSSHConfig | 同一项目业务执行点：internal/sshconfig.(*Store).SaveWithOptions |  |
| C058 profile.copy | 复制连接配置 | 无对应业务入口 | internal/mcp.(*Server).handleCopySSHConfig | 仅 DSL/MCP | 未发现等价复制 REST 路由；PUT ?from 是改名，不按函数名或 UI 按钮猜成 copy。 |
| C059 profile.delete | 删除连接配置 | internal/webui.(*Handler).handleDeleteConnection | internal/mcp.(*Server).handleDeleteSSHConfig | 同一项目业务执行点：internal/sshconfig.(*Store).Delete |  |
| C060 ext.list | 扩展列表 | internal/webui.(*Handler).handleListExtensions | internal/mcp.(*Server).handleExtensionList | 同一项目业务执行点：internal/extensions.(*Store).List |  |
| C061 ext.get | 扩展 JSON/结构化读取 | internal/webui.(*Handler).handleGetExtension | internal/mcp.(*Server).handleExtensionGet | 同一项目业务执行点：internal/extensions.(*Store).Load | 都 Load，输出投影不同；UI format=toml 分支另走 ReadRaw，不在本行汇合结论内。 |
| C062 ext.create | 扩展创建 | internal/webui.(*Handler).handleCreateExtension | internal/mcp.(*Server).handleExtensionCreate | 同一项目业务执行点：internal/extensions.SaveTemplate |  |
| C063 ext.update | 扩展更新/启停 | internal/webui.(*Handler).handleUpdateExtension | internal/mcp.(*Server).handleExtensionUpdate | 同一项目业务执行点：internal/extensions.SaveTemplate |  |
| C064 ext.delete | 扩展删除 | internal/webui.(*Handler).handleDeleteExtension | internal/mcp.(*Server).handleExtensionDelete | 同一项目业务执行点：internal/extensions.(*Store).Delete |  |
| C065 ext.retry | 扩展重试/重建 | internal/webui.(*Handler).handleRetryExtension | internal/mcp.(*Server).handleExtensionRetry | 同一项目业务执行点：internal/extensions.(*Reconciler).Retry |  |
| C066 jrpc.open | 打开 jrpc peer | 无对应业务入口 | internal/mcp.(*Server).jrpcOpen | 仅 DSL/MCP | 经 openPeer/jrpcOpen/refuseDoor；不经 callTool/checkAdmission。WebUI 创建扩展不是这个协议注册表 peer 的同一个入口。 |
| C067 mcp.open | 打开 mcp peer | 无对应业务入口 | internal/mcp.(*Server).jrpcOpen | 仅 DSL/MCP | 经 openPeer/jrpcOpen/refuseDoor；不经 callTool/checkAdmission。WebUI 创建扩展不是这个协议注册表 peer 的同一个入口。 |
| C068 cdp.open | 打开 cdp peer | 无对应业务入口 | internal/mcp.(*Server).jrpcOpen | 仅 DSL/MCP | 经 openPeer/jrpcOpen/refuseDoor；不经 callTool/checkAdmission。WebUI 创建扩展不是这个协议注册表 peer 的同一个入口。 |
| C069 未独立列为 capability | 连接测试 | internal/webui.(*Handler).handleTestConnection | 无对应公开 DSL 入口 | 仅 WebUI/REST | 真实连接测试，不当作 session.start 同一执行点 |
| C070 未独立列为 capability | 主机配置改名 | internal/webui.(*Handler).handlePutConnection | 无对应公开 DSL 入口 | 仅 WebUI/REST | PUT ?from 分支，与 DSL copy 不同 |
| C071 未独立列为 capability | 主机明文/加密导出 | internal/webui.(*Handler).handleExportConnections | 无对应公开 DSL 入口 | 仅 WebUI/REST | format 的加密由 configcrypt.Encrypt 另行处理 |
| C072 未独立列为 capability | 主机持久/临时导入 | internal/webui.(*Handler).handleImportConnections | 无对应公开 DSL 入口 | 仅 WebUI/REST | temporary 实参原样记录 |
| C073 未独立列为 capability | 扩展 TOML 原文 | internal/webui.(*Handler).handleGetExtension | 无对应公开 DSL 入口 | 仅 WebUI/REST | 没有 DSL 原文读取的对应分支 |
| C074 未独立列为 capability | 扩展导出 | internal/webui.(*Handler).handleExportExtensions | 无对应公开 DSL 入口 | 仅 WebUI/REST | 浏览器经 downloadTOML wrapper |
| C075 未独立列为 capability | 扩展导入 | internal/webui.(*Handler).handleImportExtensions | 无对应公开 DSL 入口 | 仅 WebUI/REST | 不等同 DSL create 单条模板 |
| C076 未独立列为 capability | 会话改名 | internal/webui.(*Handler).handleRenameSession | 无对应公开 DSL 入口 | 仅 WebUI/REST | 当前 DSL 无独立 rename 会话词 |
| C077 未独立列为 capability | 审批列表 | internal/webui.(*Handler).handleApprovalList | 无对应公开 DSL 入口 | 仅 WebUI/REST | 管理面，不是 MCP 工具 |
| C078 未独立列为 capability | 批准操作 | internal/webui.(*Handler).handleApprovalDecision | 无对应公开 DSL 入口 | 仅 WebUI/REST | approve 分支，批准后另见执行回放 |
| C079 未独立列为 capability | 拒绝操作 | internal/webui.(*Handler).handleApprovalDecision | 无对应公开 DSL 入口 | 仅 WebUI/REST | reject 分支 |
| C080 未独立列为 capability | 开启审阅 | internal/webui.(*Handler).handleSessionApproval | 无对应公开 DSL 入口 | 仅 WebUI/REST | PATCH enabled=true |
| C081 未独立列为 capability | 关闭审阅 | internal/webui.(*Handler).handleSessionApproval | 无对应公开 DSL 入口 | 仅 WebUI/REST | PATCH enabled=false |
| C082 未独立列为 capability | 版本 | internal/webui.(*Handler).handleVersion | 无对应公开 DSL 入口 | 仅 WebUI/REST | 仅 WebUI backend/REST；实际调用与函数值回调逐点列在全量连接及 reference 清单；不把元数据混成 session.info。 |
| C083 未独立列为 capability | 连接者 | internal/webui.(*Handler).handleListClients | 无对应公开 DSL 入口 | 仅 WebUI/REST | 仅 WebUI backend/REST；实际调用与函数值回调逐点列在全量连接及 reference 清单；不把元数据混成 session.info。 |
| C084 未独立列为 capability | 跨会话活动 | internal/webui.(*Handler).handleActivity | 无对应公开 DSL 入口 | 仅 WebUI/REST | 仅 WebUI backend/REST；实际调用与函数值回调逐点列在全量连接及 reference 清单；不把元数据混成 session.info。 |
| C085 未独立列为 capability | 实例状态 | internal/webui.(*Handler).handleDaemonInfo | 无对应公开 DSL 入口 | 仅 WebUI/REST | 仅 WebUI backend/REST；实际调用与函数值回调逐点列在全量连接及 reference 清单；不把元数据混成 session.info。 |
| C086 未独立列为 capability | 停止实例 | internal/webui.(*Handler).handleDaemonStop | 无对应公开 DSL 入口 | 仅 WebUI/REST | 仅 WebUI backend/REST；实际调用与函数值回调逐点列在全量连接及 reference 清单；不把元数据混成 session.info。 |
| C087 未独立列为 capability | 任务回执/等待 | 无对应业务入口 | internal/mcp.(*Server).directiveGetTask | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C088 未独立列为 capability | 任务列表 | 无对应业务入口 | internal/mcp.(*Server).directiveLsTask | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C089 未独立列为 capability | 任务取消 | 无对应业务入口 | internal/mcp.(*Server).directiveCloseTask | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C090 未独立列为 capability | alias 创建/编辑/删除/列表 | 无对应业务入口 | internal/mcp.(*Server).directiveAlias | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C091 未独立列为 capability | 文件/通道输出管道复制 | 无对应业务入口 | internal/mcp.(*Server).pipeCopy | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C092 未独立列为 capability | 原始协议调用/收帧 | 无对应业务入口 | internal/mcp.(*Server).jrpcChannel | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C093 未独立列为 capability | 远端 MCP 工具便利调用 | 无对应业务入口 | internal/mcp.(*Server).mcpToolCall | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C094 未独立列为 capability | 远端 MCP 工具发现 | 无对应业务入口 | internal/mcp.(*Server).mcpToolList | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C095 未独立列为 capability | 浏览器协议发现 | 无对应业务入口 | internal/mcp.(*Server).cdpProtocol | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C096 未独立列为 capability | 浏览器 target 附着 | 无对应业务入口 | internal/mcp.(*Server).cdpAttach | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C097 未独立列为 capability | 浏览器 target 解除附着 | 无对应业务入口 | internal/mcp.(*Server).cdpDetach | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |
| C098 未独立列为 capability | 协议大帧分页 | 无对应业务入口 | internal/mcp.(*Server).pageFrame | 仅 DSL/MCP | 直接调用任务/alias/复制/协议路径；没有等价 WebUI 业务路由，不经 callTool/checkAdmission，独立守卫在全量清单中保留。 |

## 8. 对统一能力过滤器的直接结论

建议的强制分发入口尚不存在。当前 capability 表主要是描述和名称映射，不能只给它“装 Hook”就拦截所有执行。

真正需要统一的是这份清单中的连接：常规 MCP handler 分发、DSL 直接读/协议/任务/复制、WebUI 独立业务函数/OS 文件分支，以及审批恢复和生命周期路径。哪些操作属于外部授权、哪些属于已授权计划的恢复或系统清理，必须在接入时明确分类；不能通过全局 skip_auth 或“人在 WebUI”推测覆盖。

本报告的结论是当前源码事实，不把权限设计文档里的拟议行为当作已有实现。未修改业务源码、未新建业务测试、未 commit/push。

## 9. 最终一致性校验记录

- 45 个现有 capability ID 均有逐项对照记录，共 98 项语义/分支验证。
- 714 个连接点逐一对应全量调用 CSV 的唯一 edge_id，实参数组与原始提取结果一致；完整 Markdown 没有只展示抽样行。
- 11,247 个调用位置的文件、行号、列号均核对到源文件，函数/闭包/初始化节点定义位置及跨平台定义位置也已核对。
- 声称汇合的对照均保存两侧到指定执行点的类型解析调用链；没有共同业务点的项保留不同路径或单侧入口结论，不补写假想连接。
- 69 个 HTTP 注册模式、4 个 WS 入站消息、19 个 DSL 动词全部进入入口覆盖表，共 92 项。
- 183 个参与提取的源文件 SHA-256 同时匹配固定 Git 提交与当前工作区，三套构建配置类型检查无错误。
- CSV 关联、Markdown 完整行数与链接、空白格式和本机标识/个人信息检查通过。

以上为静态源码连接验证，不是所有分支的运行时覆盖率证明，也不模拟第三方库内部、操作系统行为或异步调度。
