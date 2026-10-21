# Scoped Token 与审批网关：不透明 token + Principal 抽象 + 操作层拦截点

> 对应 issue #105「安全加固：连接凭据加密 Vault + 多端同步 + 面向 Agent/Harness 的作用域授权」。
> 本文覆盖该 issue 的 PR 0（双 token 拆分）与 PR 4–5（scoped token、审批
> 拦截点下沉且策略外置）；vault 与凭据分发见 `credential-vault.md`。
> 已对齐协作者评论（windworst）：人 REST 与 MCP 分离 token、gate 上浮为
> 安全网关层、termcp 不内建策略过滤逻辑。
>
> 执行顺序（2026-10 修订）：vault 优先——`credential-vault.md` 的
> PR 1–3 先落地，本文 PR 0 → 4 → 5 依次跟随。本文初稿写于 #117（MCP
> 工具面收成单 term 入口、`internal/capability` 准入表落地）之前，
> §二、§4.1、§5.1、§七已按该现状修订。
>
> 实现状态（2026-10-19）：PR 0（双 token）与 PR 4（scoped token 表、
> Principal、/api/tokens + termcp token CLI、MCP/REST 双入口 scope 生效、
> 审计字段）已实现。PR 5 的 Policy 接口与 `session_start` 入审未做。

## 一、结论先行

1. **Scoped token 用"不透明随机 token + 服务端 scope 表"（GitHub
   fine-grained PAT 模式），不用 macaroon / JWT。** termcp 是单实例有状态
   服务（会话、buffer、转发全是服务端状态），多一张 token 表没有架构成本；
   吊销 = 删行即时生效，审计 = 复用 `internal/mcp/logging.go` 结构化日志，
   没有签名密钥轮换和 JWKS 维护。macaroon 唯一不可替代的价值是 harness
   **离线 attenuation**（加 caveat 再下发子 agent），当前没有此需求，引入
   即为过度设计；未来若需要，`Authenticator` 抽象（见下）允许增量接入。
2. **先拆双 token，再上 scope。** PR 0 只做"人面 token 与 MCP 面 token
   分离"，无新密码学，立刻堵住"Agent 拿 token 越权调 REST 管理面"。
3. **Gate 的位置：拦截点下沉到操作层，策略外置成可插拔接口。** 这同时
   满足 issue 的"gate 下沉"与评论的"上浮为安全网关层"——termcp 拥有
   **唯一拦截点**（所有入口必经），不拥有**策略**（默认实现 = 现有人工
   审批，外部 gateway/harness 可替换）。
4. **OIDC 预留靠 `Principal` 内部抽象**，不在本期实现（见第六节）。

## 二、现状问题（对照代码）

| # | 问题 | 位置 |
|---|------|------|
| P1 | 一把 `--auth-token` 覆盖所有面：Web UI、REST、MCP SSE/stream、WS | `internal/auth/auth.go`（包注释自述 "One token guards the whole HTTP surface"） |
| P2 | 拿 token 的 Agent 与操作者能力完全相同，无法分权、吊销、限时 | 同上，无 scope 概念 |
| P3 | token 不区分人与 agent：REST 面按 `GateOperator` 语义对持 token 者整体豁免审阅（"操作者本人的键盘不该被审"），持部署 token 的 agent 走 REST 即被视为操作者本人。"A token holder can bypass this by calling the REST API directly" 的注释仍在，但 #117 后问题已从"gate 漏了一个面"转化为"认证不产出身份"——修法是双 token 给身份（PR 0），不是给 REST 加审批 | `internal/capability/capability.go`（GateOperator）、`internal/mcp/approval_gate.go` |
| P4 | REST 批量导出经 `ReadRaw` 返回含明文的原文 TOML：任何持 token 者可整库导出全部凭据，`X-Termcp-Password` 加密只是可选项。修复分两半：PR 0 禁 MCP token 调 REST 管理面；vault PR 关闭 vault 化 profile 的明文导出（credential-vault.md §五） | `internal/sshconfig/batch.go`（ExportBatch）、`internal/webui/handler_conn.go` |

## 三、双 token 拆分（PR 0）

- `--auth-token`（现有）：人用的面——Web UI、REST 管理 API（含
  `/api/connections/batch` 注入）、WS；
- `--mcp-auth-token`（新增）：MCP 面（`/sse`、`/message`、`/stream`）。
  持 MCP token 调 REST 管理面一律 403；
- 只配 `--auth-token` 时行为与今天完全一致（向后兼容，loopback 默认）；
- 拆分点在 `internal/auth` 中间件内按路由前缀判定，不需要新组件。

这一步不改 gate、不改 vault，是把"人 vs 机器"两个信任域先物理分开。

## 四、Scoped token（PR 4）

### 4.1 数据模型

```
tokens 表（data-dir 内，TOML 或 SQLite，随 storage 现有选型）：
  id:        tok_<random>           # 审计日志引用此 id，不引用 token 本体
  verifier:  sha256-<salt>-<digest> # 沿用 auth.Hash 的现有格式，明文不落盘
  kind:      human | agent
  scopes:
    tools:    ["shell.input", "file.write", ...]     # capability ID 白名单（见下）
    sessions: ["termcp://#ctf-1", ...]               # 限定会话/entry 定位符
    approval_required: true                          # 写操作强制走审批
  expires_at: <timestamp>
  created_by: <principal id>
  revoked_at: <timestamp | null>
```

- `tools` 白名单写 capability ID 而非工具名：#117 后 MCP 工具面收成为单
  `term` 入口，工具名不再是稳定词汇表，而 `internal/capability` 的表是
  操作的唯一定义点（`IDForTool`/`ToolForID` 提供与工具名的双向映射，
  `internal/session/restriction.go` 的 `Allows` 已按此双语义解析）。
  scope 检查挂进两个现有准入点——MCP 的 `checkAdmission` 与 REST 的
  `restRefuseTool`，它们本就查 capability 表，只需加 Principal 一个维度；
- 签发：`termcp token create --kind agent --tools ... --ttl 1h`（CLI）+
  REST 管理面等价端点；token 明文**只在创建响应中出现一次**；
- 吊销：删行 / 置 `revoked_at`，即时生效（每次请求查表，单实例无缓存
  一致性问题）；
- 审计：`internal/mcp/logging.go` 当前不记录任何 token 字段；为其结构化
  日志增加 `token_id`、`principal_kind`，REST/WS 面补同等日志。

### 4.2 内部货币：`Principal`

所有认证路径（现有 Bearer/Basic/cookie、scoped token、未来的 OIDC）的
唯一产出：

```go
// internal/auth
type Principal struct {
    ID               string   // token id 或未来 OIDC 的 sub
    Kind             string   // "human" | "agent"
    Scopes           ScopeSet // 工具白名单、会话定位符、过期时间
    ApprovalRequired bool
}

type Authenticator interface {
    Authenticate(*http.Request) (*Principal, error)
}
```

- 中间件改为依次尝试一串 `Authenticator`，产出 `Principal` 注入 request
  context；下游（MCP handler、REST handler、WS、审批钩子）只认
  `Principal`，不认 token 形态；
- scope 检查集中在中间件之后、handler 之前的一个统一函数：工具名 ∈
  tools 白名单、定位符 ∈ sessions、未过期未吊销；
- 旧的单一 `--auth-token` 适配为一个"全量 scope 的内置 Principal"，
  兼容路径零行为变化。

## 五、审批网关：拦截点下沉 + 策略外置（PR 5）

### 5.1 拦截点（按 #117 后的 capability 表修订）

拦截点**不**物理下沉到 `internal/session` 的方法上，理由有二：其一，
#117 已把"这个操作要不要审"集中到 `internal/capability` 的表，MCP 的
`checkAdmission` 与 REST 的 `restRefuseTool` 都在查它；其二，`Staged`
行级审批（`shell.input`/`shell.key` 以"行"为审批单位）必须留在各自的
写路径里，物理下沉会与调用入口的拦截重复甚至打架。

做法是判定函数统一收进 capability 包，三入口共用：

```go
// internal/capability；MCP/REST/WS 的准入判定都过这一个函数。
//
// RestrictionView 是包内定义的窄接口，session.Restriction 天然满足。
// 这里不能 import internal/session：session 已 import capability
// （restriction.go），反向依赖会成环。
type RestrictionView interface {
	Allows(id string) bool
	AllowsPath(p string) bool
}

func Admit(p *auth.Principal, capID string, rest RestrictionView) error
```

`GateOperator` 的豁免对象从"REST 这个面"改为"human Principal"：agent
Principal 且 `ApprovalRequired == true` 时，REST 面同样强制走 Policy——
P3 的"REST 绕过"随身份落地而消失。

纳入审批的操作清单新增 `session_start`（capability 表中 `session.start`
从 `GateDevice` 改 `GateReview`，一行加测试）："用哪个 entry 向哪台主机
发起新连接"本身是敏感操作（issue 原文此点保留）。

### 5.2 策略接口

```go
// internal/approval 新增；默认实现 = 现有 N-person 人工审批队列。
type Policy interface {
    // Check 在操作执行前调用。返回 nil 放行；返回 ErrNeedsApproval 时
    // 操作层挂起等待人工决定；返回其他错误拒绝。
    Check(ctx context.Context, p *auth.Principal, op Operation) error
}
```

- `Principal.ApprovalRequired == true`（如 agent token）→ 走 Policy；
  操作者本人的交互面按现有规则豁免；
- 外部安全网关（harness 侧）实现 `Policy` 做逻辑过滤（命令模式、速率、
  会话窗口），termcp 不内建这些策略——基础设施保持薄，策略由调用方
  生态自负，这是与协作者评论对齐的核心取舍；
- `internal/approval` 现有的"绝不 fail-open"语义（包注释：expired、
  rejected、session 丢失、模式中途关闭都以零字节写入收场）原样保留，
  Policy 返回任何非 nil 都按拒绝处理。

## 六、OIDC 演进路径（不实现，只把接口定对）

OIDC 是身份层（"你是谁"，浏览器 SSO），PAT 是授权层（"这个非交互调用方
能干什么"），两者共存而非替代——GitHub 即 OIDC 给人、fine-grained PAT
给脚本。Agent/harness 是机器对机器调用，没有浏览器在场，OIDC 授权码流
走不通，机器令牌永远需要 PAT 形态。

因此未来接入 OIDC = 新增一个验 JWKS 签名的 JWT `Authenticator` 实现，
把 claims 映射成 `Principal{Kind: "human"}`，纯增量、不动既有路径；
scope 表继续服务 agent token。前提只有一个：**`Principal` 抽象现在就定
干净**，所以它是 PR 4 的组成部分而不是可选项。

## 七、不变量与测试

| 不变量 | 兑现 |
|--------|------|
| token 明文不落盘 | 只存 salted verifier（复用 `auth.Hash`），创建响应之外不出现 |
| token 泄露 ≠ 凭据泄露 | scope 表与 vault 完全正交；远程主机沦陷拿不到 vault 密钥（credential-vault.md I4） |
| 吊销即时生效 | 每请求查表，无缓存 |
| gate 不可绕过 | 三入口统一经 `capability.Admit` 判定，e2e 测试分别从 MCP / REST / WS 三路验证同一写操作被同一 gate 拦截 |

测试照 AGENTS.md 与 `docs/agents/testing.md`：三 OS CI、`t.TempDir()`、
过期/并发吊销用 poll-with-deadline 而非固定 sleep；gate 下沉涉及 session
生命周期竞争，合入前跑 `make test-stress STRESS_PKGS=./internal/session/`。

## 八、PR 拆分（本文件范围内）

执行顺序：`credential-vault.md` 的 PR 1–3（vault 核心、运行时注入、
sidecar）先落地；本文 PR 0 → 4 → 5 依次跟随。PR 0 同时是 sidecar 注入
面的安全前提（注入只许人面 token 调用）。

- **PR 0 · 双 token**：`--mcp-auth-token`，MCP 面与 REST 管理面分离，
  兼容单 token 旧行为；
- **PR 4 · scoped token**：tokens 表 + 签发/吊销 CLI 与 REST +
  `Authenticator`/`Principal` 抽象 + 统一 scope 检查；
- **PR 5 · gate 下沉 + 策略接口**：拦截点移到操作层，`Policy` 接口 +
  默认人工审批实现，`session_start` 纳入审批，三路入口 e2e。
