# 简易 MCP token（已实现）

2026-10-29。此文描述当前交付，早期认证提案中的 OAuth、自动配对、自动刷新和细粒度业务策略仍属未来计划。

管理员从 `/permissions` 新建 Agent，服务端保存稳定的 Agent ID；新界面先创建空权限身份，再选配功能与主机，最后签发 token。兼容 API 仍支持一次创建并签发；也可以为已有 Agent 增发，或不选 Agent，为管理员自己签发。客户端名称只作展示，不决定身份。每个身份可以同时连接多个客户端；重启桥不登记新身份。

token 格式为 `tmcp_<随机凭据ID>.<256位随机secret>`，校验完整 token 的 SHA-256 摘要。原值仅在签发响应中展示，列表只返回 ID、归属、名称、时间。Agent 与凭据摘要保存在已有 `auth.enc`，沿用同一 Main Key；旧版本认证数据缺少新增字段时视为空列表。无需额外凭据库。上限为 256 个 Agent、1024 枚 token。

所有 MCP 请求显式携带 Bearer。token 仅允许 MCP `/stream`、`/sse`、`/message` 及最小探测 `/auth/mcp`，管理员 token 也不能访问 WebUI、REST、账号或 daemon 控制接口。Cookie 写请求继续要求 CSRF；显式 Bearer 错误时不退回 Cookie。预设角色开关与 Agent 启停每次请求都检查。Agent 的资源 namespace 固定从可信 ID 派生，忽略外部 namespace 声明；MCP 传输会话绑定到创建它的身份和凭据，不能凭另一枚有效 token 借用。

stdio 客户端使用 `TERMCP_MCP_TOKEN` 或 `--mcp-token`，沿用现有桥每次请求转发 Bearer。它不参与选择服务端认证模式，确保 daemon 时从子进程环境剔除。旧 `TERMCP_AUTH_TOKEN`/`TERMCP_AUTH_HASH` 仍只代表显式旧共享认证模式。MCP token 与旧参数互斥，防止误把客户端凭据变成全局服务端密码。

stdio 复用既有 Ensure，不新增 daemon 启动锁。管理探测被拒绝时，有 MCP token 的桥改用 `/auth/mcp` 验证接入并获取实际保活间隔，无需读取 PID、日志或获得停止权限。已部署远端使用 `stdio <URL>`；本机自动部署使用 `daemon stdio`，自定义数据目录需一致。无自动解锁时，重启后需管理员先登录，token 不解锁密钥环。

token 无自动过期或刷新。轮换由管理员为同一身份增发，手动更新客户端后撤销旧 token。撤销持久保存后取消该 token 的在途请求；停用 Agent 或禁用角色 MCP 权限取消受影响 token 的连接。已启动的终端、任务仍遵循原业务生命周期，撤销不等于终止所有远端进程。管理员退出或改主密码只撤销 Web 会话，MCP token 保持有效。

当前新增逐 Agent 的功能组和主机范围选择，详见 [权限管理页面与执行边界](permission-management.md)。新界面创建的 Agent 初始无功能、无主机权限；旧记录保持原策略直到管理员保存。MCP namespace、资源 restriction 和部署限制继续约束操作。Shell、协议调用和端口转发不是操作系统沙箱。
