## Where to look first

`AGENTS.md`（本文）只写**必须一直生效的契约**：隐私边界、并发准则、发布与测试流程。可执行的流程细节与专题规范放在 `docs/agents/`，实现参考与设计放在 `docs/`。动手前先读与本次改动相关的那一份，而不是从本文反推全部细节——本文刻意不复制它们，复制会产生第二份会漂移的真相。

| 要做的事 | 先读 |
|---|---|
| 写或改 hint、help、错误信息、MCP instructions | `docs/agents/hints.md` |
| 写或改测试 | `docs/agents/testing.md` |
| 建 issue、打 triage 标签 | `docs/agents/issue-tracker.md`、`docs/agents/triage-labels.md` |
| 发版、打 tag | `docs/releasing.md` |
| 查传输层语义与指令细节 | 运行实例的 `help <directive>`、`/api.md` |
| 查设计取舍 | `docs/architecture.md`、`docs/design/` |

如果一件事在本文与专题文档里说法不一致，以专题文档为准，并修正本文——不一致本身就是缺陷。

## Privacy and repository boundaries

项目仓库是可共享、可审查的交付物；本机身份和访问凭据属于运行环境，不属于产品实现或测试数据。代码、测试、文档与提交说明都应使用虚构示例或测试生成值；凭据通过运行时配置传递，个人或机器信息不写入仓库。提交与推送会改变共享历史，必须先取得人工明确同意。

因此：
- 不把 hostname、本地用户名、真实密钥/API key/token/password、邮箱、手机号写入项目文件；不添加 `.env` 或私钥文件（PEM、SSH key 等）。
- 不添加 `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>`。
- 未经人工明确授权，不 commit、不 push；授权了其中一项，不代表授权另一项。

## Writing hints, help, and error messages

产品内的 hint、help 文本、错误信息、MCP instructions 与本仓库的 agent 指导，统一遵循 **[`docs/agents/hints.md`](docs/agents/hints.md)**。那份文档是权威基准，改动 hint 前先读它；此处只保留一句理由，不重复它的条文。

理由：文字是操作界面的一部分。读者需要先知道**这是什么、信息如何流动**，才有可能选对动作；跳过原理直接给规则，读者不知道该做什么，只会换一种方式犯错。

## Concurrency and deadlock prevention

协调手段的目的是保护状态转换。互斥锁建立的契约是「持锁者很快会结束」；网络 I/O、SSH 握手、等待对端响应都不在这个契约内——对端可能在内核仍应答连接的情况下卡在用户态，使一次 `Write` 永不返回。于是问题不是「锁不安全」，而是**用错了协调原语**：把「保证一个字段/一组字段的瞬时一致」的工具，用来覆盖「等待一个不可控外部事件」。

本项目曾在互斥锁内部包裹跨网络等待，造成 10+ 次死锁与雪崩（#189、#206、#207、#212、#218–#222）。这类故障的传播方式是：一个卡住的持锁者不影响自己，而是把后续所有请求（包括跨会话的关闭请求）排成一队，最终整个实例挂起。

Go 标准库把并发能力做成了可直接使用的积木：`sync/atomic` 提供自包含的原子读改写；`sync.Map` 把并发 map 的加锁细节收进一次调用；channel 把同步表达为传递而不是共享字段。它们和 `sync.Mutex` 一样是原生原语，区别不在“更原子”，而在**正确性能否就地检查**：一次 `atomic.Add`、一次 `LoadOrStore` 的对错在那一行就能看出来；而 `Lock`/`Unlock` 的正确性分散在每条 return、每条 panic 路径、以及加锁顺序是否在别的文件里被改变。

记账最容易出错的地方，正是没有持久上下文时最容易遗漏的地方：错误路径忘了解锁、锁顺序在另一处被改写、把网络等待包进临界区、在锁内调用户回调、通知在有锁时发出。选择自包含的原语，就是把这些义务从“要记住的约定”变成“写在眼前的代码”。

这条推理有边界，不能当成万能替换：原语把义务换了个形式，并没有取消它。一槽 channel 能表达令牌，但它不记录持有者——令牌遗失或未被归还时没有任何机制会告诉你；`sync.Map` 是并发 map，不是所有权协议，`Load` 再 `Store` 不是一个事务；多次 atomic 操作也不构成事务。所以选原语的同一条作业里，仍然要回答两个问题：不变量是什么？哪一步可能无限阻塞？

先问两个问题：要保护的不变量是什么？哪一步可能无限阻塞？

**做法（按此顺序选择）：**

1. **临界区只放内存操作**：快速快照 / 复制字段 / 改状态标记，然后立刻释放，再去做网络 I/O、第三方调用、用户回调、排序与等待。
2. **独立的纯计数器与状态标记用 `sync/atomic`**（`atomic.Int64` / `atomic.Uint64` / `atomic.Bool`）。一个数字的累加不需要自建锁协议；多个字段若构成一个复合不变量，不要误把几次 atomic 操作当成事务，选择能清楚保护该不变量的 channel 或短锁。
3. **无跨 key 不变量的 map 查增删用 `sync.Map`**：`Load` / `Store` / `LoadOrStore` / `LoadAndDelete`，遍历快照用 `Range`。只有当不变量本身跨越多个 key、需要事务性时，才用受保护的普通 map。
4. **需要可取消的串行化用「一槽 channel 令牌」**：`tok := make(chan struct{}, 1)`，获取时 `select` 同时监听令牌、计时器与 `ctx.Done()`，超时明确报错或放弃。这样调用者随时能走，而不是无限期停在 `Lock()` 上。
5. **并发单飞用 channel 广播**：第一个请求负责拨号/握手，完成后 `close(done)`；并发请求 `select { case <-done: ... case <-ctx.Done(): ... }`，各自按自己的窗口退出。
6. **生命周期终止必须有界**：优雅关闭放到独立 goroutine，调用方等 `context.WithTimeout`；超时后调用底层 `net.Conn.Close()`，让内核中所有阻塞读写立即报错解开。

**边界（保留锁的两种情形）：**

- **多个字段必须一起改变、且没有更好的原子表达时，短锁是正确的工具**。写清它保护哪个不变量，并让临界区只含内存操作。`storage.Store`、`extension.Store` 的跨文件事务属于此类。
- **第三方库与用户回调不在你的控制下**：`mcp-go`、`charmbracelet/ssh` 内部如何加锁、是否阻塞、是否反向回调，你无法保证，也无法保证它们不会与你的锁形成 AB-BA。所以锁内只做本次快照，出锁后再调外部接口（参见 `connRegistry.list()` 与 `approval.Queue.notify()` 的做法）。

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues via `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Project docs live under `docs/`. `AGENTS.md` carries the always-on contracts; `docs/agents/` carries the per-task procedures (`hints.md` for hint/help/instruction writing, `testing.md`, `issue-tracker.md`, `triage-labels.md`), and `docs/` itself carries reference and design (`api.md`, `architecture.md`, `design/`, `releasing.md`). Read the relevant one before starting that kind of work.

### Releases

`dev` is the rebased development line; `main` only receives release PRs. Use
the process in `docs/releasing.md` before tagging. Write the `## Unreleased`
notes **before** cutting the release — one short paragraph per user-visible
change since the last release, not a design note; `make prepare-release` only
moves them. Give the PR a `release/vX.Y.Z` head branch or `release: vX.Y.Z`
title, run `make prepare-release RELEASE_VERSION=vX.Y.Z`, and review the
generated changelog and `server.json` changes. CI checks both files before
merge.

A `vX.Y.Z-rc.N` tag is an internal test build: it adds a dated changelog
section, **leaves `server.json` alone** (no registry entry, no `:latest` move)
and is marked as a GitHub pre-release. Its branch and PR name the rc itself
(`release/v0.2.8-rc.1`), because CI requires the release signal to match the
files exactly — the final release is its own PR (`release/v0.2.8`).
`docs/releasing.md` has the details.

### Tests run in CI on three OSes

Whenever you write or modify a test, remember it must pass in
`.github/workflows/test.yml` — ubuntu, macos and windows, via
`go test ./... -count=1 -shuffle=on -race` (`make test` locally, with a tighter
`-timeout`: CI widens it because a shared runner is several times slower, and a
run killed by `-timeout` names no failing test). For races the detector cannot
see — filesystem and lifecycle
races — run `make test-stress` locally before pushing. Assume concurrency, a
fresh runner, and dirty state from other tests or processes. Never hardcode a
shared absolute path; `t.TempDir()` is the default. See
`docs/agents/testing.md`.

## Driving Termcp's MCP tools

Working in this repo usually means driving Termcp itself. The reference is the
running instance, not a file: `help` lists every directive, `help <directive>`
teaches one, and an empty `term()` call returns the guide. The REST entrance
`POST /api/term` speaks the same command language, and `GET /api.md` publishes
the transport-level reference. Three invariants are cheap to keep in mind without being told:

- **Turns are the scarce resource.** An MCP call costs 2–10 ms; an assistant
  turn costs 6–9 s — ~1000×. Minimise *turns*, not calls: whatever one turn can
  hold, keep it in one turn.
- **One command = one batch.** `shell_input` (types) → `shell_key(enter)`
  (executes) → `shell_output` (reads) is *tool semantics, not turn boundaries* —
  run all three inside a single `mcpScript` turn, and let
  `shell_output(timeout≤3)` do the only waiting there is. The loop is
  input → execute → read → next input, with nothing in between.
- **`shell_id` is the I/O target, `session_id` is the connection.** Every I/O
  tool takes `shell_id`. One task gets one session (give it a `name`).

Split across turns only when a human must act first (credential prompt → read
it, then `notify_user(level="warn", duration_seconds=0, session_id=...)`), a
destructive action needs confirmation, or you must inspect intermediate state
to decide the next step.
