# 权限组、认证生命周期与交付遗漏：检查清单及方案

**2026-11-05 范围更新**：本文件中的 86 项目录是历史审计输入，不是当前实现目标。权限动作和预置组以 [#252](https://github.com/open-mcp-ai/Termcp/issues/252) 修正后的 46 项清单为准；认证范围以 [认证与权限范围收敛](auth-scope-2026-11-05.md) 为准。

日期：2026-11-02。状态：第 1–9 节保留检查时的快照；本分支实施记录见第 10–17 节，23 项在当前工作区全部完成，尚未合并。

分支跟踪 issue：[#232](https://github.com/open-mcp-ai/Termcp/issues/232)。

检查基线为 `daily/2026-10-28/beep` 的 `2ef2baf`，包含工作区尚未提交的 V2 权限适配。不是远端 dev 的全量审计。本文结合当前源码、现有测试、[Agent 权限 V2](agent-permissions-v2.md)、[#228](https://github.com/open-mcp-ai/Termcp/issues/228)、[#196](https://github.com/open-mcp-ai/Termcp/issues/196)、[#197](https://github.com/open-mcp-ai/Termcp/issues/197) 及相关 RFC 核验。

本轮按用户要求排除 #228 第 2 项 jrpc/SSE 传输问题；不修改其 baseline。会话加密、配置同步、Agent cookie/子 token 签发继续不在本轮实现范围。已有主机配置独立存储和加密方式保持现有合同。

## 1. 需要先明确的产品结论

1. **管理员浏览器登录仅在本次服务运行中有效。** 服务重启后，旧 cookie 必须被拒绝；自动解锁配置不代表管理员已经登录。持久 MCP 凭据与浏览器登录分开处理。
2. **权限管理主实体改为“权限组”。** 页面管理可复用的动作、参数与主机策略，同时管理关联的成员和凭据；主体身份、权限组、凭据在数据上分开。
3. **权限组需要完整生命周期。** 新建、复制、重命名、编辑、停用、删除、成员迁移、缺省组切换都必须有后端合同。改中文标签不能替代这些操作。
4. **使用少量可解释的预置角色模板。** 不默认给 Agent 管理员权限；不要把可以执行任意终端命令的角色称为“安全维护”或“只读”。
5. **先修权限扩张与交付阻断，再改模型。** 空白名单持久化、Windows 锁文件失败、认证生命周期、KDF 持锁、组迁移和撤权一致性排在高级条件及新审批流程之前。

## 2. 已核实的问题与待办

“源码确认”表示已核对实际实现；“待补竞态验证”表示实现缺口可定位，但本轮没有声称已运行攻击或故障复现。P0 为当前合并阻断，P1 为下一轮必须完成，P2 为其后的完善项。

| ID | 优先级 / 依据 | 现状及影响 | 修复/设计任务 | 验收要点 |
| --- | --- | --- | --- | --- |
| S00 | P0 / 临时回归实测 | `keys/tool_names/methods/forms` 用 `omitempty`，会把显式空集合 `[]` 序列化成缺字段；判定却用 nil 表示不限制。四类限制在 JSON 往返后均从拒绝变为允许，HTTP 返回/再保存或重启都可能触发。这是上轮 V2 适配遗漏。`authorization.go:36,122` | 保存/读取时保留 unset、空集合、非空集合三种语义；同步 UI、clone 与持久化合同。 | 四类字段及真实 API/重启往返均不扩权；不得只改 UI 隐藏空集合。 |
| W01 | P0 / Win11 实测 | `assertTreeOmits` 读取被 Windows 排他锁保护的 `.config.lock`；首个 repo 在失败前没有注册清理，随后 TempDir 清理也失败。`internal/configstore/store_test.go:15,24,266`、`lock_windows.go:11` | 扫描只跳过已知锁文件；取得 repo 后立即注册 Close，检查同类测试的失败路径。 | Windows 原失败用例通过，失败注入后也释放句柄；不扩大为跳过所有隐藏文件。 |
| A01 | P1 / 源码及现有测试确认 | 签名 key 和 Sessions 都持久化；解锁后会恢复旧 cookie。`managed.go:161,202`、`managed_storage.go:148`；`managed_test.go:143` 与 `managed_encryption_test.go:245` 还要求旧 cookie 恢复。 | 引入本次服务运行的 WebSession 生命周期，修改代码、测试和“记住解锁”的说明。 | 自动解锁、手动解锁、迁移、崩溃重启后旧 cookie 均不可重新生效；新登录可用，MCP token 按独立合同处理。 |
| A02 | P1 / 源码及现有测试确认 | `POST /auth/agents` 不传 policy 会留下 nil；`effectivePolicy(nil)` 返回全功能，并且省略 issue_token 会直接签发凭据。UI 新建却是空 V2。`managed_tokens.go:256`、`agent_policy.go:135`、`agent_policy_test.go:70` | 新建统一为明确的空 V2/缺省权限组；legacy_full 只服务于已存在记录的迁移。 | API、UI、CLI 新建结果一致；缺字段不产生全权身份；旧全权记录不被静默降级或伪装成新默认。 |
| U01 | P1 / 模板及路由确认 | 登录/设置/账号共享 `login.html`，权限页用 `permissions.html`；两者没有主题资源，使用独立颜色变量及系统 dark media query。`managed_http.go:251`、两个 HTML 的 head。 | 认证页面与权限页接入共同主题 shell 和主题语义 tokens。 | 四个内置主题及 auto 一致；从工作区进入后保持选择，无错误配色闪屏。 |
| U02 | P1 / CSP、静态资源规则确认 | 当前认证页 CSP 不允许同源外部样式/脚本/字体；未登录请求主题资源也不会通过仅放行 `/api.md` 的公开规则。单加 CSS link 仍不能工作。`managed_http.go:259`、`auth.go:184` | 配套调整 CSP、登录前的安全静态资源白名单、资源 fallback；保持账号/API 数据需要鉴权。 | 真正退出登录后仍能加载主题；浏览器无 CSP 违规或主题请求跳转登录；没有匿名管理 API 被放开。 |
| G01 | P1 / 数据模型确认 | `agentRecord` 同时保存身份和策略；token 的 PrincipalID 指向它。当前没有独立权限组。`managed_tokens.go:21,30,117` | 拆出 PermissionGroup、主体绑定与 PolicyResolver；UI 主列表改为权限组。 | 两个主体能复用同一组；改组名不改主体 ID、namespace 或 transport owner。 |
| G02 | P1 / 管理路由确认 | 现有记录只有创建、策略修改、停用；没有记录删除、重命名、复制、组成员/绑定操作。`managed_tokens.go:256,312,353` | 实现组 CRUD、复制、成员绑定及缺省组管理；使用 revision 处理并发。 | 前后端都能完成操作；无 UI 按钮对应假接口；重复/无效名称和陈旧 revision 有明确错误。 |
| G03 | P1 / 新增生命周期要求 | 当前 token 撤销/停用逻辑只处理一个 principal，不能直接代表组删除或一对多策略变化。`managed_tokens.go:186,365` | 组更新、删除、成员迁移在一次持久提交中完成，并使所有受影响凭据的在途请求失效。 | 删除后不残留有效孤儿 token；存储失败不出现“组已删、绑定未迁”；故障/重启不复活已撤权限。 |
| G04 | P1 / 身份和资源关联确认 | namespace 由 Actor.ID 派生，HTTP/SSE transport owner 又绑定主体与凭据。`namespace.go:34`、`auth_transport.go:20` | 迁移保留既有 Actor.ID、credential ID/digest、namespace 归属；组名和新组 ID 不参与冒充主体身份。 | 同组仍不能接管另一凭据的 MCP transport；是否共享业务资源由明确的资源合同决定。 |
| G05 | P1 / 目录及页面确认 | 13 个功能族、86 个动作均不是角色预置。页面只有全选与逐项编辑。 | 提供第 4 节的版本化模板，初次初始化实际权限组；支持从模板复制。 | 模板有准确说明和明确动作集合；升级不会给已有组自动追加动作。 |
| S01 | P1 / 调用链确认，待补竞态验证 | 主机 Identity/扩展 Target 的授权查询与执行时 Load/Acquire 是分开的；尚无绑定对象版本的执行计划。`agent_access_v2.go:59,96` → `handlers.go:455` / `handlers_extension.go:223` | 计划绑定规范 host UUID、配置/模板版本和已解析目标；执行使用绑定目标，版本变化则重建并复核。 | 在检查与执行之间改名、删除重建、替换扩展 target，不会转而操作未授权主机。 |
| S02 | P1 / 多层调用确认 | KDF 在 Store.mu 和 Repository.mu 内，登录时还持有 Managed.mu；不只是 UnlockEncryption 一个函数。`sshconfig/encryption.go:145`、`authentication.go:71`、`configstore/password.go:302`、`configstore/authentication.go:78`、`auth/managed.go:161` | 采用快照/锁外派生/版本复核后提交；连同初始化、验密、改密与自动解锁 provider 路径审查。 | KDF 被暂停时既有认证、状态读取不被业务全局锁堵住；并发 Close/改密/写配置不会提交过期 key。 |
| S03 | P1 / 展示调用确认 | 主机和扩展 getter 已脱敏，但 MCP/CDP 的 peer 列表与详情仍直接格式化 `ch.URL()`；URL 含 userinfo/query 凭据时会回显。`term_ls.go:230`、`term_generated.go:2116`、`term_cdp.go:375` | 展示层使用安全 endpoint 投影，连接层保留原始 URL；覆盖列表、详情、错误提示和日志预览。 | 使用合成 userinfo/query 凭据检查，无值出现在摘要/错误/日志；实际连接不受展示脱敏影响。此项不是 #228 的 jrpc 传输修补。 |
| P01 | P1 / 源码确认 | 初版高级条件尚未实施：字段、扩展名称、动作主机子集、转发目的限制等。不能据此宣称有受限发布或受限工具角色。 | 当前预置仅使用可证明的条件；高权限动作默认关闭；为高级受控角色补完所需条件后再开放模板。 | 不接收后忽略条件；无对应执行约束的控件不展示为已支持。 |
| P02 | P2 / parser 与 Adapter 确认 | catalog 可配置 file.read max_bytes，但普通 `read file` 没有 length 参数，不能为它提供有界读取证据；部分 local_path/分支归类还基于原始字段。`ops_detail.go:157`、`agent_plan.go:48`、`handlers_file.go:27` | 统一有效参数解析；明确并接通有界文件读取形式；不能接通的条件在 UI 标明不可用于该词面。 | 配置后既有允许用例能成功，超限/未知长度能拒绝；省略、默认值和等价词面一致。 |
| P03 | P2 / 目录及页面确认 | catalog 还没有充分表达 DSL/内部工具可达性、参数枚举和组合依赖，设置页只显示简单组合提示。`authorization.go:176`、`permissions.html` | 补充目录元数据、组合缺项解释和只做判定的 DSL 预览。 | 输入 run 能解释缺 Enter/output，并证明预览不建连、不续租、不启动 task。 |
| P04 | P2 / V1 运行路径确认 | `legacyOperations` 冻结的是升级候选；V1 运行时依然用功能组与 action 分类。`agent_policy_upgrade.go:9`、`mcp/agent_access.go:90` | 冻结旧策略真正可达的操作/参数类别；单独验证升级差异。 | 新加语义分支不会因旧组曾勾选而自动开放；V1/新组迁移均无静默扩权。 |
| O01 | P1 / 创建路径确认 | 后台 upload/download/copy 使用 Background context，已启动任务不会随 token 撤销自然结束。`handlers_file.go:54,184`、`copy_stream.go:83` | 明确组停用/删除时在途任务的合同；建议停止受影响主体的活动传输，保留回执，不承诺回滚已写字节。 | 请求取消与后台任务取消分别测试；正常“请求结束但任务继续”语义保留；不能声称断 token 已撤回远端副作用。 |
| O02 | P2 / auth 模块检查 | 工具日志不能替代组管理审计；没有覆盖组变更、绑定、删除、凭据撤销的结构化事件。 | 增加操作者、目标 ID、前后 revision、结果、影响数量；权限拒绝记录 action 与理由，不记录秘密值。 | 可追踪谁改变了谁的权限；密码、token、Cookie、URL 凭据不进入审计。 |
| D01 | P2 / git 跟踪集实测 | 11 个 CSV 共 20,960 行、7,395,908 字节；call-edges 单文件 11,644 行、5,717,982 字节。 | 大型派生结果改为生成到已忽略的 dist/audits，保留摘要、生成命令和版本；逐个判断小型人工清单。 | PR 不再携带整图 CSV；文档链接有效；有可重复生成步骤。现有历史不在此任务中重写。 |
| Q01 | P1 / 测试合同缺口 | macOS 全量通过不能覆盖 Windows 锁语义；Go 测试和临时 UI 示例也不证明真实主题接线。 | 增加第 8 节的真实入口、平台、浏览器及故障矩阵，并作为交付条件。 | 原问题有对应回归用例；明确实际运行的平台和未运行项。 |
| D02 | P2 / 文档对照确认 | cookie 跨重启保留、逐 Agent 页面、新权限组及 #214 的凭据撤销目标之间存在合同差异。 | 更新当前实现文档与迁移说明；旧设计标明替代关系，避免继续指导后续实现。 | API 文档、UI 文案、预置角色、测试与实际行为一致。 |

## 3. 管理员 cookie：采用运行期会话

当前的恢复链为：`OpenManaged/loadLocked` 读入持久 Sessions → `verify` 用持久 SigningKey 验签并查 Sessions。重启时如果 vault 尚未解锁，旧 cookie 暂时不可用；随后正确口令解锁或自动解锁便能恢复。仅在启动时清空一次 map 不够，后续 loadLocked 仍可能重新载入旧记录。

推荐方案：

- 在每次 `OpenManaged` 创建新的内存 `web_boot_id`，不持久化；JWT 的 Web claims 绑定该 ID，verify 必须核对。
- 浏览器会话记录移到运行期 map；持久认证状态只保留账号、必要签名材料、组/绑定和 MCP 凭据等。迁移时忽略旧的 Web Sessions，不能再恢复。
- 登录、退出、改密撤销和请求跟踪使用同一 WebSessionStore。修改持久策略不能用旧快照覆盖运行期会话。
- 旧 cookie 的下一次请求返回未登录并清理浏览器 cookie；服务离线时不能主动删除浏览器保存的字节，验收依据是身份已失效。
- “自动解锁”只解锁 vault。所有页面在重启后仍要求管理员重新登录；MCP token 是否有效按自身状态判断，不能因轮换 Web boot ID 一并误撤。
- 本次新要求替代早先“重启保留有效登录”的实现约定；两个明确断言旧 cookie 恢复的测试必须同步反转。

## 4. 权限组、身份、角色模板

### 4.1 企业方案的借鉴

这里采用三项已被成熟系统使用的设计原则：

- [NIST RBAC](https://csrc.nist.gov/Projects/role-based-access-control/faqs)：身份、角色和权限分开建模，借助角色分配组织权限。
- [Azure 内置角色](https://learn.microsoft.com/en-us/azure/role-based-access-control/built-in-roles/privileged)：资源操作权限与授权管理职责分开。
- [Kubernetes RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#user-facing-roles)：权限定义和主体绑定分离；查看元数据与读取凭据有区别，运行工作负载等能力也可能扩大实际可操作范围。

以下角色是针对 Termcp 的建议，不代表本项目已经实现完整的上述企业产品模型。

### 4.2 建议预置

“角色模板”是创建权限组时可选的动作集合；实际权限组持有自己的策略快照、主机范围、成员和 revision。模板本身不携带 token，不通过父项通配符授予未来动作。

| 角色模板 | 允许范围 | 默认关闭 | 主要用途 |
| --- | --- | --- | --- |
| 未授权 / No access | 静态帮助；业务动作空集合 | 全部业务动作 | 初始缺省组；新身份尚未获人授权。 |
| 资源观察 / Viewer | 主机摘要、会话/通道状态、扩展摘要列表、任务进度；使用安全元数据投影 | 文件内容、终端输出、输入、执行、配置修改、网络转发、原始协议 | 查看资源是否存在和运行。 |
| 只读诊断 / Diagnostics reader | Viewer，加本机 shell 检测、终端 output/marks 和文件读取/属性/路径查询 | 输入/Enter/exec、文件修改、reader 管理、启动/重建扩展、配置变更 | 诊断人工已分配的可达资源；`shell.detect` 只检查 Termcp 本机，不探测远端主机。 |
| 文件维护 / File maintainer | 只读诊断，加 inline write/replace/mkdir | delete/rename/truncate/chmod/chown/chtimes/link、终端执行、代理、转发、配置定义 | 更新文件内容；不是进程沙箱，修改被其他程序执行的文件仍可能产生后续影响。 |
| 主机运维 / Host operator | 诊断与文件维护，加建连、通道、显式程序、input/key/resize、reader、常用文件写删/属性、转发、已配置扩展、任务取消、通知、`alias.list` | 会话永久删除、主机/扩展定义、凭据、组与 token 管理、别名写删、raw/MCP/CDP/JSON-RPC 任意调用 | 操作已授权主机。终端执行具有目标 OS 账号的实际能力，不能靠关闭 file.delete 阻止 shell rm。 |

系统管理员 / Owner 保留为受保护的人端身份，管理账号、权限组、凭据和配置；**不能把 Agent 加入某个名为 Owner 的组就变成 RoleAdmin**。权限管理员、审计员等人端受限角色，需先完成相应管理动作门禁与审计数据，后续再提供；本轮不展示可选但实际仍拥有全部管理员权限的假角色。

默认主机范围按此前约定使用 all，清楚标明包括以后新增的已保存主机；动作权限仍由所选模板决定。创建组不自动绑定成员或签发 token。已存在记录迁移时保留其原 host_mode/hosts，不套用新默认扩权。

建议的明确动作集合：

- Viewer：`profile.list/get`、`session.list/info`、`shell.list`、`ext.list`、`task.list/info`。此处斜线表示分别存储对应明确 ID。
- Diagnostics reader 增加：`shell.output/marks/detect`、`file.read/stat/realpath/readlink/getwd/statvfs/urls`。
- File maintainer 增加：`file.write/replace/mkdir`；read/write forms 为 inline，replace 为 inline。
- Host operator 增加：`session.start/terminate`、完整终端操作、常用文件与转发操作、`ext.get/use/release/retry`、`task.cancel`、`user.notify`、`alias.list`。
- 未列出的所有动作均拒绝。集合必须展开为当前目录中的 ID 后持久化，不保存这些缩写或通配符。

只读组不能自行建连这一点要在界面说明。它需要可达会话或扩展资源；当前 namespace 上限仍有效。不能为了让“只读”组看起来能工作而隐式加上启动程序或改成共享管理员会话。资源分配与组内是否共享会话，需要独立明确合同。

### 4.3 最小数据合同

```text
Principal:       稳定 ID、kind(human/agent)、group_id、enabled、已有 namespace 归属
PermissionGroup: 稳定 ID、name、description、enabled、revision、policy（过渡期 V1/V2）、template_id/version
Credential:      稳定 ID、principal_id、digest、label、created_at
DefaultBinding:  当前缺省权限组 ID + revision
```

本轮每个主体只激活一个权限组，未单独绑定时走缺省组；不新增逐 Agent 动作覆盖或继承树。同组复用策略。多组组合、子用户词汇及凭据父子链留给 #196/#197，不能把未知组、缺失绑定或缺失 resolver 当作 legacy_full。

当前多个 token 共用旧 agentRecord 身份的行为需要兼容。迁移可先为每个旧记录生成对应组与兼容主体，保留旧主体 ID 和 namespace，原 token 不重签。旧 V1 在过渡组中保留原模式，通过升级预览明确保存后再进入 V2；数据结构迁移不等于策略升级。未来真实 Agent 身份接入后再绑定到组；此时也不把组 ID 用作 transport 的调用者 ID。

## 5. 页面与管理接口

左侧改为“权限组”：搜索、名称、启停、成员/凭据数量。右侧提供“权限”“成员与凭据”；预置模板作为新建/复制的来源。系统管理员账号单列，避免和普通 Agent 组混成一行。

建议的管理合同：

| 操作 | 建议接口 | 必要规则 |
| --- | --- | --- |
| 列出/创建组 | `GET/POST /auth/groups` | 管理员会话；创建明确 V2，不能 nil→全权。 |
| 重命名/编辑/启停 | `PATCH /auth/groups/{id}` | expected revision；名称不改变稳定 ID。 |
| 复制组 | `POST /auth/groups/{id}/clone` | 新 ID、新名称，无成员、无 token；复制当前明确策略。 |
| 删除预览 | `POST /auth/groups/{id}/delete-preview` | 列出成员、凭据、活动请求/任务、缺省引用，不改变状态。 |
| 删除组 | `DELETE /auth/groups/{id}` | 绑定影响确认、当前 revision/计划版本；原子处理迁移/停用与删除。 |
| 绑定成员 | `PUT /auth/principals/{id}/group` | 仅人端管理，不能由 Agent 自建组并加入。 |
| 设置缺省组 | `PUT /auth/default-group` | revision；明确影响未显式绑定主体，不因删组自动回退为全权。 |
| 列出预置 | `GET /auth/group-presets` | 明确 ID、版本、动作、条件和限制说明。 |
| 权限预览 | `POST /auth/access/preview` | 模拟指定 Agent/组，不接受客户端自报 admin；无执行副作用。 |

所有新增路由明确注册在 Managed.Wrap 的管理员、Origin、CSRF 校验内，不落入静态文件 fallback。旧 `/auth/agents` API 应有兼容/弃用策略：仍指向旧主体，不能突然把 agent_id 参数解释成另一种组绑定。

删除规则：

1. 无成员、无缺省引用的普通组可以删除。
2. 有成员时，预览明确选择“迁移到指定组”或“解除绑定并拒绝访问”；不能静默套用一个更宽的缺省组。
3. 删除缺省组必须同时提供新缺省组，或先切换缺省引用；永久的无权兜底和最后一个人端管理员不允许删除。
4. 保存成功后收回所有受影响 token 的活动请求；后台任务按 O01 单独停止。回执与历史资源不随组删除自动销毁。
5. 预览后成员、组策略、缺省引用发生变化，应冲突并重新预览。不能只校验组名。
6. 模板目录不等于实际组。用户创建/初始化出来的组可按以上规则编辑和删除；升级不自动重建用户已删除的组。

## 6. 关键修复的实现切口

### 空集合条件的无损持久化

缺字段/null 表示未限制，`[]` 表示没有任何值被允许，非空数组表示仅允许所列值。当前判定模型已依赖这三种语义，但 `omitempty` 会合并前两类存储形式。

例如，明确的空工具白名单目前会丢失为无字段对象：

```json
{"effect":"allow","constraints":{"tool_names":[]}}
```

经过当前 JSON 编码变成 `{"effect":"allow","constraints":{}}`，重新加载后该动作不再检查工具名。不能通过刷新页面或重启获得更宽的权限。

最小候选是四个集合字段不使用 `omitempty`，使 nil 编码为 null、空切片编码为 []；也可用经过同等测试的自定义编码保持简洁格式。不要改变为“空集合等于不限制”，因为 UI 已明确承诺 [] 的含义。必须覆盖 HTTP 响应、数据库保存、重新加载及 UI 再编辑，而不只验证内存中的 CheckPlan。

已经丢失为空 `constraints:{}` 的旧记录无法仅凭现有密文恢复原来是空白名单还是无约束。建议将这类含义不明的 allow 标记为需人复核，相关条目暂按拒绝处理；原来明确省略 constraints 的无条件 allow 保持原意。管理员可明确重存无约束 allow 或空集合限制。新写入拒绝含义模糊的空条件对象，并在升级说明列出这项收紧。

### KDF

使用已有 loginGate 约束昂贵操作并发，不把它当作避免阻塞的证明：当前 loginWithOptions 仍持有 Managed.mu。建议流程是：短锁取得仓库引用、keyring/catalog/auth generation 与账号版本 → 解锁后派生/认证候选 key → 短锁检查 Close/改密/配置版本未变化 → 原子提交候选或销毁后拒绝。

Repository 和 Store 都要遵循同一流程；只解开 Store.mu，仍会被 Repository.mu 阻塞。不得通过删除锁引入并发 Close、旧密码候选提交、密钥清零或 lost update 问题。已有 notifyChange 在锁外调用的约定继续保留。双重 UnlockPassword→VerifyPassword 的重复派生也应一并梳理，不改 Argon2 安全参数来掩盖阻塞。

### 主题

认证页面采用共享 shell 读取 `termcp.theme`、加载 theme-boot、主题 CSS 和字体；页面自身只保留布局及经过统一 tokens 的控件样式。不要直接引入整个工作区脚本去启动终端/WebSocket。

主题资源公开白名单、CSP 与页面渲染一起实施：允许必要的同源样式/脚本/字体，维持 nonce、frame-ancestors、base-uri、form-action 约束，不用放宽管理接口来解决资源 401。需要考虑登录前的主题清单、外部主题与主题不存在时的回退。共享模块避免 auth 与 webui 互相 import，可抽出纯页面/资源适配模块或由启动层注入。

## 7. #228 的逐项结论与合并安排

| #228 项目 | 当前结论 | 本轮处理 |
| --- | --- | --- |
| 1. Windows 测试 | 在现有 Win11 上已复现；隔离覆盖层中的最小测试修补已验证。 | W01，合并前必须修。 |
| 2. jrpc/SSE | 用户明确认定为 dev baseline 问题。 | 排除，不修补、不纳本轮阻断任务。 |
| 3. 锁内 Argon2 | 当前仍存在，且登录/验密/改密路径范围更大。 | S02，跨三层处理并增加可控并发测试。 |
| 4. 大型 CSV | 仍在 git 跟踪集，当前总量与 issue 的 20,960 行一致。 | D01，移出大型派生结果、修正链接与再生成方式。 |
| 5. 扩展原子发布 | `83612f9` 已是当前 HEAD 的祖先，`extState.takeDueExtras` 仍在同一锁内完成摘除、closed 信息及状态翻转。 | 保留已有修复；是否单独拆 PR 属于后续交付安排，不重复修写。 |

推荐拆成可独立评审的交付块：S00 空白名单语义 → W01 测试与清理 → A01/A02 认证合同 → S02 密码操作并发 → G01–G05 组模型/API/迁移 → U01/U02 页面主题及组界面 → S01/S03/P01–P04/O01/O02 运行一致性与可观测性 → D01/D02 文档和派生物收尾。D01 可与前面步骤独立并行，但本轮未开启并行代理或创建 PR。

## 8. 验证记录与后续验收矩阵

### 本轮实际执行

- macOS：Go 1.27.3，`go test ./internal/auth -run '^TestManagedFirstRunLoginRestartLogout$' -count=1` 通过。它证明当前代码按旧约定恢复 cookie，不是证明新要求已满足。
- Windows：PD 中运行 Win11 ARM64，系统版本 `10.0.26200.8246`；已安装 amd64 Go 1.25.9，但项目要求 Go 1.27。
- 在 macOS 用 Go 1.27.3 交叉编译 `windows/amd64`、`CGO_ENABLED=0` 的 configstore 测试程序，放到虚拟机本地临时目录运行；测试文件锁发生在 Windows 本地磁盘，不是共享目录。
- 原源码 `TestRepositoryRoundTripRenameAndDelete` 失败，表现为读取 `.config.lock` 的锁范围冲突，随后 TempDir 清理出现文件占用；退出码 1。
- 仅通过仓库外的 Go overlay 修改测试：跳过 `.config.lock`，并在首个 repo 创建后注册 Close。重新编译后，`-test.run=^TestRepository -test.count=1 -test.shuffle=on` 的 10 个顶层测试及其子测试通过，退出码 0。
- 该覆盖层没有写回工作区，不能称为 W01 已修复。没有升级虚拟机 Go，没有运行 Windows 全仓或 Windows race CI，不能把上述结果写成“三平台全绿”。
- macOS 临时 Go overlay 回归：对 keys、tool_names、methods、forms 分别构造合法的空集合 allow 条件，序列化/反序列化前后的判定全部从 false 变成 true，4 个子用例失败。该用例没有写入仓库，属于已复现的 S00 缺陷，不是 CI 本来就会检测到的问题。
- 同一临时覆盖层中，仅去掉四个集合字段的 omitempty 后，4 类往返探针及所选既有纯判定测试通过；这是候选修复验证，未写回实际源码。
- 模板静态检查：login.html、permissions.html 的外部 link/script 资源列表为空；工作区 index 则包含 theme CSS、theme-boot 和 theme.js。此处没有声称已做真实浏览器主题回归。
- 结构核验采用 codebase-memory Tier 2 与源码回读。相关 Go 文件未报索引缺口；base.css 的 149–150 行及 cyberpunk tokens.css 的 28 行存在解析缺口，已直接读取相应源码。此信号不证明全仓结构完整。

### 后续必须覆盖

| 范围 | 验收用例 |
| --- | --- |
| Web 会话 | 自动/手动解锁、正常/异常重启、首次迁移、logout、改密、旧 cookie 再提交；旧会话不能复活，MCP 凭据独立。 |
| 空白名单 | unset / null / [] / 非空列表，经 clone、GET、PATCH、密文重启后都保留语义；未授权的 key、工具、method 和 form 不因往返获准。 |
| 权限组 | CRUD/复制/改名/停用，带成员与缺省引用删除，迁移冲突、保存/持久性错误、重启恢复；组名/ID 不改变既有主体资源。 |
| 预置 | 每个角色至少一条允许与一条拒绝真实 MCP 请求；元数据观察不能读取终端/文件，运维有实际可用的组合权限；未来新动作默认禁止。 |
| 并发 | 以测试屏障暂停 KDF 或授权后的解析，验证其他请求进展、版本复核、Close/改密/组迁移；不靠“必须低于 100ms”这样的易抖阈值。 |
| 运行 | HTTP/SSE 两条入口，别名/等价词面、双端复制、旧 transport 收权、后台任务停止与回执保留、URL 合成秘密的回显检查。 |
| 浏览器 | 实际运行的完整服务；登录/设置/账号/权限组页，四主题+auto、主题缺失、退出后加载、CSRF/CSP、语言切换、桌面与窄屏、部署路径。 |
| 平台 | ubuntu、macOS、Windows 的 `go test ./... -count=1 -shuffle=on -race`；本地生命周期 stress；no_webui 与显式 legacy/open 认证模式的合同分别核验。 |

## 9. 关联 RFC 与本轮之外的保留项

- #196 的子用户 allow/reject 与专属词汇，#197 的多组、子凭据、父子撤销链继续经统一 PolicyResolver 接入；本轮不重新设计签发协议。
- #197 的 allow/deny/approval 现在映射为显式 `allow / deny / ask`。`ask` 绑定冻结的请求、原 principal/credential 与资源事实；批准时按当前 token、主体、组和主机边界重新复核后执行。带 session 的请求使用原生终端审阅面板，无 session 的策略请求使用工作区顶部列表。
- [#214](https://github.com/open-mcp-ai/Termcp/issues/214) 是 RFC/原型，不能当成已经合并的实现规范。其冻结 Intent、管理材料保护及授权管理隔离值得纳入后续安全评审。
- #214 提出改口令撤销全部 token；当前 `Managed.ChangePassword` 只清 Web Sessions，并跳过 token 请求取消。建议统一为“管理员主密码变更默认撤销全部认证凭据”，但这是一项额外的凭据生命周期变更，需连同客户端重新签发、退出行为和文档一起交付，不能夹在重启 cookie 修复中悄悄改变。
- [#180](https://github.com/open-mcp-ai/Termcp/issues/180) 的“默认 WebUI 无鉴权”描述不能直接套到当前默认 managed 模式：当前 Wrap 已执行登录、入口权限和 CSRF 检查。应保留这些已实现保护，并单独描述用户主动选用 legacy/open 模式的行为。

本文是本批权限/认证、相关 UI、存储并发和 #228 的待办方案。更广的运行沙箱、进程隔离、会话加密和配置同步没有据此被宣称完成。


## 10. 工作区实施进度（2026-10-29）

- [x] **S00**：四类集合无损 JSON/密文往返；新写入拒绝空条件对象；历史空条件或缺失选择字段的 allow 收紧为 deny，持久记录 policy_warnings，界面可明确复核保存。保留对合法无约束 allow 的支持。
- [x] **W01**：仅跳过仓库根部 .config.lock；首个及同类 repo 创建后注册清理，失败路径也会关闭句柄。
- [x] **A01**：浏览器 JWT 绑定内存 web_boot_id；运行期会话不落盘、不从旧密文恢复；本次运行内重复登录保留已有会话；logout 不依赖磁盘写入；MCP 凭据不随重启清空。
- [x] **A02**：API/UI 新建统一为空 V2，host_mode=all 且 actions={}；历史 nil 策略保留兼容，显式 V1 继续可用。空 actions 也会正常返回。
- [x] **S02**：Managed/Store/Repository 分离普通锁与密码操作门；验密、解锁、新密码派生及 Provider 工作在普通锁外执行，提交前校验 keyring 快照及关闭状态；登录读取最新认证状态，通知在密码门外发出。兼容 Provider 轮换/恢复路径同样处理。正文重封与原子发布保留必要临界区。
- [x] **S03**：MCP/CDP 连接列表、详情、初始响应路径及本服务生成的连接错误使用 endpoint 安全投影；实际连接仍保留 userinfo、path、query。修复普通 MCP 详情访问空 CDP 状态的崩溃。任意工具返回的业务数据不因此被声称已脱敏。

验证：macOS 全仓 make test（race+shuffle）通过；S00/W01/A01/A02 核心用例在 cpu=1/2/4、每档 count=2 的压力运行中通过；S03 用例同样六轮通过。Win11 PD 中运行交叉编译的 windows/amd64、CGO=0 程序，authorization、auth、configstore 三个完整测试包通过。Windows 未运行 race 或全仓 CI，Linux 需后续 CI 验证。

其余 17 项保持待办，尤其是权限组/预置与主题，当前页面仍是逐 Agent 的兼容界面。以上勾选代表当前工作区实现及上述验证完成，不代表远端分支已包含修改。


S02 本轮验证补充：新增可控屏障回归覆盖当前/新密码派生期间配置读写、登录期间旧 cookie 校验与并发权限保存、改密期间既有 MCP 凭据认证、Store 关闭时初始化候选清理、通知重入，以及 keyring 替换后拒绝旧证明。主路径和兼容 Provider 的新增回归均通过不同 CPU 并发度下的六轮 race/shuffle 验证；全仓 make test 已通过。第一次高负载全仓运行出现一例既有终端标记超时，同 shuffle seed 独立连续三次及后续全仓复测均通过，未改变终端逻辑。

Win11 PD 的 S02 复核：Go 1.27.3 交叉编译 windows/amd64、CGO=0，configstore、sshconfig、auth 三个完整测试包通过。未运行 Windows 全仓/race CI，虚拟机 Go 安装保持原状。


## 11. 权限组实施进度（2026-10-30）

G01/G02/G03/G04/G05/O01 已在工作区实现，尚未提交或合并。完整合同见 [权限组实现](permission-groups.md)。认证状态 version=3 把主体、组与凭据分开，兼容迁移保留 ID、namespace 和摘要；旧 V1 策略不自动升级。页面管理组、缺省绑定、成员和凭据，支持复制/改名/启停、删除预览、明确迁移或拒绝。五个角色模板一次性初始化，后续不重建已删除组或给原组追加权限。

后台传输登记到验证后的凭据，正常响应结束继续运行；组策略、停用、成员迁移、组删除与 token 撤销会取消活动请求及后台工作，关闭 I/O 客户端，保留 Cancelled 回执，不承诺撤回已经写入的远端字节。

验证包含 macOS 全仓 race/shuffle、组模型与撤权六轮并发回归、真实 Managed HTTP 页面创建/改名/成员关联/删除迁移与拒绝后重新分配；Win11 的 auth、task 完整包及选定 MCP 身份/新传输撤权用例通过（交叉编译 windows/amd64，CGO=0）。没有运行 Windows 全仓/race CI，主题接线仍属于后续 U01/U02。

当前共 12/23 项在工作区实现，其余 11 项保持待办。勾选仍不表示远端分支已经含有代码。

## 12. 认证与权限页面主题实施进度（2026-10-30）

- [x] **U01**：登录、首次设置、账号和权限组页接入共同主题 shell，使用工作区现有 tokens、字体、theme-boot、theme.js 及 termcp.theme 偏好。支持四主题/auto/外部本地主题；修复共享窄屏样式把主题按钮压为 32px 的冲突。
- [x] **U02**：新增 auth 自管的 GET/HEAD /auth/ui 静态入口；仅公开固定主题脚本、展示资源与主题元数据。拒绝其他脚本、数据、目录和写方法；包括文件被替换成目录的检查。CSP 保留脚本 nonce/框架/表单保护，允许必要的同源样式/字体；缺失主题和 no_webui 有可用后备样式。

完整合同见 [认证页面主题](auth-page-themes.md)。auth 通过启动层注入工作区资源 FS，不 import webui；未放开任何匿名管理 API，没有改变 Agent 的入口权限。

验证：macOS 全仓 make test（race/shuffle）通过，主题回归六轮并发通过；公开文件替换检查六轮通过。no_webui 构建及对应回归通过。内置浏览器实际检查四主题、auto、未设置账号、账号/权限组、退出后加载、390px 窄屏、外部主题发现及主题移除回退。正常页面无 CSP/控制台错误；缺失主题的预期 404 不影响页面恢复。Win11 auth 完整包及真实内置主题资源递归加载回归通过（windows/amd64、CGO=0），未跑 Windows 全仓/race 或 Linux CI。

当前累计 **14/23** 项在本分支实现，剩余 **9** 项：S01、P01–P04、O02、D01、Q01、D02。代码尚未合并；#228 的 jrpc/SSE baseline 继续排除。

## 13. 文件操作参数与字节上限（2026-10-30 历史记录）

本节保留上一轮实施记录；2026-11-02 已按用户要求收回 DSL 扩展，当前范围与完成状态以第 14 节为准。

- [x] **P02**：fileop 统一授权与执行的读取/写入参数、既有 inline 8 MiB 上限和数据解码。普通 read file 支持 --length/--bytes；明确有界的本地上传/下载与文件管道把同一上限用于两端检查；未知或超限在创建 task/写文件前拒绝。无效场景的 --length 明确报错。合同见 [文件操作权限](file-operation-permissions.md)。
- 联动修复：管道右侧递归展开参数引用，避免真实写入内部占位符文件名；补齐简版 help 和源/内嵌 API 文档。
- **O01 复核补充**：SSE HTTP 202 后的活跃 message 可以登记后台传输；登记校验实际 message context、主体修订及角色入口修订。主体/入口停用再恢复不能复活旧 message 的登记资格，新的请求可以使用恢复后的权限。

验证：新增回归及两端分别收紧、SSE 旧消息恢复用例通过不同 CPU 并发度的六轮 race/shuffle；no_webui 构建和所选回归通过。Win11 上 auth、fileop 完整包及 capability、directive、MCP 的所选实际文件读写/HTTP/SSE 回归通过（windows/amd64、CGO=0）。未运行 Windows 全仓/race 或 Linux CI。

最终 macOS make test（全仓 race/shuffle）通过。首轮发现新增 --length 缺少简版 help、后续发现源/内嵌 API 文档不同步；两处均补齐，对应回归及最终全仓复测通过。

累计 **15/23** 项在本地实现；本轮 P02 与联动修复尚未 commit/push，之前 14 项已提交为 5070c53。剩余 **8** 项为 S01、P01、P03、P04、O02、D01、Q01、D02。jrpc/SSE 传输 baseline 继续排除，本轮 SSE 修复属于 MCP 后台工作授权生命周期。

## 14. #232 范围收敛（2026-11-02）

按用户要求，仅处理 #232 的鉴权问题；DSL 只允许必要的鉴权接入改动，不扩展命令、参数、帮助或解析行为。

- 撤回普通 read 新增 --length、--bytes 别名、对应 help/适配逻辑及管道右侧参数展开修复。internal/directive 与对应 DSL 适配文件恢复到本分支已提交版本。
- 保留 P02 的有效参数/真实字节证据、内部 file_read 既有 length、本地上传既有 length、文件管道既有 --bytes 的两端授权检查。小于既有 8 MiB 上限、又没有可证明边界的普通 DSL 文件读取继续拒绝。
- 保留 O01 的 SSE message 上下文及权限修订校验，不变更凭据签发与 DSL。
- [ ] **P02**：整体恢复为部分完成。涉及普通 DSL 读取接口扩展的子项暂不实施，不能继续以 15/23 表示全部验收已满足。

当前累计完成 **14/23**，剩余 **9** 项（含部分完成的 P02）：S01、P01–P04、O02、D01、Q01、D02。之前已提交的代码保持不变，本轮修改尚未 commit/push。第 13 节的旧验收记录不代表此次收敛后已经重跑的平台范围。

## 15. 主机密文位置约束（2026-11-02）

用户追加要求：config.toml 加密后仍在原主机目录，配置正文不迁入集中对象目录。当前正文恢复为 `ssh_configs/<host>/<generation>.enc`；global state 只集中密钥封装、身份/路径索引等元数据，catalog/auth 正文仍使用独立对象。

已有集中正文须先解锁验证，再复制回主机目录、发布索引并清理集中副本。主机目录的旧信封原地接入；旧清理清单不得删除已经成为当前正文的同一路径。状态缺失不能重新初始化覆盖主机密文，正文缺失不能从旧迁移副本悄悄恢复。完整合同及验证见 [Local state 与独立密码](local-state-and-passwords.md)。

macOS 全仓 race/shuffle、相关定向验证及 Win11 所选主机存储/回迁用例通过；未运行 Windows 全仓/race 或 Linux CI。DSL 定义、帮助、解析与适配文件仍与 5070c53 一致，保留的是鉴权层改动。此项记录用户追加的存储约束，不另计 #232 的完成项；P02 仍为部分完成，统计保持 14/23。本轮无 commit/push。

## 16. 移除未发布 V1 权限模型（2026-11-02）

用户明确不需要权限 V1 兼容。P04 调整为删除旧模型：仅支持 schema_version=2 的动作策略和当前权限组状态；V1 功能族、nil 全权回退、升级预览、逐主体直接策略及兼容 API 均移除。

策略由 /auth/groups 管理，主体由 /auth/principals 管理，token 仅接收 principal_id。新主体默认不签发凭据；显式 issue_token:true 可签发。页面和当前 API 文档使用同一合同。旧业务权限数据明确拒绝，不导入或静默扩权。登录/存储格式迁移和加密信封 v1 不属于旧权限管理，保持独立。

- [x] **P04（范围替代）**：不再冻结或维护 V1；直接删除权限 V1 运行、迁移、界面和升级代码。相关回归覆盖新接口、当前策略持久化、旧字段/旧状态拒绝和共享组热收权。

本轮仅触及鉴权及其管理面；DSL 定义、参数、解析和执行不变。term_generated.go 的改动仅移除权限投影 helper 中的 V1 放行分支。完成项统计为 15/23，尚未 commit/push。

## 17. 剩余八项收口（2026-11-02）

按用户要求完成 #232 剩余八项。除权限接入所需的目标绑定和保护判断外，DSL 定义、语法、帮助、参数解析和执行顺序不变。

- [x] **S01**：MCP 预检把 profile UUID/正文摘要、extension 模板摘要和目标 profile 绑定写入可信 context；handler 按绑定加载，名称改名可保持授权，删除重建、正文变化或 target 替换拒绝。扩展的授权 acquire/retry 使用预解析 target plan。
- [x] **P01**：catalog 增加 risk/access/dependencies/parameter evidence；预置只使用当前可证明条件，初始组无动作，viewer/diagnostics 不获得终端输入、文件写或 raw 协议；操作员预置的终端执行能力有明确高风险说明。
- [x] **P02**：执行和授权共用 `internal/fileop` 有效参数与解码字节；inline 使用既有 8 MiB 上限，local/pipe 只在有效正长度可证明时通过。未增加普通 DSL 参数，无法证明更小上限时保守拒绝。
- [x] **P03**：`/auth/access` 返回可达面、参数说明、依赖与三种 recipe；`POST /auth/access/preview` 和权限页只读预览逐项解释缺失动作。预览不执行、不建连、不续租、不 acquire、不创建 task，执行 override 字段拒绝。
- [x] **O02**：权限管理写入结构化审计，含已验证 actor、目标、前后 revision、结果、原因和影响数量；MCP 拒绝记录动作/目标/原因。凭据值与请求正文不进入审计。
- [x] **D01**：9 个大型 capability CSV 从源码树移除；`make audit-capabilities` 使用 `scripts/capability_audit` 在已忽略的 `dist/audits/` 重新生成源码哈希、函数、调用表达式和权限目录。
- [x] **Q01**：加入 `make test-permissions` 与三平台 CI 重复矩阵；真实 HTTP/SSE、浏览器、主题、预览、资源绑定和并发生命周期测试纳入包级执行。Linux/Windows 测试二进制交叉编译通过；实际 Windows/Linux 运行由 CI matrix 执行。
- [x] **D02**：API、权限组、权限管理、静态 token、客户端、CLI、README、UI 文案、测试矩阵和 CHANGELOG 统一到权限组/主体/凭据、当前动作策略、预览与审计合同；历史设计明确标注替代关系。

完成项统计更新为 **23/23**。本批验证包括定向 auth/authorization/capability/fileop/MCP/webui/sshconfig/extensions 测试、全仓 race/shuffle、权限生命周期重复矩阵、生成器实际运行和真实内置浏览器检查。具体运行平台及未运行项按 [权限测试矩阵](../agents/permissions-testing.md) 记录。
