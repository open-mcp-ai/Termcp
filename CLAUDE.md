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

## Multi-session parallel work

Agents using interactive-process MCP tools must follow these rules for non-blocking, multi-session operation.

### Rules

1. **One task = one session.** Start a new session per independent task via `session_start(ssh_config=...)`. Keep both `session_id` (connection) and `shell_id` (terminal I/O). Use the `name` param for tracking.
2. **Never block on reads.** Always use `shell_output` with `timeout` ≤ 3. A long timeout blocks the entire agent — other shells go unserviced.
3. **Run a command as three calls.** `shell_input(shell_id, text)` types only; `shell_key(shell_id, key="enter")` executes; then `shell_output(shell_id, timeout≤3)`. Do not put newlines in text.
4. **Poll in rotation or use event notifications.** When managing N shells, poll `shell_output(timeout=1)` each, or register event-driven wake-ups via `shell_notify(shell_id=..., channel="resource"|"sampling", event="output"|"exit"|"silence")`.
5. **Clean up.** `session_terminate(session_id)` closes the connection (cascades shells + forwards) and removes the session. Use `force=true` for immediate kill. `shell_close(shell_id)` only closes one channel.

### Multi-agent shared shell

When multiple agents need to observe the same process:

1. Agent A: `session_start(...)` → session_id + shell_id, default reader_id=0
2. Agent B: `shell_reader_register(shell_id=...)` → gets its own reader_id
3. Each agent calls `shell_output(shell_id=..., reader_id=<theirs>)` — independent cursors, no output stealing
4. Agent B leaves: `shell_reader_unregister(shell_id=..., reader_id=...)`

### Anti-patterns

- ❌ `shell_output(timeout=30)` — blocks 30s, other shells starve
- ❌ Waiting for session A to finish before starting session B — start both, poll both
- ❌ Multiple agents using the same reader_id — output gets consumed, others miss it
- ❌ Using `session_id` for I/O tools — I/O is always `shell_id`
- ❌ Relying on removed tools: `start_session`, `read_output`, `send_input`, `press_key`, `terminate_session`, `send_and_read`, `background_send`, `press_enter`, `forward_port`
