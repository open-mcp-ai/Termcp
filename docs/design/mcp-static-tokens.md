# 简易 MCP token（已实现）

2026-11-02。此文描述当前静态 Token 交付；外部 OAuth Authorization Server 的 resource-server 模式见 [OAuth resource server](oauth-resource-server.md)，Termcp 自有 OAuth 授权服务器、自动配对、自动刷新、多个权限组继承和子凭据仍属未来计划。逐操作策略、权限组、主体、审计和只读预览见 [权限组合同](permission-groups.md)。

**2026-11-05 更新**：同一 principal 现只允许一枚当前静态 Token。普通签发在已有当前 Token 时返回冲突；轮换使用原子替换，旧 Token 立即失效并进入历史。历史记录可清理但不能重新启用。签发/轮换可设置可选 `expires_at`，成功 MCP 准入会更新 `last_used_at`。外部 OAuth resource-server 支持不改变静态 Token 表；自有 OAuth 授权服务器、AgentGrant 和多用户仍不做。完整边界见 [认证与权限范围收敛](auth-scope-2026-11-05.md)。

管理员从 `/permissions` 选择或创建权限组，再创建主体并签发 Token。主体保存稳定 ID 与组绑定，权限属于组；创建主体默认不签发凭据，`issue_token:true` 才同时返回一次 Token。也可以为没有当前 Token 的主体签发，或不指定主体，为管理员自己签发。一个 Agent 标识一个独立客户端/进程安全边界；同一 Token 可以承载多个 MCP 连接和会话。重启桥不登记新身份。

token 格式为 `tmcp_<随机凭据ID>.<256位随机secret>`，校验完整 token 的 SHA-256 摘要。原值仅在签发响应中展示，列表只返回 ID、归属、名称、时间。主体、权限组和凭据摘要保存在加密认证状态中，沿用同一 Main Key。当前格式拒绝未发布的 V1 业务权限字段，不做静默迁移。上限为 256 个主体、1024 枚 token。

所有 MCP 请求显式携带 Bearer。token 仅允许 MCP `/stream`、`/sse`、`/message` 及最小探测 `/auth/mcp`，管理员 token 也不能访问 WebUI、REST、账号或 daemon 控制接口。Cookie 写请求继续要求 CSRF；显式 Bearer 错误时不退回 Cookie。角色入口、主体启停、组启停和当前动作策略每次请求都检查。主体 namespace 固定从可信 ID 派生，忽略外部 namespace 声明；MCP 传输会话绑定到创建它的身份和凭据，不能凭另一枚有效 token 借用。

stdio 客户端使用 `TERMCP_MCP_TOKEN` 或 `--mcp-token`，沿用现有桥每次请求转发 Bearer。它不参与选择服务端认证模式，确保 daemon 时从子进程环境剔除。旧 `TERMCP_AUTH_TOKEN`/`TERMCP_AUTH_HASH` 仍只代表显式旧共享认证模式。MCP token 与旧参数互斥，防止误把客户端凭据变成全局服务端密码。

WebUI 的“连接者”列表和 Agent 详情会把当前 MCP 传输会话归属到认证后的 Agent 与 Token 记录 ID。stdio bridge 额外携带仅用于展示的 `X-Termcp-Client-Mode: stdio-bridge`；该请求头不参与授权，客户端自报的 clientInfo 或 namespace 也不能改变归属。一个 Agent 可以同时有多个 MCP 会话，因此活动连接数是会话数，不是 Token 数；页面不输出 Token secret 或摘要。

stdio 复用既有 Ensure，不新增 daemon 启动锁。管理探测被拒绝时，有 MCP token 的桥改用 `/auth/mcp` 验证接入并获取实际保活间隔，无需读取 PID、日志或获得停止权限。已部署远端使用 `stdio <URL>`；本机自动部署使用 `daemon stdio`，自定义数据目录需一致。无自动解锁时，重启后需管理员先登录，token 不解锁密钥环。

Token 默认为永久有效；签发或轮换可通过可选 `expires_at` 设置绝对到期时间，到期后新的 MCP 准入返回 `401`。管理员通过 `POST /auth/tokens/{id}/rotate` 原子轮换当前 Token，响应中的新 secret 只展示一次；旧 Token 立即停止认证，并在持久提交后取消其活动请求。撤销会把 Token 移入历史，`DELETE /auth/tokens/history?principal_id=...` 可清理已撤销或已过期记录。停用主体、停用组或禁用角色 MCP 权限会取消受影响 token 的连接，并取消登记在凭据上的后台传输。已启动的终端、任务仍遵循原业务生命周期，撤销不等于终止所有远端进程。管理员退出或改登录密码只撤销 Web 会话，MCP token 保持有效。

权限页支持明确动作、参数条件、主机范围、风险/可达面/依赖、主体与凭据生命周期、只读 DSL 预览和结构化审计，详见 [权限管理页面与执行边界](permission-management.md)。初始缺省组不授予业务动作；预设、组成员和凭据各自有明确边界。MCP namespace、资源 restriction 和部署限制继续约束操作。Shell、协议调用和端口转发不是操作系统沙箱。
