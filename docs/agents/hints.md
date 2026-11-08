# Termcp HINT 编写指南

本文是 Termcp 产品内 HINT、HELP、错误提示、MCP `instructions` 与 Agent 使用指引的中文设计基准。编写或修改这些内容时，以本指南为准；具体操作语法以运行中的 `term()`、`help <directive>` 和 `/api.md` 为准，避免在多处复制会漂移的语法细节。

**能力、DSL 与提示是一体的。** 一个能力做了却没写进提示，等于不存在；一个提示写了却解析不过或执行不通，那是错的提示。所以每次改动都要做到三件事：DSL 语法真的可解析（拿解析器跑一遍示例）、执行真的走能力层出口（包括审批与白名单）、提示与示例与实际行为一字不差。

## 1. 先说明 Termcp 是什么

Termcp 的 `term` 是一个**终端式 DSL**。人和 AI 用短小、可读的命令表达操作；地址用于引用会话、通道、文件、peer 等资源。它不是要求使用者手动组装 SDK 请求或协议帧的 API。

这个模型的目标是降低心智负担：一条命令表达一件事，并可以用 `;`、`&`、`|` 组合成工作流。人类不会手打几十个字段的长 JSON；当一个普通任务看起来需要这样做时，应先检查是否有更高层、更自然的 Termcp 操作。

## 2. HINT 的组织原则

按使用者最需要的信息排序：

1. **原理与数据流**：先用简洁的话说明这是什么、目标和结果如何流动、地址代表什么。
2. **高层意图操作**：优先给直接表达任务意图的做法和可照抄示例。非常规但有用的能力（如工具发现、工具调用、DSL 组合、自定义工具）要明确展示；类终端的常规动作无需写成长篇教程。
3. **协议或底层原语**：按需介绍，并解释它解决什么高层操作没有覆盖的问题。不同 peer 的协议要分清：MCP 工具调用、MCP 原始 JSON-RPC、CDP 和 JRPC 不是同一条通道。
4. **风险与边界**：危险操作说明具体影响范围和安全确认/恢复路径。只有真实安全边界、协议约束或能力缺失确实需要拒绝时才写禁令；每条禁令都要说明原因和可行替代。
5. **精简表达**：在不损失读者正确选择所需事实的前提下尽量简短。少写不是省略关键原理。

正确的能力说明比抽象禁令更有用。知道推荐做法后，读者才知道为什么某些路径不适合，以及被拒绝时该换什么做法。

## 3. 使用 Termcp 时的 Agent 行动建议（优先使用高层抽象）

HINT 和使用指引必须给出明确的行动建议：**先复用现有高层能力；复杂或重复的工作流，按需提升为扩展或 MCP 工具；最后才降到协议原语或一次性脚本。**高层抽象不只是选一个高层动词，也包括把复杂实现封装成有名字、有参数、可复用的能力。

按以下顺序行动：

1. **先检查工具说明和已有能力。** 空 `term()` 看总览，`help <directive>` 看具体用法，`ls ext` 看已安装的扩展（包括 MCP 服务、skills、prompts），`/api.md` 查完整参考。工具自身带详细提醒，先磨刀再砍柴。
2. **一次性、简单任务直接用终端式 DSL。** 用 `run`、`read`、`write` 表达操作，用 `;`、`&`、`|` 组合短步骤。人类能自然输入和看懂的流程，通常也是 AI 更可靠的调用方式。
   - **通道模式要选对。** 非交互、批量命令和脚本优先 `pipe`（`pipe <target>` 或 `open <session> --argv <cmd>`）：无回显噪声，stdout/stderr 可分开，进程退出码可观察。`pty` 只在 TUI/全屏程序、需要观察人机交互、密码/确认输入时使用；pty 没有「命令结束」信号，一次 `read` 是采样而不是完成。复合 shell 命令走变量注入（`--argv sh -c $1` + args/vars），不要把 shell 语法写进 `--argv`。
3. **一条命令能做完的事，不要为了复用连接先开会话。** 无交互的单次命令和文件读写可以直接以主机为目标：`run use:<host> $1`、`read use:<host> file <path>`、`write use:<host> file <path>`。它们借用连接池的传输，不注册会话、不留归档项；代价是没有通道可回读，所以要交互、要观察长任务或要稍后再读时才用 `connect`。这个选择要在提示里讲清楚代价，而不是只讲省了几步。
4. **复杂、重复或需要结构化交互时，提升为高层能力。** 先复用现有扩展或 MCP；如果没有合适的能力，就按需定义扩展，或准备一个提供目标工具的自定义 MCP 服务并通过扩展/peer 接入。把多步业务逻辑、数据处理和协议细节封装在能力内部，终端侧通过发现和调用具名工具来使用；例如 `get <session> mcp tools` 后，用 `call <session> mcp:<n> <tool> {…}` 调用。这样能避免每次重新拼长脚本、复杂管道或协议帧，也让意图、参数和返回结果更清晰。
5. **把别扭当作用法或能力缺口的信号。** 如果一项调用很长、很累，或感觉“本来应该有却没有”，先重新检查 help、扩展与 MCP 能力。如果仍缺能力且人可以回答，简要说明打算执行的工作流并请其确认；这能自然暴露流程或产品问题，而不要求非开发者诊断实现细节。
6. **无人值守或人无法协助时再降级。** 确认没有合适的高层能力后，再用协议原语或一次性脚本兜底；选择最稳妥、最容易审查的方式，并报告缺失能力与关键假设。

## 4. 例子：调用已挂载的 MCP peer

MCP peer 暴露一组可发现、可调用的工具。普通工具调用由 Termcp 负责 JSON-RPC 封包，并可把工具结果解包为文本；先发现工具，再按工具名与参数调用：

```text
get <session> mcp tools
get <session> mcp tools <tool-name>
call <session> mcp:<index> <tool-name> {"query":"error"}
```

协议层操作按 peer 类型使用对应入口：

```text
call <session> cdp:<index> {"method":"Page.navigate","params":{"url":"about:blank"}}
call <session> mcp:<index> '{"jsonrpc":"2.0","id":1,"method":"ping"}'
```

第一条协议调用是 CDP peer 的原生方法调用；第二条是 MCP peer 的原始 JSON-RPC 调用，适用于工具调用以外的协议方法或通知。它们不是普通 MCP 工具调用的写法。实际支持的参数和返回形态以 `help call`、`help mcp` 为准。

## 5. 写进 MCP `instructions` 的原则

MCP `instructions` 会反复进入模型上下文，预算有限。优先写会改变下一步行为的事实：Termcp 是终端式 DSL、先查 help、常见任务用哪个高层入口、何时才需要协议层操作、遇到缺口如何向人确认。复杂语法放在按需读取的 `help` 与 `/api.md`，不要把整份手册塞进每轮 instructions。

每次改 MCP instructions 或 directive help 时，应检查它是否遵循本指南，并用实际解析器/执行测试确认示例语法有效；不要只检查文字是否“看起来像命令”。

### 5.1 常驻文本写成查表，不写成散文

一段话只要进入常驻上下文，它**每一轮都在花读者的注意力**。所以判据不是“写得是否清楚”，而是“为什么用句子而不是符号”。

同一个事实，列表述的代价差一倍：

```text
散文    termcp cannot ask a person: notify reaches open Web UI pages, and delivery is
        not acknowledgement — an answer comes back only as the human's reply or as
        channel bytes. A password, sudo, MFA or confirmation therefore goes out
        through notify and the wait is for that answer.

查表    notify reaches open Web UI pages: it carries a message to a person and brings back
        only their reply or channel bytes — delivery is not acknowledgement.
        password, sudo, MFA, confirmation -> notify, then wait for that answer;
```

第二版短约三分之一，信息一条不少。分歧不在文采，在**谁承担组织工作**：散文让读者从句子的语法结构里拆出事实，查表把事实直接按列摆好。

具体做法：

1. **能写成 `键 -> 值` 的，不要写成“因为……所以……”。** 映射关系是结构，不是叙述。`review_pending=true -> HELD`、`password, sudo -> notify`、`cmd/args -> server log verbatim`，读者一眼扫完；换成“当 X 发生时，系统会 Y，因此你应当 Z”，读者要做三步推理。
2. **删掉连接词。** `therefore`、`rather than`、`and … anyway`、`because`、`only a` 都不携带事实，它们只标记“作者在论证”。压缩时先删它们，通常就能省下三成。
3. **正面陈述起手，不要用否定句开场。** `termcp cannot ask a person` 把一个限制放在第一句，读者先接到的是“做不到什么”，而**限制只有在知道能做做什么之后才有意义**。先说是通道、消息往哪走、回来什么，限制自然落在最后一句。
4. **按字段对齐，不要按句号分行。** 同一类事实排成一列（`review_pending` / `approval_id` / `resending` 三行对齐），读者按列扫；写成三句独立的话，读者要重新建立它们属于同一组的关系。
5. **一段只承担一个主题。** 上面那段散文 43% 的字节花在“人怎么被通知”“凭据怎么走”“审阅怎么排队”“日志怎么记”四件事上。四件事挤在一段，读者无法判断哪一条是前提；拆成四行，每行都可以单独被引用。

### 5.2 重复的副本要删掉，不是改写

常驻文本变长的最常见原因不是“写得太啰嗦”，而是**同一个事实在同一份文本里出现了两次**：

- **回复里每条都带的字段名**（`Waited` / `Content-Range` / `Still-Running` / `Error-Code`）：第一次收到回复就看到了，不需要提前预告。
- **该能力自己的输出会说的话**：`ls` 打印的就是存在的地址，不需要再用一句话保证“ls prints the addresses that exist”。
- **工具结果已经逐字说过一遍的规则**：审阅被扣住时 `reviewWaitTail` 已经写了等待方式，instructions 里只留一条会改变下一步行为的规则（`review_pending=true (approval_id) -> HELD`）。

判据：**这条信息，读者在读这句话之前是否必须已经知道？** 不是，就删——它会在需要它的那一屏出现。

删完要能回答“细节在哪”：每删一处，都要指出读者遇到该情形时会看到它的位置（tool result、`help <verb>`、`/api.md`）。指不出来的，是丢失，不是精简。

### 5.3 描述能力本体，不要只描述协议形状

`CDP: call <peer> {method}` 说的是**怎么调**，没说**它是什么**。一个不知道手里握着浏览器的模型，会去 `curl` 一个页面、拿到静态 HTML，然后以为页面就长这样。

所以每个外部能力至少要交代三件事，缺一个就会让读者在错误的方向上试错：

| 要素 | 反例 | 正例 |
|---|---|---|
| 它是什么 | `call <peer> {method}` | `CDP controls a real browser` |
| 怎么进 | （没有） | `open <session> cdp <url>` |
| 能做什么 | （没有） | `navigate, click, screenshot, eval` |

这三个要素都不构成“第二份真相”：方法名由 peer 自己的 `protocol` 列出，入口由 `help open` 校验，动作名是协议标准词汇。**省略它们省不下字节，只是把成本推给了读者的试错。**

### 5.4 预算要用实测数字，不要用感觉

`internal/mcp/token_budget_test.go` 把上限钉死在字节数上，并且会把当前值打进日志。改完读一次：

```
token_budget_test.go:46: tools/list total: 2451 B
token_budget_test.go:47: instructions: 1612 B
```

"感觉短了不少"不是证据。**省下多少字节、余量还剩多少，是这次改动唯一可核对的收益**，也是判断“还能不能加一句”的唯一依据。

配套的纪律是：**压缩不得改变被测试钉住的措辞。** 断言查的是片段（`resending queues a second`、`polling asks a page, not a person`），不是整句，所以可以重组句子，但不能把片段改写成同义的另一种说法——那会让测试变红，而测试红的理由是对的：那些片段是读者靠它做判断的原话。
