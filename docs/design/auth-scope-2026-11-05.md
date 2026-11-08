# 认证与权限范围收敛（2026-11-05）

状态：已确认。2026-11-07 新增外部 authorization server 的 OAuth resource-server 切片，见 [OAuth resource-server](oauth-resource-server.md)；Termcp 自有授权服务器、多用户和 AgentGrant 仍未实现。

## 目标

当前 managed 模式继续保持：

- 单个本机 Admin，使用登录密码与独立解密密码；
- Admin 浏览器会话、Origin/CSRF 校验和本次进程内存会话；
- Agent principal 绑定权限组，使用静态 MCP Bearer token；
- Token 只进入 MCP，不能进入 WebUI、普通 REST、认证管理或 daemon 控制面；
- 权限策略继续保存在加密认证状态中，并支持动态收权和后台工作取消。

## 已确认决策

| 范围 | 决策 |
| --- | --- |
| AgentGrant | 暂不新增。当前使用 `Principal + PermissionGroup + Token`；Token 已绑定主体，权限由组解析。 |
| 多用户 | 暂不实现。保持单本机 Admin；不增加 human principal、member、RoleBinding、tenant 或邀请登录。 |
| OAuth/OIDC | 已实现外部 AS 的 resource-server discovery、JWKS JWT 验证、resource/audience/scope 校验和 Agent claim 映射。自有 authorization code、PKCE、token endpoint、refresh、DCR/CIMD 和 stdio OAuth profile 仍延期。 |
| Canonical origin / 可信代理 | 不纳入本阶段。部署必须让反向代理保留正确的 Host 和 Origin；不新增 forwarded-header 信任配置。 |
| 退出所有设备 | 不新增独立入口。保留“退出当前会话”和“修改登录密码时撤销全部 Web 会话”。 |
| 忘记密码恢复 | 不纳入本阶段。不新增 `termcp auth recover`、专用 socket/named pipe 或离线恢复流程。 |
| Access/Refresh 双层会话 | 本阶段不改造。继续使用当前 24 小时 JWT Cookie；idle deadline、absolute deadline 和自动续期以后单独立项。 |
| Token 生命周期 | 按 issue #253 收敛：每个 Agent principal 最多一个有效 Token，轮换必须是原子替换。补充到期、最近使用和撤销元数据。 |
| 权限目录 | 以 issue #252 为唯一来源。目标为 46 项能力；在 #252 修正旧 86/45 数字前不得按旧清单改实现。 |

## 待开发范围

### 1. Token 生命周期

- [x] 同一 principal 的普通签发拒绝已有有效 Token 的情形；
- [x] 新增原子 rotate：新 Token 持久化成功时旧 Token 同时失效；
- [x] 旧 Token 立即停止认证，并取消其活动请求和后台工作；
- [x] 撤销记录保留 `revoked_at` / `replaced_by` 历史，并支持按 principal 清空历史；
- [x] 重启后保持有效和撤销状态；
- [x] UI 显示当前 Token、轮换、撤销和可清理的历史记录；
- [x] API 支持可选 `expires_at`，成功 MCP 准入更新 `last_used_at`；两者显示在权限页当前/历史 Token 元数据中。

详细语义由 [#253](https://github.com/open-mcp-ai/Termcp/issues/253) 继续跟踪。

### 2. 认证页面部署前缀（[#265](https://github.com/open-mcp-ai/Termcp/issues/265)）

- [x] 认证页面、`auth-session.js`、设置菜单和 Token 配置生成使用当前部署前缀；
- [x] 反向代理挂载到 `/termcp/` 时，认证请求、跳转和生成的 MCP URL 均留在挂载点内；
- [x] 不新增 canonical origin 配置；页面从自身 URL 推导前缀，根路径输出保持原样。

### 3. 桥的认证保活（[#265](https://github.com/open-mcp-ai/Termcp/issues/265)）

- [x] Bridge 在 MCP session 建立前使用保留部署前缀的 `/auth/mcp` 保活；
- [x] idle watcher 位于认证之后，缺失、错误或已撤销 Token 的请求不能延长 daemon 空闲期限；
- [x] 当前 `/auth/mcp` 继续作为最小探测，没有新增 bridge-context 或 presence 端点；
- [x] 探测响应不暴露 PID、日志、主机列表或其他 Agent 信息。

### 4. 权限管理

- 以 [#252](https://github.com/open-mcp-ai/Termcp/issues/252) 为唯一需求来源；
- 先修正 #252 中“86 项目标 / 45 项现状 / 46 项目标”的冲突；
- 最终 catalog、预置组、UI 动作列表、服务端校验和测试必须使用同一动作集合；
- 不增加动作级主机子集、`profile.fields`、`ext.names`、任意端口/目标条件或工具 arguments 条件，除非它们出现在修正后的 #252 46 项范围内。

## 明确不做

- AgentGrant、grant 级撤销、owner/tenant 和父子授权；
- 多用户、RoleBinding、OIDC 或 member 角色；
- Termcp 自有 OAuth authorization server、动态客户端注册、PKCE、refresh token 和自动凭据轮换；
- canonical external origin、可信代理 CIDR 或 forwarded-header 信任规则；
- 退出所有设备、grant 列表或批量设备撤销；
- 忘记密码恢复工具；
- access/refresh 双层 Cookie、idle deadline、absolute deadline；
- 与本阶段无关的加密格式或主机密文布局迁移。

## 验收

- 认证页面和 Token 配置在根路径及反向代理子路径下均可用；
- 已认证桥可以维持 daemon，未认证请求不能延长空闲期限；
- Token 满足 #253 的单有效凭据、原子轮换、到期、最近使用和撤销合同；
- 权限目录与 #252 修正后的 46 项完全一致；
- OAuth 自有授权服务器、AgentGrant、多用户、canonical origin 和忘记密码恢复不出现在本阶段 UI 或 API 中；
- HTTP/SSE、重启、锁定、撤权和并发场景继续通过 `make test-permissions`。
