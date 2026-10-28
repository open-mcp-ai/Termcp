# Agent 权限机制设计

状态：待实现的权限管理设计，修订版 4。基线：`dev@76b320976ae671721f3370983f5b0d6b412685b1`。日期：2026-10-26。

实施补充：已按 `dev@6a8af55` 核验并编写 [具体实施方案](agent-access-implementation.md) 与 [69 个函数的证据表](agent-access-implementation-evidence.csv)。本页中的稳定主机 ID、标签选择属于目标态；当前代码没有相应字段，第一版按实施方案使用 profile 名称和 all/selected/none。源码行号、Hook 落点、角色接入及可用审批范围以新实施方案为准。

对应 [接口与操作清单](interface-inventory.md) 及 [CSV 底表](interface-inventory.csv)。本次聚焦权限划分、统一管理、人/Agent 区分和 WebUI 配置；不设计 cookie 或 token 的签发机制。所有新接口和格式均为提案，尚未实现。

签发机制的后续设计见 [人与 Agent 的身份与凭据签发机制](auth-issuance.md)，涵盖人端登录、Agent token、可选配对、续期与撤销；本文仍负责共享业务策略。该补充尚未实现，不代表当前已具备可信角色隔离。

需求参考：[子用户身份与专属词汇表 #196](https://github.com/open-mcp-ai/Termcp/issues/196)、[用户/组、签发链与防伪 #197](https://github.com/open-mcp-ai/Termcp/issues/197)。本期吸收词面权限与别名管理，为用户/组和子身份留接缝，不把 #197 的完整身份安全体系纳入本期。

## 1. 本期范围与后续边界

### 1.1 本期要完成什么

1. 人与 Agent 使用不同权限边界，WebUI 由人管理 Agent 权限。
2. 所有 Agent 默认使用同一份缺省策略，不要求逐个注册、逐个分配资源。
3. 本期不实现单个 Agent 的权限、主机范围或词汇例外；只在后续接缝中预留位置。
4. 资源范围第一版只做到主机；缺省为全部主机，其下会话、通道、文件、扩展、转发、peer、任务继承所属主机范围。
5. 动作细分到 DSL 动词、名词与决定实际动作的参数；支持禁止、需审批、允许。
6. 可为全体 Agent 配置共用别名，并支持以这些别名作为公共直接词汇表；专属分配留到后续。
7. 使用统一入口过滤器和操作过滤器：Agent 只开放 MCP，人使用 WebUI/REST/WS；各操作的能力映射共用同一注册表。

沿用既有范围：主机配置逐条独立存储，加密机制参考 `encryption-test`，具体落地见 [主机配置逐条信封加密实施方案](config-encryption.md)；不加入会话输入输出或历史加密。人主动导出默认可为明文，也可自选文件密码加密；Agent 的默认业务权限不包括读取/导出凭据。

### 1.2 本期不展开什么

单个 Agent 的例外策略、专属主机/词汇设置、cookie 签发、登录流程、token 签发/派生/吊销链、连接身份自动分配、用户/组创建与入组生命周期、子用户持久化，以及数据流防伪均留给后续设计。本期不新增相应管理 DSL 或凭据管理界面。

这不意味着用请求体中的 role、agent_id 或 clientInfo.name 当身份。权限层只消费服务端入口提供的可信访问上下文；身份如何可靠产生由接入层负责。没有可信识别时，单纯配置 UI 或隐藏词汇不构成防伪保证。

## 2. 核心模型

把配置拆为三个明确维度：**操作许可、主机范围、直接可用词汇**。

| 维度 | 人设置什么 | 回答的问题 |
| --- | --- | --- |
| 操作许可 | 禁止 / 需审批 / 允许，以及动作参数约束 | 可以做什么 |
| 主机范围 | 全部 / 指定 ID / 标签选择 / 无 | 可以控制哪些主机 |
| 直接可用词汇 | 标准 DSL 加已分配别名，或仅已分配别名 | 可以从哪些词汇入口发起动作 |

第三项不能代替前两项。分配一个叫“发布”的别名，并不自动授予它展开后的输入、文件写入或网络访问权限。

统一门禁接收规范化操作计划，包含访问上下文、直接入口词汇、实际操作、目标主机和参数。词面检查与执行前检查是同一套策略的两个执行时点，不是相互独立的两套权限。

## 3. 人与 Agent 的访问上下文与入口边界

### 3.1 本期只区分两类调用者

权限层消费服务端提供的 AccessContext，不负责设计其签发机制。

| 字段 | 本期用途 |
| --- | --- |
| actor_kind | human 或 agent；不能仅根据 REST/MCP 猜身份 |
| context_id | 请求关联、暂存输入、MCP 结果路由；不用于选择另一份权限 |
| policy_revision | 共享缺省策略版本，用于执行复核和缓存失效 |

本期不按 subject_id 或 agent_policy_key 查找例外配置，也不把它们作为权限管理输入。人的业务操作不套用 Agent 策略，例如禁止 Agent 输入不会锁住人在 WebUI 的终端；人仍遵守资源自身与部署硬限制。

所有 Agent 共用业务策略与资源视图，不自动形成私有 namespace。owner/context 等已有字段可继续用于协议路由和在途操作，但不是本期的逐 Agent ACL。

### 3.2 Agent 默认只允许 MCP

| 已识别的调用者 | 入口 | 本期结果 |
| --- | --- | --- |
| agent | GET /sse、POST /message、/stream 的合法 MCP 方法 | 允许到达 MCP 协议层，再检查实际操作权限 |
| agent | POST /api/term | 拒绝；相同 DSL 不使 REST 自动成为 MCP |
| agent | 其他 /api/*、WebUI 页面、GET /api/ui/ws | 拒绝，包括只读业务接口和 WS Upgrade |
| human | WebUI、REST、WS、MCP | 沿用人的对应能力和资源限制；不套用共享 Agent 策略 |
| 未识别或无效上下文 | 受保护业务入口 | 不得默认为 human 或获得较宽权限 |

MCP HTTP 白名单按注册的确切路由及合法方法判定，不做字符串前缀放行。stdio 桥是 MCP 访问方式，不额外开放 REST。仅能进入 MCP 不代表能调用其中的管理动作；人端保留能力仍拒绝 Agent。

已识别 Agent 的非 MCP 请求在进入业务 handler、DSL 解析或 WS 升级前拒绝。未来若开放额外入口，需明确变更入口策略，本期不提供对 Agent 放开 REST/WS 的开关。

公开文档的匿名访问规则单独保留；它不是 Agent 的业务授权。若非 MCP 请求已经携带可识别的 Agent 上下文，仍按上述边界拒绝。本期不声称能从匿名流量判断背后是否是 Agent。

## 4. 一份共享缺省策略

系统只有一份 default_agent 生效策略，统一保存操作许可、主机范围和公共词汇设置。WebUI 操作控件为“禁止 / 需审批 / 允许”，本期没有“继承”或“Agent 例外”控件。

| 配置项 | 本期行为 |
| --- | --- |
| operations | 全体 Agent 使用同一张操作表，未列出操作禁止 |
| hosts | 全体 Agent 使用同一个主机选择器，初始 all |
| vocabulary | 全体 Agent 使用同一套直接词汇及别名分配 |
| revision | 保存后递增，后续检查与缓存使用新版本 |

执行时只加载共享策略，不进行个体覆盖合成，也不实现 clear_constraints 等继承辅助字段。保存策略时完整验证操作、参数与词汇；配置损坏或读取失败应拒绝，而不是回退到不受限制的模式。

修改共享配置对所有 Agent 生效。未列出的操作默认禁止，新功能不会自动加入已有 grant；UI 勾选词树父项时只展开当时明确的已知子项。

以后通过 PolicyResolver 接入个体、组或子用户策略；本期该服务对 agent 只返回共享缺省策略。当前配置格式与接口不接受 agents、operation_overrides、host_override、vocabulary_override 或其他未实现的覆盖字段。

## 5. 主机范围与标签兼容

### 5.1 主机是第一版资源授权单位

主机指连接配置 profile 的稳定 ID，不是只比较名字或 IP。internal 也是明确的主机。主机改名不换 ID；删除后同名重建不继承旧 ID。

| 资源 | 授权时取的主机 |
| --- | --- |
| session、shell、reader、输出、marks | 会话建立时保存的 profile ID |
| 文件、forward | 实际承载连接的 profile ID |
| ext、协议 peer、浏览器 target | 扩展 target 或父会话的 profile ID |
| 复制、传输 task | 全部源与目标主机，必须同时获准 |
| local-path 文件接入 | 本地端 internal，加上实际远端目标 |
| alias | 不靠别名拥有主机，展开后检查实际主机 |
| 列表、resolve、活动、WS 推送 | 每个条目所属主机，先过滤后返回 |
| 帮助、自身权限、管理操作 | 不通过 hosts=all 获得管理权限 |

缺省 hosts=all 包含当前和以后新增的可用可信主机；主机下的业务资源不按 Agent 创建者隔离。现有基础设施隐藏、namespace 和资源 allow/paths 仍保留。

### 5.2 主机选择器

- all：全部主机，可显式配置 exclude，初始没有排除项。
- selected：显式 IDs 与标签命中结果取并集，再减去 exclude。
- none：无主机；不是继承或全部。

同一 tag_set 内标签 AND，多个 tag_set 之间 OR；排除优先。selected 的 include 为空就是空集合。空 tag_set 拒绝保存，避免空 AND 意外命中全部。all 不接受 include，none 不接受无意义的选择条件。

标签选择器动态生效：增加匹配标签后主机进入范围，移除或命中排除标签后退出范围，已有会话的后续操作也重新判断。想固定集合时存明确 IDs。授权标签只有人能修改，普通 profile.edit 不隐含标签管理权。

### 5.3 与现有机制的关系

当前基线已有 Session.Namespace、ns:* 调用者分组和 ext:* 基础设施分组；未发现 sshconfig.Entry 的通用主机 tags 字段。[#45 标签授权](https://github.com/open-mcp-ai/Termcp/issues/45) 的目标可由主机选择器补上，不能说现有 namespace 已经等同主机分类标签授权。

namespace 继续决定会话分组/可见域，不能改变 actor_kind、共享策略来源或主机集合。模板 allow/paths 继续是资源自身上限。

有效范围是：**主体有效策略 × 主机选择器 × 既有可见域 × 资源上限 × 部署限制**。

第一版不额外分配具体 session、文件目录或 peer ID。主机范围不能约束任意 shell 程序最终能访问的全部文件与网络；允许终端执行时，OS 权限与网络隔离仍有自己的边界。

## 6. DSL 词面与语义操作统一

### 6.1 词面是语法节点，不是字符串包含

从现有 DSL 声明表生成权限词树：动词 → 名词/资源种类 → 决定实际动作的参数分支。示例：

| 词面节点 | 实际操作 ID | 可分别设置 |
| --- | --- | --- |
| connect host | session.start | 与删除会话分开 |
| rm session | session.delete | 不等于禁止所有 create/start |
| span channel | shell.marks | 读取状态跨度 |
| reader register | shell.reader.register | 注册 reader |
| reader unregister（--drop） | shell.reader.unregister | 注销 reader |
| get host | profile.get | 安全摘要，不能顺带读取凭据 |
| get file --op realpath | file.realpath | 与 truncate 分开 |
| get file --op truncate | file.truncate | 即使动词叫 get 也是写操作 |
| edit ext --restart | ext.retry | 与修改扩展定义分开 |
| raw tools/call / 工具名调用 | mcp.tool.invoke | 共享同一方法/参数权限 |

reject 是 UI/词面配置中“禁止”的称呼，持久化统一为 deny。不能通过改同义词、别名、传输或入口避开同一实际操作的禁止。

在 UI 勾选一个父动词时，展开为当前注册表中明确的子节点，并显示受影响动作；不授权未来未知词或未知参数分支。若只想禁止 rm file 而允许 rm alias，设置对应子节点，不对整个 rm 一刀切。

### 6.2 统一过滤器，子命令只声明语义

不在每个 handler 中重复写 allow/deny 判断。统一 AccessFilter 提供两个时点的检查：入口检查 CheckSurface，以及规范化计划检查 CheckOperation；两者使用同一访问上下文与策略来源。

这两个检查只返回允许、拒绝或待审批结果，不执行 shell、写文件或建立连接。策略读取失败、资源无法解析或操作未注册时不返回允许。执行器只接受服务端完成检查的计划，不能接受客户端伪造的检查结果；审批通过后仍复核同一计划与当前策略版本。

```mermaid
flowchart LR
    A[服务端访问上下文] --> B[统一入口过滤]
    B --> C[只解析 AST / 别名展开]
    C --> D[注册表解析实际操作与主机]
    D --> E[统一操作过滤]
    E --> F[审批或执行]
```

入口过滤首先阻止 Agent 访问非 MCP 接口。MCP 内再进行词面与操作过滤：

- 解析/展开时只生成 AST，不运行任何内嵌命令；尽早拒绝禁止、未知或不在公共直接词汇表中的词。
- 执行前解析实际动作、主机和参数，统一决定 deny、ask、allow。解析期通过不能跳过这一步。
- 业务 handler 消费服务端已经解析和核对的计划，不接受客户端自报“已经检查”或“我是某个别名”。

注册表每项声明以下元数据：

| 声明 | 示例 | 谁负责 |
| --- | --- | --- |
| DSL 词面节点 | reader 注册、reader --drop | DSL 定义 |
| 动作分支判别 | --drop 是否出现；--op 的枚举值 | 表驱动映射，必要时用纯解析 resolver |
| 语义操作 ID | shell.reader.register / shell.reader.unregister | 能力注册表 |
| 目标解析方式 | channel → session → profile ID | 共用资源解析器 |
| 参数约束 schema | key 枚举、文件写入模式、启动计划 | 操作声明 |
| 等价入口 | MCP 内部调用、人的 REST/WS 适配 | 路由/消息注册元数据 |
| 执行和审批方式 | 直接执行、行级暂存、确定计划审批 | 现有执行器与统一授权结果 |

大部分新增分支只增加声明，不新增鉴权逻辑。例如 reader 的声明列出有 --drop 时映射 unregister，否则映射 register；get file 的声明将 --op realpath 与 truncate 分别映射。过滤器只读取计划并检查对应许可，不需要写一份专属权限 if。

不能只靠现有 Flags 名称自动推断副作用。开发者仍需登记新增行为的语义映射；复杂动态分支可以有专门的解析代码，但不得自行决定权限。未声明分支默认拒绝，并用覆盖检查要求每个可达分支都有映射。

同一注册表生成词树、语义 ID、等价入口映射、WebUI 标签和检查入口，避免用户、组、Agent 各有一张不同白名单。人的 REST/WS 虽不向 Agent 开放，仍使用同一语义注册表及资源上限检查；将来扩展入口无需复制操作授权逻辑。

#### 6.2.1 参考 Windows Hook 的能力执行过滤链

借鉴的是事件进入统一分发点后按顺序经过处理链的模式。Windows 不同 Hook 类型的能力不同，有的只能观察，有的可以阻止事件继续传播，见 [Microsoft Hook Chains](https://learn.microsoft.com/en-us/windows/win32/winmsg/about-hooks)。这里将能力调用视为事件，不引入 Windows DLL 注入或修改函数地址。

能力目录是静态描述，不会自动产生可 Hook 的事件。需要建立一个强制的能力分发入口 CapabilityDispatcher.Invoke：所有外部请求形成规范化能力调用后都经过它，再到真正的业务实现。入口过滤仍在解析前执行，不能让 Agent 先进入 REST/WS 再等操作过滤拒绝。

```text
MCP 请求 → 入口过滤 → term/DSL 解析与别名展开
                              ↓
                  规范化的能力调用 Invocation
                              ↓
                 CapabilityDispatcher.Invoke
                              ↓
             能力许可 → 主机范围 → 资源限制 → 参数限制
                              ↓
                    审批判定与最终决策
                    ↙       ↓       ↘
                  拒绝    待审批     执行
                              ↓
                       只读的结果观察
```

Invocation 是服务端解析出的不可变快照，包括 actor_kind、入口类型、语义操作 ID、真实主机 ID 集合、规范化参数、上下文标识和策略/资源版本。Filter 不接受客户端自报的能力、主机或“已授权”标记。

| 过滤项 | 检查内容 | 本期行为 |
| --- | --- | --- |
| CapabilityFilter | 已注册操作及共享 Agent 策略 | 禁止/未列出操作拒绝；human 不套用 Agent 策略，但不跳过后面的资源检查 |
| HostScopeFilter | 全部真实目标主机 | 每一端均需在共享主机范围内 |
| RestrictionFilter | namespace、模板 allow/paths、部署限制 | 作为独立资源上限，不能被能力 allow 覆盖 |
| ConstraintFilter | 注册表声明的动作参数 | 无法解析或违反约束时拒绝 |
| ApprovalFilter | 当前计划是否必须审阅且可安全排队 | 返回待审批，不自行执行业务或阻塞整个分发线程 |
| AuditObserver | 规范化的决策与执行结果 | 只观察经过字段分类的非秘密信息，不改变权限结果 |

安全 Filter 的返回值只表示“本检查通过 / 需要审批 / 拒绝”，不接收 next 函数或业务 handler。只有分发器能继续调用下一个 Filter 或执行能力，避免某个回调主动调用 next 越过其他检查、重复执行或自行调用业务实现。

聚合规则固定：

1. 注册能力和必要 Filter 必须完整，缺失时拒绝启动相关执行入口；不能让空链默认等于全部允许。
2. 任一拒绝立即禁止执行。某项要求审批时仍继续其余必要检查，后续拒绝优先于待审批。
3. 全部必要检查通过且共享策略明确允许时，才形成允许结果；没有 Filter 的“通过”可以单独代表最终放行。
4. Filter 报错、panic、超时或上下文失效不能转成允许。
5. Filter 不修改调用；目标或参数变化需重新解析和授权。审批恢复执行时用当前版本复核，不能直接跳到 handler。

这条安全链在启动时由受信代码固定构造，不提供给 Agent 卸载、重排或替换的能力。借鉴 Hook 链式处理，不照搬任意回调可决定是否调用下一项的控制方式。观察型 Hook 与裁决型 Filter 分开：执行后的审计/计时不能补救已经发生的未授权副作用。

#### 6.2.2 在当前代码中的接入位置

若先执行“阶段二：命令模式解耦”，实施边界见 [命令解耦方案](command-decoupling.md)：先把既有路径迁到统一执行入口并保持行为，不在纯重构阶段同时启用本设计的新权限规则。

现有 MCP 内部能力调用路径是 `callTool → withLogging → checkAdmission → handler`，对应 `internal/mcp/term.go`、`logging.go` 和 `admission.go`。它已经具有执行前拦截的形状，但授权附着在日志包装器中，且不是所有 DSL 路径都经过它。

改造顺序是：

1. 从日志包装器中分离强制授权，将 `callTool` 适配到统一能力分发入口；关闭日志不影响鉴权。
2. 将直接读取 manager 的 DSL 查询也登记为能力调用；审计已核实 directivePS/directiveInfo 等直接路径。span 当前实际经过 callTool("message")，不能仅按 runDirective 的旧注释判断其旁路；不能只 Hook callTool 就宣称覆盖全部操作。
3. 将人的 REST/WS 资源操作映射到同一分发入口或共享授权 Interface；Agent 在这些入口之前已被 CheckSurface 拒绝。
4. 用注册表覆盖校验约束新路径：未登记或可绕过分发器的公开调用不能作为可用能力交付。

外层 term 仍负责入口与 DSL 解析，不能只对 tools/call 的工具名 term 授一次权，就把其中所有动作一起放行。统一分发器要看到展开后的 session.start、shell.input、file.truncate 等实际能力。

内部复合调用和审批恢复不得使用全局 skip_auth 标记。若实现使用执行许可，许可必须由服务端生成并绑定确定的子计划、参数和版本，只能由受保护执行器消费。行级输入、流式输出及异步任务在其执行检查点复用相同授权规则，不能因只在最外层 Hook 一次而绕过后续策略变更。

#### 6.2.3 MCP 与 WebUI 的 Hook 兼容

两者不处于相同的执行时点，不强行复用同一个协议回调。MCP Hook 位于 DSL 展开后、实际执行前；WebUI HTTP Hook 位于已识别身份之后和业务路由分发之前，WS 先在 Upgrade 前检查身份/入口，再保留消息执行处的资源检查。

兼容的是规范化调用描述和决策 Interface：两个 Hook Adapter 把不同输入转换成同一种授权描述，使用同一策略来源与操作注册表，再分别处理各自的协议结果和生命周期。WebUI 不调用 MCP 工具来执行自己的操作，MCP 也不构造伪 HTTP 请求去复用 UI handler。

```text
MCP term → AST/别名展开 → MCP Hook Adapter ─────────┐
                                                ├→ 共享 AccessFilter
WebUI HTTP/WS → 入口检查 → WebUI Hook Adapter ────┘
                    ↑                                ↓
             Agent 在此拒绝              决策返回各自的协议/执行适配
```

| 规范化字段 | 来源与用途 |
| --- | --- |
| actor_kind | 服务端确认的原始发起者 human/agent，不从入口名称或请求体猜测 |
| surface | MCP、WebUI REST、WebUI WS 等承载方式；不能单独决定操作权限 |
| operation_id | 展开后的语义动作，不是字符串 term 或泛化的 GET/POST |
| host_ids | 服务端解析的实际主机集合，不相信客户端自报的范围 |
| arguments | 类型化动作参数；保留 nil/0、缺省/显式值和原有业务语义 |
| execution_phase | 初次调用或受信审批恢复；由内部流程产生，不能用作通用跳过检查开关 |
| context / policy_revision | 请求关联、取消与当前策略版本；不扩展为本期的 cookie/token 签发机制 |

两个 Adapter 的职责不同：

| 检查/流程 | MCP Adapter | WebUI Adapter |
| --- | --- | --- |
| 入口许可 | Agent 只能从合法 MCP 入口进入 | Agent 对 REST/UI/WS 一律拒绝；人的入口保留 |
| 业务操作许可 | Agent 使用公共缺省策略、主机范围与词汇许可 | 人不套用 Agent 策略；保留资源与部署限制 |
| 输入 | 将 DSL 动作、别名和参数解析为真实能力计划 | 从路由/body 或 WS message type 得到能力计划，不要求经过 DSL |
| 决策返回 | 编码为已有 MCP/tool/envelope 结果 | HTTP 使用原状态/JSON；已升级 WS 只能使用其协议允许的处理方式，不能写 HTTP 错误响应 |
| 需审批 | 进入对应确定计划或行级输入审阅 | 人的直接终端操作不套用 Agent staging；人的审批操作另走管理能力 |
| 原有实现差异 | 保留 MCP 输入来源、游标、任务和结果语义 | 保留 API 输入来源、本机 OS 分支及 UI 返回形态，不为共用 Hook 改成 MCP 行为 |

原始 profile TOML 与投影摘要、归档与删除等不同语义应使用不同操作描述，不能为兼容而归入同一个宽泛 ID。两个 Adapter 只共享语义合同，不统一本来就不同的业务结果。

审批有两次不同的动作：人批准请求是 human 的 approval.decide；随后恢复的是原发起者的业务计划。不能因为在 WebUI 点了批准，就把原 Agent 计划永久改成人的权限。授权阶段启用新 Hook 时，恢复执行核对该次批准、原计划和当前策略；已满足的审批条件不重复排队，其他限制仍生效。

若正在进行纯命令解耦阶段，上述新增授权规则先不启用：两个 legacy Adapter 复现原有检查顺序、来源和回放行为。先以行为对照证明兼容，再在明确的授权变更阶段开启新规则，不混在同一个“行为不变”的重构里。

本期最小落地不要求先完成 WebUI 全量命令对象化：可先在 MCP/DSL 执行出口部署 Agent 操作过滤，WebUI 先完成拒绝 Agent 的入口检查与人的权限配置/预览。以后接入统一命令分发器时复用同一调用描述和 AccessFilter，逐个迁移 Adapter。

兼容验收至少包括：Agent 的 MCP 输入按公共策略判断；相同动作从 WebUI/REST/WS 发起仍被入口拒绝；人的 UI 终端不被 Agent 策略锁住；协议错误不混用；纯重构阶段保持原行为；新授权阶段的审批恢复不把 Agent 提升为 human。

### 6.3 复合操作

run 包含输入、提交和所请求的输出读取；需要的权限由注册表声明并在任何副作用前核对。新建会话的默认启动计划与覆写 command/args 分开判断，不能靠 start 权限任意启动程序。

同一批量操作先检查全部目标，复制检查两端。DSL 的独立 statement 不承诺整体事务回滚；已成功的前一条不会因后一条拒绝而被撤销。

### 6.4 操作目录

以下为本提案的操作词汇；未存在于当前 capability 表的项需要新增。使用同一 ID 的入口必须具有相同的授权含义。

| 资源 | 操作 ID | 主要约束或说明 |
| --- | --- | --- |
| 会话 | `session.list`, `session.info`, `session.start`, `session.rename`, `session.terminate`, `session.delete` | 范围统一落到所属主机；start 还检查启动方式，删除与断开分开 |
| 通道 | `shell.list`, `shell.info`, `shell.open`, `shell.close`, `shell.delete` | 通道归属父会话的主机；close 保留记录，delete 删除记录 |
| 终端 I/O | `shell.input`, `shell.key`, `shell.output`, `shell.resize`, `shell.marks` | 输入、按键、输出、布局分别检查；可限制 PTY/pipe、按键集合 |
| 输出 reader | `shell.reader.register`, `shell.reader.unregister` | 继承通道的主机范围；注册与注销分开；不默认按 Agent owner 隔离 |
| 文件内容 | `file.list`, `file.stat`, `file.read`, `file.write`, `file.replace`, `file.delete`, `file.rename`, `file.mkdir` | 按所属主机和实际读写动作检查；模式和大小可作为动作约束；改名检查两个端点 |
| 文件属性 | `file.chmod`, `file.chown`, `file.chtimes`, `file.readlink`, `file.symlink`, `file.hardlink`, `file.truncate`, `file.realpath`, `file.statvfs`, `file.getwd` | 区分只读和修改；链接检查两个端点；第一版不另建目录授权层 |
| 本地文件接入 | `local.file.read`, `local.file.write` | 本地端点归属 internal 主机，不能只检查上传目标主机 |
| 主机配置 | `profile.list`, `profile.get`, `profile.create`, `profile.edit`, `profile.rename`, `profile.copy`, `profile.delete`, `profile.test`, `profile.import` | list/get 只返回安全字段；编辑约束可区分描述与连接目标等字段 |
| 凭据设置 | `profile.credentials.set`, `profile.temporary.inject` | 只写不读；继承主机范围；注入目标、TTL 与一次性用途单独校验 |
| 扩展 | `ext.list`, `ext.get`, `ext.use`, `ext.enable`, `ext.disable`, `ext.retry`, `ext.create`, `ext.update`, `ext.delete`, `ext.import` | get 仅安全摘要；定义变更与使用/重启分开，不能偷偷扩大会话资源上限 |
| 扩展定义 | `ext.definition.read` | 只读不自动开放；服务端认证头与秘密引用不返回，无法安全投影的字段不返回 |
| 转发 | `forward.list`, `forward.local.open`, `forward.remote.open`, `forward.dynamic.open`, `forward.close` | 按承载主机与方向分开；目标地址限制为后续可选动作约束 |
| 协议连接 | `mcp.open`, `cdp.open`, `jrpc.open`, `peer.list`, `peer.info`, `peer.close` | 从扩展或父会话继承主机；URL 与传输合法性另行校验 |
| 协议调用 | `peer.methods.read`, `peer.invoke`, `peer.receive`, `peer.events.read`, `peer.frame.read` | 主机内解析到的 peer、协议种类、精确方法名与参数；通用帧与便利调用同规则 |
| MCP 工具 | `mcp.tools.list`, `mcp.tool.invoke` | 主机内的 peer、精确工具名、结构化参数约束；协议原始 tools/call 必须归一到此操作 |
| 浏览器 target | `cdp.target.list`, `cdp.target.attach`, `cdp.target.detach` | target 身份和所属 peer；URL 限制不能冒充浏览器网络沙箱 |
| 反向采样 | `sampling.forward` | 发起子 peer、目标 Agent MCP 会话、可支持的请求预算；不允许跨主体转发 |
| 传输任务 | `task.list`, `task.info`, `task.cancel` | 从任务源/目标继承主机集合；不默认按 Agent owner 隔离；初始传输检查两端 |
| 运行时别名 | `alias.list`, `alias.create`, `alias.edit`, `alias.delete` | 按权限配置分配可用别名；展开后检查实际动作及主机，别名不授予额外权限 |
| 通知与元数据 | `user.notify`, `instance.status.read`, `activity.read` | 通知目标、最小实例状态、资源范围内活动；不返回完整 daemon 或范围外主机信息 |

帮助、协议初始化与读取自己的有效权限是认证主体的最小发现能力，不授予业务资源可见性。普通 HTTP 文档维持现有免认证规则。`activity.read` 不等于管理审计访问权。

人端另有 `profile.credentials.read`、`profile.export`、`ext.export`、`approval.decide`、`approval.manage`、`agent.permissions.manage`、`host.tags.manage`、`storage.manage`、`instance.manage`、`audit.read`。它们不进入 Agent 可授权目录。

## 7. 公共别名与词汇表

### 7.1 分配别名，不另建一套权限

人通过缺省策略为全体 Agent 统一分配 alias IDs。别名具有稳定 ID、名字、定义、参数结构和版本，别名定义属于实例资产，分配关系属于权限配置。

- standard 模式：使用获准的标准 DSL 词以及已分配别名。
- assigned_aliases 模式：业务动作只能从已分配别名发起；保留最小帮助/自身权限查询。
- 公共词汇设置直接对全体 Agent 生效；显式空 alias 列表不代表全部别名。

“禁止某个实际操作”与“不给它作为直接入口词汇”分开。alias-only 可以允许别名内部读取 span，而禁止直接输入 span；但如果 shell.marks 本身为 deny，别名内部也不能读取。隐藏一个词而不限制可达的等价 REST 操作不算完成词汇收敛，其他入口必须执行同一入口限制。

### 7.2 展开后的限制

分配、创建或修改 alias 时解析其完整定义，生成所需操作集合并显示给人；定义变更使旧编译计划和待审批项失效。alias 不能隐式授予这些依赖操作。

示例中的 required_operations 是服务端编译生成的预览结果，不是信任客户端自报的依赖清单；不能通过删掉其中一个 ID 来跳过实际展开动作的检查。

Agent 若被允许创建自己的 alias，只能引用当时可使用的词/操作；ask 可以被引用但不能变成 allow。参数只可出现在声明的值/目标位置，不能借参数注入新的动词、名词或语句。实际目标到执行时再校验主机范围。

每次执行继续按当前有效权限检查，不能永久缓存“创建时允许”。多层别名递归展开并限制深度，检测循环；扩展携带的 alias 也一样检查。

alias-only 的展开由服务端真实解析产生上下文，不接受请求体自报“我是某个已分配别名”来跳过直接入口限制。管理 alias 的权限不等于给自己分配更高权限或修改别人共享词汇的权限。

### 7.3 当前与后续的边界

本期由人通过 WebUI 配置全体 Agent 共用的别名及权限。现有全局 alias 需要有明确的公共分配适配；修改共享定义仍是独立动作，不能因可使用 alias 就自动获得修改权。运行结果回到统一能力门禁。

#196 的子用户和专属词汇分配整体留到后续，届时复用同一词树、语义操作和别名展开模型，不建立独立执行器。

## 8. 权限配置示例

以下是拟议的共享配置；不是当前运行时已经接受的文件。本期示例不包含 agents 或任何个体覆盖字段。

示例表示人已经选择的一组业务权限，不是要求出厂默认开放这些动作。#197 的“缺省权限最小化”可体现在更保守的初始动作模板；主机范围仍为 all，两者不冲突。

```json
{
  "schema_version": 4,
  "default_agent": {
    "revision": 1,
    "unlisted_operation": "deny",
    "hosts": {
      "mode": "all"
    },
    "vocabulary": {
      "mode": "standard",
      "alias_ids": [
        "alias_status"
      ]
    },
    "operations": {
      "session.list": {
        "decision": "allow"
      },
      "session.info": {
        "decision": "allow"
      },
      "session.start": {
        "decision": "allow",
        "constraints": {
          "launch": "approved_profile_default"
        }
      },
      "shell.list": {
        "decision": "allow"
      },
      "shell.output": {
        "decision": "allow"
      },
      "shell.marks": {
        "decision": "allow"
      },
      "shell.reader.register": {
        "decision": "allow"
      },
      "shell.reader.unregister": {
        "decision": "deny"
      },
      "shell.input": {
        "decision": "ask"
      },
      "shell.key": {
        "decision": "ask"
      },
      "file.read": {
        "decision": "allow"
      },
      "file.write": {
        "decision": "ask"
      },
      "file.delete": {
        "decision": "deny"
      },
      "session.terminate": {
        "decision": "allow"
      },
      "session.delete": {
        "decision": "deny"
      }
    }
  },
  "aliases": [
    {
      "id": "alias_status",
      "name": "status",
      "revision": 1,
      "cmd_template": "span $CHANNEL",
      "inputs": {
        "CHANNEL": {
          "type": "channel"
        }
      },
      "required_operations": [
        "shell.marks"
      ]
    }
  ]
}
```

全体 Agent 共用该策略：可以连接主机和注册 reader，注销 reader 与删除记录禁止，输入需审批；公共 status 别名读取获准的 shell.marks。可以统一把 vocabulary 改为 assigned_aliases，使所有 Agent 的业务入口只暴露 status，但不能给某一个 Agent 单独设置。

新操作默认禁止；若人以后主动把某项新操作加入缺省允许，全体 Agent 都会得到它。

## 9. WebUI 与统一管理

### 9.1 页面结构

1. **公共缺省权限**：全体 Agent 共用的操作表、主机范围和词汇模式。
2. **动作树**：按功能显示动词/名词/参数动作，每项为禁止、需审批、允许。
3. **公共别名**：统一分配 alias，展示展开计划、依赖动作与审批结果。
4. **人/Agent 对照预览**：查看同一计划对人和公共 Agent 的结果；预览不改变真实请求上下文。
5. **Agent 接入列表**：显示 Agent 名称、稳定 ID、所属用户、grant 状态、最近使用、当前 MCP 连接和 token 轮换/撤销。该列表管理身份与接入生命周期，仍不提供个体 ACL。

本期没有个体例外页、继承控件、专属主机或专属别名设置。Agent 身份、登录和凭据签发按 [人与 Agent 的身份与凭据签发机制](auth-issuance.md) 实现；“身份管理”与“共享权限配置”是两个页面，不能提供一个隐含着个体扩权的 Agent 覆盖控件。

### 9.2 管理服务提案

下列接口只服务于人的 WebUI 权限管理，要求人端上下文和并发版本校验；Agent 不可通过它们配置或查询权限。

| 接口 | 作用 |
| --- | --- |
| GET/PUT /api/agent-policy/default | 公共缺省策略，保存后统一生效 |
| GET /api/permission-catalog | 词树、语义 ID、约束和审批元数据 |
| POST /api/agent-policy/preview | 人端预览人/公共 Agent 的效果，不执行业务 |
| GET/PUT /api/agent-aliases/{alias_id} | 人管理公共 alias 定义与版本，不隐含授予调用许可 |
| GET/POST /api/agents | 列出或创建 Agent Principal；仍使用共享策略 |
| POST /api/agents/{id}/tokens | 签发一枚可直接配置到 MCP 的 Agent token；secret 只在响应中出现一次 |
| GET /api/agents/{id}/tokens | 列出 token ID、创建时间、到期时间和最近使用，不返回 secret |
| GET /api/agents/{id}/status | 查看 grant 状态、最近使用和当前 MCP 连接 |
| POST /api/agents/{id}/revoke | 撤销该 Agent 的全部后续访问 |

Agent 如需发现自身可用词汇，通过 MCP 的帮助/权限发现能力返回公共策略摘要，不新增 Agent REST 自查入口。个体配置相关接口本期不注册、不接受相关字段。

## 10. 审批、结果与权限变化

需审批是业务操作的有效决策，不是突破 deny 或范围外主机的通道。Agent 不可批准自己的请求、关闭审阅或修改权限配置。

本期复用既有审阅机制，为尚不能安全审阅的动作标明不可用；不能把 ask 保存成实际 allow。首次建连等无 session 的动作需要实例级队列支持，完成前不能宣称已支持其 ask。

审批绑定确定计划、上下文、共享策略/alias/主机版本及一次性执行标识；执行前复核。配置、标签或别名变化后重新判断，旧批准不能为改变后的内容授权。

input 与 Enter 应按确定的一行处理，不把未批准的文字/控制字节先送到 PTY。暂存区按 context 和通道绑定，不由另一个请求身份隐式提交。不能安全处理的交互方式拒绝或要求另行允许直接交互。

列表、resolve、状态、输出订阅、WS 广播和活动按主机范围及字段权限过滤。主机/扩展摘要不能顺带返回私钥、代理认证、headers 等秘密。生成文件 URL 不使下载绕过权限。

共享策略、标签、alias 变更触发策略缓存失效，后续请求和推送应用新规则；在途可取消操作复核。已经交给 OS 的命令或字节不能回滚。这里定义权限变化后的执行语义，不在本期规定 token 如何吊销。

## 11. 为 #196/#197 预留的接缝

### 11.1 单个 Agent 配置与专属词汇（后续）

后续可由服务端可信主体标识选择例外配置，对操作、主机范围和词汇逐项覆盖缺省；未覆盖项继续继承。届时才增加 agent_policy_key、个体数据模型、继承合成规则、专属 alias 分配与 WebUI 例外页。

当前 PolicyResolver 对 agent 仅返回共享缺省策略，其对外结果仍为统一 EffectivePolicy；以后加入具体用户、组或子用户来源，不修改业务 handler 的授权含义。本期不预建未使用的个体配置表或接口，未支持字段一律拒绝而不是忽略。

### 11.2 用户/组及子权限上限（后续）

#197 的组与子用户接入同一词树和能力门禁。后续子 Agent 的有效权限还应受父主体/委派上限约束，只能收紧，不能把父级 ask 改成 allow 或扩大主机集合。

普通缺省继承与委派上限是不同概念：缺省是未来个体配置的后备来源；父级上限不可突破。该上限在未来策略合成后执行，不另写一套业务授权实现。

### 11.3 不在本期实现的身份安全事项

连接时自动签发身份、根/子 token 链、级联失效、同一连接的不同子主体、可信身份反查、用户/组创建入组及可选持久化，均在 #197 的身份层处理。它们只向权限层提供可信上下文和上限，不改变动作语义。

本期不开放建组、加组或签发子凭据的 Agent 词。将来若开放，也需注册独立管理能力，不能让 AI 通过建一个更宽组再加入它来扩权。

### 11.4 #196 的短路和内嵌创建

本期不新增反引号执行语法。为其预留 AST 操作计划：未来必须先解析整个内嵌/外层计划，检查实际需要的创建、复用、执行权限，再产生副作用；不能为了求参数先执行内部 create。

get-or-create 命中既有活跃会话时仍检查真实主机和可见域，不能靠“子用户名 + 业务名”字符串冒用另一主体。确定性名字用于幂等，主体和目标由服务端上下文解析；复用与重建分别核对实际所需动作。

## 12. 实施顺序与验收

先接入人/Agent 访问上下文与统一入口过滤，确认 Agent 只可进入 MCP；再实现共享策略服务、动作声明注册表、主机绑定与统一操作过滤。WebUI 只提供公共动作树、公共主机范围、公共别名与人/Agent 对照预览。

各子命令补足语义声明，不逐个复制权限判断。主机标签尚未实现时可先按 ID 选择，但不能接受标签配置后忽略。单 Agent 覆盖不列入本期开发或验收，只保留第 11 节扩展位置。

| 验收情形 | 预期 |
| --- | --- |
| 多个 Agent 发起相同请求 | 使用同一公共策略、主机范围和词汇，不查询个体配置 |
| 保存共享策略 | 全体 Agent 后续检查统一应用新版本 |
| 请求包含个体覆盖字段或访问未实现的个体设置接口 | 明确拒绝字段或不存在该路由，不静默接受 |
| Agent 使用 /sse、/message、/stream 的合法 MCP 请求 | 到达 MCP 后仍检查实际动作 |
| Agent 调用 /api/term、其他 /api/*、WebUI 或 WS Upgrade | 入口过滤拒绝，不进入业务逻辑 |
| 人使用 WebUI/REST/WS | 不套用 Agent 策略，仍遵守资源和部署限制 |
| 允许 start、禁止 rm session | 可连接获准主机，不能删除记录 |
| reader 注册允许、--drop 禁止 | 同一声明表分类，统一过滤器拒绝注销 |
| get file --op realpath 与 --op truncate | 表驱动解析为不同操作，无需独立权限判断代码 |
| 新增行为未登记动作映射 | 覆盖检查失败，运行时默认拒绝 |
| 关闭日志或漏装必要 Filter | 关闭日志不跳过授权；缺失必要 Filter 时不开放能力执行 |
| 某 Filter 返回待审批，但另一项拒绝 | 最终拒绝，不排队执行 |
| Filter 异常、修改参数或试图跳过下一项 | 不放行；改变后的计划必须重新检查，回调不能直接控制执行 |
| DSL 查询直接读取 manager | 改造后仍通过能力分发器，不能绕过 Hook |
| 词树父项含多个子动作 | 展示明确集合，未知新节点不自动授权 |
| 公共词汇只分配 status alias | 全体 Agent 使用同一业务入口，展开动作仍须获准 |
| alias 被修改或公共权限收紧 | 重新解析/复核，不能使用旧允许缓存 |
| 注入新词、嵌套语句或伪造 alias 上下文 | 拒绝，参数只能占声明的值位置 |
| 主机标签/名称变化与存量会话 | 按稳定主机归属和当前公共范围检查 |
| Agent 请求管理动作、秘密原文或自我审批 | 即使通过 MCP 也拒绝，与 hosts=all 无关 |
| 后续个体/组/子用户接入 | 复用过滤器，扩展策略来源而不复制 handler 检查 |
| 未来内嵌创建 | 所需权限未通过时不先创建资源或执行内部命令 |



示例 JSON、目录引用、共享策略与 alias 依赖关系需自洽。实现阶段遵守三 OS CI 与生命周期压力测试要求。本次仅交付权限设计，不含运行时实现与身份签发流程。
