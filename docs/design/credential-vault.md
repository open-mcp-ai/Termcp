# 连接凭据 Vault：age recipients 加密 + 运行时注入 + sidecar 分发

> 对应 issue #105「安全加固：连接凭据加密 Vault + 多端同步 + 面向 Agent/Harness 的作用域授权」。
> 本文覆盖该 issue 的 PR 1–3（vault 核心、凭据运行时注入、sidecar 注入流）；
> token 分权与审批网关见 `scoped-tokens.md`。
> 已对齐协作者评论（windworst）：本地 ssh_configs 文件可关可加密、sidecar 走
> 临时主机 API 注入、termcp 不做策略过滤逻辑。
>
> 执行顺序（2026-10 修订）：本文 PR 1–3 **先落地**；scoped-tokens.md 的
> PR 0/4/5 后置。sidecar 注入面在 PR 0（双 token）落地前的过渡风险见 §六。
>
> 实现状态（2026-10-19）：PR 1–3 已实现——`internal/vault`（age recipients
> + keychain 自动解锁链）、`sshconfig.Store` vault 后端（含导出收紧）、
> `termcp vault` CLI 与 `/api/vault` 端点、临时 profile 的 TTL 与会话绑定。
> §六 表格中"需要补的"两行的机制已落地；注入通道的人/MCP 分离仍待
> scoped-tokens.md 的 PR 0。

## 一、结论先行

1. **加密方案用 `filippo.io/age`，不自研信封加密，不用 SOPS。**
   issue 设想的 recipients 模型（每条 secret 用一次性文件密钥、文件密钥分别
   封送给各设备公钥、新设备加入 = 重封、无主密码传输）就是 age 的原生模型，
   且有审计过的实现。SOPS 是文档导向（整文件加密 + 云 KMS 依赖），会把云厂
   SDK 拖进依赖树，违背单二进制、纯 Go、~10 MB 的分发定位。
2. **身份私钥不进文件系统明文，也不引入 cgo。** 通过 shell out 到 OS 原生
   凭据 CLI 集成 keychain（macOS `security` / Linux `secret-tool` /
   Windows `cmdkey`），保持 `CGO_ENABLED=0`；go-keyring、99designs/keyring
   在 macOS 走 cgo，不可用。兜底为 passphrase 加密的 age identity 文件
   （age 自带 scrypt 格式）。
3. **远程实例（零信任主机）零持久秘密。** vault 只在本地可信端；远程实例
   的凭据由本地 sidecar 经已认证 REST 通过**临时主机 API**（已有的
   `ImportBatch(temporary=true)` 内存 profile 路径）注入，仅存内存、绑定
   会话、带 TTL 自动清除。
4. **termcp 不做策略判断。** vault 只提供"凭据从密文到 SSH 握手"的受控
   通道；谁能用、何时用，由 token scope 与审批网关决定（`scoped-tokens.md`）。

## 二、威胁模型与不变量

零信任远程主机：任何远程主机（包括部署了 termcp 实例供 Agent 调用的主机）
都不可信。由此固定四条不变量，所有设计取舍先过这四条：

| # | 不变量 | 兑现机制 |
|---|--------|----------|
| I1 | 凭据在任何持久化与同步环节都是密文 | age 密文落盘；同步内容 = 密文 + recipients 表 |
| I2 | 解锁私钥永不离开其所在设备 | 每设备一对 X25519 identity，私钥存 OS keychain |
| I3 | Agent / harness 永远拿不到凭据或密钥 | `session_start` 只接受 profile 名；凭据唯一出口是 SSH 握手 |
| I4 | 远程主机完全沦陷时，损失上限 = 内存中存活期的临时凭据 + scoped token | 临时 profile 内存驻留 + TTL；token 与 vault 密钥正交（见 scoped-tokens.md） |

反过来说，本设计**明确不防**：本地可信端本身沦陷（keychain 已解锁、内存
被读）。那是设备安全的范畴，不是本系统能解决的。

## 三、存储格式

```
data-dir/
├── vault/
│   ├── identity.age            # 本设备私钥（keychain 不可用时，scrypt+passphrase 加密）
│   ├── recipients.toml         # 已知设备公钥表：name, age1... 公钥, 加入时间, 备注
│   └── ssh_configs/
│       └── <name>.age          # 每个 profile 一个 age 密文文件，明文是同形 TOML
└── ssh_configs/                # 旧版明文 TOML（迁移后应清空；无 vault 配置的部署不变）
```

- **每 profile 一个密文文件**：同步粒度、吊销粒度、冲突合并粒度都是单
  profile，避免整库加密带来的"全量重封"和合并困难。
- **recipients 表与密文并列**，同步时一起走；任一持有私钥的设备都可把文件
  密钥重封给新设备公钥（age 的多 recipient 解密语义天然支持）。
- 非秘密元数据（host、port、user、description、jump 链结构）**可以**保留
  明文索引文件以支持不解锁列出 profile 列表；但只有 `password` /
  `private_key` / `key_passphrase` 三个字段（`internal/sshconfig/config.go`
  L26–28 的 `DialSpec`）必须且仅需进密文。初版更简单：**整个 profile TOML
  加密**，列表只显示文件名（profile 名），不引入双轨格式。
- **范围边界**：vault 只管连接凭据。会话输出历史（`internal/storage` 的
  逐 shell 落盘日志）仍是明文，属 #108 前两项（OS 钥匙串 / 透明加密磁盘）
  的威胁模型，不在本文范围——不要把"加密所有落盘数据"混进本 PR。

## 四、身份私钥的存放与解锁链

解锁链按优先级降级，全部走 OS 原生 CLI，不引入 cgo：

1. **macOS**：`security find-generic-password` / `-add-generic-password`
   （login keychain，解锁由系统弹窗或 unlock 状态托管）；
2. **Linux**：`secret-tool`（Secret Service / gnome-keyring、KWallet）；
   headless 环境直接跳到 4；
3. **Windows**：`cmdkey` 只能存取不能读回明文以外的结构化数据，实际用
   PowerShell 调 DPAPI `ProtectedData`（用户作用域，无需管理员）；
4. **兜底**：`vault/identity.age`，age 原生 scrypt passphrase 加密，权限
   0600，解锁时交互输入（经 Web UI 的密码通道或 CLI prompt，**绝不进
   `shell_input`**，防止落进 `shell_output` 历史）。

keychain 不可用且无 passphrase 时，vault 保持锁定，仅消耗凭据的操作
（`session_start`、连接测试）失败并给出解锁指引；只读操作（列 profile
名）不受影响。

## 五、凭据运行时注入：`CredentialProvider`

现状明文链路：`Store.Load`（`store.go`，temporary → 磁盘的解析顺序）→
`RemoteFromEntry` / `dialSpecToRemote`（`config.go`）——**明文在这里跨进
`session.RemoteSSH`**——→ `buildChainClient`（`session/dial.go`）→
`sshclient.BuildClientConfig`（`internal/sshclient/config.go`），在那里构造
`ssh.Password` / `ssh.PublicKeys`。Resolve 的注入点定在 `dialSpecToRemote`
之前：Entry 出 vault 后秘密字段以引用形态存在，dial 前一次性解出，明文不
出 `buildChainClient` 调用栈。凭据经接口取得：

```go
// internal/sshconfig 或独立的 internal/vault 提供实现。
type CredentialProvider interface {
    // Resolve 返回 dial 用的三个秘密字段；vault 锁定时返回 ErrLocked。
    // 返回的明文只允许存在于这次 dial 的调用栈内。
    Resolve(profileName string) (*Credentials, error)
}
```

- `sshclient` 不再感知 vault；session 创建路径（`internal/session` 的
  Create）在 dial 前一次性 Resolve，结果喂给现有 `DialSpec` 构造逻辑，
  dial 返回后即弃（Go 无法保证内存清零，尽力而为：`runtime.KeepAlive`
  边界内缩短明文生命周期即可，不承诺 zeroize）。
- **临时 profile 优先**：Store 已有的 `temporary map[string][]byte`
  （`store.go` L29，进程内存、不落盘）就是 sidecar 注入的落点。Resolve
  顺序：temporary → vault → 旧明文（迁移期只读回退）。
- `ssh_config` 的 MCP/REST 读接口行为不变：只返回 profile 名与"已配置"
  标记，永不返回秘密字段——README 安全模型已对外承诺此语义，vault 化后
  只是让存储层配得上这个承诺。
- **导出收紧**：REST 批量导出（`Store.ExportBatch`）现状经 `ReadRaw`
  返回原文 TOML，是持 token 者的整库凭据出口（scoped-tokens.md P4）。
  vault 化 profile 的导出只出密文，或强制 `X-Termcp-Password` 二次加密；
  明文导出仅对未迁移的旧明文 profile 保留。

## 六、sidecar 注入流（远程零信任部署）

```
本地可信端（持 vault + 私钥）                远程 termcp 实例（无 vault）
┌──────────────────────┐                  ┌──────────────────────────┐
│ sidecar 进程/脚本      │                  │ --no-internal             │
│  1. 解锁 vault         │                  │ 人/MCP 分离 token（见      │
│  2. 解密目标 profile   │                  │   scoped-tokens.md）      │
│  3. POST /api/connections/batch           │                           │
│     ?temporary=true    │ ── HTTPS+token ─>│ 4. 内存 profile（TTL，绑定 │
│     （明文只在 TLS 内）  │                  │    会话，过期/断开即清）    │
│                        │                  │ 5. session_start(引用名)   │
└──────────────────────┘                  └──────────────────────────┘
```

现有差距与补法：

| 现状 | 需要补的 |
|------|----------|
| `ImportBatch(temporary=true)` 已是纯内存、不落盘（`batch.go`） | 无 TTL、无会话绑定，进程活着就一直在 |
| 临时 profile 永久驻留内存 | 加 `expires_at` 与 `bound_session`：到期或会话 DEAD 即清除；清除走 session 已有的 `ResourceScope`（`internal/session/scope.go`），创建点挂载、`finalize` 级联释放 |
| 注入通道只靠单一全局 token | 依赖 scoped-tokens 的 PR 0/4：注入用"人"面 token，Agent 的 MCP token 无权调用注入 API |
| 本文 PR 1–3 先于 PR 0 落地 | 过渡期注入 API 仍由单一全局 token 守护：靠 temporary 不落盘 + TTL + 会话 DEAD 即清兜住 I4 的大部分；部署文档写明此过渡语义，并建议过渡期注入面只绑 loopback |

**安全语义**：明文只在 TLS 通道内和远程实例内存中出现，且远程实例拿到的
是**单个 profile 的单份凭据**、有 TTL——即使远程主机沦陷，攻击者拿不到
vault、拿不到其他 profile、拿到的凭据会过期。这满足不变量 I4。

**长期方向（本文不实现，仅预留接口）**：本地维护 SSH CA（CA 私钥进 vault），
目标主机配 `TrustedUserCAKeys`；远程实例每次会话生成临时密钥对，本地
sidecar 签分钟级证书下发。静态凭据整体消失，`CredentialProvider` 接口
不变、多一种实现（返回 `ssh.Signer` + 证书）。

## 七、迁移与兼容

- `termcp vault init`：生成设备 identity（进 keychain 或 identity.age），
  初始化 recipients.toml；
- `termcp vault migrate`：扫描 `ssh_configs/*.toml`，逐条加密进 vault，
  校验通过后 scrub 明文文件（先写 `.age`、重新解密比对、再删原文）；
- `termcp vault unlock / lock / recipients add <pubkey>`：解锁状态管理与
  新设备授权；
- 无 vault 目录的现有部署行为完全不变（明文 TOML 路径保留为只读回退，
  新写入强制走 vault）；
- 检测到明文 TOML 与 vault 并存时，以 vault 为准，明文侧在每次启动时
  打印一次迁移提醒。

## 八、测试要求（照 AGENTS.md / docs/agents/testing.md）

- 三 OS CI：keychain CLI 不存在时必须干净降级到 identity.age 路径，测试
  用环境变量强制降级，不依赖 runner 上有没有 secret service；
- 不硬编码共享路径，`t.TempDir()` 起 data-dir；
- 迁移测试必须验证"scrub 后磁盘上无明文残留"（读回原始文件字节断言）；
- sidecar 注入测试走 in-process HTTP server + temporary import，验证 TTL
  到期与 session DEAD 两种清除路径（`make test-stress` 覆盖生命周期竞争）。

## 九、PR 拆分（本文件范围内）

1. **PR 1 · vault 核心**：`internal/vault`（age 封装、recipients 管理、
   keychain CLI 链）、`termcp vault init/migrate/unlock`、`sshconfig.Store`
   接 vault 后端，读接口永不返回明文；
2. **PR 2 · 运行时注入**：`CredentialProvider` 接口落进 session 创建路径，
   明文 TOML 转只读回退；
3. **PR 3 · sidecar 注入流**：temporary profile 的 TTL + 会话绑定 +
   ResourceScope 级联清除，sidecar 参考脚本与部署文档（`docs/deploy.md`
   增补零信任远程实例章节）。
