# Agent 权限 V2：操作、参数与主机条件

状态：首批 V2 适配已落地，见第 15 节的交付范围。日期：2026-10-29。源码基线：`2ef2baf15fbac2343eb0fe9ee189f14f3926d238`。

本方案结合 [当前权限实现](permission-management.md)、[早期权限设计](agent-permissions.md) 和 [原实施方案](agent-access-implementation.md)。当前已有的账号、cookie、MCP token、逐 Agent 页面、稳定主机 ID 与动态收权继续使用；只细化业务授权。下文保留完整目标方案；实际可用范围和待实现项以第 15 节为准。

## 1. 结论

把当前 13 个功能组改成**界面分组和批量预置**，实际授权保存为明确的语义操作 ID。每个操作可附加类型化参数条件，主机条件始终受 Agent 总主机范围约束。

```text
这个 Agent → 哪些主机 → 哪个实际动作 → 哪些参数条件
```

例如，同一个 Agent 可以允许建连而禁止删除会话，允许读取 span 而禁止管理 reader，允许写入文件而禁止删除/chmod，允许某个 MCP 工具而拒绝其他工具。

权限不能只根据动词推断：`get file --op truncate` 是截断，`write file --op realpath` 如果现有语法接受则仍是路径查询。不同词面、别名和 raw frame 到达同一个实际动作，必须得到相同业务权限结论。

V2 第一阶段继续只有允许/禁止。委托审批、用户/组与权限继承单列后续工作，不作为提升粒度的前置依赖。当前已实现逐 Agent 配置，不退回早期“只配置共享 Agent”的候选范围；公共配置先用可复制的预置，不新增运行时继承链。

## 2. 当前代码已具备什么，粒度卡在哪里

以下位置均核对过本次基线源码。使用本仓库图谱进行 Tier 2 查询、调用追踪和源码核验；33 个主要证据文件的 coverage 均无已记录问题。该信号不证明全仓调用完整性，本次不是重新进行全量连接点审计。

| 代码位置 | 当前事实 | V2 对策 |
| --- | --- | --- |
| `internal/auth/managed.go:29`，`Actor`；`internal/auth/managed_tokens.go:127`，`admitToken` | 身份来自验证后的凭据；Agent token 注入动态策略读取器，管理员 token 不套用 Agent 业务策略 | 复用身份接入，不重新设计 cookie 或签发 |
| `internal/auth/agent_policy.go:14/27`，`AgentPolicy/functionGroups` | 只保存 Functions、HostMode、Hosts；共 13 个组 | 新增版本化 actions，组不再直接代表执行许可 |
| `internal/auth/agent_policy.go:62`，`effectivePolicy` | 旧记录 Policy=nil 会得到全功能/all-host；UI 新建明确为空 | 明确迁移旧策略；不能继续用动态全量组推导未来新增操作 |
| `internal/capability/agent.go:16`，`AgentFunction` | session_manage 合并 start/terminate/delete/close，terminal_read 合并 output/marks/reader，file_write 合并多种写操作 | 在现有 capability 表上细分语义操作描述 |
| `internal/capability/agent.go:49`，`AgentRequirement` | 主要按 tool 或大 action 分类；file_fs/link/perm 的实际 sub-action 没拆细 | classifier 读取规范 action 和参数，而非只看工具名 |
| `internal/mcp/agent_access.go:126`，`agentStatementCheck` | DSL 按动词/种类分组；随后 handler 路径又按工具分组 | 两个 Adapter 生成同一种操作需求，不保留互相独立的粗分组判断 |
| `internal/mcp/term_directives.go:798`，`fileGroupForOp`；`internal/mcp/term_generated.go:806`，`fileToolForAction` | realpath/statvfs/truncate 同归 file_fs，readlink/link/symlink 同归 file_link | 复用旧资源能力 ID，但 Agent 细分为不同操作 |
| `internal/mcp/term_directives.go:629`，`runOne` | 先 shell_input，再 shell_key，再读取；语句预检只判断 terminal_write | 写入前合并输入、键与实际输出读取需求 |
| `internal/mcp/agent_access.go:66`，`agentToolCheck` | 原权限预检在资源 admission 前；需要审阅的 Agent 操作主动拒绝 | 沿用 Hook 接缝，先完成 Allow/Deny；不把 ask 偷换成 allow |
| `internal/mcp/guard.go:174/197` | 会话解析可能 Touch 扩展，权限目标解析调用了这些方法 | 拆成只读解析与授权后的续租 |
| `internal/mcp/term_mcp_tools.go:31`；`internal/mcp/term_jrpc.go:429` | 工具名调用和 raw JSON 最终都直接调用外部协议 channel | 两种格式统一描述工具名、method、arguments 后检查 |
| `internal/mcp/agent_access.go:251`；`internal/task/task.go:63` | task 鉴权依赖一个 SessionID；复制源被存为 LocalPath 的展示文本 | task 保存完整源/目的主机 ID，不从展示字符串还原授权事实 |
| `internal/sshconfig/identity.go:290/317`；`internal/mcp/agent_access.go:41` | profile 已有稳定 UUID，session 已携带 SSHConfigIdentity；namespace 也参与可达性 | 直接使用现有 ID 和 namespace，不再按旧方案另做名称身份 |
| `internal/auth/permissions.html` | 每 Agent 编辑 13 个复选框；profile_write/extension 限制为全部主机 | 保留页面框架，增加动作树、条件编辑与有效权限预览 |

具体后果：当前 realpath/statvfs/readlink 在外层按 file_read、内层按 file_write 判断。这不是独立的“只读路径查询”授权。给 file_write 也不意味着能够独立表达“允许 write，但拒绝 delete/chmod”。V2 需要解决分类合同，单纯把复选框画得更多不够。

当前 namespace 由验证后的 Agent ID 派生。它仍是资源可见性上限，选中某主机不等于可以接管其他 namespace 的所有 session；V2 不顺手改变这一行为。

## 3. 三层配置，界面不直接暴露大量底层工具名

| 层次 | 人看到的内容 | 后端保存什么 |
| --- | --- | --- |
| 功能族 | 会话、终端、文件、转发、外部协议、主机、扩展、任务、别名、通知 | 展示/筛选元数据，不产生通配授权 |
| 语义动作 | 建连、关闭、删除、读取 span、注册 reader、截断文件等 | 明确 operation ID 的 allow/deny |
| 条件 | 指定按键、最大重复次数、工具名、method、允许修改字段等 | 按操作声明并验证的类型化约束 |

父项显示全选/部分选中/未选中；勾选只展开当前 catalog 的明确子项。升级后新增子项默认禁止，不因为父项曾经勾选就自动开放。读/写/执行可以作为说明和过滤标签，不作为唯一的权限模型。

同一动作默认沿用 Agent 总主机范围；需要时可附加动作自己的 selected 主机子集。该子集只能缩小总范围。第一次交付可以先隐藏这个高级控件，但数据合同要表达清楚，不能设计成可扩大总范围的 override。

## 4. 语义操作划分

保留现有可复用的 capability ID；补充 ID 的目的是拆开旧能力中的不同动作，不要求改 DSL 或为每个函数建一个权限项。下表每个反引号中的 ID 均是独立操作，组合项另外说明。

| 功能族 | 可分别设置的动作 | 需要注意的分支 |
| --- | --- | --- |
| 会话 | `session.list`、`session.info`、`session.start`、`session.terminate`、`session.delete` | connect、close、rm 分开；创建通道/执行程序按实际计划附加需求 |
| 通道 | `shell.list`、`shell.open`、`shell.close`、`shell.exec`、`shell.resize` | shell.exec 指显式指定启动程序/argv；不等于向已有终端输入 |
| 终端 I/O | `shell.output`、`shell.input`、`shell.key`、`shell.marks` | run 是组合；span 映射 marks，旧资源限制仍映射 session.message |
| reader | `shell.reader.register`、`shell.reader.unregister` | reader 与 reader --drop 分开 |
| 环境检测 | `shell.detect` | 当前普通 DSL 没有公开词面，保留已有内部工具映射，不新增 DSL |
| 文件读取 | `file.read`、`file.stat`、`file.realpath`、`file.readlink`、`file.getwd`、`file.statvfs`、`file.urls` | fs/link 的只读 sub-action 不再要求 file_write；URL 不授予 REST 下载/上传权限 |
| 文件修改 | `file.write`、`file.replace`、`file.delete`、`file.mkdir`、`file.truncate`、`file.chmod`、`file.chown`、`file.chtimes`、`file.symlink`、`file.link` | write --old-text、仅 --mode、--op，以及 create --target/--op link 分别正规化 |
| 文件重命名 | `file.rename` | 当前有 handler 映射但没有普通公开 DSL 分支；目录标记内部可达，不制造新入口 |
| 转发 | `forward.list`、`forward.open.local`、`forward.open.remote`、`forward.open.dynamic`、`forward.close` | close --forward-id 也必须归 forward.close |
| 外部连接生命周期 | `mcp.open/list/info/close/read`、`cdp.open/list/info/close/read`、`jrpc.open/list/info/close/read` | 这里的斜线表示各自独立 ID；主机来自父 session，不信客户端声称 |
| MCP 工具 | `mcp.tools.list`、`mcp.tool.invoke` | 具名调用和 raw tools/call 检查同一工具名及 arguments |
| CDP | `cdp.protocol.read`、`cdp.attach`、`cdp.detach`、`cdp.method.invoke` | 原生 Target attach/detach method 不能绕过相应语义许可 |
| JSON-RPC | `jrpc.method.invoke` | 按 method 与参数约束，不笼统允许任意调用 |
| raw 协议 | `mcp.raw`、`cdp.raw`、`jrpc.raw` | 对无法描述的自定义 frame 独立授权；已识别的 tool/method 仍须满足具体动作许可 |
| 任务 | `task.list`、`task.info`、`task.cancel` | get/close task 不再借 session_read/file_write 表达 |
| 主机摘要 | `profile.list`、`profile.get` | 使用安全投影，不等于配置原文/凭据读取 |
| 主机配置 | `profile.create`、`profile.edit`、`profile.copy`、`profile.delete` | 沿用配置管理的部署开关和全部主机范围上限；编辑字段可细化 |
| 扩展使用 | `ext.list`、`ext.get`、`ext.use`、`ext.release`、`ext.retry` | 能看、能启动、能释放、能重建分开；use/release/retry 按存储目标解析主机 |
| 扩展定义 | `ext.create`、`ext.update`、`ext.delete` | 保留全局配置写入的全部主机要求，不用单主机条件放宽 |
| 公共别名 | `alias.list`、`alias.write`、`alias.delete` | 当前 create/edit 都修改全局表；先按实际行为合并 write，不伪造私有别名 |
| 通知 | `user.notify` | 与主机执行、管理面独立 |

`ext.use/release/retry/list/get` 不再因为同属 extension 就全部要求 all hosts；它们只接触现有模板及其所属主机，可按实际目标限制。修改全局模板仍遵守原上限。这是有意细化的行为变化，迁移预览必须提示。

当前 `ls <session> file` 直接拒绝。不要照搬 file_read 的中文“目录”说明就宣称 Agent 已有目录浏览。独立操作目录必须包含可达性：公开 DSL / 内部工具 / 人端专有 / 保留未实现。

## 5. 参数限制怎么设置

动作决定“做什么”，条件决定“这个动作的哪些有效参数可以用”。第一版使用有限的集合、布尔值和整数范围，不开放任意脚本、正则表达式或通用策略语言。

| 操作 | 建议支持的条件 | 示例 |
| --- | --- | --- |
| shell.key | keys、repeat_max | 仅 enter/tab，最多重复 1 次 |
| shell.open / shell.exec | 有效 mode、启动形式 | 允许配置默认终端；拒绝显式 argv；或者只允许受控的直接程序模板 |
| file.read / file.write | 读取/写入形式、单次数据量上限 | 允许内联数据；拒绝 local-path 中转；大小按实际字节计数 |
| file.chmod / chown / chtimes / truncate | 各自的有效数字/字段限制 | 单独禁止 truncate；允许 chmod 时设置有效 mode 范围 |
| forward.open.* | listen host、端口集合/范围、目标 host/port | 仅允许明确的监听位置与端口；不让 dynamic 默认等同任意代理权限 |
| mcp.tool.invoke | tool_names、选定 arguments 字段的类型化条件 | 只允许某几个工具；工具的 destructive 参数必须为 false |
| cdp.method.invoke / jrpc.method.invoke | methods、选定参数条件 | 只允许声明过的方法，不因拿到 peer 就允许所有 method |
| profile.edit | fields | 只修改 description，不准改变目标、凭据、启动程序或 DefaultApproval |
| ext.get/use/release/retry | names | 仅允许人配置的扩展名称；实际目标还必须在主机范围内 |
| alias.write | 是否允许改公共词汇定义 | 不与调用既有别名混成一个开关 |

所有条件作用于**缺省值填充、别名展开和语义解析后的有效参数**。省略字段、显式默认值、等价词面得到相同结果。条件字段若无法被当前操作证明检查到，拒绝保存；不能收下配置后忽略。

`max_bytes` 第一版只对执行前能证明长度的形式启用；未知长度流不能先写到超限再称为无副作用拒绝，应在预检时拒绝该受限形式。字段限制检查请求实际触及的修改字段，额外字段应报错，不静默删掉后执行另一种意图。

第一阶段不增加文件路径 ACL。当前模板 paths 继续生效；如果后续新增路径条件，要处理双路径、符号链接、远端 OS 规则及实际打开文件的一致性，不能拿字符串前缀当作沙箱。

终端输入是发给终端程序的任意内容。允许 shell.input 与提交按键，就可能运行 shell 命令；禁止 Termcp 的 rm session 或 file.delete，不会阻止获准终端中的 shell rm。限制 shell.exec 的 argv 也不约束已经打开的交互式 shell。若需要仅允许运行“查看状态”等固定业务动作，应使用人配置的受控执行模板/专用工具，不用终端字符串黑名单。

方法/URL 条件同样不等于远端网络沙箱。要承诺严格目的地址限制，需要在拨号和重定向处额外执行相同限制；未完成时 UI 只说明其实际约束。

## 6. 策略格式与判定规则

继续把策略保存在现有认证密文，不新增另一个独立 agent-policy.toml，也不重复加密、签发或存储账号。策略使用独立 schema_version，不复用 managedState.Version。

```json
{
  "schema_version": 2,
  "host_mode": "selected",
  "hosts": ["<existing-profile-uuid>"],
  "actions": {
    "session.start": {"effect": "allow"},
    "session.delete": {"effect": "deny"},
    "shell.output": {"effect": "allow"},
    "shell.marks": {"effect": "allow"},
    "shell.reader.register": {"effect": "deny"},
    "shell.input": {"effect": "deny"},
    "shell.key": {
      "effect": "allow",
      "constraints": {"keys": ["tab"], "repeat_max": 1}
    },
    "file.write": {
      "effect": "allow",
      "constraints": {"forms": ["inline"], "max_bytes": 65536}
    },
    "file.delete": {"effect": "deny"},
    "file.truncate": {"effect": "deny"},
    "mcp.tool.invoke": {
      "effect": "allow",
      "constraints": {"tool_names": ["lookup_status"]}
    }
  }
}
```

上面的 UUID 和外部工具名是格式示例；保存时验证主机 ID 与约束的类型/结构，外部工具的实际存在性在调用时判断，预览不为了验证工具名建立网络连接。未列出动作禁止。此例能读终端和写文件，不能 run；session.start 若实际创建通道还需 shell.open，UI 应展示缺少的组合权限。

判定规则固定如下：

1. 先确认验证后的 Actor 及有效策略。RoleAgent 缺少动态策略读取器时拒绝，不能把 restricted=false 当作人端放行；管理员和既有明确的内部调用保留原合同。
2. 资源必须满足 namespace、部署限制、模板 allow/paths 和 Agent 总主机范围。动作自己的主机条件只能再求交。
3. 单个动作：缺少 grant、effect=deny、条件不满足、目标不能解析，任一情况拒绝。deny 不接受附带条件，避免出现不明确的例外排序。
4. 复合计划：所有 Requirement 都允许才能开始；没有“任一子权限允许即可”。
5. 一个 allow 的所有条件是 AND；无条件 allow 表示接受该动作当前声明的所有合法参数。未知新语义参数不能默默进入旧 grant。
6. 不从父分组、通配符或 UI 勾选状态推导未来权限。服务端拒绝未知 ID、未知条件、保留操作和不支持的 ask。

以后若需要多条 allow clause，必须逐条独立匹配，不能把不同 clause 的主机、method 和参数拆开合并成更宽的组合；V2 初版先不加入这层复杂性。

## 7. 复合动作和统一 Hook

| 请求 | 计划要求 | 授权失败时 |
| --- | --- | --- |
| run channel | shell.input + shell.key(enter) + 实际请求的 shell.output | 在输入前拒绝；不把 run 再设成能绕过这三项的独立授权 |
| connect host | session.start + 实际创建通道时的 shell.open + 显式程序覆写时的 shell.exec | 拨号/创建资源前拒绝 |
| open session --argv | shell.open + shell.exec | 启动程序前拒绝 |
| 文件下载到本地 | 源 file.read + internal 的 file.write | 不打开传输、不建后台 task |
| 本地文件上传 | internal 的 file.read + 目标 file.write | 同上；条件分别检查两端，不再笼统要求任意文件读写 |
| read source \| write destination | 源 file.read 或 shell.output + 目的 file.write | 两端主机/条件都检查后再开始复制 |
| get/write file --op ... | 具体 sub-action，不是动词名字 | 不能经另一词面越权，也不要求无关的粗组权限 |
| tools/call raw frame | mcp.tool.invoke + 真实 tool name/arguments | 与具名调用一致；--send、stdio 不能跳过 |

一次语句中的批量目标先全部预检；语句之间仍遵循现有 `&&/||/;/&`，不把整段 DSL 改为事务。预检发现已有拒绝时保证尚未执行；执行期间撤权、断连或资源变化仍可能造成部分执行，必须如实返回阶段，不承诺撤回已发出的字节。

推荐保留现有 Hook 的位置，替换其内部实现：

```text
可信 Actor + 动态策略 Resolver（现有）
  ↓
DSL 解析/值替换/别名展开（现有）
  ↓
PrepareDirective → 规范参数 + Requirements + 目标身份
  ↓
CheckPlan（同一个策略快照）
  ↓
原 DSL 分派
  ├─ 内部 tool → DescribeToolCall → 核对真实调用被计划覆盖
  └─ task / peer / 列表 / copy 等直接分支 → 使用已检查的规范目标
  ↓
原资源 admission、业务实现和结果编码
```

新增小的 `internal/authorization` Module：保存 Operation/Grant/Requirement/Decision 和纯条件判断。`capability` 仍负责旧资源能力、审批元数据及操作分类目录；`auth` 仍负责凭据、Agent、加密持久化与动态策略读取；MCP Adapter 解析资源并生成计划。不要再在第三处复制一张 group switch。

内部合同示意：

```go
DescribeToolCall(tool string, effectiveArgs Arguments) ([]Requirement, error)
CheckPlan(policy EffectivePolicy, plan Plan) Decision
```

MCP 的 `PrepareDirective` 需要窗口参数 win，因为 run 是否真正读取输出受窗口影响；当前只接收 stmt 的 agentStatementCheck 不足以描述完整计划。

对文件、forward、profile/ext 分支，抽出纯“构造规范调用”函数，给分派与计划共同使用。调用方声明语义和参数，不自行决定权限。未迁移的分支保持 V1 模式；不能标成 V2 支持后再漏回旧 group 检查。

`checkAdmission` 的旧资源限制保留，但 Agent 细粒度判断必须使用相同 classifier。DSL 已预检不等于内部 tool 永远免检：真实 operation/主机/参数必须被私有服务端计划覆盖，不能凭客户端提供的 operation_id 或一个 skip-auth 标志执行。直接工具调用则构造单次完整计划。

计划内保存规范对象 ID 和必要版本。实际执行时不重新把别名/名称解析成另一个对象；版本已变化则重建计划并复核。显式拆开 guard 的只读 lookup 和 Touch；预览、拒绝路径不能续租或 Acquire，也不能为查 ID 创建/修复 profile identity。

这仍是局部的语义过滤重构，不要求所有能力先搬进命令对象。未来若做命令模式，命令携带相同 Requirements，策略判断不重写。

## 8. 列表、扩展摘要和任务归属

列表不通过“整个功能族必须都有权”才返回。按具体 list 权限筛选资源和子项，再计算数量并格式化：例如只有 session.list 的 Agent 可以看到会话，不应因为没有 shell.list 就完全不能使用 ls；通道、forward、peer 信息按其具体操作权限隐藏。

`termGuide`、空命令、learn_mode 和错误提示中的动态资源也用同一过滤后视图。静态语法帮助可以解释被禁止的词，不因此赋予执行权；不要为了隐藏帮助重写 parser。

安全摘要继续与凭据使用分开。当前 host getter 返回 private_key/proxy 字段，扩展 getter 的公共序列化包含 headers、URL、command/args。新增面向 Agent 的白名单 DTO，在内层日志预览之前投影；profile.get/ext.get 不自动授权原文读取。人端导出原文仍由管理员主动操作。

task 增加创建时绑定的 ResourceRefs，包含读端和写端的 host UUID、角色与必要路径事实。三个启动点（download/upload/copy）同时写入；task.list/info/cancel 读取这份元数据，session 已销毁时也不靠名称或展示文本猜主机。历史无归属的在途任务不向 V2 Agent 放行，由管理员处理。

后台传输一旦启动，沿用当前生命周期，不因为存入了 Actor 就声称能自动回收所有远端副作用。后续查看/取消 task 依据当前策略；需要“取消谁创建的任务”时另加所有权语义，本次不新增这个 ACL。

## 9. 公共别名与 #196/#197

第一批可以沿用“任何别名展开后都检查动作”，不要求先完成专属词汇管理。要支持 alias-only/分配词汇时，必须同时改正常 `aliases.Expand` 和解析失败后的 `expandErrorCycle`，保留原始入口词、展开链和定义版本。

直接词汇限制和实际操作权限分开。例如可以只允许通过人分配的“查看状态”别名进入，但它展开的 span/read 仍必须获准。禁止底层动作不能靠别名绕过；允许调用既有别名不等于允许 alias.write。

当前 alias 是进程级共享表，不能把 alias.write 包装成“编辑自己的词汇”。专属词汇以后接 #196 的分配合同；#197 的组/子用户通过 PolicyResolver 求交，不能扩大父级主机、动作和参数许可。现有 cookie/token 校验只负责构造 Actor，不把新策略全文写进凭据。

## 10. 审批范围

当前实现对要求资源审阅的 Agent 操作主动拒绝，由管理员在 WebUI 执行；V2 第一阶段继续这一行为，catalog 明确 approval_supported=false，UI 不提供不可用的“需审批”。

以后支持 ask，至少需要：保存原 Agent/credential 和规范计划、固定审批意图、按当前策略重新复核、同时覆盖普通操作回放与 WebUI 直接 ChildShell 输入回放，并解决 staged text/key、无 session 建连以及结果交付。两个回放落点仍分别是 `internal/mcp/approval_gate.go:198` 和 `internal/webui/approval.go:253`。

这些完成以前，不允许仅增加 effect=ask 然后落入现有 withApproved(background) 路径。粒度升级与审批机制升级分开交付。

## 11. 管理接口与 WebUI

沿用当前 `/permissions` 的 Agent 列表、权限/Token 标签页与保存栏。权限页默认展开常用族，危险写操作与 raw 协议单独显示；条件折叠在对应动作下，不把所有设置摊成一张巨大的复选框表。

每个动作显示：名称、对应 DSL、允许/禁止、条件摘要、主机条件、当前可达性。父组只用于批量勾选，显示部分选中状态。增设“这条 DSL 能不能运行”的预览，解释实际需求和拒绝原因，例如：

```text
run <channel> $1
  shell.input       允许
  shell.key(enter)  禁止
  shell.output      允许
结果：拒绝，尚未发送输入
```

建议接口变更：

| 接口 | V2 内容 |
| --- | --- |
| `GET /auth/access` | 增加 catalog_version、操作树、条件 schema、组合 recipe、可达性；旧 functions 字段作为 V1 展示兼容 |
| `GET /auth/agents` | 返回 policy schema_version、Agent revision 和旧策略模式；不伪装成已迁移 |
| `PATCH /auth/agents/{id}` | 使用现有 revision 冲突检测，保存 policy V2；未知字段与不支持条件拒绝 |
| 新增 `POST /auth/access/preview` | 管理员提交拟议策略与 DSL/窗口参数；只做计划与判定，不执行 |
| 新增 `POST /auth/agents/{id}/policy-upgrade-preview` | 展示 V1→V2 展开、差异、危险能力和无法精确等价的项 |

新增端点必须在现有 Managed.Wrap 中明确注册/分派，经过管理员会话与 CSRF 检查，不让它们落入静态页面 fallback。preview 不接受客户端 Actor=admin 来绕过判断，它始终模拟指定 Agent 的业务权限。

## 12. 旧策略迁移，不能简单全量替换

1. 策略引入独立 schema_version；无字段表示 V1。既有 token 和登录会话无需更换。
2. V1 继续按当前路径执行，UI 显示“功能组策略”；不在登录或读取时悄悄转换。新增 V2 动作不自动纳入旧功能组的历史许可，应冻结升级时的旧操作集合。
3. 新建 UI Agent 使用显式空 V2 策略；没有 policy 的旧 Agent 显示 legacy_full，不能误当成空权限或继续对未来操作自动全开。
4. 管理员发起升级预览，先把旧组展开为明确动作，再展示差异，保存时以 Agent revision 做并发检查；保存成功后继续沿用现有取消在途 token 请求的机制，新请求读取 V2。
5. 不能宣称所有转换都完全等价。V1 的 DSL 与工具层可能有额外组交集，例如 realpath/readlink；V2 去掉这些偶然耦合后，单纯展开组会改变可执行集合，必须显式确认。
6. 预览采用保守候选：已确定允许的动作才勾选；只读 fs/link、显式 exec、raw 协议、扩展目标范围和 task.cancel 等单独解释。无法证明等价则保留 V1，不猜测转换。
7. 冻结 V1 的历史可达集时使用确切 canonical 参数类别，不能只冻结 operation ID 后便把一个旧动作未允许的 sub-action 也放进来。迁移测试同时覆盖 DSL、内部工具和别名入口。
8. 旧客户端可以更新 V1 Agent；对 V2 Agent 写回旧 functions 格式返回明确冲突，不能把细权限重新扩大成整组。降级二进制不认识 V2 时必须拒绝读取/启用相应策略，不能丢掉字段后继续运行。

UI 不提供“允许未来新功能”通配项。现有全选只选当前 catalog 的已实现动作。策略更新是授权行为变化，不能夹在纯重构中以“兼容”名义自动放宽。

## 13. 文件与实施顺序

| 阶段 | 文件落点 | 验收结果 |
| --- | --- | --- |
| A：语义目录 | `internal/capability/agent.go`、新增 operation catalog 和纯 action resolver | 动作分类不再依赖 13 组；每个 V2 可达分支有规范描述；旧执行逻辑尚不切换 |
| B：V2 数据和判定 | `internal/authorization/*`、`internal/auth/agent_policy.go`、`managed_tokens.go`、持久化校验 | 版本化 actions、类型化条件、稳定主机 ID、动态 revision；未知默认拒绝 |
| C：统一计划执行 | `internal/mcp/agent_access.go`、`admission.go`、`term_directives.go`、相关 DSL Adapter | 预检复合需求，内部调用与计划核对；移除 V2 的粗组二次拒绝 |
| D：资源细节 | `guard.go`、`term_generated.go`、`term_task.go`、`copy_stream.go`、文件和摘要 handlers、`internal/task/task.go` | 无授权前 Touch/Acquire/Start/Cancel；列表、凭据摘要与任务双端完整 |
| E：管理与迁移 | `internal/auth/permissions.html`、`managed_http.go`、`agent_policy.go` | 动作树、条件控件、组合解释、V1 升级预览；旧 token 不必重签 |
| 后续独立交付 | alias trace/专属词汇、审批恢复 | 解决 #196 和安全 ask；不阻塞前五阶段 |

阶段 A 的局部解析重构保持原参数、返回与副作用顺序；阶段 C 启用 V2 时才引入明确的权限行为变化。无需先进行完整命令模式改造。

建议第一轮垂直交付范围：session.start/terminate/delete、shell.output/marks/reader/input/key、文件各 sub-action 与 run 组合预检。第二轮扩展工具名/method/参数条件、转发与扩展使用。这两轮均使用同一数据模型，不能另增一套专用开关。

## 14. 必须通过的验收样例

| 配置 | 请求 | 预期 |
| --- | --- | --- |
| 允许 session.start，禁止 session.delete | connect 与 rm session | 建连计划条件满足时允许；删除拒绝 |
| 允许 marks/output，禁止 reader 管理 | span/read/reader/reader --drop | 前两项允许，后两项分别拒绝 |
| 允许 input，key 不允许 enter | run | 输入前拒绝；不能因已输入而遗留半条命令 |
| 允许 realpath/readlink，禁止 truncate/chmod | 等价的 get/write --op 请求 | 只读查询允许；修改拒绝；具名工具结果一致 |
| 允许 file.write，禁止 delete/chmod | 写入、rm file、仅 --mode | 只允许写入；不隐式获得其他 file_write 子动作 |
| 只允许部分 MCP 工具/method | 具名、raw、--send、stdio、批量 frame | 全部按规范语义检查；批量任一禁止则发送前拒绝 |
| Agent 只有主机 A，动作仅选 B | 任意对 B 的请求 | 总主机范围不满足，不能靠动作条件扩大 |
| ext.use 只选 A 上模板 | open ext；create/edit ext | 使用按目标判断；全局定义修改维持原上限 |
| 源 A、目的 B 任一无权限 | pipe copy/upload/download | 两端检查，零传输启动；回执后 get/cancel 也检查完整归属 |
| 公共别名展开到被禁动作 | 别名和原词面 | 同样拒绝，不能靠定义变化绕过 |
| 策略收紧/撤 token | 新语句、旧 transport、后续 task 操作 | 使用动态状态并保留现有断连/取消语义 |
| V1 legacy_full 或细权限 V2 | 升级、旧客户端覆盖、二进制降级 | 不隐式扩权；不能无声丢失细权限 |

测试应同时证明允许路径仍工作、拒绝路径没有副作用，并覆盖两种真实 MCP transport。只调用私有 handler 不能代替身份/context/transport 所有权测试。文件和生命周期测试使用临时目录，按仓库要求在三平台执行 race/shuffle；涉及扩展租约、后台传输及并发保存时补 stress。

此表作为逐步实施的验收清单，尚未覆盖的目标见下一节。细粒度授权不会改变已授予终端/代理能力的系统本质，也不把接口权限开关宣传为进程沙箱。


## 15. 首批适配的实际交付范围

本批实现 V2 可配置、可执行的完整纵向路径。没有更换 cookie/token，也没有迁移主机加密存储或加密会话。

- `internal/authorization` 保存 86 个明确语义动作及纯 `CheckPlan`。未列出动作、未知动作、未知条件、`ask` 和条件不满足均拒绝。
- `internal/capability/agent_plan.go` 统一识别内部工具的实际分支和有效参数；MCP Adapter 解析主机、namespace 与资源限制。DSL 的普通操作经原工具 Adapter；run、pipe、peer 等组合/直接调用另做完整预检。`run` 在输入前检查 input、key(enter)、output；建连检查 session.start、shell.open 和显式程序时的 shell.exec。
- 文件动作分别授权，包括 realpath/readlink/statvfs 与 truncate/chmod/chown/chtimes。读写词面不决定权限。按键 keys/repeat_max、工具 tool_names、方法 methods、文件 forms/max_bytes 可设置；仅 catalog 声明过的条件可以保存。forms 使用 inline、local、pipe；replace 只支持 inline，不接受无法证明的整文件写入大小上限。max_bytes 的读取证据来自内部 file_read 的正 length，现有普通 read file DSL 不声明 length，不能用它证明有界读取；该受限形式会拒绝，不改变原读操作参数行为。
- 具名 MCP 工具、raw tools/call、`--send` 与 stdio batch 识别具体工具名。实际协议发送处再次核验解析结果；raw 许可不能覆盖已识别的禁用工具。stdio 发送前也检查其输入/Enter/实际读取需求；CDP 原生 attach/detach 与临时 attach/call/detach 分别核验。
- 主机总范围沿用 stable profile identity。复制在启动前检查两端；download/upload/copy 的 task 记录两端 HostID、namespace、read/write 角色。V2 后续查看/取消依据保存的两端事实，不依赖展示路径或仍然存活的 session；没有完整归属的旧 task 拒绝。
- Agent 主机摘要隐藏 private_key、proxy、default_shell 原文，改为存在标志。扩展摘要保留使用说明及能力元数据，隐藏 command/args/URL/headers；投影在内部工具结果及日志格式化之前。列表过滤主机和 namespace；会话列表按 shell.list/peer.list 隐藏未授权的子项。历史 V1 同样受摘要脱敏和转发列表作用域收紧影响。
- guard 的资源检查只做 lookup；工具实际通过 admission 后才续租，避免预检或拒绝操作续租。Agent 对需要资源审阅的操作继续拒绝，不进入管理员回放路径。
- `/permissions` 新建空 V2 策略，提供分族动作选择、参数条件、主机范围和原 token 页。旧 V1 保留原组编辑器；`POST /auth/agents/{id}/policy-upgrade-preview` 返回保守候选、revision 与差异提示，页面加载候选后仍需明确保存。没有自动升级；旧客户端不能把 V2 写回功能组。
- 策略仍保存在现有认证密文。HTTP 与重新加载都拒绝未知策略字段；schema_version 未识别时校验失败。已有 token 继续动态读取当前策略。

本批没有实现：动作各自的主机子集、profile.fields、ext.names、端口/目的地址条件、外部工具 arguments 字段条件、DSL 无副作用预览端点、词汇分配/子用户组继承、Agent 审批回放。对应控件和不支持的条件不会假装可用。上文中的完整计划绑定及名称/版本并发复核仍是后续工作；本批对普通工具在有效参数构造后重新判定，对直接协议发送在解析后重新判定，不把一次预检当作可复用的绕过凭据。

验证覆盖真实 HTTP/SSE 认证入口、原始与具名工具形式、拒绝前无副作用、别名、主机边界、任务双端、策略持久化和 V1 兼容。完整测试与 stress 执行结果由本次变更交付记录说明。
