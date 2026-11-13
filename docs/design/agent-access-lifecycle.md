# Agent 来源、凭据与 MCP 会话生命周期

状态：已实现，Issue #315。

## 三个独立维度

Agent 管理不从名称、namespace、`clientInfo` 或连接 Header 推断身份：

| 维度 | 取值 | 持久化/观测依据 |
| --- | --- | --- |
| 创建来源 | `oauth`、`daemon_stdio`、`manual` | Agent 创建记录。OAuth 只来自成功完成的内置授权；`daemon_stdio` 只来自服务端本机受管登记；旧记录缺少来源时保持 `manual`。 |
| 持有凭据 | OAuth grant、daemon managed credential、static Token | OAuth grant 与 opaque token 记录、daemon registration/token 记录、现有单有效静态 Token 记录分别展示。 |
| 当前连接方式 | Streamable HTTP、SSE、stdio bridge | 已认证 MCP transport 与 display-only `client_mode`。连接方式不改变 Agent 来源。 |

daemon 的握手文件位于 data directory 的 `local-daemon-credential.json`，权限为 owner-only；
加密 auth JSON 只保存 token 摘要和 registration/Agent/token 关联。网页没有读取或复制该
原文的入口。撤销 token、删除 Agent 或显式撤销登记会写 tombstone，重启不复活。

## 策略变化与身份失效

| 事件 | 凭据/grant | MCP session | 请求与后台工作 |
| --- | --- | --- | --- |
| 改组、修改动作/约束/主机范围、组停用/删除 | 保留 | 保留 | 下一次操作实时解析；按工作登记的 operation/host authority 取消失权部分 |
| OAuth access 到期并刷新 | 同一 grant/client/resource；Agent/owner/group 不变 | 可继续 | 使用当前策略 |
| Agent/用户/client 停用、grant 撤销、静态 Token 撤销/替换 | 失效 | 注销受影响 session | 取消对应请求与后台工作 |
| 保险库锁定或授权配置失效 | 不能继续安全验证 | 必要时终止 | 不继续使用缓存授权 |
| 单个 transport session 丢失/服务重启 | 保留 | 有界重建 | 不重放结果不明的业务请求 |

`commitState` 仍负责加密状态的原子发布和 revision 复核。保存失败时不调用 after-cancel，
不发布撤销通知；网络、文件写入和 transport 注销都在普通状态互斥锁之外。

## 传输恢复

- Streamable HTTP：有效 Bearer + 未知 session = 404；真实 owner/grant/client/resource
  不匹配 = 403；无效 Bearer = 401。OAuth refresh 只允许同 grant/client/resource 续接。
- SSE：重新建立 GET event stream，读取新的 `endpoint` 事件和 `/message` URL，再用已观察的
  initialize 参数执行内部重握手；不复用 Streamable HTTP 的 session header 状态码规则。
- 自管 stdio bridge：Streamable HTTP 与 SSE 都限制重试次数和每次握手时间，并服从 bridge
  取消。发现 session 丢失时，当前请求返回 JSON-RPC error，`data.kind` 为
  `termcp_session_replaced`，`result` 为 `unknown`，`request_was_replayed` 为 `false`。
  后续新请求使用新 session；已发送且结果不明的命令、按键、写入或删除不会被自动发送第二次。
