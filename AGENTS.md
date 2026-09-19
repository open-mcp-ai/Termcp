## Security & Privacy

禁止在项目中出现以下敏感信息和敏感文件：

- 本机 hostname、本地用户名称
- 真实密钥、API keys、tokens、passwords
- 邮箱地址、手机号等个人信息
- .env 文件、私钥文件（PEM、SSH key 等）
- 禁止出现 `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>` 相关字样
- 只有在人工同意后才能push和commit。在多次commit后，人工要求push再进行push，绝对不要主动push。

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues via `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Project docs live under `docs/` (`docs/mcp-tools.md`, `docs/api.md`, `docs/architecture.md`, `docs/design/`).

## Driving termcp's MCP tools

Working in this repo usually means driving termcp itself. The full tool
reference is `docs/mcp-tools.md`; every running instance also serves its own
docs (`resources/list` returns real `http://<origin>/api.md` and
`http://<origin>/skills.md` URLs). Three invariants are cheap to keep in mind
without being told:

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