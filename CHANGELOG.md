# Changelog

## Unreleased

### 修复

- **Windows 上「shell 已退出」的状态会永久丢失：磁盘永远停在 running，重启后已结束的 shell 变回活会话（#92）**。这是从一条随机失败的测试（`TestManager_ShellExitStatusIsPersisted`）里查出来的真实数据损失，不是测试报错了事：同一台机器上磁盘追平内存通常只要 ~10ms，但偶尔**永远不追平**。内存说 exited、磁盘说 running，而且失败的目录里 `updated_at == created_at` —— 说明那份 shell manifest 自创建后再没被重写过一次。

  根因是 Windows 与 POSIX 在「改写一个正被读的文件」上的行为差异。manifest 用「写临时文件再 rename 覆盖」发布，这在 POSIX 上即使别人正开着旧文件也能成功，而 Windows 会直接失败：`rename ... manifest.json: Access is denied`。而 manifest 正是这个形状——目标文件总是存在，且从进程外读它是正常操作（它们是磁盘上的普通 JSON，本项目的测试就靠轮询它来等待写入）。实测在热读下 **431 次 rename 失败 178 次（41%）**，不是什么罕见竞态。真正把「一次失败」变成「永久丢失」的是调用方：`persistOne` 把存储层的错误 `_ =` 丢掉了，于是那次写入没了就是没了，没有任何重试、没有日志。

  修复分两层。存储层 `renameOver` 对**唯一一个值得重试的失败**（Windows 的 sharing violation：`ERROR_ACCESS_DENIED` / `ERROR_SHARING_VIOLATION`）做有界退避重试（1,2,4,8,16ms，共 31ms），其余错误原样返回——权限、磁盘满、文件不存在这些重试只会推迟报错。重试在这里是安全的，因为操作幂等：临时文件里仍是完整的新内容，重试成功发布的正是那次失败本该发布的东西。POSIX 上一次都不重试（那边没有这个失败模式），常量与判据都放在 `rename_windows.go` / `rename_other.go`，所以不会把等待预算泄漏给从未需要它的平台。会话层的 `persistOne` 不再吞错：写失败会带 session/shell id 与状态记进日志——重试挡住的是那一个瞬时失败，这条日志是让**其余任何**失败不再静默消失的原因。

  验证分四层。**L1**：新增 Windows 专属回归测试，用一个真实的打开句柄把 manifest 按住一段时间，断言写入等到句柄释放后发布、且磁盘上的字节确实是 exited（不是「没报错」——报成功而什么都没发布正是这个 bug）。**L3 反事实**：拆掉重试（改回 1 次）→ 立刻以同一个 `Access is denied` 失败；保留重试但让「是否值得重试」的判据永远返回 false → 同样失败。**L4**：用之前能稳定复现丢失的探针（4 路并行、每路 30 轮）重跑，修复前 4 路中有 3 路各丢 1 轮，修复后 **120 轮零丢失**；原作者测试 `-count=30 -cpu=1,2,4 -shuffle=on` 与全包 `-count=6 -cpu=1,2,4 -shuffle=on` 均稳定（修复前全包压力下曾 2/4 次失败）。**平台横切**：新增 POSIX 专属测试，断言 rename 一个正被打开的文件既不等待也成功、且 POSIX 上没有任何错误会被当成可重试；并交叉编译 linux/darwin 的测试二进制确认两组 build tag 各自只出现在自己的平台上（Windows 二进制里没有 POSIX 测试，反之亦然）。
- **平铺的终端不再盖住页面顶栏，拖到屏幕外的窗口也弹回顶栏下面（#88）**：顶栏（wordmark、文档与语言入口）是页面的框架，但平铺层 `.pane-workspace` 是 `position: fixed; inset: 0; z-index: 10000`，一开平铺整片终端就压在顶栏上——顶栏是回主页面、切语言的唯一入口，被盖住就只能先退出平铺。拖动浮窗只按窗口容器回收（容器又是 `inset: 0`，等于整个视口），所以松手时窗口可以停在顶栏上，同样把页面自己的控件挡了。

  修法是**量一次、三处共用**：`shell-windows.js` 量出顶栏下沿并发布成 `--app-header-h`，平铺层的 `inset` 用它作上边距，拖动回收与新建窗口落位的「上边界」也用它。没有把 57px 写成常量，因为顶栏不是定高：窄屏时导航会折行、切语言会换掉标签文字、Web 字体会在首帧之后才落地——三种情况都会改变它的高度，所以用 `ResizeObserver` 盯住顶栏本身而不是只听 window 的 resize（后两种它都收不到），顶栏一变高就重发变量。页面没有顶栏时变量回落到 0，退化成原来的全视口行为。

  这条修复**没有改动最大化与移动端全屏**：它们的语义是「占满屏幕」，本来就要盖住顶栏。

  验证分两层。几何层：两个 node 探针直接把真实函数切片进 sandbox 跑（不是打桩）——拖动用例断言顶栏 60px 时「丢在顶栏上 / 丢在顶栏带里」都落在 y=60、顶栏长到 120px 时 floor 跟着上移、窗口比剩余空间还高时以顶栏下沿为准；落位用例断言点在顶栏里的点击不能把窗口放到顶栏下沿之上。**六条反事实逐条撤掉修复的一半，全部被抓**：撤掉 drag 的 floor、撤掉 placement 的 floor、CSS 改回 `inset: 0`、发布常量而非测量值、去掉 `ResizeObserver`、让测量函数返回固定 57px——最后一条尤其说明「量」的必要性：固定值在真实页面里就是错的，而只看几何的测试抓不到。**另有 `node --check` 确认改后的整份 JS 仍能解析、一个自包含探针确认浏览器真正收到的资产里带着修复**（打桩测试可以在源码正确时依然放行一份没生效的资产）。
- **SSH 断了但会话没有活着的 shell 时，会话永远停在「运行中」，从不进归档（#89）**：会话是连接容器，shell 只是容器里的一个终端 —— 敲 `exit` 结束的是终端，不是那条 SSH 连接（连接还能开新 shell、做转发和 SFTP），所以容器保持运行是刻意的。但检测「连接死了」的那套等待**全部挂在某个 shell 的 watcher 上**（`shell.go`、`shell_output.go` 各一处 `<-execSession.Done()`），容器一旦没有活终端就无人守望：远端 sshd 消失、网络断开都不会被察觉，卡片继续亮着 running。实测 25 秒不变，且此时连新开 shell 都返回 `new session: EOF` —— 卡片显示的状态是假的。

  现在会话自己盯住传输层：`sshclient` 暴露 `WaitTransport()`（包装 `ssh.Conn.Wait()`），`Session` 在创建时起一个独立守夜人，连接关闭即 `markDead`。**这不是新增 channel，也不占 fd** —— `Wait()` 挂的是 x/crypto mux 的条件变量（`sync.Cond`，断开时 `Broadcast`），所以多个观察者可共存（每个 shell 自己的 watcher 照旧工作），阻塞期间零资源、零网络开销。实测守夜人阻塞时仍可正常新开 shell 并执行命令，5 个并发等待者全部被唤醒。

  `markDead` 幂等，所以 shell watcher 与守夜人谁先看见都只记一次；故意关闭（Terminate/Disconnect/Delete）先置 `closing`，不会被误报成网络掉线。回归测试先让 shell 自己退出（容器必须仍为 running，锁住容器契约），再杀掉服务器，断言会话转为 DEAD——**在干净 HEAD 上同一测试会卡满 20 秒并以「session still \"running\" ... never move to the archive」失败**。

- **`session_start` 新增 `on_exit`：一次性命令跑完可以自动归档，不再在会话列表里堆积（#90）**：Agent 常为了跑一条命令就开一个会话，而 `mode=pipe` 的命令毫秒级退出后，容器仍按上面的容器契约留在「运行会话」里 —— 与「有人正在用」的会话长得完全一样，于是一次性会话越堆越多。

  新参数 `on_exit` 三档语义（实际两档，见下）：`"keep"`（默认）= 现状，最后一个 shell 自行结束后容器继续运行、连接可复用；`"close"` = 最后一个 shell 自行结束时 terminate，会话转入归档。**两种取值下输出都仍可用 `shell_output` 读取** —— 归档不等于丢弃，这正是当初开这个会话的目的。

  触发点是 shell 退出 watcher 里的 `closeIfLastShellEnded()`：在 `shellStateMu` 下检查是否还有 `running` 的 shell，因此与「正在新开 shell」互斥——要么新通道先落地（会话保留），要么它看到 `closing` 而被拒绝，不可能往一个正要关闭的会话里加通道。

  参数校验拒绝拼错的值（`on_exit="clsoe"` 报错而不是静默按默认处理）；`normalizeOnExit` 把任何未知值收敛为 keep，所以一个手误不会静默掐断用户的连接。回归测试三条：`close` 归档且输出仍可读、默认 `keep` 必须保持运行（反向保护，防止把不想要的会话也自动关掉）、非法值被拒绝——三条各自退回修复后都确实失败。另有一条 wire 守卫断言 `on_exit` 真的出现在模型看到的 `session_start` 描述与参数 enum 里：模型看不见的参数等于没有（**为它腾空间把 4 条未被任何测试引用的描述无损压缩了 29 B，工具描述余量从 4 B 回到 18 B**）。

  实测也覆盖了远端路径（真实 TCP sshd）：`on_exit="close"` 在 remote 会话上同样正确归档。探针顺带确认了一个既有行为：`Terminate` 对 remote 会话只关 channel（`CloseSessionOnly`），共享 SSH client 由 `Disconnect` 显式关闭——这与 `on_exit` 无关，`session_terminate` 一直如此。

- **关闭「连接中」占位窗口现在真的会中断拨号（#79）**：占位窗口上的关闭键一直写着「关闭（取消连接）」，但取消的只是浏览器那个 fetch——服务端从未看到中止，照旧把 dial 拨完。**迟到的拨号成功后会注册一个无人持有的会话**：页面上的占位窗口已经没了，它却真实存在（占着远端 shell、出现在会话列表里、Agent 通过 `list_sessions` 也能看到）。一个「取消」如果留下活会话，比不取消更糟——用户已经被告知它停了。

  现在请求上下文一路传到拨号：`DialConn` 用 `DialContext`，而握手阶段（x/crypto 不吃 context）由 `watchCancel` 在 ctx 结束时关掉 socket，把阻塞中的读唤醒——只有关闭 socket 这个信号能穿进去，所以取消才会立即生效而不用等拨号自己的 30 秒超时。同一个 `Config.Ctx` 也让「测试连接」按钮与 MCP 的 `session_start` 在调用方收手时停下来。拨号刚好在取消同一瞬间完成这种竞态也被堵上：`session.New` 在注册前重查 ctx，取消后不会留下任何会话。

  取消不再按错误级别写日志：那是调用方撤回请求，不是目标主机的故障，否则每关一个占位窗口都会报一次「连接失败」。

  回归测试用一台**故意拖延但最终会接受**的 SSH 服务器：拨号必然成功，所以只指向不可达主机的测试即使有 bug 也会通过。测试断言取消后会话数为 0——去掉修复后它确实以「canceled request left session ... (status=running)」失败。`sshclient` 层面另有一条测试卡住握手中的取消（没有 `watchCancel` 会挂到超时）。

- **`message` 的描述解释 `status` 五个字母的含义，Agent 因此能判断「人是否操作过终端」（#87）**：`message(action=list)` 返回的每个区段都带一个单字母 `status`，而这个字母**只写不解释**：`toolopts.go` 里模型看到的那份描述只说「status, time, and byte offsets」，`docs/mcp-tools.md` 只列了 `o`/`a`/`i` 三个（`q`、`A` 两个从 4b2e400 起就在日志里，文档从未提过）。`i`（人的输入）与 `a`（AI 的输入）正是「人是否碰过这个终端」的答案，而**两者在字节日志里长得完全一样**（终端回显不区分来源）——描述不说，Agent 就拿着一堆读不懂的数据。

  现在模型可见描述列出全部五个取值（`o` 输出 / `a` AI 输入 / `i` 人输入 / `q` 请求审批 / `A` 审批通过并写入），并直说 **`i` 或 `q` 即「人操作过」的证据**；长描述额外说明为什么这值得读（回显不分来源）、以及输入区段是**零长度标记**（`start == end`，字节在回显里、不重复写入）。`docs/mcp-tools.md` 补齐五个取值与两处细节：`q` 后面不一定有字节（审批被拒/超时就不产生输入），标记的是**提交那一行**的时刻而不是开始打字。

  回归测试不是文字匹配：它把一段 AI 输入（走 `shell_input`/`shell_key`）与一段人的输入（走浏览器 WebSocket 同一条 `SendTerminalBytes` 路径）打进同一条日志，再用真实的 `message(action=list)` 读回来，断言描述里命名的两个字母确实是服务端写下的那两个（并把 `a`/`i` 区段断言为零长度）——一份与 handler 漂移了的描述仍然能通过纯字符串断言，而漂移正是这里唯一的风险。

- **`ssh_config` 的模型可见描述给出导入/导出文件格式，并指出嵌套 bastion 这个坑（#81）**：Agent 经常被要求把用户粘进来的一串主机整理成「能导入的文件」，但文件格式此前只存在于 Web UI 与 `docs/api.md`，MCP 侧一个字也没有——`ssh_config` 的描述只说 `action=list names, or (if enabled) create/edit/copy/delete`。

  写这段格式时实测出一个真实的坑：**文件格式不是本工具的参数形状**。参数里 bastion 是平的 `jump_host`/`jump_user`/…，而文件里必须是嵌套的 `[connections.jump]`。把参数拼法写进文件**解析不报错、但 bastion 被静默丢弃**（`ent.Jump == nil`，导出的文件里也没有它）——直到真的拨号才发现连不上。描述里因此明写了 `[connections.jump]` 与 `[connections.jump.jump]`，并说明平的 `jump_host` 是本工具的参数、不是文件字段。

  格式规格写在 `sshConfigImportFormat` 一处，同时拼进**两种** `ssh_config` 描述：只读列表的那份，以及 `RegisterSSHConfigWriteTools` 在 `--mcp-manage-ssh-configs` 下**整段替换**的那份。后者是首次实现时差点漏掉的一半——它不走 `compactToolDescriptions`，而是直接赋值，所以在唯一允许 Agent 写 profile 的部署下，模型看到的描述里原本不会有这段格式。回归测试对两种列表都断言，正是它把这一半拓住了。

  测试不只匹配文案：`TestSSHConfigDescriptionAdvertisesTheFormatTheStoreAccepts` 把描述里宣传的形状（含嵌套 bastion）丢给真实的 `sshconfig.Store` 导入，再导出、再导回，逐项确认 bastion 存活——一份与解析器漂移了的描述仍然能通过纯字符串断言。`docs/mcp-tools.md` 同步补上完整 TOML 示例与可选字段清单。

- **告诉 Agent「等人在终端里输入」时用 `shell_notify` 挂监听，而不是空转轮询（#82）**：命令跑到一半停下来等人（`sudo` 密码、交互式安装器、任何必须由人在终端里回答的提示）时，Agent 此前只有一条路：反复 `shell_output` 轮询。工具面里没有任何一处说明还有别的做法——`shell_notify` 的描述只说它“管理通知规则”，规则 9 只说它“能反向唤醒”，而 `notify_user` 的描述里写了「问完要 WAIT」却没写「wait 的时候靠什么被叫醒」。于是这几句话合起来反而在鼓励轮询：一条看起来能收通知、但没人说什么时候该用它的工具，不会被用。

  现在三处都写明了这个用法：`shell_notify` 的模型可见描述（`toolopts.go` 的紧凑文案，即真正发给模型的那份）与长描述都补上「人停在提示符前（sudo/密码、交互式安装器）时，先 `notify_user`，再在这里注册 `event=output`，然后停止轮询——人的下一次输出会唤醒你；人答完后 `unregister`（shell/会话关闭也会自动级联清理）」；initialize instructions 的规则 9 尾随一句「Human at a prompt: notify_user, then register event=output」；`docs/mcp-tools.md` 新增「典型用法二（等人输入）」的完整四步。选 `output` 而非 `silence` 是刻意的：人去碰提示符时会产生输出（密码回显被关掉也仍有换行），而 `silence` 描述的是「没人说话」，正好把等人的场景排除在外。

  两条回归测试按真实线上载荷断言：`TestShellNotifyDescriptionTeachesWaitingOnAHuman` 走 `tools/list` 拿描述，而不是读 `tools.go`——因为 `compactToolDescriptions` 会在发给模型前把长文案换掉，只查源文件会在模型实际什么也没收到的情况下通过；`TestInstructionsPairWaitingOnAHumanWithNotifyUser` 除了断言规则 9 在，还断言它排在规则 5 的 `HARD BOUNDARY` 之后——只读到「挂监听」而没读到「先通知人」的 Agent，要么继续轮询，要么默默等着。两句都实测过：把文案改回去时对应测试失败。instructions 的 2400 B 预算只剩 78 B，因此规则 9 只加了一句。

- **连接对话框不再把上一次的状态留给下一个 profile，并清掉三处只写不读的对话框状态（#83）**：六个 modal 都是同一个常驻 DOM 元素，每次打开只切一个 class——于是凡是「下一次打开不会重写」的字段都成了残渣。最坏的是测试判定：`#conn-test-result` 的结论从不清理，在一个从未拨号的主机下面写着「✓ Connected in 12 ms」，用户读到的当然是当前这个 profile 的答案。「Test」按钮可能停在上一次的 `disabled` + 「Testing…」上，重开对话框像是永远在忙；form/TOML 的**图标**不跟视图复位，重开后图标和视图互相说谎；`#modal-conn-err` 只清 `display` 不清文本；`#modal-conn-import` 继承上一次导入的读数与半途禁用的按钮。

  更难看见的一半是慢响应：profile 的 GET 与 Test 都是无上限的拨号，旧代码没有任何守卫，于是「打开 A → 慢请求在飞 → 打开 B → A 的响应落地」会让 **A 的表单画在 B 的名字下面**。`openConnModal` 现在推进一个代数计数器（`#modal-conn._termcpOpenSeq`），profile 读取的成功/失败两条臂与 Test 的三条臂都比对该代数，关闭对话框也推进一代——属于某次打开的结果不会再画到另一次打开上。

  另外三处状态只有写入者、没有读者，其读者早已被删掉：`_connDirty`（第 12 个写入者还在，唯一的读取者在 5089e71 把离开页守卫收窄到「有终端窗口打开时」时被删除）、`_fwdSshCfg` 与其 hidden 字段 `#fw-ssh-config-modal`（服务端自己从 session 推导 `ssh_config`，这值只被写进一个没人读的 input）、`startConnName`（与 `#start-ssh-config` 重复，且两处写入，落败的那个只可能和赢家不一致）。

  顺带修掉两个同类缺陷：转发对话框的「没有会话」路径在重绑 `_fwdSessionId` 之前就返回，于是一个写着「没有会话」的对话框上按「Create」，会把转发建到**上一次打开的那个会话**上；启动对话框的目标存在 hidden 字段里，现在一律从入参写入，不再可能沿用上一个 profile。

  `internal/webui/modal_state_test.go` 用 node 跑**真实的** `openConnModal`（沿用 `host_attribution_test.go` 已有的切片边界）而不是做字符串匹配，并覆盖残留清理、慢响应丢弃、只写不读状态与两个对话框的重绑顺序；把修复 stash 掉后这些测试全部失败。

## v0.2.6 — 2026-10-07

### 本版要点

- **Web UI 变成工作台**：访问列表从「点开才有的浮层」变成常驻的 **NetHub** 侧栏（宽屏展开、中屏收成状态灯轨道、窄屏仍是左侧抽屉），会话拆成「运行中」与「已归档」两块板，节点卡回答「这是哪台机器、能不能连上、上面有几个会话」。
- **批量操作**：NetHub 一个开关进入选择态，一次级联开窗多台主机、按选择集导出配置、逗号分隔批量删除。
- **连接配置可整体搬运**：TOML 批量导入/导出（`/api/connections/batch`），并新增**临时主机**（只存内存、进程退出即消失）。
- **定位符（`termcp://`）在所有接受 id 的 MCP 工具上真正生效**，同时修掉一个审批绕过——同一个写操作，用定位符写法可以跳过人工审阅。
- **频道编号终生不变**：关掉靠前的频道不会让后面的编号前移，复制过的 `termcp://#<会话>:N` 永远指向同一个频道。
- **时间轴轨道（rail）与真实终端对齐**：清屏、滚动缓冲上限、行号坐标系全部按 xterm 的实际行为重做；大日志重放从 7.2s 降到 0.19s。
- **终端字体支持中文**：字体栈补上 CJK，Kali 这类最小化系统不再把汉字画成超宽点阵（#77）。
- **Agent 侧更稳**：工具 handler 里的 panic 不再杀掉整个进程；`notify_user` 的模型可见描述写明「问人之前先通知人」。
- **会话内核与存储更省**：每个日志一个写者 goroutine；manifest 未变则不重写（100 会话一次 persist 从 1380ms 降到 13.8ms）；shell 的退出状态落盘。

### 新功能

- **NetHub：常驻的资源侧栏，而不是一个要点的按钮**：访问列表原先只是布局轨道里的一个按钮，点开才覆盖出来——宽屏上一组常年要用的机器每次都要点一下才能看见，关着的时候又什么都不回答。现在它是页面的控制层：工作区旁边的 `<aside>` 列，收起时不消失而是缩成一条**状态灯轨道**。宽屏默认展开、中屏默认停在轨道、800px 以下仍是左侧抽屉；桌面两种形态由同一个布尔值（`setNetHubCollapsed`）驱动同一个长度变量（`--nethub-w`），所以收起是让工作区重新排布而不是把它盖住，手动选择沿用既有的 localStorage 记忆。

  节点卡回答的是「这是哪台机器、我能不能连上」：身份、Termcp 拨号的协议、运行状态、地址元数据、以及它上面有几个活会话。服务端没有 per-host 健康字段，所以状态**由页面已经收到的证据推导**——正在拨号的窗口是「连接中」，有 running 会话是「在线」，一个会话都没有意味着 Termcp 目前没有活的通路；逐帧探测每个已存主机不是选项。轨道保留同一个答案：每个节点一盏灯、有窗口打开的节点带标记、外加一个展开键；点一盏灯会直接揭示它命名的那张卡，而不是展开成一个显示不了它的列表。

  会话卡片的归属改为按 **SSH profile**（`ssh_config` 字段，非机密的标签）判定，而不是显示名——显示名是用户可改的，重命名过会话的灯不该灭（`api.Session` 新增该字段，`docs/api.md` 同步）。

- **运行中 / 已归档两块板**：「还在跑什么」和「我跑过什么」是两个不同的问题，却挤在同一个列表里。现在会话板只列 running，下面一块归档板承接其余；两块板各有自己的选择集、计数和垃圾桶（垃圾桶是隐藏而不是禁用——它本身就是「有东西被选中」的信号），选择与删除按区域独立，批量删除路径共用，因此两块板不会各自漂移；已经离开服务端注册表的选择集会被清理一次，两块板一起。

  会话板的「+」新建卡片改为**空态读数**：没有活会话时，网格原先是「一张虚线创建卡 + 一行空文案」，同一个问题两个答案，其中一个还是通往 NetHub 那扇门的第二个入口。现在无行的板会说清为什么无行，并且在收到服务端第一份快照之前不表态——在那之前每块板都是平凡为空的。

  工作区的框架改成**三轨网格**（NetHub 宽、内容、同样的宽），只有中间那轨伸缩。此前 NetHub 键作为 flex 兄弟会和两块板抢宽度，标签最长的那个翻译决定它挤扁它们还是换行；镜像的两端也正是让两块板在 dock 上居中的原因，并且保留量与实际贴靠都读同一个 `--dock-pad-x`，跨断点跟着走。800px 以下网格堆成一列并丢掉尾部保留轨，而不是留一行空的。

- **NetHub 批量选择：一个开关，一行之内对节点做全部批量操作**：工具栏原「导入/导出」两个全局按钮重构为一个批量选择开关（列表勾选图标）。收起时点击节点卡直接连接（与从前一致）；开关激活后缩为行内最左侧——选中任意节点后**开关的脸直接变成选中数**——播放键在其右侧展开：播放键主体为「打开所选主机」（逐个 150ms 级联开窗，避免同瞬窗口糊成一堆），其下拉菜单携带「导出配置」（只导出选中节点，与全量导出同 TOML 格式、可导回）与「删除」（逗号分隔批量 `DELETE /api/connections/{names}`，逐名返回结果，一条失败不影响其余）；反选键紧随开关右侧：镂空勾图标，有选中后变实心勾，空选时反选即全选。

  工具栏这三个键（开关、反选、播放分键）**与面板是同一块板**：切角、折角与角落描边全部来自 `theme.css` 唯一那条 `clip-path: polygon(...)` 规则，本文件不为它们另写多边形。它们只声明“画在什么底色上”（描边是 background-image 层，透明的键会让描边压在下层内容上）。此前这三个键被一条**同特异性、源码更晚**的规则重新钉成“关闭折角、7px 切角”，而单个类选择器打不过源码顺序，于是多边形整个消失、只剩直角描边——看上去就像样式被删了。现在那条覆盖规则已删除，键回到 `--cut: 14px` + 折角 + 底板填充。为此 `notch_geometry_test.go` 新增 `effectiveKnob()`：不再只问“选择器在不在规则里”，而是**按级联解析**每个键最终拿到的 `--cut/--notch-left/--notch-run`，这正是原先漏掉回归的盲区。

  下拉菜单挂在分键的**兄弟层**上而非其内部：多边形是 `clip-path`，而 clip 会连同**后代**一起裁掉，菜单本该出现在按键下方、正好被裁没。现在板在内层 `.nethub-open-plate`（承载多边形），菜单是它的兄弟，不再被裁。菜单锚定 `right: 0`——触发它的是右端的箭头，原先 `left: 0` 让它跑到整条键的远端下方，看起来像“弹到别处去了”；`min-width: 100%` 让菜单至少与按键同宽，菜单项恢复正常高度。（`.nethub-open-split button { height: 100% }` 曾连菜单项一起命中，按特异性把每项压成文字高度；该规则现已限定到 `.nethub-open-plate`。）

  未选中任何节点时播放键与菜单项处于禁用态；再次按下开关或按 Esc 退出并清空选中（有下拉先收下拉），**退出时会按选择集逐卡同步 `batch-selected`**——此前只清空集合、不摘类名，关掉再开会让旧卡片带着残留类重新点亮而计数为 0。选择不使用复选框，进入选择模式也不改变任何卡片的外观（编辑/快启/复制按钮原位保留）——点过的卡片点亮 accent 描边、左侧扫描条与节点名；选择只重新着色，绝不改动底板的多边形形状（切角几何仍由 theme 统一声明，选中态用 `background-color` 而非 shorthand，避免重置角落描边层）。选中状态存于模块级 `_nodeSelIds` 并在每次重画时重新套用，因此**切换开关不重建网格**（只翻类名与换 tooltip），会话帧触发的重画、语言切换也都不丢选中。内置回环节点同样可选（「打开」对它有效），但选中含它时点导出/删除立即报错——前端直接 toast，接口同名限制：`GET /api/connections/batch?names=` 含 `internal` 返回 400，批量删除对它逐名报 `reserved_profile`（新增 `sshconfig.ErrReserved` 哨兵，`IsInternalName` 一并导出，前端不再重复判断与复述文案）。底部「新增接入」由独占一行改为与导入键同行（导入改上传图标、仅图标，行宽让给新增）。新增测试覆盖导出按名过滤（含 internal 拒绝）、批量删除逐名结果，以及四项结构性回归测试按新决策更新（切角面板组、底行动作、抽屉侧入口的中置开窗）。

  嵌入式静态资源现在带 `Cache-Control: no-cache`：embed 里的文件没有修改时间，`http.FileServer` 既不发 `Last-Modified` 也不发 `ETag`，浏览器于是**启发式缓存**上一版二进制的样式表，用旧规则渲染新标记，直到硬刷新——这正是本轮改样式时反复看到的假象。

- **批量导入/导出连接配置，以及只活在内存里的临时主机**：`GET /api/connections/batch` 把远端 profile 导成一个 TOML 文件（**含**临时主机、**不含**内置 `internal`；文件里有凭据，按机密处理），`?names=a,b` 只导指定几个，其中已经不存在的名字跳过而不是让整份文件失败。`POST /api/connections/batch?temporary=false` 导入同一格式（请求体上限 16 MiB）；重名、大小写不同的重名与保留名 `internal` 会拿到 `-2`/`-3`… 后缀，**既有 profile 永不被覆盖**；任何一个 profile 非法都在写入前拒绝整批。Web UI 把文件原样交给这个端点，解析与校验都在服务端。

  `temporary: true` 标记一个只保存在进程内存里的远端 profile：它照常出现在 REST / MCP / Web UI 里，直到 termcp 退出；**从不写入** `ssh_configs/`。`PUT /api/connections/{name}?temporary=true|false` 可显式指定，省略时既有 profile 保持当前存储模式、新建的持久化；内置 `internal` 不能是临时的。批量导入/导出与临时主机共用同一套 store，因此临时 profile 的改名、导出、删除与普通 profile 走同一条路径。

- **Blackwall 动态背景与玻璃材质 token 分层**：页面底下铺一层可替换的 Blackwall 图层，表面透过它取色——板、表头、会话卡改用 glass token，且**由板这一个元素做模糊**，所以调整玻璃就是整体一起调，22 个会话的列表也只付一次 filter 而不是 22 次（画布颜色移到 `html`，否则 `body` 自己的背景会盖掉负 z-index 的图层）。`icons/blackwall.svg` 是这套槽位围绕的占位文件：替换该文件就是全部接入流程。随后背景换成成品自包含插画，只用一个 opacity 动画让它呼吸，`prefers-reduced-motion` 下关闭该装饰。

  Blackwall 的反应**从页面已经画出来的东西推导**，不新增状态源：它读频道 chip、待审计数与窗口的连线标记——断线压过工作、排队等人压过 Agent 活动、Agent 正在写的一行压过普通打字——因此新增一个写入者也不会让背景与它描述的页面不一致。扰动是叠在最前台终端上的一块合成层，绝不是给插画加 filter，所以流式会话不为它付任何代价。

  颜色 token 拆成两层：调色板拥有每一个色值字面量并携带该色的 RGB 通道（半透明用途只取色相、只加自己的 alpha），语义 token（`--accent`、`--success`、`--warning`、`--danger` 与 rail 的各状态）命名含义并指向调色板而不是重述数字。去掉重复字面量顺带修好了「一个粉色三种拼法、一个藏青两种、一个 rail 边框漂到与它框住的格子不同的色相」。

  这套皮肤**把时间轴轨道的配色也换掉了**，v0.2.4 那套（人=蓝 `#35b8ff`、AI=橙 `#ff9d2e`）不再是当前值：现在人=**黄**（`--yellow`）、AI=**粉**（`--pink`）、输出仍为绿（`--green-bright`）、审批徽章为紫（`--purple`）。四个状态由 `theme.css` 的 `--term-rail-*` 直读调色板（刻意不经 `--danger` 这类名字无关的页面状态，名字会往读者脑子里塞错词），`timeline.js` 只负责给出状态码。**升级后看到颜色变化属预期**；要改回旧配色就改这四个变量，`--assets` 可以直接覆盖。

- **窗口的连线状态与放置语义**：每个窗口的身份旁多一个点——拨号中是「连接中」，有会话是「在线」，只读是「已结束」；被抬起的窗口带 `.win-active`，所以「哪个窗口在最前」从窗口自身就能读出来，而不是只能看内联 z-index；已经开着窗口的会话在卡片上也会标出来。放置不再从事件里取值：主机条目是在钉在左缘的面板里点的，在那里抓到的坐标会把新窗口落在左墙下面板底下（那个「保证窗口在屏幕内」的 clamp 恰好把它钉在那里）。调用方现在传 `{x, y}` 或 `null`，`null` 表示居中，所以猜出来的坐标不可能把这个角落问题带回来。拖拽中的窗口仍然不受限，**松手时**才被夹回容器，因此标题栏与关闭键永远够得到；平铺面板不受影响。

### 改进

- **定位符相关实现收敛到一处（#73 的根因）**：这一轮修复暴露出同一条规则被复制在多个包/多个 handler 里，任何一份漂移都会让「同一个字符串」在两个入口得到不同结论（#73 与审批绕过都是这个成因）。现在只有一个解析入口：`requireSession` 统一接受**裸 id / session 定位符 / shell 定位符 / 裸 shell id**（shell 属于哪个容器是确定的），`requireShell` 统一接受**裸 id / session id / 定位符**；每个 handler 一律对**解析后的真实 id** 行事，而不是对传参行事。审批闸门也改为走同一条解析（否则闸门与它保护的操作会对同一个字符串给出不同结论）。所有响应中的 `session_id` / `shell_id` 与由它们拼出的 URL 都改为用解析后的值（`shell_open` / `shell_list` / `message` / `file_stat` / `file_urls` 之前会把定位符原样回显，而调用方通常会把这些字段贴回下一次调用）。`shell_notify` 的 `list` 过滤、`notify_user` 的卡片高亮同样先解析。`forward` 创建动作补上 nil 检查，与 `list`/`close` 行为一致。修复后用探针验证：上述八个场景全部转为正确行为，且新增回归测试大多在旧代码上会失败。`session.PrimaryShellIndex()`、`session.ShellIndexOutOfRangeError()`、`api.LessShellCreationOrder()`、`session.IsInternalPrimaryShell()` 分别取代 MCP 与 Web UI 各自的私有实现，同一个定位符不会再因入口不同而收到不同解释。

- **频道编号由服务端分配，终生不变、不复用**：`termcp://#<会话id>:N` 里的 `N` 曾有两套算法——复制时用频道创建当时的序号，解析时却把「当前存活列表里的下标」当序号，于是只要关掉靠前的频道，剩下的频道就整体前移（关到只剩一个时它变成 `:1`），先前复制的 `:2` 被解析成别的频道。现在编号在频道成功创建时由服务端分配（`ChildShell.Index` / `api.Session.Index`），终生不变、不复用：关掉靠前的频道不会让后面的编号前移，编号已关闭的频道解析为 `shell_not_found` / `404`，不会滑到邻居身上。`/api/sessions/{id}/shells`、`session_start` 与 `shell_open` 的返回都带上 `index`，Web UI 的标签与复制按钮直接用这个服务端值（不再自行编号，拿不到编号时禁用复制而不是瞎猜）。已关闭（DEAD）/ 重启恢复的会话按持久化快照里的同一编号解析；早于该字段的旧 manifest 在恢复时按创建顺序回填，因此重启不改变定位符的含义。MCP 的 `shell_output`、REST 的 `/api/resolve` 与 Web UI 现在共用 `session.ShellByIndex` / `SnapshotShellByIndex` 同一套查找。

- **`notify_user` 提示 Agent「要问人之前先通知人」**：此前模型实际收到的 `notify_user` 描述只有一句功能说明（`compactToolDescriptions` 会覆盖注册处的长描述），关于「何时该用」的指引根本没有到达模型。现在两条**保证送达模型**的通道都写上了这条硬性顺序：需要密码/sudo/passphrase/MFA、确认、批准、选项或任何交互式输入时，**先 `notify_user` 再发问**；长任务结束或失败同样要通知。阻塞类用 `level=warn`/`error` + `duration_seconds=0`（不自动消失）+ `session_id`（高亮对应卡片）。理由是它真的会卡住：人可能没盯着这个对话，一句没预告的提问会一直等下去，直到他碰巧看到。`initialize` 的规则 5/9 同步收紧，措辞压缩过以守在既有 token 预算内（instructions 2349/2400 B、工具描述 2741/4000 B），并新增回归测试直接断言**模型面向的那份描述**含该指引（只测 `tools.go` 会漏掉被 compact 覆盖的情形）。描述同时补上另一半：**问完就等**，不要轮询、猜答案或伪造人的回答。

- **审阅模式的规则只声明一次**：审阅模式拦截调用有两个点，各自拼了一份回复（`reviewPendingResult` 与 `reviewPendingOperationResult`），同样的 JSON 形状、同样的「没有可轮询的 id：不要重提、不要改写、不要重试——等着」句子写了两遍，于是同一条规则有两种措辞，模型会按弱的那份行事。现在形状与等待语只在一处（`reviewPendingReply` / `reviewWaitTail`），两个调用方只差「实际发生了什么」那一句：暂存的文本还需要它的结束键，已入队/被挂起的工作不可重试、用 `shell_output` 读回。两句必须不同——模型在暂存文本后读到「不要重试」会放弃一条它还必须写完的命令行。

- **终端字体栈补上 CJK 覆盖，并改为不内嵌字体（#77）**：终端字体栈此前是写死的 `'Consolas, Monaco, monospace'`，整条列表**不含任何汉字字体**。在装了 Consolas/Monaco 的机器上看不出问题，到 Kali 这类最小化 Linux 上整条栈落到 `monospace`（Debian 系 = DejaVu Sans Mono，同样无汉字），于是每个汉字都由浏览器最后的兜底字体（通常是点阵 Unifont）绘制——issue 里说的「终端变成超宽字体」不是字体丑，是字体不存在。更隐蔽的是同一界面里并存两套字体标准：xterm 不吃 CSS，它把 `fontFamily` 当 JS 选项接收并注入自己的样式表，所以只有终端格子走那串硬编码字符串，页面其余部分走 CSS 变量（本来是对的）；`ui-socket.js` 量列宽时又抄了同一串，连列宽都按错误字体算。

  曾评估过内嵌字体，实测后放弃：CJK 全集 woff2 约 9–10 MB，对单二进制分发不划算；子集到 GB2312 一级（3755 字）能压到 608 KB，但**该子集实测不含 ASCII、制表符、方块元素、盲文、箭头中的任何一个**（`fontTools` 逐码位清点：ASCII 0/95、制表符 0/128、盲文 0/256），只能当汉字补充层而无法独立支撑终端，且生僻字仍有缺口。改为**由 CSS 持有唯一的字体栈**：`tokens.css` 的 `--font-mono`，`util.js` 新增 `termcpMonoFontFamily()` 读取它（JS 保留等值常量作兜底，供变量读不到时使用），`terminal-view.js`（建 Terminal）与 `ui-socket.js`（量列宽）都改用同一来源。栈的排序由实测确定（`scripts/fontlab.py` 在 headless Chrome 里跑真实 xterm.js，量 cellW 与汉字 padding；方法与各平台矩阵见 `docs/design/font-stack.md`）：
  - **CJK 等宽面排最前**：它们被设计成拉丁 0.5em、汉字 1em，即**汉字恰好 2 倍拉丁**，是唯一能让 xterm 不必拉伸汉字的做法；Debian 系 `fonts-noto-cjk` 自带 `Noto Sans Mono CJK SC`，所以 Kali 装完这个包即命中。
  - **比例式 CJK 面垫底**：它们自带拉丁字形且推进是比例值，排在前面会接管拉丁文本，实测把 cellW 从 7.15px 抬到 13.2px（+85%），而 xterm 正是用拉丁 `W` 定 cellW、再按 `letterSpacing = cells*cellWidth - glyphWidth` 纠正**每一个**字形，于是全屏间距都被它决定。
  - **`NSimSun` 单列在比例式组之前**：比例式组一旦排在通用 `monospace` 前，Windows 的简体汉字就改由 `Microsoft YaHei`（推进 13.0px）绘制，而 `Consolas` 的格子要求 14.3px，于是每个汉字被撑开 1.3px（旧栈为 0.3px）——这是本次修复自己引入的回归，由 `scripts/fontlab.py` 量出。`NSimSun` 是 SimSun 家族的等宽面（`fontTools` 直读：汉字/拉丁 = 2.000），实测把它放在比例式组前即回到 14.0px 且 96 个测试字宽度均匀；不能用 `SimSun`（同样 2:1，但在栈中不让位），也不宜用 `MS Gothic`（实测 96 字中 40 个落到 13.0px，是逐字回退导致的宽度混杂）。
  - **`ui-monospace` 只作后期兜底**：实测它在 Windows/Chromium 上解析到的是比例式字体（W=11.41 / M=10.56 / i=3.58，并非等宽），排在 `Consolas` 前会让 cellW 从 7.15 涨到 7.61~11.4；它真正的用处是 iOS/WebKit 这类不暴露具名 Apple 字体的环境。

  新增 `internal/webui/font_stack_test.go` 锁定这套约束：`--font-mono` 与 `util.js` 的兜底常量**逐名比对**（xterm 读不了 CSS 变量，这份拷贝本身就是原缺陷的缩小版，静默漂移在开发机上完全不可见；比较前剥离注释、折叠空白，即浏览器对声明本就施加的归一）；比例式 CJK 面是否垫底、`monospace` 是否收尾、是否存在 CJK 等宽面，用结构性断言检查；并扫描 `static/js` 与 `static/css` 下的**全部**脚本与样式（由 `fs.Glob` 取文件列表而非手写清单，否则新增一个表面就能绕过它），禁止再出现只含拉丁的短字体栈（`ui-monospace, monospace` 这种，每写一次就多一个静默丢弃 CJK 的表面）。另确认随仓库分发的 xterm 构建**只声明、不消费** `rescaleOverlappingGlyphs`，设了是静默无效，故未启用。

  顺带把界面字体也收回一处：`--font-sans` 补齐各平台中文面（新增 JP/KR 与 Emoji 兜底），繁体栈 `--font-sans-hant` 从 `base.css` 的行内短列表（只有 `PingFang TC`/`Microsoft JhengHei` 两个中文名）改为 tokens.css 里的一份变量，与应用栈**同名单、仅换序**。初版实现已经漂移过——Hant 侧漏了 `Source Han Sans SC`、两个文泉驿和 JP/KR 名字，即“只是重排”变成了静默掉覆盖，因此新增 `TestHantSansStackIsSansReordered` 逐名对齐两份列表（只允许 Hant 侧多一个无 SC 对位的 `PingFang HK`）。壳内文件面板的两处行内 `ui-monospace, monospace` 也改为 `var(--font-mono)`。

- **前端资源按域拆成可缓存的分块**：两个携带 Web UI 大部分字节的文件还是整块的，改一行就要重发整个文件：`static/js/terminal-view.js`（98462 B / 1861 行）拆成 5 个模块（终端视图、面板、模板、窗口、会话切换器），`static/css/app.css`（147096 B / 2742 行）变成一个 8 行 manifest + 7 个 `@import` 分块（tokens / base / approval / terminal / timeline / workspace / theme），`index.html` 相应多 4 个 `<script src>`。

  两次拆分都按「搬移」核对而不是按「重写」：把 5 个模块按 `index.html` 的顺序拼接后与旧文件 diff，只剩顺带修掉的重复；CSS 分块按 token 流与旧 `app.css` 比较，差异恰好只有被删掉的那一条规则——没有规则被新增、改写或重排，级联由 manifest 的导入顺序保持。拆分让残留的重复显形并一并清掉：刷新转圈那块代码被复制了三遍（转发、通知、文件浏览器的 Load 键），现在是一个 `spinIconOnce()`；文件的下载/改名/删除动作存在两份（右键菜单与详情页按钮），且改名提示文案已经漂移，现在是一个 `fileMenuAction(action, path, name)`。同时删掉死代码：`terminal.css` 里为已不存在的会话菜单审阅行准备的 `.shell-switch-review` 规则（`approval_test.go` 断言它不许回来），以及每次打开菜单都写、但无人读的 `_ctxIsDir`。

  测试读取拆分后的结构时不再各自维护一份文件清单：`readTerminalJS` 从 `index.html` 推导模块顺序，`readAppCSS` 展开 manifest——手写清单会让「重排」通过测试而浏览器在模板被使用前就失败。CSS 拆分与 `--assets` 的交互也写进契约：`app.css` 现在是 manifest，所以只覆盖它一个文件会得到「既不旧也不新」的样式，通过覆盖目录换肤意味着覆盖整个 `static/css` 目录（两份 README 的 `--assets` 行同步说明），`TestAssetsCSSChunksResolveThroughTheOverride` 断言每个被导入的分块都能通过 HTTP 解析。

- **存储与持久化路径：写得更少，但不变的东西不写**：`persist()` 曾在每次变更时重写每个会话及其每个 shell 的 manifest（各自一次 fsync），九个调用点里有八个只改一个会话。现在 `persistOne(id)` 只写那一个会话及其 shell；`persist()` 留给真正动整张表的两个（`MarkAllDead`、`RestoreDead`）与启动时的扫描。每次 manifest 的 fsync 约 6.8ms（占总耗时 7.0ms），因此 100 个会话时一次 persist 从 **1380ms 降到 13.8ms**，10 个会话从 138ms 降到 14.3ms，且不再随会话数增长。`Delete` 根本不需要 persist——它把会话从注册表移除再删目录，被描述的对象已不存在，原来的循环只会重写无关会话。

  另外，`writeManifest` 现在对编码后的字节取哈希，与上次为同一路径写下的字节相同时**跳过写入**（manifest 是传入值的纯函数、不含自身时间戳、无人读它的 mtime，检查过）；写成功后**才**记哈希，所以失败的写会被重试而不是被记成已完成。两条删除路径都会丢弃缓存条目——否则「磁盘上删掉、哈希还在」会让它跳过回写，留下一个没有 manifest 的目录，而 `LoadSessions` 永远跳过那种目录。fsync 本身**刻意保留**：它占剩余开销约 97%，但去掉它 rename 的持久性就没有保证，而这里丢掉一次 rename 不是「值过期」，正是那个不可恢复的孤儿目录；这个代码库也没有文件锁或单实例保证，store 不能假设自己是唯一碰数据目录的进程。`atomicWriteFile` 补上文档注释并记下评估过的替代方案（批处理 fsync 无帮助：20 个文件批处理 226ms vs 逐个 196ms；并行 sync 有效但典型操作只写一个 manifest，不适用），`Store` 的两个磁盘缓存（`deleted`、`manifestHash`）在字段声明处写明「每条删除路径都必须使其失效」这条不变量。

  shell 的退出状态此前**从不落盘**：退出路径上没有任何地方写 manifest（watcher 只把最终状态留在内存，`notifyExit` 唯一注册的 hook 只更新通知规则），所以已经退出的 shell 在磁盘上仍写着 running，重启会把它当作活 shell 复活。`notifyExit` 现在持久化其所属会话（从 shell 反查，且只在所有者仍存在时——并发关闭的 shell 已经没有会话可描述）并通知列表变更，因此退出状态与退出码都能挺过重启。`session.go` 里说退出 watcher 会做「最终日志写入与 persist()」的注释描述的是一次已经不存在的 persist()，这正是缺口没被发现的原因，注释一并订正。

  写回已删除会话被明确拒绝：关闭会话就是删除，而尚未察觉的写入者（转录循环可能正排在 drain 中间）的下一次 append 会因为它创建所需路径而**重建**会话目录，留下一个有日志、没有 manifest 的目录，`LoadSessions` 从 manifest 推导会话列表，于是永远跳过它、也没人清理。难点是「已删除」不等于「还不存在」：会话在 `New` 里就启动了输出管道，早于 `Create` 落盘，所以一个会话生命最初的字节合法地早于它的目录。因此这个事实被显式记住（一个 deleted 集合，由 `SaveSession` 清除，使复用 id 可再写），而不是从目录缺失推断。

- **每份日志一个写者 goroutine，替掉「生命周期短于它所保护的数据」的锁**：转录路径上曾有两把这种形状的锁（一个 map 里的 mutex）。message 那一半是**真实缺陷**：一次 append 是两次 store 调用（追加字节、记录字节起点），必须不可分离，而那个 per-session mutex 住在 map 里，`ForgetSession` 会删掉条目——在删除前载入 mutex 的 append 与删除后载入的 append 持有**两把不同的 mutex**，两半于是可以交错，mark 可能被写在「另一次写入的字节落盘之前读到的偏移」上。删条目这件事无法靠加锁变安全：任何「序列化访问的东西可以被移除并重建」的设计都有两个它同时存在的窗口。改为所有权即消除该窗口——每个会话一个写者 goroutine，每次 append 都是对它的请求，两半由同一个 goroutine 执行，mark 的偏移天然正确；记住状态的 map 也归它所有，它的锁一并消失。不变量写作「条目存在当且仅当其写者正在运行」：创建用 `LoadOrStore`（只在没有条目、也就是没有写者在跑的地方创建），写者自己的 goroutine 作为最后一件事移除自己的条目，因此包外没有任何地方需要给「删除」和「停止」排序。

  storage 那一半是**一致性、没有复现出缺陷**：`logsMu` 保护按需打开、在删除 shell 或关闭时关掉的 per-shell 句柄，同样的推理适用，但旧代码通过了全部三个新测试（含 close/delete 与 append 竞争的 40 次重复），所以这一半立足的是形状论证而不是失败复现；纳入它是为了让「一份日志一个写者」在整个包里一致。代价是实测的，不是估计：message 每次 4 KiB append 从 9.5µs 变 14.5µs（+5µs，281 vs 432 MB/s），storage 约从 295 MB/s 变 340 MB/s（约快 20%）外加每个 Store 一个常驻 goroutine；两条路径都远未到上限（交互式 shell 约 0.05 MB/s），所以这个代价被记录而不是被当作否决理由。偏移会**静默**出错（丢一次 append 仍留下一个大小看似合理的文件），因此并发测试把每个返回的偏移读回来核对其中确是那个写者自己的字节，而不是数字节数。

  同一轮里 `buffer` 的等待从 `sync.Cond` 改为通道：一次等待读曾要两个 goroutine（调用者，外加一个只为让定时器或 context 能调 `Cond.Broadcast` 而启动的帮手，因为 `sync.Cond` 只能无超时等待），现在只要调用者自己一个，select 在截止时间、context 与写者每次变更都会关闭的唤醒通道上。实测每次 4 KiB 等待的 goroutine 从 2 降到 1、分配从 5 次 410 B 降到 3 次 248 B；空等的墙钟时间不变（那就是超时本身加调度噪声），**截止精度也没有改进**（两者对 5/50/200ms 都平均超时约 375µs），所以这里只声称「等待的代价更低」而不是「超时更准」。唤醒通道在锁仍持有的时候读、释放锁之后再处理，因此变更不可能落在等待者的检查与等待之间；被无关变更唤醒的等待者会重新检查再等，与 `Cond` 需要的循环相同。

- **`session_start` 与 `POST /api/sessions` 不再固定 sleep 100ms**：那 100ms 读起来像「给 shell 一点时间稳定下来」，但它不可能在做这件事，也没在做别的事——sleep 之后被读的任何东西在 sleep 期间都不会变（响应由 `sess.ID`、`PrimaryShellID`、profile 名与 `sess.PID` 构成，前三个在 `Create` 返回时就固定了，第四个恒为 0）。实测首个输出在 `Create` 之后约 240ms 才到，所以这 100ms 即便在它被加入时也从不保证输出就绪；而真正要紧的输入**完全不需要等待**：`Create` 返回后立刻发输入会被接受并回显，往返约 225ms 是它自己的。因此它只是给两种入口的每次会话创建都加了 100ms 延迟。让移除安全的性质现在是一条测试（`TestSession_InputWorksImmediatelyAfterCreate`）：`Create` 返回的那一刻就发输入并要求进程收到它——这是防止「稳定延迟」被重新引入的锁。该测试随后又修了两次平台问题：unix 分支原用 `/bin/cat`，它只是把行回显成 `hello`，而断言等的是 `GOT_hello`，于是测试等满五秒后失败在「立即输入从未到达进程」上——正是它存在要反驳的那件事；现在两个分支都用一个**会应答**而非仅回显的命令（POSIX 的 read 循环打印 `GOT_`，PowerShell 同理），并且注释写明为什么必须应答而不能回显。

- **`api.Session.PID` 被移除**：它从未被赋值过，所以每条会话记录、每个 `session_start` 响应和每张 Web UI 会话卡自该字段引入起就一直报告 `"pid": 0`；`docs/api.md` 的示例写着 12345，让这件事看起来像「一个真实的值出了问题」而不是「一个装不下值的字段」。根因不可修：进程跑在 SSH 连接的另一端，而 SSH 不会把它的号码报回来，这里任何值都只能是常量，所以诚实的修法是删掉字段而不是塞进一个编出来的数。守护进程自己的 `InstanceInfo.PID` 是另一个字段、持有真实的 `os.Getpid()`，两者容易混淆，因此改动只限会话路径；每处剩余位置都注明了字段为何不存在、以及怎样拿到真正的 pid（问 shell 要 `$$`），避免它作为疏忽被重新引入。

- **测试与 CI 只有一条命令，并补上「检测器看不见的竞态」**：`make test` 与 CI 曾在作用域与参数上都不同（`./...` 无超时 vs `./internal/...` 120s），且两边都没开竞态检测。现在两者逐字节相同——`go test ./... -count=1 -shuffle=on -race -timeout 240s`——所以 CI 失败可以用它打印的 shuffle 种子在本地复现，且 CI 覆盖整棵树（含根包的测试）。三个 CI job 都开了 `-race`（`CGO_ENABLED=1`，三个 runner 镜像都自带 gcc，windows-latest 是 15.2.0），纯 API 构建那一步也开。本地 `make test` 每个 target 只探测一次工具链，在缺 cgo/C 编译器时降级为无 race 并打印提示，`RACE=0/1` 可强制任一边；探测是一个递归变量，从不展开它的 target（build、dist…）不付任何代价。新增 `make test-stress` 覆盖 `-race` 看不到的竞态——文件系统/生命周期竞态，碰撞发生在磁盘上而不是内存里：`-cpu=1,2,4` 把 goroutine 挤到一个调度器上、按满载 CI runner 的样子把拆卸与后台写入者交错，`-count` 重复、`-shuffle` 重排。它正是复现出 session 包 TempDir 竞态（本机 30 次里 28 次）的那个工具，而普通 `-count=15` 从未抓到过。对应的生命周期清理模式（`t.Cleanup(m.Delete(id))` 要注册在 `t.TempDir` 之前）记入 `docs/agents/testing.md` 与 AGENTS.md。

- **行尾统一为 LF，并把策略写进仓库**：开发在 Windows、CI 在 Linux 与 macOS，一个文件的行尾取决于最后保存它的人。`.gitattributes` 现在声明 `* text=auto eol=lf` 并对在用扩展名显式标注 `text`，所以任何机器上的检出都一样，未来的提交也无法重新引入漂移。一个以 CRLF 提交的文件（`internal/webui/ws.go`）与两个混合行尾的设计文档被归一；另外四处与行尾无关但同样只有一行的 gofmt 问题（文件末尾缺换行、`_ ,`、字段错位、行尾空行）一并修掉，因为它们是同一类问题且否则会一直脏着。整棵树现在 gofmt 干净。没有一行 Go 逻辑改变。

- **代码按行为拆分，而不是按大小切**：十个文件承载了仓库大部分生产代码（`handlers.go` 1710 行、`session.go` 1518、`handler.go` 1466、`shellrail.go` 830、`store.go` 749、`bridge.go` 653、`server.go` 637、`manager.go` 628、`forward.go` 546），根目录的 `main.go` 965 行同时装着 serve 路径、daemon CLI、stdio 桥与 auth-hash 生成器。现在最大的文件 420 行。两次拆分都是**纯搬移**（没有声明被新增、删除、改名或修改，方法保持接收者、标识符保持可见性、包边界未变），并且是按行为切分，所以读者落到一个主题上而不是许多主题的切片。搬移经过机械核对而不是目测：每个原文件按声明区间重建后逐字节比对（9/9 与 8/8 完全一致，这正是区间平铺整个文件的证明），随后用 AST 审计把每个声明与当前包对照（internal 各包 374 个、根目录 27 个、其余包 216 个），没有丢失、没有改变、没有凭空多出。

  另有八个 104–367 行的函数各自装着两件以上不相关的事，按代码里本来就有的接缝切开：`ws.go`（连接 vs 终端输出流，输出侧移入 `wswatch.go`）、`handleDownloadFile`（问的是哪个字节窗口 / 从 termcp 主机流式发送 / 经 SFTP 流式发送，各自成函数，handler 21 行）、MCP 的 `New()`（组装服务器 vs 内联声明全部 31 个工具，表移入 `registerTools`，231 行逐字节一致、31 个 `AddTool` 顺序与名字不变，`New` 89 行）、`handleEditSSHConfig`（两段 `if v := getString(...)` 之间的四条语句，`applyEntryEdits` / `applyJumpEdits` 各接一段）、`handleReadOutput`（尾窗 / 定位字节区间 / 活游标三种读共用一个结果形状，改为 `parseReadParams` + `readOutputWindow`；这一处**不是**纯搬移且不作此声称）、根目录 `main()`（读命令行 vs 构建运行时并服务到停止，`runServer(cfg, idleTimeoutArg, idleTimeout, isDaemonChild)` 接后半，四个参数由 `.pi-tools/freevars` 走 AST 得出而非猜的）、`session.New`（141 行里 53 行在回答「这个会话跑在哪里」——进程自己的 loopback sshd 还是真实远端，`dialTransport` 回答它并报告选中的端点；这次搬移把两条 revoke 路径放在一起，因为 internal 拨号会铸造一次性凭据，拨号失败与握手失败都必须撤销它，否则服务端的 pending map 会为每次失败尝试留下一个死条目直到进程结束）、`sshserver.handleSession`（会话的一生 vs 启动进程这一步，`startProcess` 报告是否启动成功）。

- **`shell_detect` / `shell` 等低频工具面与文档同步**：`docs/api.md` 新增 `/api/connections/batch` 两节、`temporary` 语义、`index` 字段、会话记录新增 `ssh_config` 且不再有 `pid`，以及定位符在两个面上的差异（MCP 处处接受、HTTP 路径/查询参数只接受裸 id、写操作的 `ssh_config` body 字段接受 entry 定位符）——这个差异是刻意的，定位符含 `#` 与 `:` 本来也无法安全地放进 URL 路径。`docs/api.md` 在 `internal/webui/assets` 里的副本由 `TestSyncedDocsMatchSource` 保证与源文件一致。

### 修复

- **连接编辑对话框里，除内置 `internal` 外的 profile 又能改名了**：名字输入框对所有编辑场景都被设成只读，于是「编辑主机」打开后名字填着却不接受输入，看起来像被禁掉而不是改名功能没了。后端一直支持：保存时用旧名作 `?from=` 调 `PUT /api/connections/{name}`，store 改名并让持有该 profile 的会话跟着走。只读条件是 `!!edit`，而它上方的注释只为内置 `internal` 辩护（那个名字是寻址内置回环连接的唯一方式，改了就没人再找得到），需求是「锁 internal」却写成「锁所有编辑」。现在锁的只是 `internal`，其余 profile 与新建都可编辑。新增回归测试在 node 里跑真实的 `openConnModal` 断言四种场景的最终 `readOnly`（不是字符串匹配），并经变异测试确认两个方向都会失败。

- **审批模式可被定位符绕过（最严重）**：审批闸门（`gateOperation`）用**原始参数**查会话，查不到就「不拦截」，而 handler 随后自己把同一个参数解析成功并执行操作。结果：同一个受审批保护的写操作，用裸 id 写会被挂起等人工批准，**换成定位符就直接执行了**（探针实测：`file_write` 裸 id → 进审核队列、文件未创建；`termcp://#<sid>` → 文件立刻创建、队列为空）。`file_write` / `file_delete` / `file_rename` / `file_mkdir` / `file_perm` / `file_link` / `file_fs` 与 `forward` 八个写操作全中。闸门与它保护的操作现在走同一条解析，八个场景全部转为正确行为。

- **定位符在若干 MCP 工具上「成功」了但什么也没做，或直接拒收**：
  - **`shell_close` 直接拒收定位符**（报 `shell_not_found`），尽管同一个定位符在 `shell_resize` / `shell_reader_register` 上都能用。
  - **`session_terminate` 更糟：它“成功”了但什么也没做**。它用定位符解析出会话，却把**原始参数**传给 `Manager.Terminate`——那里找不到 id 就静默 no-op，于是工具回 `{"success":true}` 而会话仍在跑。假成功比报错危险：调用方以为资源已经关了。（`session_delete` 则因把定位符当存储路径名校验而被拒。）
  - **forward 把定位符当 `session_id` 存进注册表**（`internal/forward`），而会话 DEAD 时的级联回收是按真实 id 匹配的——这个本地监听端口会活过它所属的会话，成为一个没人能再关掉的死端点（实测复现：terminate 后 `forward(list)` 仍在）。
  - **`shell_notify` 同病**：规则把定位符存成 `ShellID`，于是（a）退出 watcher 按真实 shell id 的级联清理找不到它，定时器为已不存在的 shell 继续跑；（b）`channel="resource"` 广播的 uri 变成 `termcp://shells/termcp://#<sid>:2`，这个名字不对应任何东西。
  - **`message(action=list)` 静默返回空转录**：marks 用原始参数去读日志，定位符指向一个不存在的日志文件，于是“没有输出”与“读错了位置”无法区分。
  - **`forward` 的三个创建动作在无 forward manager 的部署（`-tags no_webui` 纯 API 构建）里会 panic**（nil 解引用），而同一工具的 `list`/`close` 都做了 nil 检查。
  - **`file_stat` / `file_urls` 把定位符回显进 `session_id` 与 URL**：`download_url` / `upload_url` 直接用原始参数拼路径，得到 `/api/sessions/termcp://#<sid>/files/download` —— 一个含 `#` 和 `://`、指向不了任何会话的 URL。

- **HTTP 的 `ssh_config` 字段也接受 entry 定位符**：`POST /api/sessions` 的 `ssh_config` 之前会直接送去 profile 存储校验名字，于是从连接卡片复制的 `termcp://rock64` 在 MCP 的 `session_start` 能用、在 curl 里却报 `invalid ssh config name`。它是 JSON body 字段（不是 URL 路径），`#` 与 `:` 在这里没有歧义，所以现在与 MCP 一致：接受 `termcp://<entry>`，解析成 profile 名；传入 session/shell 定位符仍报错并提示改用 entry。

- **读取路径不再继承写路径的状态检查**：`message(action=list)` 显式给 `shell_id` 时会在已关闭（DEAD）会话上报错，而只给 `session_id` 时却能正常读——同一个读操作因为写法不同而两种结果，原因是它复用了写路径的 `requireShell`（后者必须拒绝已死会话，避免写进已关闭的 transport）。marks 存在 `log.bin` 里，本来就活过 transport，现在读路径用自己的解析（不检查会话状态），两种写法一致可用。**裸 session id 读取保留为“频道 1”语义并写明**：`shell_output(shell_id=<裸会话id>)` 仍按 `PrimaryShell()` 判断走活缓冲区还是持久化日志，与 `:N` 路径改用的 `HasLiveShells()` 不同。这是有意保留而非遗漏：裸 id 问的是“1 号频道”，若因为它恰好不在世而改答另一个活着的频道，就是在回答另一个问题。

- **裸 `session-<id>` 被误判为定位符（#73 后续）**：`LooksLike` 声称 `session-abc123` 是定位符，但 `Parse` 把它当成 **entry 名**（`KindEntry`）——也就是说一个 profile 名会被拿去当会话 id 使。而 profile 名与会话 id 共用同一命名空间：ssh_config 名字允许 `session-` 前缀，所以 `session-foo` 可以是一个正当的 profile。现在约定收紧为：**裸名字（无 scheme、无 `#`）不是定位符**，保持原来的意义；只有显式会话写法（`#<id>`、`termcp://#<id>`、`termcp://<entry>#<id>`）才按会话解析，`session-` 前缀在这些位置剥掉。新增 `TestLooksLikeAgreesWithParse` 把 `LooksLike` 与 `Parse` 的类型判定逐个对齐锁定，并显式覆盖 `session-foo` 作为 profile 的正当性。

- **`shell_output(offset=0, max_bytes=0)` 会把进程打挂**：`ByteRange` 收的是绝对偏移、报告的是绝对总长，但它返回的窗口取自 `master`，是相对的；压缩正是分开这两者的东西（丢掉 `master` 的前缀会把它加进 `baseOffset`，于是绝对总长涨过 `len(master)` 而 `master` 变短）。夹取用的是绝对总长，所以「从绝对总长推出的窗口」——正是调用方读「一直到流末尾」时会做的事——向切片表达式要了比 `master` 更多的字节，边界检查直接 panic（`slice bounds out of range [:12582912] with capacity 3211264`）。这条路径**有文档可循**：`shell_output` 的 `max_bytes` 文档写着「0 = 不限」，handler 把它变成 `max = int(total - offset)`，于是对已经压缩过的会话调一次「从头读全部」就会杀掉进程——而且没有任何东西接住它，因为 MCP 服务器当时不是用 `WithRecovery` 建的：每个会话、每个浏览器标签页一起消失。同一个「量级错位」还有更安静的第二形态：当相对起点夹到 0、而 n 仍装得进 `cap(master)` 时，切片表达式靠容量而非长度成立——不 panic，调用方拿到 n 个它以为是终端输出的零字节。静默损坏比崩溃更糟，所以两种都有测试覆盖而不只是显眼的那一种。

- **工具 handler 里的 panic 不再杀掉进程**：mcp-go 在它自己的 goroutine 上派发工具调用（streamable-HTTP 与 SSE 服务器把每个请求交给各自的 goroutine），所以没有任何 HTTP handler 的 recover 能接住它，它上面的东西也都不会跑 defer：每个会话、每个浏览器标签页与守护进程的 HTTP 监听一起死掉。已核实而不是假设——去掉这层中间件会让一个调用 panic 工具的测试中止测试二进制。中间件是手写的而不是用 `mcpserver.WithRecovery()`，因为两者对「客户端该看到什么」意见不同：`WithRecovery` 返回普通 Go error，mcp-go 把它变成 JSON-RPC 协议错误（-32603）并把 panic 值当作消息——那会跳出这里其他失败都遵守的 `error_code` 契约，于是按该字段分支的客户端无事可分，而 panic 值通常是点名内部文件的运行时消息。所以两个受众拿到不同的东西，这正是该改动的意义：**客户端**收到 `{"error_code":"internal_error"}` 作为一次普通失败的工具结果（消息说明这是 termcp 自己的故障，因为对己方的 bug 说「参数非法」会把 agent 送去纠正输入；panic 值刻意排除，它点名内部实现）；**运维**在 Error 级别拿到 panic 值、`debug.Stack()` 与工具名。只恢复不记日志等于把一次响亮的崩溃换成一次安静的错误回答，所以那行日志是契约的一部分而不是点缀。两半都通过真实的 streamable-HTTP handler 端到端断言。这层中间件在所有工具中间件的最外层，所以日志包装器内部的 panic 也覆盖到——这是刻意的顺序而不是注册的偶然。它是安全网而不是任何具体 bug 的修复：被恢复的 panic 仍然是 bug，那行日志才是让它可被找到的东西。

- **整个页面不再能上下滚动**：NetHub 侧栏把自身高度上限写成 `calc(100dvh - 134px)`，而那 134 是「上下占用」的手算值——顶栏 57、dock 上内边距 16、间距 8、触发键 50、dock 下内边距 16，**实际合计 147**。少了 4px，于是侧栏恒定高出 13px，文档自己长出滚动条。它是**常数错、不是内容错**，所以很难看出：节点 30 个与 60 个溢出量都是同样的 13px，侧栏为空时也照样滚（均在 headless Chrome 中实测）。改为 147 后，700/900/1000/1400px 视口与 0/6/30/60 节点组合下页面滚动量全为 0，折叠态同样为 0。同时删掉 `.dock` 上那句`min-height: calc(100% - 52px)`：它把一个猜的顶栏高度从一个本就解析为 `auto` 的百分比里减掉，**从未生效**，只是看着像把 dock 压在视口内。新增 `TestNetHubSidebarKeepsThePageFromScrolling`：把 147 的每一项分别钉到拥有它的规则上（顶栏 padding、dock padding、触发键高度、面板间距），并要求面板与折叠轨**共用同一个常数**——两个数字正是当初 134 与 147 并存的原因；已验证把 147 改回 134 会让该测试失败。

- **rail 与终端文本错位**：行↔字节映射的服务端模型有若干处与真实终端不符，累积起来让格子落到离文本很远的地方。
  - **`height` 不再被 `count` 抬高**：`/rail` 曾用 `if height < count { height = count }` 把模型的屏幕高度补到请求的窗口大小。客户端为了「滚动落在余量内只重绘、不请求」会一次要屏幕两侧各一屏的余量，于是模型以为自己有 121 行屏幕，`ESC[H`／`ESC[2J`／`ESC[K` 这些「相对屏幕」的序列全部按错误的屏幕原点执行，`clear` 更是从错误的位置擦掉一整屏。现在 `height` 就是终端高度（仅省略时回落到 `count`），窗口大小不再影响布局。
  - **`ESC[3J` 现在会裁剪并重编号**：这是 `clear` 实际发出的「擦除已保存行」，xterm 收到后把滚动缓冲整段丢掉、只留一屏，并让幸存行的编号整体下移。模型此前完全忽略它，于是它继续描述一个终端已经没有的缓冲——实测某个真实会话：模型 122 行、xterm 25 行，每个格子偏了约四屏。等价地，退出全屏程序后的行号也一并归位。
  - **缓冲上限是 `scrollback + height` 而不是 `scrollback`**：xterm 的行缓冲按「配置的 scrollback 加上当前屏幕」来定容（`getCorrectBufferLength`），模型少留一屏就会让长会话靠后阶段的每个行号固定偏掉一个屏幕高。
  - **行号就是终端自己的行号**：模型去掉了内部的行号偏移，`rows` 的下标即 `viewportY` 坐标系里的行号，与 `/rail` 的 `top`／`total_rows` 契约一致，裁剪时整段前移而不是留下漂移。
  - **窗口外的 clear 不再让已答的窗口过期**：端点曾「光标越过请求窗口就停止重放」，但行不是光标离开就算定稿的——更靠后的 `ESC[H` 会改写已「越过」的行，`ESC[3J` 更是把整个滚动缓冲丢掉并重编号。于是窗口之后出现一次 `clear` 时，返回的 25 行全部指向已经擦掉的文本（实测）。现在一律重放到日志末尾，按最终编号回答窗口。这个「优化」本来也几乎不省：流式输出时视口就在日志底部，`top+count` 已在末尾，循环照样读到底；它唯一省下的就是视口停留在滚动历史里的情形，而那正是它答错的情形。
  - **行裁剪改为摊销 O(1)，大日志不再卡住 rail**：`trimFront` 原用 `append(rows[:0], rows[drop:]...)` 整段搬移——在 10 万行容量下每滚掉一行就 memmove 1.6 MB，20 MB 日志实测重放要 **7.2 秒**（纯拷贝，不是解析），这段时间里 rail 要么不响应、要么一直显示上一次的格子。现在改为切片前移，由 Go 的扩容来摊销那次拷贝：同一条 20 MB 日志从 7.2s 降到 **0.19s**，60 MB 约 0.59s。
  - **超过一个读取块的日志只被回放了第一块**：`OutputByteRange` 的第二个返回值是日志的**总长度**（那是为了区分「空流」与「被截断」），而读取循环把它当作「本块结束位置」赋给了游标，于是读完第一个 256 KiB 块后游标直接跳到日志末尾——任何大于 `shellRailReadChunk` 的日志都只按开头那一块推导，后面已经滚过去的行全都描述错，最新输出没有格子。现在按实际读到的字节数推进。
  - 校验方式是把同一条日志分别喂给 Node 里的 xterm.js 与本模型，逐字节比对光标所在行：400 个合成会话、323053 次比对、行文本 9636 行全部一致（真实 `log.bin` 亦逐字节一致）。另有 `TestRowLayoutAgreesWithXterm` 作为常驻差分测试：76 个用例（含生成会话）、19335 次字节位置比对，严格要求每个字节所在行与最终光标一致，行文本则只对「未被回车/光标寻址/退格改写的行」要求完全相等。

- **rail 比真实输出「短一截」**（流式输出时反复出现）：客户端判断「手上这份布局是否已覆盖屏幕」时用了 `spans.length`，而 `spans` 是按请求的 `count` 补齐的数组（末尾不足的行是 `null`），它表示**要了多少行**，不是布局真的有多少行。流式输出时取回一份「当时还没写到这里」的布局后，这个判断会认为窗口已覆盖，于是**不再重新请求**，最新几行永远没有格子——实测每秒采样都短 1–5 行，直到屏幕滚出那个假覆盖范围才补上，然后再次发生。现在覆盖判断同时受 `total_rows`（绝对行号）约束，超出布局真实范围就不再算覆盖，会重新取。

- **rail 改为「并排」而不是覆盖终端**：轨道此前绝对定位在终端右缘之上，压住最后几列文本，还占着滚动条的 gutter（想拖滚动条会点到轨道）。现在 `--term-rail-w`（桌面 14px、触屏 22px）在 `.shell-channel-body` 上预留成独立一列，终端盒（含滚动条）仍占满整个 body，`fitShellTerminal` 按 `--term-rail-w + --term-rail-gap` 少算相应的列数，因此顺序是「文本 → rail → 滚动条」：滚动条留在它原本最右侧的位置，文本在它之前结束，中间是 rail。悬停标签与详情卡改从轨道左侧展开，回到终端上方，回到底部的按钮同样避开这一列。预留是无条件的：若随标记有无而出现／消失，终端宽度会跟着变，PTY 与整份行映射每次都要重排。

- **输入标记只在提交一行时写入**：v0.2.3 里 `WriteStdin` 成功就记一条零长度标记，于是「开始打字」这个时刻本身成了区段起点——从那一刻起的所有字节（包括别的命令正在输出的内容）都被算进这段输入，后面的输出区段被吃短（实测：输入 `sdfaf` 加 5 个退格，产生一条 27 字节的 `i` 区段，把提示符和回显都圈了进去）。现在只有回车（或 ctrl+c/d/z）才写标记：一行 = 一条标记，未提交的按键只用于通道状态，不进日志。密码仍然不进日志——终端不回显就没有字节可存。

- **两个数据竞争与一个 fixture 抖动（`make test -race` 抓到）**：
  - **`internal/daemon`**：`NewIdleWatcher` 在未持锁的情况下用 `time.AfterFunc` 武装倒计时，而回调的 `expire()` 会取 `w.mu` 并写同一组 `w.timer`／`w.gen` 字段。`AfterFunc` 返回的那一刻定时器就是活的，所以一个很短的超时可以在构造函数发布它所读的状态之前跑进 `expire`——对同一个字两次无序写，`-race` 无论它们执行得多远都会报告。这正是 `idle_test.go` 里 200ms watcher 失败的原因。现在倒计时在 `expire()` 所取的锁下武装，因此定时器只有在它的状态完整之后才可达。
  - **`internal/sshserver`**：`handleSession` 结尾的 `sess.Signals(nil)` 与库从两处读取同一槽位的读没有顺序——请求循环（持会话锁）与它在「信号在 channel 注册之前到达」时启动的重放 goroutine，而那个重放 goroutine **不持锁**读取该槽位。这是常规而非罕见路径：客户端可以在 exec 被接受与 `sess.Signals(sigCh)` 之间发信号，请求循环把它缓冲下来，注册时重放。这就是 `TestRace_ConcurrentTerminate` 报出的那一对。现在槽位只写一次、永不重写；由于之后没有别的东西会读这个 channel，转发器在子进程存在之前就启动——因此请求循环不可能在持会话锁时阻塞在向它发送上——并一直排空到 `sess.Context()` 被取消，那是库停止发送的唯一时刻。进程通过 `atomic.Pointer` 而不是直接读 `cmd.Process` 拿到它，后者会与 `Start` 里的写竞争。
  - **`internal/mcp`**：`TestShellNotify_RegisterListUnregister` 也间歇失败，但在 HEAD 上同样失败，所以不是上面两个修复造成的。`startTestSession` 跑的是一个 `echo` shell、立刻退出，而 `OnExit` 会级联清掉正在退出的 shell 的规则，于是规则可能在注册与注销之间被扫掉，测试就失败在一个与它要测的东西无关的 `rule_not_found` 上。fixture 改用 `testShellIdleArgs()`——它本来就是为这个隐患记录在案的：改后 12 次 0 失败，改前 10 次里 4 次失败。
  - 两个竞态各有一条刻意制造碰撞的回归测试，且各自在修复被回退时于 `-race` 下失败：一纳秒的倒计时把回调落进构造窗口内，而在 exec 之前发信号会留下被缓冲的信号，于是注册 channel 时启动重放。

- **Windows PTY 生命周期上与库抢跑（两个只在 Windows 出现的数据竞争）**：两者都由 `make test`（`-race`）发现，且在 macOS/Linux 上不可见，因为写入者在 charmbracelet/ssh 的 Windows 构建里（`pty_windows.go`），并且只通过 ConPTY 触及 conpty 未加锁的几何缓存。
  1. `cmd.ProcessState` 每个 PTY 会话被写两次。`pty.Start` 在 Windows 上不通过 `exec.Cmd` 管理子进程；库自己的 start goroutine 会回收 `cmd.Process` 并写 `cmd.ProcessState`，而 `handleSession` 同时在调 `cmd.Wait()`——同一个字段两个未同步的写者。竞态中输的那一方读到 nil 状态，所以它必须回落到 127 而不是子进程的退出码。该机制被直接确认（对已完成的 `cmd.Wait` 再取一次 Process/Wait 会返回 nil 状态，而库是无条件赋值的），但没有端到端观测到：`exec.Cmd.Wait` 自己的「Wait was already called」保护挡住了常见交错，只剩这一侧先赢、库随后写 nil 的窄窗口。
  2. `conpty.ConPty.size` 同时被两个窗口变更消费者写：库的 winch 排空与 `drainWindowChanges`（它是有意并行跑的，见 `3f3ed5a`）。两者都调 `Pty.Resize`，后者把新的几何缓存进那个未加锁的字段。

  现在 Wait 与应用按平台分流，Unix 保持既有行为，Windows 路径不再触碰库拥有的状态：`waitChild` 在 Unix 上用 `cmd.Wait()`，在 Windows 的 PTY 上等它自己对同一进程的句柄（`os.FindProcess(pid).Wait`），不读任何库拥有的字段，退出码经 channel 传回，因此不需要共享字段——这件事能成立依赖一个上游怪癖：x/conpty 从不关闭 `Spawn` 返回的进程句柄（只关线程句柄），所以内核进程对象与 pid 活得比库的回收更久；这个依赖就地写明，回落路径现在会记日志而不是静默报告 127，`TestServer_PtyExitCode` 是这道护栏响亮的那一半（模拟那个句柄被关闭时验证它以「reported exit code 127, want 42」失败）。`applyWindow` 在 Unix 上保留 `pty.Resize`（一个 ioctl，没有共享 Go 状态），Windows 上直接对 PTY 句柄调 `ResizePseudoConsole`，绕过几何缓存——那个缓存是给 `ConPty.Size` 用的，而没有任何东西调它，句柄在分配时就固定且这个 Win32 调用是线程安全的。两个消费者都保留：丢掉我们自己那个会重新引入 `3f3ed5a` 修掉的 macOS resize 丢失。Windows 上此前没有回归覆盖，现在补上：`TestServer_PtyExitCode` 断言 PTY 会话报告子进程的真实退出码；`TestSession_PtyResizeReachesChild` 不再跳过 Windows（原先用 `stty`），改为通过 PowerShell 的控制台 API 读几何，并把数字包在标记里，使被回显的命令行不会被误当作答案——把 Windows 的 resize 重新路由回 `Pty.Resize` 会让 `-race` 经这条测试报出 `conpty_windows.go:159`。

- **退出 watcher 与 `t.TempDir` 清理抢跑（CI 上出现过两次）**：四个保留 DEAD 的生命周期测试把一个仍然活着的写入者交给了 `t.TempDir` 的 `RemoveAll`。`Terminate` 有意不等 per-shell 的退出 watcher（在那里 join 会让自然退出路径自死锁，因为正是 watcher 自己驱动 DEAD 转换），所以 watcher 的最后一次 manifest 落盘（`notifyExit` → `persistOne` → `SaveShell`，一次写入 shell 目录的 `atomicWriteFile`）会在 `Terminate` 返回后几毫秒才落地。紧接着结束的测试随后在清理阶段失败：一个 `.tmp-*`（或已改名的 manifest）出现在 `RemoveAll` 的目录列举与 rmdir 之间，于是以「directory not empty」中止。没有内存被触碰，所以 `-race` 看不见它；它两次上到 CI（runs 37486929684、37336490236），并在本机 `GOMAXPROCS=1 -count=30` 下 30 次复现 28 次。`Delete` 就是 join 点：`finalize` 在 `DeleteSession` 移除目录之前等 `watchWG`，而 store 的 deleted 集合此后拒绝任何掉队的 append。因此紧接 `Create` 之后、按 LIFO 早于 `t.TempDir` 注册 `t.Cleanup(m.Delete(id))`；对重启形状的测试要通过**第一个** manager 删除（它拥有带 watcher 的会话对象，恢复出来的副本没有）。

## v0.2.5 — 2026-10-01

### 新功能

- **stdio MCP 桥 + 按需后台实例**：给只能拉起本地子进程的 MCP 客户端（Claude Desktop 等）一条 stdio 命令——`termcp stdio` 前台按行读 stdin，把每条 MCP 消息转发到 HTTP MCP 端点，服务端回复（含通知）写回 stdout。端点是 `stdio` 后紧跟的可选值：不写就是本机 `http://<host>:<port>/stream`（streamable）；`sse`（或 `/sse`、或以 `/sse` 结尾的完整 URL）改用 SSE 传输——桥打开事件流，从 `endpoint` 事件学出消息 POST 地址；完整的 `http(s)://` URL 则指向任意 MCP HTTP 端点。桥是独立命令、纯转发，面向已在应答的实例：先跑 `termcp daemon start`（或直接 `termcp`、交给服务管理器）再用 `termcp stdio` 进桥；`termcp daemon stdio` 则是组合命令——一条命令先把后台实例拉起、再进桥（端点值直接跟在动作后，如 `termcp daemon stdio sse`；仅限落在 `--host`/`--port` 实例上的端点）；子命令必须写在最前、动作紧跟其后，动作写错位会被指回正确拼写（`termcp daemon start stdio` 会提示 `termcp daemon stdio`）。后台实例就是完整 Termcp（Web UI、REST、两种 HTTP MCP 全都在，浏览器打开即可旁观 Agent 正在驱动的同一批会话），日志落在 `<data-dir>/termcp.log`（追加写，不轮转）。Web UI 的 API / MCP / SKILLS 页（`/api.html`）在两种 HTTP 传输旁同步给出 stdio 的现成配置——mcpServers 的 `command`/`args` 与 `claude mcp add` 命令，`--host`/`--port` 随页面所在实例自动生成（回环默认值省略）。
  - **批量管道不丢回复**：`cat 脚本 | termcp stdio` 这类整批写入后立刻关闭 stdin 的用法也能取回全部回复再退出——streamable 侧首条消息按序转发、等 initialize 响应带回会话 ID 之后其余才并发（此前后续消息会抢在会话建立前发出，被服务端以 404 Invalid session ID 拒绝）；SSE 侧登记未答复的请求，EOF 后等到每条回复都出现在事件流上（此前 EOF 会立刻拆流，整批回复丢失、进程静默退出）。
  - **凭据不被接受在启动前就说清楚**：本地端点的预检除「有没有实例」外还检查认证——实例要求认证而桥未带（或带错）凭据时，直接提示 `pass its token with --auth-token, or its hash with --auth-hash` 并退出，而不是让每条消息去撞 401。
  - **空闲自动退出（默认按动作分）**：`termcp daemon stdio` 拉起的实例在无连接、无请求满 30 秒后自动退出，`termcp daemon start` 拉起的实例默认不限时、运行到被停止为止（两者都用 `--idle-timeout` 调整、`0` 关闭）。倒计时数的是「在途请求」——WebSocket、SSE、streamable GET 流这类整个连接都阻塞在 handler 里的请求让它随客户端在线而存活，客户端断开后重新开始倒计时；倒计时自实例创建即开始，因此没人连过的新实例同样会退出。这是常规退出路径，走与 Ctrl+C 相同的优雅关闭（会话置 DEAD、日志落盘）；`--idle-timeout 10m` 调整、`0` 关闭，前台实例绝不自动退出。前台跑着的 stdio 桥同样算一条连接——initialize 之前桥以轻量心跳（GET `api/version`）刷新倒计时，会话建立后由桥的长连接接管，因此 `termcp daemon stdio` 不会在首批 MCP 消息到来前被自己拉起的实例甩掉。心跳间隔按**实例自己上报**的倒计时算（`GET /api/daemon` 带 `idle_timeout_ms`），而不是按 30 秒默认值假设，所以实例即便用 `--idle-timeout 6s` 这类短倒计时起，桥也 ping 得比它快。实例报 0（不限时）就不 ping。
  - **管理动作收在 `daemon` 子命令里，且全部走 HTTP 端点**：裸写 `termcp daemon` 列出动作（start / stop / status / stdio）并提示 `--help`，`termcp daemon --help` 展开完整参数与细节；`termcp daemon status` 被动汇报端点上的实例（pid/url/version/日志路径，其探针请求整体不计入活跃、不会把倒计时「喂活」——探针的每个请求都带 `X-Termcp-Probe`，包括在 `/api/daemon` 被 401 后回落到的公开 `/api.md`，所以对开了鉴权的实例做无凭据 status 也不会喂活它）；`termcp daemon start` 确保实例在跑——端点上有东西应答就复用，**手动起的实例同样算数**（提示它没有空闲倒计时、需在原处停止），否则拉起一个分离实例等它就绪；`termcp daemon stdio` 在此基础上再留在前台当 stdio 桥；`termcp daemon stop` 请求实例经 HTTP 优雅停机。子命令必须写在最前、动作紧跟其后（`termcp --port 9000 daemon start` 这类写法会被指正）。查找与停止都不查进程表：实例在哪里监听就能在哪里找到，跨 data-dir、跨平台都成立；只有守护实例接受 stop，手动实例被如实汇报而不被「猎杀」。
  - **服务端新增两个端点**：`GET /api/daemon`（是否守护实例、pid、版本、启动时间、日志路径、生效的空闲倒计时 `idle_timeout_ms`；受认证保护）与 `POST /api/daemon/stop`（仅守护实例生效，手动实例回 409；先回 200 再优雅关闭）。`docs/api.md` 同步记录。
  - **认证贯穿 daemon 与 stdio**：管理命令（status/start/stop）与桥都按配置出示凭据——明文 token（`--auth-token` / `$TERMCP_AUTH_TOKEN`），或只保留了哈希时用哈希串本身（`--auth-hash` / `$TERMCP_AUTH_HASH`）；哈希配置的实例同时接受哈希串作为凭据（与明文等同机密），因此 `GET /api/daemon`、`POST /api/daemon/stop`、`/stream`、`/sse` 在两种配置下都可用。凭据经环境变量传给后台实例，不进 argv。

- **Agent 现在知道实例地址，也就能把人指到对的地方**：客户端连进来的那个地址挂在 `tools/list` 里 `notify_user` 的工具描述上，Agent 因此能在该让人去看、去输密码、去批准某条复核命令时直接给出 URL，而不是说"打开 Web UI"却不给地址。选这条通道是因为它是**唯一保证送达模型**的：工具描述不到达，模型就根本调不了该工具；而 `initialize` 的 instructions 在 MCP 里是可选的、很多客户端直接丢弃，把随请求变化的地址塞进这份固定规则还会多出一个真相来源。也不提供 `termcp://instance` 资源：`termcp://` 是 SSH 主机/会话/shell 的定位符命名空间，那个 URI 会被解析成一台名为 `instance` 的主机配置。地址每次请求各算一次、不会重复追加：主机名取自请求本身（`X-Forwarded-Host` 优先，反代改写了 `Host` 时靠它兜住），协议由 TLS 或 `X-Forwarded-Proto` 判定；只有不带 `Host` 的请求（HTTP/1.0）才回落到绑定地址，因此 `termcp stdio` 桥这类走回环的客户端拿到的是回环地址。`resources/list`、`resources/read` 的返回 URI 与 `learn-api` prompt 里的地址同样按请求各算一次，同一个实例对同一个客户端只给一个地址；读取时按路径匹配回注册的 URI，所以客户端拿到的地址直接读回来即可。
- **会话生命周期命令支持批量（逗号分隔 id）**：`session_terminate` / `session_delete`（MCP）与 `DELETE /api/sessions/{id}`、`POST /api/sessions/{id}/terminate|/disconnect`（REST）的 id 参数接受逗号分隔列表（如 `a,b,c`；MCP 侧每项同样可以是 `termcp://` 定位符）。逐条独立执行、逐条返回结果（`ok` / `code` / `error`，code 为 `session_not_found` 或 `operation_failed`），单条失败不再中断其余——此前 Web UI 清理已结束会话与批量删除逐个发请求，中途一条失败（会话已被其他客户端清掉、Windows 上日志文件被占用）整条链就断，后面的全部不执行。单 id 的请求与响应契约完全不变（204/404 等）；Web UI 两处批量流程改为一次请求，部分失败时在提示里列出具体是哪几条。
- **纯 API 构建（`-tags no_webui`，`make build-api`）**：产物是 `dist/termcp-api-<os>-<arch>`，可与完整版并存。该构建不嵌入也不注册 Web UI——`/`、`/api.html`、`/static/*` 一律 404——只保留 REST、MCP、WebSocket 与两份 agent 文档（`/api.md`、`/skills.md`）；默认构建（`make build`）完全不受影响。CI 在三种平台上编译该变体并跑配套测试。
- **Web UI 标题旁显示构建版本**：`h1` 右侧由新端点 `GET /api/version` 填写版本号，与 `termcp -version` 的首行相同（HEAD 正好带 tag 时为该 tag，否则 `dev-<commit>`，脏工作区再缀 `-dirty`；`go build`/`go install` 则回落到 Go 工具链嵌入的模块版本——release tag 或本地检出对应的伪版本；`dev` 表示二进制里没有任何版本信息，`go run` 与 `-buildvcs=false` 都属此类，不代表源码未打 tag），脚本可据此判断实例版本而无需解析 CLI 输出；取不到就留空，不占位。浏览器标签页图标（Web UI 与 `api.html`）新增为与终端一致的 `terminal-shell.svg`。`docs/api.md` 新增第 13 节记录该端点。

## v0.2.4 — 2026-09-29

### 新功能

- **终端时间轴轨道（rail）**：终端右缘一条细轨，每个终端行一个格子，按该行内容的状态着色——绿色是 shell 输出，蓝色是人在 HTTP/WebSocket 上的输入，橙色是 AI 经 MCP 的输入；悬停（桌面）显示状态与时刻，点按（触屏）打开详情卡，给出时刻、距今、字节区间与长度。轨道只读：点格子不会滚动终端。
  - **行↔字节的映射由服务端推导**：`GET /api/shells/{id}/rail?cols=…&top=…&count=…&height=…` 把该 shell 的字节日志在指定宽度下重放进一个小终端模型——按列宽换行、回车覆盖、退格、制表、光标寻址、行/屏擦除、宽字符占两列——每个终端行得到它当前显示的字节区间（`spans`；该行没有字节则为 `null`，一个格子都不画），空行与输出的换行照常占格。同一响应体里附上覆盖这些字节的 marks 与总行数，画一条时间轴因此不用下载字节，也不用第二次请求。映射从不记录：重载的通道一次写入整个转录，缩放窗口又让每一行重排，记录下来的行号在那一刻就已过时——日志与宽度是它仅有的输入，所以每次请求按当时的宽度重新推导，加载、缩放、滚动走同一条路径。`cols` 必填（缺失或超出 2–1000 返回 `400`），`top`/`count`/`height` 越界回落到默认值；布局读取在窗口边缘停止，窗口以下的日志不再重放。客户端在终端有新输出与缩放时刷新，并有最小间隔兜底，持续输出不会变成请求风暴；取回的窗口在屏幕两侧各留一屏余量，落在余量内的滚动只是一次重绘，全屏程序占屏时轨道收起、退出后原样回来。
  - **一格是一行，不是一段字节**：进度条把一兆字节画进一行，慢速粘贴把两个字节铺成十行——按字节比例铺开的轨道会把这两者排反。一行上多种状态并存时取「Agent 输入 > 人的输入 > 输出」；复核决定（`q`/`A`）不夺走该行的颜色，而是以徽章出现在它判定那行的卡片上。
  - **一段输出带上产出它的输入的颜色的框**：最近一条前置输入（人=蓝、Agent=橙）给其后的一段输出描边，输出本身仍是绿色，因此「这段是谁的命令打出来的」一眼可读，而「这里有输出」不因此被稀释。框只包住整段——首行上边、末行下边——中间的行没有横边，一段命令的输出读起来是一个整体。颜色取自样式表里输入状态自身的值，`--assets` 改一处即可换色。
  - **窗口化取数，成本不随日志增长**：`marks` 端点接受 `start`/`end`，只回答决定该窗口的标记——窗口起点所在的一条、窗口内的每一条，以及作为最后一段终点的下一条（它不是窗口的区段，只是边界）；两个参数必须成对出现，半个窗口返回 `400` 而不是悄悄给出整份索引。服务端为此维护一份稀疏索引（每 64 条标记记一个 `log.jsonl` 内位置，只在内存中），窗口请求只读到窗口附近。200k 标记 / 97 MiB 日志下，整份索引约 12 MiB、约 0.36 s，一屏窗口是 KB 量级、亚毫秒级。`rail` 的 marks 走同一条平铺路径，两个端点不可能对「一段是什么」给出不同答案；不传参数的 `marks` 形状与语义完全不变。
- **shell 通道状态标签**：标签条上的 chip 从只显示「结束」变成实时状态——`输入中…`（人在打字）、`AI…`（Agent 在写）、`输出中…`（已提交，命令在跑）、`已完成`（数秒无字节）、`结束`。服务端为每次输入推送 `shell_activity` 帧（`shell_id`、`src`、`submit`），浏览器自己的按键也走这条服务端路径，而不是前端自己猜——Agent 经 MCP 的输入根本不会经过页面。`submit` 由服务端给出，因为终端字节流不划分行：重绘会输出换行与光标移动却不结束任何一行，靠看换行判断会把还在输入的行判成已提交；提交（回车）与 ctrl+c/d/z 这类结束一行的控制字符由输入侧知道真相。
- **`--assets`：外置静态资源目录**（默认 `~/.termcp/assets`，可用 `$TERMCP_ASSETS_DIR` 覆盖）：目录里存在的文件覆盖内嵌副本，目录里没有的文件回落到内嵌，目录不存在等于什么都没发生。Web UI 与 MCP 文档资源经同一个组合 FS 解析，因此改一处样式不必重新编译。路径存在但是文件（不是目录）时启动即报错，而不是带着一个永远不生效的覆盖继续跑。

### 改进

- **定位符相关的重复实现收敛到一处**：这一轮修复暴露出同一条规则被复制在多个包/多个 handler 里，任何一份漂移都会让「同一个字符串」在两个入口得到不同结论（#73 与审批绕过都是这个成因）。因此顺手收敛：`session.PrimaryShellIndex()` 取代 MCP 与 Web UI 各自私有的 `primaryIndex` / `primaryShellIndex`；`session.ShellIndexOutOfRangeError()` 统一四处「序号越界」文案（MCP 活会话、MCP DEAD 会话、MCP 读路径、REST `/api/resolve`），同一个定位符不会再因入口不同而收到不同解释；`api.LessShellCreationOrder()` 成为唯一的创建序比较器（`storage` 排序持久化快照、`session` 排序内存快照与位置回退共用），避免 tie-break 不同导致同一个 N 解析到不同频道；`session.IsInternalPrimaryShell()` 收拢「内部主 shell 的关闭是无操作」这条策略（MCP `shell_close` 与 Web UI `DELETE /api/shells/{id}` 共用），避免一个入口删掉另一个入口拒绝删的东西。行为不变，仅收敛实现。

- **`docs/api.md` 记录时间轴与新的 WS 帧**：新增 `/rail` 一节——查询参数、`spans`/`marks`/`total_rows`、模型能精确到什么、备用屏为什么什么都不画；`marks` 一节写明窗口参数与两种取数的成本差；WS 帧表补上 `shell_activity`，`terminal` 帧不再携带日志字节区间——能记录的映射都会在重载或缩放时过时，行映射由 `/rail` 推导。

### 修复

- **输入标记只在提交一行时写入**：v0.2.3 里 `WriteStdin` 成功就记一条零长度标记，于是「开始打字」这个时刻本身成了区段起点——从那一刻起的所有字节（包括别的命令正在输出的内容）都被算进这段输入，后面的输出区段被吃短（实测：输入 `sdfaf` 加 5 个退格，产生一条 27 字节的 `i` 区段，把提示符和回显都圈了进去）。现在只有回车（或 ctrl+c/d/z）才写标记：一行 = 一条标记，未提交的按键只用于通道状态，不进日志。密码仍然不进日志——终端不回显就没有字节可存。

## v0.2.3 — 2026-09-27

### Breaking

- **会话不再决定容器的生死**：shell 的自然退出、手动关闭（哪怕关掉最后一个）都不再把 Session 翻为 `exited`/DEAD。Session 持有的是 SSH transport，端口转发、SFTP 和 `shell_open` 都只依赖它，因此“零 shell 但 `running`”是合法且可复用的状态；`exited` 只由 `session_terminate`、断线、server shutdown 产生。此前 `session_start(mode="pipe")` 的单次命令跑完就丢掉整个连接（连带关闭端口转发），现在需要显式 `session_terminate`。
- **模式（pty/pipe）改为 shell 级属性**：`session_start` 的 `mode` 只作用于它创建的首个 shell（`default_mode` 同理），后续 shell 由 `shell_open` 的 `mode` 决定；会话记录不再带 `mode` 字段。`shell_resize` 只接受 pty shell，对 pipe shell 报错而不是静默无效。

### 修复

- **Web UI 端口转发弹窗不再报“没有活动会话”**：弹窗打开时没有记录当前会话上下文，导致每次创建转发都被前端拒绝；现在恢复该上下文赋值（回归来自 v0.2.2 删除旧工具面板的改动）。
- **不再拿本机 PATH 去猜目标机的 shell**：`command` 为空时客户端曾用本机探测到的 shell（Windows 上常是 `C:\Program Files\WindowsApps\...\pwsh.exe`）作为 SSH exec 命令，远端 zsh 因此报 `command not found`。现在空命令按**命令优先级链**解析：调用方 command → profile 的 `default_shell` →（仅 pty）目标机自己的登录 shell。`pipe` + 空命令且 profile 无 `default_shell` 直接拒绝——pipe 通道没有登录 shell 可申请，猜一个只会把本机路径发到远端。`default_shell` 现在是该连接上**每个** shell 的默认命令，不再只影响首个 shell。

### 新功能

- **Web UI 新建 shell 通道改为按钮 + 展开菜单**：标签条尾部与空面板的 “+” 按钮为主键（直接新建默认 pty 登录 shell）+ 下拉箭头（向上展开菜单）。菜单可选 `pty` / `pipe`，选定后弹对话框填写命令：pty 留空即登录 shell，pipe 必填。对话框接受**命令行**（`ls -la`、`python -m http.server`），前端拆成可执行文件 + 参数再发请求——REST/MCP 接口收的仍是 argv，整行直接当作可执行文件会报 `executable file not found`。
- **主机配置表单按连接顺序排列**：凭据 → 本配置项（默认 Shell、默认审核）→ 网络路径（代理 → SSH 跳板链），跳板链置于表单最下方——它是最后真正拨号到目标的一段。内建回环配置只需隐藏网络路径部分，不再连带隐藏默认 Shell / 默认审核。

## v0.2.2 — 2026-09-25

### Breaking

- **会话输出改用追加式字节日志**：每个 shell 现在以 `log.bin` 保存终端看到的完整字节流，并以 `log.jsonl` 记录来源、时间和偏移；会话与 shell 列表直接从 `sessions/<session_id>/<shell_id>/` 目录树派生，不再维护 `sessions.json`、`messages/` 等重复索引。旧布局不会迁移、删除或继续读取，因此升级后旧会话不会出现在 `session_list` 中。所有时间戳统一为 Unix 毫秒；`message` 工具只保留 `action="list"`，内容改由 `shell_output` 按 offset 读取。
- **终端传输不再使用 base64**：WebSocket 与 REST 响应中的 `d` 现在是普通 JSON 字符串，客户端必须停止 base64 解码。磁盘上的 `log.bin` 仍保留原始字节；只有传输层遇到非法 UTF-8 时会显示为 U+FFFD。
- **删除 `GET /api/connection-templates`**：新建主机表单的默认模板由 Web UI 自己维护，服务端不再提供该端点。

### 新功能

- **复核模式：Agent 写入前由人审批**：可按会话开启，覆盖该会话的全部 shell。开启后，Agent 经 MCP 发起的终端输入、文件写操作和端口转发变更都会先进入复核队列；读取操作以及人通过 WebSocket / REST / Web UI 发起的操作不受影响。
  - `shell_input` 先暂存文本，`shell_key` 再把文本与结束键合成一条完整命令行，避免把一次命令拆成多次审批；裸 `Ctrl+C`、`Enter` 等按键仍可独立进入队列。文件与转发请求显示可读摘要和操作类型。
  - 每个请求由一名复核者批准或拒绝；AI 不能自行审批，也拿不到可用于审批的 `pending_id`。批准会立即执行原操作，执行失败返回 `409`；拒绝、过期、关闭复核或会话结束都会 fail-closed，未执行内容不会在之后恢复。
  - 复核面板由终端标签条中的锁打开，锁、会话标签和会话卡片都会显示待复核数量；点击审批通知会直接聚焦对应会话并打开面板。手机端标签条可横向滚动，复核入口不会挤掉会话标题。
  - 终端输入的复核状态写入对应 shell 的 `log.jsonl`（`q` 表示进入队列，`A` 表示批准并写入）；Agent 可用 `shell_notify` 等待审批结果，无需轮询。
  - 连接配置新增 `default_approval`，可让由该配置创建的会话默认开启复核，包括 Agent 创建的会话。内置 `internal` 配置也可保存这一覆盖项；未配置时默认关闭，损坏的覆盖文件会报错而不是静默放行。
  - 复核的边界是 MCP 写操作，不解析 shell 语义；`session_start(command=…)` 当前仍不进入复核队列。
- **Web UI 支持三种语言**：新增 English、简体中文和繁体中文（台湾用语），首次访问按 `navigator.languages` 选择，也可在标题栏切换并持久化为 Auto / ENG / 简中 / 繁中。切换语言不会刷新页面、重新请求列表或重建正在运行的终端。
- **触屏端专用工作区**：手机和平板按触控能力识别，终端使用全屏布局并适配旋转、动态视口和安全区；标题栏提供主机抽屉与实时会话切换。主机列表改为左侧抽屉，会话区直接出现在首屏，新增主机入口位于列表末尾。

### 改进

- **Web UI 导航与窗口交互**：桌面会话标签栏可拖动并记住位置；终端窗口初始尺寸会限制在视口内，四角均可调整大小。触屏端可在未开窗、最小化、折叠和全屏状态之间可靠地切换、恢复并置顶会话。
- **会话卡片语义更直接**：点击名称即可重命名，点击 session ID 即可复制；运行状态改为 ID 左侧的绿/红指示灯，选择框与名称归为一组。带 `session_id` 的通知支持鼠标和键盘直达对应会话。
- **错误信息始终可见**：连接失败和 WebSocket 重连提示移到可折叠区段之外，并可单独关闭；即使主机抽屉已收起或会话区已折叠，也不会隐藏关键状态。
- **前端资源模块化**：原本约 252 KB 的单文件 Web UI 拆为精简 HTML、独立 CSS 和按功能划分的 JavaScript 模块，便于浏览器分别缓存；同时删除无入口且与终端内 Files / Forwardings 重复的旧工具面板。

### 修复

- **SSH 客户端断开后不再遗留子进程**：内部 SSH 会话会在连接上下文结束时终止仍在运行的 child process，避免泄漏进程、goroutine 和 Windows PTY 句柄。
- **修复触屏端会话恢复与层级问题**：最小化不再销毁终端；全屏窗口可正确置顶；会话切换菜单使用最新服务端快照；主机编辑弹窗、加载提示和新增主机卡片在窄屏下保持可见、可点。

## v0.2.1 — 2026-09-20

### 新功能

- **`--disable-auth` / `TERMCP_DISABLE_AUTH_TOKEN`**：显式关闭 HTTP 认证，非 loopback 绑定也放行（此前无认证绑定非 loopback 直接启动失败）。用于录屏、演示、单用户工作站等 loopback-only 场景；启动日志降为 warn。与 `--auth-token`/`--auth-hash` 同时配置视为矛盾并直接报错，不做静默取舍。环境变量只接受 `1`/`true`/`yes`/`on`，`=0`/`=false` 等按未设置处理，避免误开。
- **`--mcp-defer-tools`（默认关闭）**：开启后才给 19 个低频工具（11 个 SFTP `file_*`、`forward`、`shell_resize`/`shell_detect`/`shell_notify`/`shell_reader_register`/`shell_reader_unregister`、`message`、`ssh_config`）打 `defer_loading` 标记，12 个核心工具永远立即可见。默认全 31 个工具带完整 Schema 一次列出——不支持懒加载的客户端、或经网关丢弃标记的链路（实测 Codex 0.155.1 + AxonHub 会吞掉带标记的工具，模型侧看到“零工具”）会因标记丢失而完全看不到这些工具。`Server.shouldDeferTool` 为唯一判定点，`TestDeferLoadingPolicy` 覆盖两种模式。

## v0.2.0 — 2026-09-20

### Breaking

- **非 loopback 绑定必须配置认证**：监听 `0.0.0.0`、局域网 IP 或非 `localhost` 主机名时，若未提供 `--auth-token` / `--auth-hash` / `TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH`，启动直接失败（此前允许无认证运行）。loopback 绑定保持无认证默认行为不变。
- **`session_start` 必填 `ssh_config`**：此前未传 `ssh_config` 时会隐式回退到 `"internal"`（本机 loopback），导致当调用方意在连远端主机但漏传配置（如只传了 `name`）时会误打开宿主机终端。现在必须显式指定 `ssh_config`（连宿主请显式传 `"internal"`，连远端传 profile 名称）；未传或为空时直接返回 `invalid_argument`（`ssh_config is required`）。
- **`shell_output` 统一游标读取**：唯一输出读取入口，活会话（内存缓冲）、已退出会话（保留缓冲）、已关闭/重启恢复会话（磁盘消息流）全部同一套字节流游标语义。新增 `offset`（无状态字节定位，配合 `start_offset`/`end_offset`/`total_bytes`/`has_more` 翻页）与 `tail_lines`（只取末尾 N 行，token 友好）；返回体新增 `source`/`session_id`/`shell_id` 等游标元数据。
- **删除 `history(action=get_transcript)`**：已关闭（DEAD）会话输出读取并入 `shell_output`（`shell_id=会话id或shell_id`），不再提供全量转录导出，避免一次性把整个会话拖入 LLM 上下文。随归档子系统一并移除 WebUI 的 `GET /api/history/{id}/transcript` 导出，翻页改用 `/api/shells/{id}/output-range`。
- **低频工具合并为 action 枚举**：`local_forward` / `remote_forward` / `dynamic_forward` / `list_forwards` / `close_forward` → `forward(action=...)`；`message_list` / `message_get` → `message(action=...)`；7 个 `history_*` 工具 → `history(action=...)`；5 个 `ssh_config_*` 工具 → `ssh_config(action=...)`（写操作通过 `--mcp-manage-ssh-configs` 开关）。
- **删除 9 个低频文件工具**：`file_chmod` / `file_chown` / `file_chtimes` / `file_readlink` / `file_symlink` / `file_link` / `file_truncate` / `file_realpath` / `file_statvfs` → `file_perm` / `file_link` / `file_fs`（各带 `action` 枚举）。
- **工具总数 31**：`history(action=get_transcript)` 移除、新增 `session_delete`（彻底删除会话），`shell_output` 新增 `offset`/`tail_lines` 两参数。

### 新功能

- **单一静态 Token 的 HTTP 认证**：新增 `internal/auth` 包 + 共享 mux 外层中间件，一次覆盖 Web UI、REST、MCP SSE、`/stream`、WebSocket。配置 `--auth-token` / `TERMCP_AUTH_TOKEN`（明文）或 `--auth-hash` / `TERMCP_AUTH_HASH`（`sha256-<salt_hex>-<digest_hex>`，`digest = SHA256(salt || token)`，服务端不落明文），二者互斥、flag 优先于环境变量。凭据按序尝试 `Authorization: Bearer` → `Authorization: Basic`（密码字段即 token，用户名忽略）→ `termcp_token` cookie（Basic 成功后自动下发：HttpOnly、SameSite=Strict，TLS 下带 Secure；解决浏览器 WebSocket 握手无法自定义请求头的问题）；全部失败返回 `401` + `WWW-Authenticate: Basic`（浏览器原生登录框，无自定义登录页、无 `/auth/login`）。校验用 `crypto/subtle` 常数时间比对，token 不进日志、不进 URL。新增 `--gen-auth-hash` flag：生成 token 的 salted SHA-256 哈希（供 `--auth-hash` 使用）后退出；token 取自参数，或在不带参数时从 stdin 无回显读取（不进 shell 历史）。文档同步更新 README（中英）、`docs/api.md`、`docs/architecture.md`、Web UI `api.html` 与 Docker 示例。
- **反向唤醒通知 `shell_notify`**（信令与数据分离）：新增 `internal/notify` 统一通知内核，支持 `event=output`（双沿触发：立即 + 2s 尾沿兜底）/`exit`（一次性，进程退出/SSH 中断）/`silence`（N 秒无输出，一次性）；双通道 `resource`（广播 `notifications/resources/updated`，uri `termcp://shells/<id>`）与 `sampling`（向**注册规则的客户端 session** 发送 `sampling/createMessage`，systemPrompt `termcp notification daemon`）；全局 1s 冷却阀防刷屏；`register`/`unregister`/`list` 三个 action，shell 退出/关闭/会话删除自动级联反注册（零协程/定时器泄漏）。MCP 层用 `AddTerminateListener` 与 forward 清理共存（不再互相覆盖）；sampling 在注册时捕获 `ClientSession`，解决定时器 goroutine 中无 session 导致发送失败的问题。Web UI 终端窗口新增 **Notifications 标签页**：实时列出该会话已注册的通知规则（event/channel/silence 秒数），可一键拆除；新增 `GET /api/notifications`（支持 `shell_id`/`session_id` 过滤）与 `DELETE /api/notifications/{id}`，规则变更经 `notify.Manager` 回调广播实时刷新。文档：`docs/mcp-tools.md` 新增 `shell_notify` 章节，README 特性表补充。
- **资源 URL 寻址与复制按钮**：为 Termcp 资源定义统一的 URL 寻址——entry `termcp://[entry_name]`（如 `termcp://internal`）；session `termcp://#[session_name]`；shell `termcp://#[session_name]:[shell_index]`。`[session_name]` 取**会话 id**（卡片上等宽小字，无 `session-` 前缀），`[shell_index]` 取会话内**频道顺序（1 起）**，与频道标签 `shell-1`/`shell-2` 一致，省略序号 = 首个 shell。session/shell 一律使用无 entry 前缀的**短格式**（会话名由用户命名、与 entry 名无对应关系，前缀不可靠）；旧的长格式 `termcp://[entry]#[session][:N]` 仍被接受以兼容既有链接，但 entry 部分被忽略、以会话 id 为准。`termcp://shells/<shell_id>` 是通知广播专用 URI，明确拒绝作为定位符。Web UI 新增/改造小复制按钮，统一复制短格式 URL：entry 卡片名字旁（新增，紧贴名字）、session 卡片（原复制 session id 改为 URL）、终端窗口标题栏与 Tools 面板、以及**每个底部 shell 频道标签**（新增，复制该频道的 `:index` URL）。点击复制按钮不触发连接/切换频道/关闭窗口。
- **MCP 工具直接接受 `termcp://` 定位符**（`internal/mcp/resource_url.go`）：新增定位符解析器并接入工具参数解析链，用户可直接把 Web UI 复制的 URL 粘进对话，无需先查询再换裸 id。`session_start(ssh_config="termcp://mac")` 解析为 entry profile；`session_terminate` / `session_info` 的 `session_id` 接受 `termcp://#<sid>`；`shell_input` / `shell_key` / `shell_output` 等 `shell_id` 参数接受 `termcp://#<sid>`（→ 首个 shell）与 `termcp://#<sid>:N`（→ 第 N 个频道），同时兼容传入裸 session id（自动落到主 shell）。已关闭（DEAD）会话的定位符或裸 id 返回带提示的 `session_not_found` 错误（引导改用 `shell_output` 读取），频道序号越界返回 `shell_not_found` 并附会话实际频道数，畸形定位符返回 `invalid_argument` 并附解析诊断。MCP instructions 与工具 schema 同步说明定位符用法。
- **AI 直读文档（HTTP + MCP resources）**：实例把自己的文档随二进制发布，同一 HTTP 端口直接提供 `GET /api.md`（全英文 REST + WebSocket 权威参考，`text/markdown` 显式声明、不依赖平台 mime 表）与 `/skills.md`（curl 技能包，下载后存为 `~/.agents/skills/termcp/SKILL.md` 即为可加载的 agent skill）；MCP 侧工具参数全部由 `tools/list` schema 自描述，不冗余提供工具文档——不装 MCP、不用打开 Web UI 也能让 AI 学会调用 HTTP API。MCP 侧把这两份文档注册为 resources（`resources/read` 的 URI 就是实例的 `<origin>/…` HTTP 地址，一个 URI 两条获取路径），新增 `learn-api` prompt 引导 agent 先读文档再动手；initialize instructions 增补第 10 条指向这批文档。Web UI 的 **API / MCP** 页（`/api.html`）新增 **2. Agent docs & skill** 区：技能下载地址（可点 Download / 一键 Copy）、`curl` 安装命令与两份文档的 URL 列表，不再只能靠手拼路径。仓库内 `docs/api.md` 为唯一真源，`make sync-assets` 同步到 `internal/webui/assets/`，`TestSyncedDocsMatchSource` 防漂移；同时修正 `WithResourceCapabilities(true,true)` 的虚假声明（mcp-go v0.50 无 subscribe 处理，改为不宣称 subscribe/listChanged）。
- **文档端点免认证（仅 GET/HEAD）**：`/api.md` 与 `/skills.md` 是纯静态、零数据、零机密的文档，配置 `--auth-token` 后也允许**无凭据**获取——否则"不装 MCP、先读文档再用 API"的流程在带 token 的实例上直接 401 死锁（agent 还没拿到 token 就没法学怎么用）。其余所有面（Web UI/REST/MCP/WS）依旧全部要求凭据；写方法与路径穿越均不放行（`internal/auth` 白名单精确匹配 + 测试覆盖）。
- **REST 定位符解析 `GET /api/resolve?url=termcp://...`**：把 Web UI 复制的定位符在 HTTP 侧一次性解析成具体 id，纯 REST/curl 的 agent 不再需要自己实现语法解析。返回 `{"kind":"entry"|"session"|"shell", ...}`：entry → `ssh_config`；session → `session_id`/`name`/`status`；shell → `session_id`+`shell_id`+`index`（1 起，与 `shell-1`/`shell-2` 标签一致）。已关闭（DEAD）会话返回 `"status":"exited"`（只读）；错误语义：400 畸形定位符、404 未知 profile/会话/序号越界、409 对已关闭会话用 shell 定位符。定位符解析器抽到共享包 `internal/locator`（MCP `internal/mcp/resource_url.go` 改为薄适配层，两边同一份语法与测试）。
- **skill 安装目录写清 Claude Code 与共享约定**：实测确认 Claude Code（2.1.x）只读 `~/.claude/skills/<名字>/SKILL.md`，**不读** `~/.agents/skills/`（后者是其他 agent 的共享约定目录），此前文档只写了 `~/.agents/skills/termcp/SKILL.md`，导致装完 Claude 侧根本没加载、自然"不知道 termcp:// 是啥"。README（中英）、`docs/api.md`、`/skills.md`、`/api.html` 统一改为：Claude Code 用 `~/.claude/skills/termcp/SKILL.md`，其他 agent 用 `~/.agents/skills/termcp/SKILL.md`，项目级为 `<project>/.claude/skills/termcp/SKILL.md`；并说明 skill 在会话启动时加载、需重启会话，Claude Code 无 per-skill 子命令（安装=写文件，卸载=删目录，插件形态用 `claude plugin install/uninstall`）。README 同步把"三种入口"扩为"四种入口"（新增 Agent Skill 行）、介绍段与快速导航加入 skill 条目。
- **Agent Skill 更名 `termcp-http` → `termcp` 并补齐定位符用法**：`/skills.md` 的 frontmatter `name: termcp`、安装目标 `~/.agents/skills/termcp/SKILL.md`（目录名即发现名，与 name 一致）。description 明确点名 `termcp://` 定位符，使 agent 在用户说“打开 termcp://rock64”时能命中该 skill；正文新增 **3. Resource locators (termcp://)** 一节：三种形式的语法表、`GET /api/resolve` 的 curl 用法与返回示例、“打开 entry 定位符 = `POST /api/sessions {ssh_config}`”的语义、已关闭会话只读与错误码，以及“别把定位符当命令/网页去打开”的硬规则。README（中英）新增 **Agent Skill** 独立小节并强化特性条目。
- **REST 终端 I/O（无 MCP 的 CLI 路径）**：新增 `POST /api/shells/{id}/input`（`{"text","press_enter"}`）、`POST /api/shells/{id}/key`（`{"key","repeat"}`，键名同 `shell_key`）、`POST /api/shells/{id}/resize`（`{"rows","cols"}`），把此前仅有 WebSocket 可写的输入/按键/尺寸能力开放给脚本与 curl；错误语义统一（404 未知 shell、409 非运行态、400 参数错误）。

### 改进

- **交互式 shell 裸启动，不再注入任何启动参数**：移除 `disableHistoryExpansion`（zsh `-o NO_BANG_HIST` / bash `+o histexpand`）。这些参数是 shell 私有选项，目标机没有 bash/zsh、`$SHELL` 或 `/bin/sh` 指向 dash/busybox ash 时会把参数当非法选项直接拒绝：`/bin/sh: illegal option +o histexpand`，交互会话根本起不来。现在 `$SHELL` 检测到什么就原样启动什么，不追加任何 flag；代价是 bash/zsh 交互会话恢复默认的 `!` 历史展开，含 `!` 的密码/URL 需自行 `set +H`（bash）/ `unsetopt banghist`（zsh）或加引号。
- **Web UI 仅在窗体打开时拦截离开页面**：离开页面保护（`beforeunload`）此前因统计了服务端后台运行的会话和端口转发，导致即使用户关掉了所有 session 终端窗体，关闭/刷新网页时仍会弹出“系统可能不会保存您所做的更改”。现在调整为仅在页面上实际存在打开的终端窗体（含浮动与平铺网格窗体）时才弹窗拦截；窗体全部关闭后直接离开，不打扰用户。
- **修复快速命令输出被截断**：SSH 会话在进程退出时立即上报 exit-status，此时末尾 stdout 可能仍在通道缓冲中未读，而旧的读取循环一看到进程退出就停止、随后立刻封存缓冲，导致 `echo`/`ls` 之类快速命令丢失最后几行。现在读取循环以通道 EOF 为准持续读取，封存缓冲前先等待该 shell 的输出管道排空（带超时兜底）。
- **修复 PTY 窗口改动被丢弃**：内部 SSH 会话（`pty` 模式）此前在服务端又开了一个 window-change 消费者，与库自身的 resize 处理竞争同一个通道，约一半的 resize 事件被丢弃——WebUI/MCP 调整窗口后子进程终端尺寸时大时小。现在统一交给库处理，客户端 resize 可靠地传到子进程（新增 `stty size` 回归测试）。
- **清理并发读写隐患**：内部 SSH 服务端不再跨 goroutine 直接读会话的 PTY 结构（改为在 session 请求 goroutine 上取一次再交接），PTY 的 fork/exec 与库的 PTY 关闭用同一把锁串行；`ExecSession` 的 stdin 写入与关闭也串行（x/crypto 的 `Write` 与 `CloseWrite` 并发会竞争通道 EOF 标志）。内存 SSH 传输 `duplexConn` 的 `Close`/`Write` 不再可能 `send on closed channel`。`go test -race ./...` 现全绿。
- **结构化工具错误码**：工具失败结果不再是纯文本消息，而是一个带专用字段的 JSON 对象 `{"error_code":"...","error":"..."}`（仅失败结果携带）。Agent 可像处理 `(value, error)` 一样按 `error_code` 分支，无需字符串匹配；`snake_case` 码包括 `invalid_argument` / `session_not_found` / `shell_not_found` / `session_not_running` / `reader_not_registered` / `history_not_found` / `forward_not_found` / `ssh_config_not_found` / `rule_not_found` / `conflict` / `not_configured` / `connection_failed` / `operation_failed`。`forward` 与 `ssh_config` 内部改用 sentinel error（`ErrNotFound`），经 `errors.Is` 映射为对应错误码；WARN 日志新增 `error_code` 属性。
- **修复 `file_read` 越界/超大读取导致服务端 Panic**：当 `offset` 超过文件大小且 `length` 省略（或为 0）时，旧的 `length = totalSize - offset` 会得到负数而触发 `makeslice: len out of range`。新增纯函数 `normalizeReadRange` 归一化窗口：不计算 `offset+length` 避免溢出，`offset` 钳制到 `[0, totalSize]`，`length` 不为负；text/hex 模式单次读入上限 8 MiB（超出置 `has_more` 并可用 `offset` 翻页），`mode=file` 下载仍不限流。

- **修复已退出/恢复会话调用文件及转发工具导致 Panic 的问题**：当对已退出（DEAD）或重启后从磁盘恢复的无活跃 SSH 连接的会话调用 `file_write`/`file_read` 等 SFTP 工具或端口转发工具时，`sftpClient` 与端口转发函数补充了 `SSHClient == nil` 的防御性校验，返回规范的 MCP 工具错误，避免了 `pkg/sftp.NewClient(nil)` 空指针解引用崩溃；同时在 `internal/sftp.NewClient` 与 Web UI 文件 API 中增加了对空连接的防守。
- **Web UI 窗口拖拽 resize 体验与 PTY 尺寸同步**：增大拖动手柄触发面积至 30×30px 并提升 z-index 防止被右下角滚动浮标遮挡；改用 Pointer Capture 杜绝甩出窗口时的鼠标丢帧；rAF 节流配合强制重排消除拖动滞后；修复松开鼠标（`onUp`）时活动 shell 通道未派发远程 PTY 尺寸同步的问题。
- **Web UI Sessions 列表批量选择与删除**：标题栏常驻三个纯图标按钮——全选/取消全选（复选框两态图标）、红色垃圾桶批量删除选中（无选中时置灰，气泡提示选中数量）、扫帚一键清理已退出 Dead 会话（弹窗确认后顺序批量删除）；标题栏左侧在选中数 N>0 时实时显示 `[N selected]`。卡片右上角叉号始终可单删；右下角复选框常驻，点击（`stopPropagation`）切换选中态，卡片主体点按仍打开/聚焦终端；选中卡片显示蓝色描边。动态刷新保留已选集合并与全选状态、计数双向联动。
- **引导 Agent 偏好长连接交互会话**：精简 instructions 第 2 条明确指出推荐单个交互会话（保持 cwd/env/审计历史），澄清 `shell_output` 返回的是新增字节（读空 ≠ 没输出，需继续轮询），警告 `session_start` 的 `command/args` 是 run-and-exit 单次程序，不应用于多次分拆 `bash -c`。
- **已关闭会话默认读取不再全量倾倒**：无 `offset`/`tail_lines` 的已关闭会话读取默认返回末尾最近一块（≤8 KiB，行对齐），配合 `has_more`/`end_offset` 翻页；`max_bytes` 缺省 8192 与 schema 一致（显式 `0` 仍表示不限）。
- **SSH 连接失败可见性**：会话创建失败（如 `ssh dial` 超时/拒绝）现在在 Termcp 终端打出 `[ERROR] session create failed`（含目标地址、超时、模式，不含凭据）；MCP 工具错误结果从 Debug 升级为 `[WARN]` 并附带错误预览；Web UI "测试连接"失败同步打 `[WARN]`。连接类错误（超时/拒绝/不可达/重置）自动追加 `Hint:` 诊断提示，MCP 工具结果与 Web UI 响应同样携带。
- **拨号错误上下文**：直连失败错误信息包含目标地址与拨号超时，如 `ssh dial: connect 192.168.0.145:22 (timeout 30s): dial tcp ...`，不再只有裸的 `i/o timeout` / `connectex ...`。

- **`ReadOutput` timeout 修复**：接受 `timeout=0`（非阻塞轮询），下限从 0.1 改为 0。
- **`ssh_config` 写操作双保险**：schema 层面 `action` enum 默认仅 `list`；dispatcher 层面即使客户端绕过 schema 也会拒绝。
- **统一 dispatch 架构**：新增 `group_handlers.go`，4 个 action dispatcher 复用现有 handler。
- **指令精简**：12 条 → 7 条（3,054 B → 1,467 B）。

### 修复

- **已关闭（DEAD）会话的文件/转发类工具统一返回 `session_not_running`**：此前 `file_read`/`file_write`/`file_stat` 等拿到的是已关闭的 SSH 连接，报出底层 `SFTP: sftp: use of closed network connection`（错误码 `operation_failed`）；`forward` 更会在关闭的会话上成功创建一个永不工作的本地监听；`shell_open` 返回模糊的 `session failed`。现在四类入口（`file_*` 含 `file_urls`、`forward`、`shell_open`）统一以 `session_not_running` 拒绝，错误文本提示改用 `shell_output` 读取输出；REST 侧 `GET /api/sessions/{id}/files` 与 `POST /api/sessions/{id}/forwards` 同步改为 409。
- **会话转入 DEAD 时级联关闭其端口转发**：`session_terminate`、进程自然退出、连接断掉或服务重启（`MarkAllDead`）都会触发 `internal/session` 的 `SetOnDeadHook`，由 `ForwardManager.CloseBySession` 释放该会话的全部转发（此前监听一直残留为 active，直到 `session_delete` 才清理）。
- **MCP `initialize` 的 `serverInfo.version` 跟随构建版本**：此前硬编码 `0.0.4`，与 `termcp --version` 报告的真实版本不一致；现在 `main` 把同一个版本串传给 MCP server。
- **`GET /api/resolve` 对已关闭会话的 shell 定位符返回 409**（文档早已如此描述），会话定位符仍返回 200 + `status=exited` 只读提示；同时移除 `resolveResponse` 中从未赋值的 `archived` 字段。

### 文档

- **README（中英）Docker 示例改为单行并精简整节**：以 `\` 结尾的续行在 bash 可用但在 PowerShell 是语法错误，现在所有 shell 示例（`docker run`/`docker build`/`curl`/`claude mcp add`）均为单行；同时去掉 Dockerfile 注释与说明的重复、`--mcp-manage-ssh-configs` 单独占一条 run 示例、与示例重复的 `export` 代码块，Compose 去掉冗余 `build args`（仓库 Dockerfile 默认走 `proxy.golang.org`）。
- **Dockerfile 去掉 `EXPOSE` 与硬编码 `ENTRYPOINT`**：镜像只提供二进制，监听地址/端口由部署命令传入（示例：`docker run ... ghcr.io/open-mcp-ai/termcp:latest termcp --host 0.0.0.0 --port 18765`）；`release.yml` 与镜像构建补上 `-X main.version` 版本注入，使 `termcp --version` 与 MCP `serverInfo.version` 在发布产物中真的跟随 git tag。
- **清理归档（archive）残留措辞**：README.zh 简介、`docs/mcp-tools.md`、`internal/mcp/docs.go` 的 skills 资源描述、`/skills.md` frontmatter 不再提 “归档/transcripts/screenshots”；注释与错误文本统一为“已关闭（DEAD）会话”。
- **README 工具表去重**：`session_start`/`session_list`/`session_info`/`session_terminate` 不再出现两次，`session_delete` 并入会话容器行（31 个工具一一列出）。
- **README（中英）新增 “AI-native by design / AI Native 设计” 小节**：把“Agent 是常驻用户”的定位落到具体机制上——同层平级入口、token/轮次预算（延迟加载、tail/offset 游标、唤醒信号）、实例自描述（`/api.md`、`/skills.md`、`learn-api`）、密钥留在平台侧、DEAD 可恢复、人工保留中断权。
- **README（中英）标语与简介精简**：去掉“不仅是一个 MCP…更是一个平台”的句式，标语改为“一个 AI Native 的终端平台：跨平台、可视化、人机协作 / An AI-native terminal platform: cross-platform, visual, built for human–agent collaboration”（避免“跨平台…平台”叠字）；简介首段删掉 PTY/标签页/SFTP 与“接入本机/远程”等实现细节和 MCP 角色说明，改为“多主机、多会话同时管理、全程可视化”与“连接配置由平台独立维护，Agent 无需读取凭据即可使用”；结尾改为“跨平台、云原生、纯 Go 无 CGO；单二进制、低开销、可长期驻留”。
- **“为什么选 Termcp”重排为两大板块**：“多会话可视化管理”（功能强大的 Web UI，本地一行命令或云端容器同一套界面）与“AI Native 设计”（无缝人机交互、结对操作），把原“打破边界”（跨轮次驱动 TUI/REPL 的能力）并进 AI Native 开头，删除与简介重复的四入口表格与独立的“可视化管理”小节。

---

## v0.1.12 — 2026-08-17

### Breaking

- **MCP 工具全面改名**：所有工具改为两级 `<group>_<action>` 命名（如 `session_start`、`shell_send_input`），与 resource model 对齐。旧名称无别名。
- **默认数据目录改为 `~/.termcp`**：优先级 `--data-dir` > `$TERMCP_DATA_DIR` > `~/.termcp`。目录权限收紧为 `0700`（SSH 配置属敏感数据）。

### 新功能

- **断开 ≠ 删除，会话保留只读历史**：会话异常退出后保留为只读 DEAD 磁贴（缓冲与输出不丢，重启后仍可恢复查看）；主动关闭的会话归档后不再残留 DEAD 磁贴。仅显式删除才会清理缓冲与消息文件。
- **会话历史工具**：新增 `list_history` / `get_transcript` / `search_messages` / `rename_session` / `update_session_meta` / `purge_session` / `screenshot` 七个 MCP 工具，及配套 `/api/history` REST 端点，支持检索、重命名、清理历史记录与终端截图。
- **Web UI 平铺工作区**：终端窗口可一键平铺为网格布局（iTerm2 风格），支持单窗格最大化、活动窗格高亮、自动/列/行/双列四种排布策略。
- **平铺/浮动单按钮切换**：切换按钮并入标签栏右侧，与 eye（隐藏全部）、排布策略下拉同行；平铺时按钮蓝色高亮提示当前状态。
- **SSH 连接测试按钮**：新建连接前可直接测试连通性。
- **离开页面保护**：关闭页面前提醒，防止误关正在运行的会话；终端新增"回到最新输出"按钮。

### 改进

- **构建**：新增平台感知 Makefile；默认按 release 模式构建（`-s -w -trimpath`）。
- 精简全部 MCP 工具描述与服务端 instructions（这些内容每轮注入模型上下文，显著降低 token 占用），并修正多处描述的歧义表述。

## v0.1.11 — 2026-08-02

### Breaking

- **删除 admin HTTP API**：移除 `--admin-host` / `--admin-port` / `--admin-token` 开关与独立 admin 端口。SSH 配置管理改为 MCP 工具（用 `--mcp-manage-ssh-configs` 启用）。
- **删除 `ssh-config init` / `ssh-config list` CLI 子命令**（从未出现在 `--help`）。SSH 配置改由 Web UI、`list_ssh_configs` 及 `--mcp-manage-ssh-configs` 的 MCP 工具管理。

### 新功能

- **MCP SSH 配置管理**（`--mcp-manage-ssh-configs`，默认关闭）：
  - `create_ssh_config`：结构化参数创建 remote SSH profile（host/user/password/private_key/jump）。
  - `edit_ssh_config`：增量修补已有 profile，省略字段保持原值（含凭据）。
  - `copy_ssh_config`：服务端复制 profile（含凭据），凭据不会经过 AI。
  - `delete_ssh_config`：按名称删除 profile。
  - 凭据写入后不可读取，日志不记录敏感字段。
- **WebUI 面板折叠**：Entries/Sessions 面板可折叠，状态持久化到 localStorage。
- **连接删除确认弹窗**：样式与整体 UI 统一。

### 修复

- 修正用户可见字符串中的配置文件名。

### 文档

- 新增 Docker 部署文档；README 全面重写；新增演示视频。

## v0.1.10 — 2026-07-20

### 修复

- **会话级联清理**：会话退出时级联回收子 shell 与转发资源，并对齐 shell API 行为。

## v0.1.9 — 2026-07-15

### 修复

- **转发生命周期**：端口转发创建时与会话绑定，UI 入口统一封装，不再出现孤儿转发。

## v0.1.8 — 2026-07-14

### 文档

- **Web UI `api.html`**：首页标题栏新增 **API / MCP** 入口；精简为 MCP 可复制配置 + HTTP/WebSocket API 速查表，含 `claude mcp add --transport http`、通用 `mcpServers` JSON 与 URL-only 说明；`/sse` 与 `/stream` 对等说明同步补齐（README / README.zh / `docs/mcp-tools.md` / `docs/api.md` / architecture）。

## v0.1.7 — 2026-07-11

### Breaking（MCP 重设计）

- **Session / Shell 双 ID**：`start_session` 返回互不相同的 `session_id`（连接容器）与 `shell_id`（终端通道）。首个 shell 不再与 session 共用 id。
- **I/O 只认 `shell_id`**：`send_input` / `press_key` / `read_output` / `resize_pty` / `register_reader` / `unregister_reader` / `close_shell` 参数改为 `shell_id`。连接级操作（forwards、files、terminate、delete、start_subshell）仍用 `session_id`。
- **删除工具**：`send_and_read`、`background_send`、`delete_session`（硬切换，无别名）。
- **删除参数**：`send_input.press_enter`。执行命令改为 `send_input` + `press_key(key="enter")`。
- **新增 `press_key`**：命名按键白名单（enter/tab/esc/方向键/backspace/delete/home/end/ctrl+c|d|z|l|u|w），可选 `repeat`。
- **转发 OpenSSH 命名**：`local_forward` = ssh `-L`；新增 `remote_forward` = ssh `-R`；删除 `forward_port`。旧版 `local_forward` 曾错误实现为 `-R`，现已纠正。
- **`start_subshell` / `list_subshells`**：参数 `parent_session_id` → `session_id`；返回字段对齐 `shell_id` / `shells`。
- **去掉恒空 `initial_output`**；`read_output` 默认 `timeout` 从 5 改为 3。
- **生命周期叙事**：MCP 只保留 `terminate_session`；`force=true` 立即强杀，HTTP `DELETE /api/sessions/{id}` 仍保留。

### 修复

- **`read_output.max_lines` 丢数据**：行数限制改为 buffer 层按换行截断游标；未返回行保留且 `has_more=true`（原先先消费再字符串截断）。
- **internal `close_shell` 误拆会话**：子 shell 主动关闭设 `deliberateClose`，退出 watcher 不再把 intentional channel close 当 SSH 断连。

### 文档

- 重写 MCP `instructions`、`docs/mcp-tools.md`、CLAUDE multi-session 规则；对齐 resource-model。

## v0.1.6 — 2026-07-11

### 新功能

- **SSH profile 改名**：配置文件同步迁移，引用不断链。
- **internal profile 虚拟化**：内置 loopback 连接不再落盘，且禁止编辑/删除。
- **`--no-internal` 开关**：禁用内建 loopback SSH profile。

## v0.1.5 — 2026-07-10

### 修复

- 密码框眼睛按钮纵向居中；密码/私钥字段自动 trim，防止首尾空格干扰认证。

## v0.1.4 — 2026-07-10

### 修复

- **`go install` 后 Web UI 资源缺失**：`vendor` 目录重命名为 `static`，避免 Go module zip 默认排除 `vendor` 路径导致 xterm.js 等静态资源丢失。

## v0.1.3 — 2026-07-09

### 修复

- 新建连接弹窗中 Key Passphrase 字段随 Auth 方式切换显隐，仅在 Private Key 时显示。

## v0.1.2 — 2026-07-03

### 新功能

- **SFTP 文件工具套件**：新增 10 个文件管理 MCP 工具（读写、列目录、删除、重命名、建目录等），统一走 SSH 路径。

### 改进

- **REST API 统一**：清理冗余端点，统一资源式设计。
- **转发/子 shell 变更实时推送**：创建与删除后通过 WebSocket 自动刷新 UI，无需手动刷新。
- Shell 平等化与会话层重构（sync.Map、SSH 断线检测）。

### 修复

- **exited session panic**：`SendTerminalBytes` 增加 nil 检查，会话退出后写入不再崩溃。

## v0.1.1 — 2026-07-01

### 新功能

- **SOCKS5 代理与跳板链**：SSH 配置支持 SOCKS5 代理与 ProxyJump 多级跳板。

### 修复

- **Enter 按目标系统发送**：换行符按目标 shell 所属平台（Windows CRLF / Unix LF）而非 Termcp 本机 OS 决定，修复从 Windows 管理 Unix 主机时的输入异常。

## v0.1.0 — 2026-06-30

### 新功能

- **单会话多 shell 通道**：SSH shell 通道多路复用（统一 ChildShell 模型），一个会话内可开多个终端通道。
- **`close_shell` / `list_subshells` MCP 工具**：父子 shell 生命周期拆分，支持显式关闭单个 shell 与列出全部子 shell。
- **MCP 文件工具回归**：重新引入 SFTP 文件读写工具。

### 改进

- internal SSH 支持 SFTP subsystem；SSH 配置迁移至 TOML；包结构与命名重构、清理死代码。

### 修复

- **root shell 自然退出不再带垮 internal session**：主 shell 退出时正确区分连接关闭与整体断开。
- WebUI 连接加载指示器纵向居中，清理启动日志。

### 文档

- 文档与代码同步（31 个 MCP 工具清单、内存 SSH 架构说明）。

## v0.0.4 — 2026-05-23

### 新功能

- **Web UI**：浏览器端终端（xterm.js + WebSocket），支持实时会话列表（SSE）、全量输出回放、连接模板一键启动。单端口 18765 同时服务 MCP 和 Web UI。

- **服务端 SSH 配置**：SSH 连接信息存储在 `data/ssh_configs/<name>/config.json`，MCP 工具只需传配置名。支持 `internal`（loopback）和 `remote`（远端 SSH）两种类型，可选 `default_shell` 和 `default_mode`。新增 `ssh-config init` / `ssh-config list` CLI 子命令和可选 Admin HTTP API。

- **Streamable HTTP 传输**：新增 `/stream` 端点（MCP Streamable HTTP 规范），兼容 Open WebUI 等客户端。

- **read_output / send_and_read 增强**：返回 `session_status`、`session_uptime_seconds`；新增 `max_bytes` 参数（默认 8KB）配合 `has_more` 实现大输出分页。

- **MCP Agent 规则注入**：`initialize` 返回 `instructions`，引导 Agent 正确使用工具链（多步执行、crash-loop 检测、密码安全等）。

### 改进

- **PTY 标准终端模式**：SSH 客户端请求 PTY 时设置完整 termios（`ICANON`/`ICRNL`/`ONLCR`/`OPOST`/`ISIG`），修复 Python 3.13 pyrepl 崩溃等交互式程序异常。

- **输出缓冲区重构**：从 ring buffer 改为 append-only 多读者缓冲。每个 reader 独立游标，已消费前缀自动压缩，无固定容量覆盖。

- **TERM 环境变量传播**：PTY 请求的 `TERM` 值（如 `xterm-256color`）正确传递给子进程环境，修复 CI 中 `TERM=dumb` 导致的测试失败。

### 修复

- **Shell 历史展开干扰**：交互式 shell 启动时自动禁用 `!` 历史展开（zsh: `-o NO_BANG_HIST`，bash/sh: `+o histexpand`），防止含 `!` 的密码、URL 执行失败。

- **移除 SFTP 工具**：删除 `upload_file`/`download_file`/`list_files`，清理 sftp subsystem 残留，`trust_unknown_host` 默认 `false`。

- **Windows 跳过 TERM 测试**：`TestServer_PtyEnviron` 在 Windows 跳过，TERM 是 Unix 概念。

- **优化 .gitignore**：防范二进制文件误提交。

## v0.0.3 — 2026-05-12

### 新功能

- **detect_shell MCP 工具**：探测 Termcp 主机上的可用交互 shell（bash/zsh/fish/pwsh/cmd），返回路径、family 和提示。跨平台混合环境中 Agent 可据此选择正确的命令语法。

### 改进

- **Shell 检测重构**：提取为可注入的 `Detector` 结构体，测试时可替换为固定实现，消除环境依赖。

- **Debug 日志增强**：MCP 工具调用时记录请求参数和输出预览，便于排查 Agent 行为。

### 修复

- **Kali sudo 密码提示泄漏**：修复 sudo 密码提示输出到父进程 TTY 的问题。

## v0.0.2 — 2026-05-07

### 新功能

- **Windows 平台支持**：全平台 Windows 支持，通过 ConPTY 运行 PowerShell PTY 会话。charmbracelet/ssh 提供原生伪终端分配，输入编码（CRLF）和输出规范化自动处理。

- **跨平台 CI**：测试 workflow 覆盖三平台 — `ubuntu-latest`、`macos-latest`、`windows-latest`，PR 和 push 到 `main`/`dev` 均触发。

- **多平台构建发布**：Release workflow 构建 6 个目标 — `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64`。

### 改进

- **SSH 库替换**：`gliderlabs/ssh` → `charmbracelet/ssh`。charmbracelet/ssh 内置 `AllocatePty()` 自动管理 PTY 生命周期，移除手工 `pty.StartWithSize`/`io.Copy`/`pty.Setsize` 代码。公共 API（`New`/`Start`/`Stop`/`Addr` 等）签名不变。

- **跨平台输入处理**：`SendInput` 在 `press_enter=true` 时自动选择平台换行符 — Windows 用 CRLF（`\r\n`），Unix 用 LF（`\n`）。

- **SFTP 路径规范化**：远程路径自动将反斜杠转为正斜杠，避免 Windows 路径分隔符导致 SSH 文件操作失败。

- **信号转发验证**：新增 `TestServer_SignalTerm` 和 `TestServer_SignalInterrupt` 验证 SIGTERM/SIGINT 通过 SSH 通道正确转发至目标进程。

### 修复

- **Windows PTY 交互输出为空**：ARM64 上 PowerShell/ConPTY 输出时序问题导致首次读取为空。改为 `Write-Output` 原生命令配合 marker 轮询读取（`testReadOutputUntil`）解决。

- **Windows 进程退出行为不确定**：PowerShell `-Command` 在 ConPTY 下完成命令后保持交互态，自然退出测试在 Windows 跳过。

- **Windows 跳过 POSIX signal 测试**：`SIGTERM`/`SIGINT` 测试在 Windows 跳过，Windows 不支持 POSIX 信号。

## v0.0.1 — 2026-05-05

### 初始发布

- Termcp 首个版本：把交互式程序作为持久 SSH 会话暴露给 AI Agent 的 MCP server。
