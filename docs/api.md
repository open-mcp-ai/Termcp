# Termcp HTTP API

Base URL: `http://localhost:18765`

Termcp is a terminal-session platform. One port serves the same sessions to four
kinds of callers:

| Entrance | Path | Caller |
|----------|------|--------|
| Web UI | `/` + `/api.html` | Humans (browser) |
| MCP | `/sse` + `/message` or `/stream` | AI agents with an MCP client |
| Agent Skill + REST | `/skills.md` (install once) + `/api/*` | AI agents driving Termcp with `curl` |
| REST + WebSocket | `/api/*` + `/api/ui/ws` | Scripts / programs |

This document covers the REST surface (scripting). Real-time terminal I/O goes over
the WebSocket (`/api/ui/ws`); everything else is REST.

## 1. MCP transports (AI integration)

MCP shares the same port and the same session core as REST; it is the platform's
integration surface for AI agents. Pick one transport depending on client support:

| Transport | Path | Notes |
|-----------|------|-------|
| SSE | `GET /sse` + `POST /message` | Configure only `/sse`; JSON-RPC goes over `/message` |
| Streamable HTTP | `/stream` | Single endpoint; used by Open WebUI and similar |

MCP tool arguments, results, and error codes are described by the tool schemas from
`tools/list` (and summarized in the repository's `docs/mcp-tools.md`). Browser users
can open `/api.html` (Web UI **API / MCP / SKILLS**) for copy-ready MCP config, the
HTTP API cheat sheet, and the agent-skill download.

## 2. Agent-facing documents (HTTP + MCP resources)

The instance serves its own documentation on the same origin, so agents and scripts
can learn the API without MCP and without version drift:

| Document | Path | When to use it |
|----------|------|----------------|
| HTTP API reference (this file) | `/api.md` | You drive Termcp over HTTP yourself (REST/WebSocket): endpoints, bodies, output-cursor semantics |
| HTTP skill (curl recipes) | `/skills.md` | Install once into a skills directory so the curl workflow is available on demand later |

`/skills.md` is served flat (as guessable as `/api.md`) but is a real Agent Skill:
save it as `termcp/SKILL.md` in your agent's skills directory — the directory name
is what a skill loader discovers. Claude Code reads `~/.claude/skills/termcp/SKILL.md`;
agents following the shared convention read `~/.agents/skills/termcp/SKILL.md`
(project-scoped: `<project>/.claude/skills/termcp/SKILL.md`). Restart the agent
session afterwards; adding/removing a skill is just writing/deleting that folder.

MCP clients get the same bytes as resources: `resources/list` exposes these two files
and the `resources/read` URIs are exactly the `<origin>/…` HTTP addresses above (one
URI, two access paths). The `learn-api` prompt primes an agent with these documents
before it acts.

Authentication does not apply to these two read-only documents: they carry no data
and no secrets, and a fresh client (an agent before MCP setup, a script) has to be
able to fetch them before it can use the API, so `/api.md` and `/skills.md` stay
public even when a token is configured. Everything else — Web UI, REST, MCP, WebSocket
— still requires credentials.

```bash
curl -fsS "$BASE/api.md"                       # authoritative reference
DIR=~/.claude/skills                           # Claude Code
# DIR=~/.agents/skills                         # other agents
mkdir -p "$DIR/termcp" && curl -fsS "$BASE/skills.md" -o "$DIR/termcp/SKILL.md"
```

## 3. Authentication (optional, off by default)

With `--auth-token` / `--auth-hash` (or `TERMCP_AUTH_TOKEN` / `TERMCP_AUTH_HASH`),
**every** HTTP surface requires credentials: Web UI, REST, MCP SSE, `/stream`, and
the WebSocket. One shared `internal/auth` middleware guards the whole mux. Missing or
wrong credentials return `401 Unauthorized` with `WWW-Authenticate: Basic` (browsers
show their native login prompt).

| Caller | Credentials |
|--------|-------------|
| REST / MCP / curl | `Authorization: Bearer <token>` |
| Browser (Web UI) | Native Basic prompt — username is ignored, **password = token** |
| Browser WebSocket (`/api/ui/ws`) | The `termcp_token` cookie set after a successful Basic prompt is sent automatically on same-origin handshakes |

```bash
# Generate a salted SHA-256 hash (no echo in the terminal, no shell history)
termcp --gen-auth-hash

# Two equivalent ways to call REST
curl -u :<token> http://127.0.0.1:18765/api/sessions
curl -H "Authorization: Bearer <token>" http://127.0.0.1:18765/api/sessions
```

Constraints:

- `--auth-token` and `--auth-hash` are mutually exclusive; the flag wins over the
  environment variable for the same setting.
- Binding a non-loopback address (`0.0.0.0`, a LAN IP, …) without auth **refuses to
  start**.
- `--disable-auth` (or `TERMCP_DISABLE_AUTH_TOKEN=1`) lifts that requirement on
  purpose, for loopback-only setups where the token protects nothing (demo
  recordings, single-user workstations). Combining it with a token or hash is a
  startup error, and the env var only accepts `1`/`true`/`yes`/`on` — `=0` means
  "not set" rather than "open the server".
- Tokens are never logged and must not go into URLs (query strings) — use headers.
- Public exception: the two documentation endpoints `/api.md` and `/skills.md`
  (GET/HEAD only, no data inside) are served **without** credentials, so agents
  and scripts can fetch the docs before they have a token. All other surfaces
  require the token.

---

## 4. Connection profiles

### `GET /api/connections`

Lists summaries of all connection profiles.

```
Response 200:
{
  "connections": [
    { "name": "pi", "kind": "remote", "host": "192.168.1.100", "user": "pi", "port": 22, "default_approval": false }
  ]
}
```

`default_approval` is the profile's **review default**: with it on, every
session created from this profile starts with review mode enabled, so each AI
write waits for a human decision before any byte reaches the shell. It is a
property of the connection rather than of one session because the decision is
about the host — a production box is the reason to want every write reviewed,
and remembering to flip the switch after each launch is the step that gets
forgotten. Set it in the connection editor's checkbox or as
`default_approval = true` in the profile's TOML.

### `GET /api/connections/{name}`

Returns the raw TOML of one connection profile.

### `PUT /api/connections/{name}`

Creates or updates a connection profile. Body is TOML.

```
Response: 204 No Content
```

### `DELETE /api/connections/{name}`

Deletes a connection profile.

```
Response: 204 No Content
```

### `POST /api/connections/test`

Backend of the Web UI's "test connection" button: validates connectivity over the
full dial path (SOCKS5 proxy → bastion jump → target host) including opening an exec
channel. **Nothing is kept after a successful test.** Body is TOML (same as `PUT`).
`internal`-kind profiles skip dialing and return `{ "ok": true }` directly.

```
Request: TOML body

Response 200:
{ "ok": true, "duration_ms": 123 }
{ "ok": false, "duration_ms": 123, "error": "<failure reason + diagnostic hint>" }
```

---

## 5. Resource locators (termcp://)

A locator names a Termcp object in one string. The Web UI's copy buttons emit
them (entry cards, session cards, shell tabs) so a user can paste "open this"
into a chat, an issue, or a script. MCP tools accept locators anywhere an id or
profile name is expected; over plain HTTP, resolve them first with
`GET /api/resolve`.

| Locator | Names | Resolves to |
|---------|-------|-------------|
| `termcp://<entry>` | a connection profile (ssh_config), e.g. `termcp://rock64` | `ssh_config` name |
| `termcp://#<session>` | a session | `session_id` |
| `termcp://#<session>:<N>` | shell channel N of that session | `session_id` + `shell_id` |

The shell index is 1-based creation order, matching the `shell-1`/`shell-2` tabs;
without `:N` the primary (first) shell is meant. The long form
`termcp://<entry>#<session>` is accepted for back-compat, but the entry prefix is
ignored — session ids are unique, profile names are not. `termcp://shells/<id>`
is a notification broadcast URI, not a locator.

### `GET /api/resolve?url=<locator>`

Turns a locator into concrete ids. The response is one object whose `kind` tells
you what you got; only the relevant fields are set.

```
Request:
  GET /api/resolve?url=termcp://rock64
  GET /api/resolve?url=termcp://%23abc123
  GET /api/resolve?url=termcp://%23abc123:2

Response 200 (entry — "open termcp://rock64" means connect to that profile):
{ "kind": "entry", "entry": "rock64", "ssh_config": "rock64" }

Response 200 (session):
{ "kind": "session", "session_id": "abc123", "name": "rock64", "status": "running" }

Response 200 (closed / DEAD session — read-only):
{ "kind": "session", "session_id": "abc123", "name": "old-box", "status": "exited" }

Response 200 (shell channel):
{ "kind": "shell", "session_id": "abc123", "shell_id": "def456", "index": 2, "name": "shell-2", "status": "running" }
```

Then use the ids with the ordinary endpoints: an `entry` becomes
`POST /api/sessions` with that `ssh_config`; a `session_id` drives output,
files and forwards; a `shell_id` drives input/key/resize/output-range.

Errors: `400` malformed locator, `404` unknown profile / session / shell index
out of range, `409` shell locator on a closed session (read it with the
session-level `output-range` instead).

## 6. Session

**Timestamps.** Every timestamp in every response (and on disk) is **Unix
milliseconds as a number** — `created_at`, `updated_at`, `mod_time`, and the
`time` field of a transcript span. There are no locale or RFC3339 strings on the
wire: the unit is always ms, and formatting for display is the client's job.

**Concept:** a Session is an SSH connection container holding 0..N shells and 0..N
forwards. Shell IDs are separate from the session ID; the first shell has its own ID.

### `GET /api/sessions`

Lists every session in the registry: running ones plus closed (DEAD) read-only tiles
(the tile keeps its retained output readable).

```
Response 200:
{
  "sessions": [
    { "id": "abc123", "name": "pi", "mode": "pty", "status": "running",
      "pid": 12345, "rows": 24, "cols": 80, "ssh_endpoint": "remote", "created_at": 1758499200123 }
  ]
}
```

### `POST /api/sessions`

Creates a new SSH connection and session (including its first shell).

```
Request:
{
  "ssh_config": "pi",    // profile name, defaults to "internal" (host loopback);
                         // MCP's session_start requires it explicitly
  "command": "",         // command; empty = login shell
  "args": [],
  "mode": "pty",         // "pty" | "pipe"
  "name": "my-session",  // display name, defaults to ssh_config
  "rows": 24,
  "cols": 80
}

Response 200:
{ "session_id": "abc123", "shell_id": "def456", "pid": 12345, "ssh_config": "pi" }
```

### `GET /api/sessions/{id}`

Returns one session.

```
Response 200: Session object (same shape as a list element)
```

### `DELETE /api/sessions/{id}`

**Permanent deletion**: disconnects the session (shells → forwards → SSH connection)
and removes its on-disk session directory (`manifest.json` + `log.bin` + `log.jsonl`).
Irreversible.

```
Response: 204 No Content
```

> Note: this differs from `POST /api/sessions/{id}/terminate` (close only — the
> session stays in the registry as a read-only DEAD tile and its output remains
> readable).

### `PATCH /api/sessions/{id}`

Renames a session.

```
Request:
{ "name": "my-ctf-box" }

Response 200: Session object
```

---

## 7. Shell

A shell is a sub-resource of a session; shell IDs are globally unique.

### `GET /api/sessions/{id}/shells`

Lists a session's shells.

- A `running` session lists only **existing** channels: live shells plus shells that
  exited naturally but were retained (status `exited`, so their tail output stays
  readable). **Manually closed shells are removed** — they do not appear here and do
  not come back as dead tabs.
- An `exited` (DEAD) session returns the retained shell snapshot (only shells that
  exited naturally or lost the connection).

```
Response 200:
{
  "shells": [
    { "id": "abc123", "name": "pi", "status": "running", ... },
    { "id": "def456", "name": "shell-2", "status": "running", ... }
  ]
}
```

### `POST /api/sessions/{id}/shells`

Opens another shell channel on an existing session (reusing the SSH connection).

```
Request:
{ "command": "", "name": "shell-2", "mode": "pty", "rows": 24, "cols": 80 }

Response 200:
{ "shell_id": "def456", "session_id": "abc123", "name": "shell-2" }
```

### `DELETE /api/shells/{id}`

**Deletes** one shell channel (path id is a **shell_id**). Manual close is a delete,
not DEAD: the shell disappears from the live list, the retained snapshot, and
`sessions/<session_id>/`, leaving no `end`/dead tab. The SSH connection and the session's other
shells are untouched. Closing the internal primary shell is a no-op (the process can
outlive the tab).

Closing the last shell of a pipe session turns the container `exited` (DEAD, read-only);
a PTY container stays `running` and can open new shells.

Closing a shell that no longer exists also returns 204 (idempotent).

```
Response: 204 No Content
```

---

## 8. Terminal I/O

### WebSocket `GET /api/ui/ws`

Bidirectional real-time channel.

**Client → Server:**

| type | Fields | Meaning |
|------|--------|---------|
| `watch_add` | `id` | Subscribe to terminal output |
| `watch_remove` | `id` | Unsubscribe |
| `input` | `id`, `d`(string), `nl` | Send keystrokes (dropped for a session in review mode — see §12) |
| `resize` | `id`, `rows`, `cols` | Change PTY size |

**Server → Client:**

| type | Fields | Meaning |
|------|--------|---------|
| `sessions` | `sessions` | Session list (on connect and on change) |
| `terminal` | `id`, `d`(string) | Terminal output chunk |
| `terminal_done` | `id` | Shell exited |

> Terminal I/O `id` values are shell IDs; session IDs are only for connection-level
> REST resources.

### `GET /api/shells/{id}/output-range`

Reads a slice of a shell's retained output (**path id is a shell_id**).

Compat: `GET /api/sessions/{id}/output-range` still works — a shell_id hits directly,
a session_id falls back to that session's primary shell.

```
Query:
  start=0          first byte (default 0; mutually exclusive with tail=1)
  max=262144       max bytes returned (hard cap 512KiB)
  tail=1           take max bytes from the end (ignores start)

Response 200:
{
  "start": 0,
  "end": 1024,
  "total": 4096,
  "d": "<bytes as a JSON string>"
}
```

> `d` is the raw byte window placed in a standard JSON string (not base64). A
> byte sequence that is not valid UTF-8 cannot survive a JSON string; on Windows
> ConPTY already replaces such sequences before Termcp sees them, and on Linux
> `cat` of binary data may appear as U+FFFD. The bytes in `log.bin` are never
> altered — only this transport representation is lossy.

### `POST /api/shells/{id}/input`

Write text bytes to a running shell (path id is **shell_id**). Script-friendly
counterpart of the WebSocket `input` message and the MCP `shell_input` tool.

```
Request:
{
  "text": "ls -la",
  "press_enter": false   // true appends the shell's line ending
}

Response 200: { "ok": true }
```

**In review mode** the text is *held*, not written and not yet queued, and the
response is `202 Accepted`:

```
Response 202:
{
  "ok": true,
  "approved": false,
  "review_pending": true
}
```

A command line is text plus the enter that ends it. `press_enter: true` ends it
here, so the held text becomes one reviewable request immediately; with
`press_enter: false` the text waits for a `/key` call. Nothing reaches the shell
until a human accepts it (see §12).

Errors: `404` unknown shell, `409` shell is not running, `400` malformed body.

### `POST /api/shells/{id}/key`

Send a named key sequence (path id is **shell_id**).

```
Request:
{
  "key": "enter",   // enter, tab, esc, up/down/left/right, backspace, delete,
                    // home, end, ctrl+c/d/z/l/u/w
  "repeat": 1       // 1..20, default 1
}

Response 200: { "ok": true }
```

**In review mode** the key commits whatever text was held for this shell, as one
request (`202 Accepted`, same shape as `/input`). With nothing held, the key is
the request on its own: `ctrl+c` interrupts a running process, so it is reviewed
rather than sent. The request records the key *by name* (`"keys": ["ctrl+c"]`),
never as a raw byte, so a reviewer reads `Ctrl+C` instead of an invisible
`0x03`.

### `POST /api/shells/{id}/resize`

Resize a shell's PTY window (path id is **shell_id**). TUI programs re-layout on the
next redraw.

```
Request:  { "rows": 40, "cols": 120 }
Response 200: { "rows": 40, "cols": 120 }
```

---

## 9. Port forwarding

### `GET /api/forwards`

Lists all active port forwards.

```
Response 200:
{ "forwards": [{ "forward_id": "L-abc123", "session_id": "abc123", "direction": "local", ... }] }
```

### `GET /api/sessions/{id}/forwards`

Lists one session's forwards.

### `POST /api/sessions/{id}/forwards`

Creates a forward on a session. The session_id comes from the URL; no `ssh_config`
needed.

```
Request:
{
  "direction": "local",      // "local" | "remote" | "dynamic"
  "remote_host": "localhost",
  "remote_port": 80,
  "local_host": "0.0.0.0",   // required for remote mode
  "local_port": 8080          // 0 = auto-assign
}
```

| direction | Required fields |
|-----------|-----------------|
| `local` | `remote_host`, `remote_port` (1-65535) |
| `remote` | `local_host`, `local_port`, `remote_host`, `remote_port` (1-65535) |
| `dynamic` | none |

Errors: `404` unknown session, `409` the session is closed (DEAD — a closed
session is read-only and accepts no new forwards).

```
Response 201: ForwardInfo
```

### `DELETE /api/forwards/{id}`

Closes a forward.

```
Response 200: { "ok": true }
```

---

## 10. Files

All file operations go over the session's SFTP channel (remote) or the local file
system (internal). A closed (DEAD) session is read-only: every file endpoint
returns `409` (even when its SSH client is still open after a clean command exit),
and output is readable via `GET /api/shells/{id}/output-range` instead.

### `GET /api/sessions/{id}/files`

Lists a directory or stats a file.

| Parameter | Type | Meaning |
|-----------|------|---------|
| `path` | query | Path (required) |

```
Response 200:
{ "name": "home", "size": 4096, "is_dir": true,
  "children": [{ "name": "file.txt", "size": 1024, "is_dir": false, "mod_time": 1758499200123 }] }
```

### `GET /api/sessions/{id}/files/download`

Downloads a file. Supports HTTP Range.

| Parameter | Type | Meaning |
|-----------|------|---------|
| `path` | query | File path |

```
Response: application/octet-stream (Range/206 Partial Content supported)
```

### `POST /api/sessions/{id}/files/upload`

Uploads a file. Accepts multipart/form-data or a raw body.

| Parameter | Type | Meaning |
|-----------|------|---------|
| `path` | query | Target path |
| `offset` | query | Write offset, default 0 |
| Content-Range | header | Resume support |

```
Response 200: { "bytes_written": 1024 }
```

### `DELETE /api/sessions/{id}/files`

Deletes a file or an empty directory.

| Parameter | Type | Meaning |
|-----------|------|---------|
| `path` | query | Path |

```
Response 200: { "ok": true }
```

### `PUT /api/sessions/{id}/files`

Renames/moves a file or directory (same file system).

| Parameter | Type | Meaning |
|-----------|------|---------|
| `from` | query | Source path |
| `to` | query | Destination path |

```
Response 200: { "ok": true }
```

### `POST /api/sessions/{id}/files/dir`

Creates a directory (including parents).

| Parameter | Type | Meaning |
|-----------|------|---------|
| `path` | query | Directory path |

```
Response 200: { "ok": true }
```

---

## 11. Notification rules (shell_notify)

Reverse-wake-up rules registered by the MCP `shell_notify` tool are listed and
deleted here. The Web UI's Notifications tab reads the same source.

### `GET /api/notifications`

Lists all active notification rules.

| Parameter | Type | Meaning |
|-----------|------|---------|
| `shell_id` | query | Optional filter by shell |
| `session_id` | query | Optional filter by session |

```
Response 200:
{ "notifications": [{ "rule_id": "notif_...", "session_id": "...", "shell_id": "...",
                     "channel": "resource"|"sampling", "event": "output"|"exit"|"silence",
                     "created_at": 1758499200123 }] }
```

### `DELETE /api/notifications/{id}`

Unregisters one rule (`id` = `rule_id`).

```
Response 200: { "ok": true, "rule_id": "notif_..." }
Response 404: { "error": "notification rule not found" }
```

---

## 12. Review mode

A session can require **review** of everything the AI sends to its terminal.
The switch is per-session and covers every shell channel in it.

Why it exists: an agent driving a production host can issue a destructive
command as easily as a safe one. Review mode puts every write in front of a
human before any byte reaches the shell.

Scope, stated plainly: this gates **every change an AI makes through MCP** —
terminal input, file transfers, and port forwarding. An agent's three ways to
change a host are all covered, because a gate with a hole in it is worse than no
gate: it reads as protection without being any.

What is **not** gated is the operator's own surface. The WebSocket terminal, the
REST terminal endpoints, and the Web UI's file browser (which is the REST file
API) keep working while review is on. The rule is "the AI's programmatic surface
is reviewed; the human's interactive one is not", and it is not a loophole: a
holder of the deployment token can approve their own request just as directly as
they could bypass the gate, so the gate was never protecting against them. It
protects against an agent changing a host that nobody was watching.

Reads are not gated either — `file_read`, `file_stat`, `forward list` and the
like change nothing, and a reviewer asked to approve a directory listing learns
to click Accept without reading, which is how a gate stops working.

### Turning it on

Two entrances write the same state through `PATCH /api/sessions/{id}/approval`:

- **The lock at the head of the tab strip** in the terminal — a closed amber lock
  while the gate is on, an open one while it is off. Clicking it opens the review
  panel.
- **The switch inside that panel**, which is where the state is spelled out.

Either entrance may be used; the session card's lock is a third, for a session
with no terminal open. None turns it on behind your back, and none asks for an
approver count: review has one reviewer.

The lock sits **in the tab strip rather than beside the session title**, because
the strip is the part of the header that gives way when there is not enough room:
it takes the leftover width and **scrolls horizontally**. Put beside the title, the
lock competed with the connection name for the same fixed space — at a 320px
viewport that name collapsed to **zero width** and the tab bar was laid out over
the lock, so a press at its centre hit a tab button. In the strip the trade is
different, and at 320px the name keeps **108px of visible text** with the lock
present. The strip's scrollbar is hidden: the row is 34px tall and a bar would eat
the glyphs it exists to reach. On touch the **collapse** button is hidden instead —
collapsing a full-screen phone window leaves a header bar over the page with the
terminal gone, which is a state with no way back that a user reaches by accident.

To make it the **default** for a host rather than a switch flipped per session,
set `default_approval = true` on that connection profile (see §4). Sessions
created from it start gated — including sessions created by an agent through
MCP, which is the case the setting exists for: a human who forgets to flip the
switch is exactly the failure it prevents. The card shows a closed lock while the
profile is set that way. It can still be turned off per session afterwards.

The lock carries the mode as a glyph **and the count as a small red number on its
corner**. The count belongs there and only there, because the lock is the one
control that opens the queue the count refers to.

An earlier version badged the pane tab each request belonged to — a file write
badged Files, a forward badged Forwardings — on the theory that it would say
*which* face was waiting. In use that was wrong: those tabs show the file listing
and the forward list, **not the queue**, so a dot on them promises something the
tab cannot deliver. You click Files, see nothing pending, and the badge has taught
you to ignore badges. The count sits on the lock's corner so it never changes the
lock's 26px and the header row does not reflow when a request arrives.

The lock opens a **panel that floats over the window**, anchored top-right under
the header. It floats rather than taking height on purpose: as a flex sibling
it pushed the terminal up by its own height, so every open reflowed the PTY and
moved the lines being read. It is a child of the window content rather than of
the terminal wrapper, so it stays on screen while the Files or Forwardings tab is
open — those tabs hide the terminal wrapper, and an approval for a file write
arrives exactly there. It holds two things:

- the **switch** (with the state spelled out, since this is where it is changed),
- the **queue** of writes waiting for a decision. Each entry is a **read-only
  box, two lines tall and scrolling** for anything longer, with **Accept over
  Reject** stacked in a column beside it. A command line shows its text; a file
  or forward operation shows a **summary** ("write 1.2 KB to /etc/hosts", "open
  local forward :8080 -> db:5432") plus a **kind chip**, because the payload of
  an operation is not something a human can read back. On a touch device the box
  spans the row and the two buttons go back **side by side below it** at a 40px
  touch target.

There is no composer and no editable field. The commands in the queue are
written by the AI; a human's job is to accept or refuse them, and typing a
command belongs in the terminal behind the panel. The page header carries no
approval entry point: a decision is about one command line in one session, and
the terminal already says which session that is.

Named keys are shown as chips beside the command, with one exception: **Enter is
not shown**. It is the key that ends a command line, so it never varies and the
request already *is* a command — a chip saying "Enter" is noise on every row. A
key that a human could not infer from the text is still shown, because seeing it
is the whole point of approving it: `Ctrl+C` interrupting a running process,
`Tab` completing, an arrow moving the cursor. A request whose only key was Enter
renders no chips at all.

**A minimized window** cannot show the panel, so its queue shows up on the
**session tab** in the tab bar (a red count on the tab) and on the **session
card** in the grid (`N awaiting review`). Both also mark the session so a queued
command is never invisible just because its terminal is hidden.

**A notification about a pending decision opens the decision, not just the
session.** Clicking the toast focuses the session (creating, restoring, expanding
or raising its window as needed — the same entrance the session tiles use) and then
opens the review panel on it. Landing on a terminal and still having to find the
queue is the step the click exists to remove. The minimized case is the one that
matters most: there, the notification is often the only signal that anything is
waiting.

### Approving runs the thing, and by the right machinery

An approved request executes **when the decision is made**, in the same call as the
decision itself. The write happens after the state flip, so a failed execution
cannot leave the queue believing an unexecuted operation ran; a failure returns
`409` with the reason rather than reporting success.

There are two kinds of request and they execute on different machinery:

- **A command line** (text plus keys) is written to its shell.
- **A file transfer or a port forward** is replayed through the MCP handler that
  submitted it, with the session id re-resolved from the request rather than
  trusted from the payload.

Both routes matter, and confusing them fails in the worst possible way: the
decision is accepted, a failure is reported, and **nothing happens** — leaving the
reviewer believing the operation ran. That is exactly what shipped once, because
the executor looked up a shell for every request and an operation's payload carries
a session id and no shell id, so it failed with `shell  is no longer attached`
(note the empty id) before the operation was attempted. The kind now decides the
route, and a deployment with no MCP server refuses operation approvals explicitly
rather than failing obscurely.

### The Notifications tab is not the toast list

The terminal's **Notifications** tab (now titled *Notify rules*) lists **wake-up
rules** — the ones an agent registers with the `notify` tool, over
`/api/notifications`. It does **not** list the toasts that appear in the corner;
those are transient UI events, including the review notifications above. An empty
tab next to a toast you just saw is therefore correct, and the empty state says so
rather than leaving it ambiguous.

### One reviewer

Whoever turns review on is the reviewer, and one approval is enough. There is no
approver count, and no name is collected.

This is deliberate rather than a simplification. Termcp authenticates a
deployment with a single token, so it cannot verify who clicked; a self-declared
name would look like attribution in the audit trail while proving nothing, and a
threshold over claimed names would look like a second opinion without being one.
A deployment that needs proven reviewer identity issues a token per reviewer and
records the authenticated subject — at which point a threshold becomes
meaningful and can be added.

The decision is recorded as `local` in the audit trail, which says what the
server actually knows: that the request was accepted through this deployment.

### The queue holds command lines, not keystrokes

An agent types a line and then presses enter, and those are two calls. Reviewing
them separately would be wrong twice over: the reviewer would decide twice on one
command, and could approve the text while rejecting the newline, leaving a
half-typed line in the shell.

So `shell_input` **holds** its text, and the ending key **commits** it:

```
shell_input("echo hi")   → held            (nothing queued yet)
shell_key("enter")       → one queued request: text="echo hi", keys=["enter"]
```

With nothing held, the key alone is the unit: a bare `ctrl+c` interrupts a
running process, and a bare enter runs an empty line, either of which a reviewer
must still see.

Holding is per shell. Text that is held but never ended is discarded when review
is turned off or reconfigured: it was never reviewable, and carrying it into the
next policy would let it reappear as something nobody saw.

### The one-way rule

While review is on, a write reaches a shell through exactly one path:

```
MCP shell_input / shell_key       ─┐
MCP file_write / file_delete / …  ─┼─→ queue → a human → executed → host
MCP forward (local/remote/dynamic)─┘
```

The operator's own paths are **not** in that diagram, and that is the point:

- The **WebSocket `input` message** goes straight to the shell. It carries a raw
  character stream (xterm's `onData` hands over one keystroke at a time), and a
  per-keystroke queue would ask a reviewer to accept `l`, `s`, and the newline
  separately. The decision worth reviewing is the assembled command line, and an
  agent's assembled line arrives through MCP.
- The **REST terminal endpoints** (`/api/shells/{id}/input`, `/key`) and the
  **REST file and forward endpoints** are the Web UI's own backend — its file
  browser is built on them. They stay open, because locking the operator out of
  their own tooling protects against nobody who holds the token anyway.

So the gate follows the surface, not the byte: MCP is the AI's, WebSocket and
REST are the human's.

An operator typing into a reviewed session is not interrupted by any notice,
because nothing is refused: typing works.

### What the agent is told

A queued write answers `202 Accepted` with no id:

```json
{ "ok": true, "approved": false, "review_pending": true }
```

The agent cannot decide the request, so an id would only invite a retry loop. It
is told the write is waiting and continues; the shell's output afterwards shows
whether the command ran. (An agent that wants to be woken when a decision lands
can register `shell_notify`.)

### `GET /api/sessions/{id}/approval`

```
Response 200:
{
  "session_id": "3f2a…",
  "approval_mode": true,
  "need": 1,                 // always 1: review has a single reviewer
  "pending_count": 1,
  "requests": [ … ],         // every request this session has seen
  "websocket_input": true    // always true: the operator's stream is never gated
}
```

### `PATCH /api/sessions/{id}/approval`

Turn the gate on or off. Authorization is the deployment's existing HTTP auth:
whoever holds the token can flip it.

```
Request (enable):  { "enabled": true }
Request (disable): { "enabled": false }
```

`need` and `timeout_seconds` are accepted but optional: an omitted `need` means
1, and an omitted timeout means a pending request never expires. A `need` greater
than 1 is refused with `400` rather than silently downgraded, so a caller cannot
believe it configured a threshold the server ignores.

Turning it **off cancels everything pending**, and so does enabling again with a
different threshold: an input queued under one policy must not become
executable under another. A queued request that is cancelled is fail-closed —
its bytes are never written.

Errors: `400` `need` > 1, negative timeout, malformed body; `404` unknown session.

### `GET /api/approvals`

Every request across all sessions, so an approver's page shows one worklist.

```
Query:
  session_id=3f2a…   optional filter

Response 200:
{
  "count": 1,
  "approvals": [
    {
      "need": 1,
      "request": {
        "id": "9f0c…",
        "session_id": "3f2a…",
        "shell_id": "5e71…",
        "source": "mcp",          // mcp | rest
        "text": "rm -rf /tmp/x",
        "keys": ["ctrl+c"],       // named keys, never raw bytes
        "created_at": 1790129915403,
        "expires_at": 1790130035403,
        "state": "pending",       // pending|approved|rejected|expired|cancelled
        "approvals": ["local"],   // who accepted: always the deployment, never a claimed name
        "need": 1
      }
    }
  ]
}
```

### `POST /api/approvals/{id}/approve` · `POST /api/approvals/{id}/reject`

```
Request: {}                       // a decision needs no body
Response 200: { "request": { … } }
```

A rejection may carry `{"reason": "…"}`. No name is sent: the decision is the
click, and the server records it as `local` because that is what it can actually
know. See "One reviewer" above for why a self-declared name was removed rather
than kept.

Semantics:

- **One approval is enough**, and the first decision wins. A second decision on
  the same request is refused (`409`) rather than re-applied.
- **One rejection is final.** Review gates a dangerous command; it is not a vote
  that can be outvoted.
- Expiry is fail-closed: the request becomes `expired`, never `approved`.
- An approved request is executed by the deciding call. If the write fails, the
  response says so (`409`) rather than reporting success.
- `404` unknown id; `409` already decided or execution failed.

> **What is approved.** The request records the *input string*, not a parsed
> command. `shell_input("rm -rf / ; ls")` is one request covering two commands.
> Splitting it would require shell parsing, which is not attempted.

> **A dead session cancels what is pending.** A request whose session has gone
> is cancelled rather than left decidable: approving it would record a grant
> while the bytes are refused, making the audit trail claim something that never
> happened.

### Audit trail

Approval transitions are written into the shell's own byte log
(`log.jsonl`), whose status field is an open set, so no schema change is needed:

| Status | Meaning |
|--------|---------|
| `q` | an input was queued for approval at this offset |
| `A` | approved input was written at this offset |

A rejected, expired, or cancelled request leaves the log at `q` — no bytes ever
followed it, which is exactly what a replay should show. Input that passed a gate
is distinguishable from input that never met one.

### Waking a waiting agent

An agent does not have to poll. Registering `shell_notify` on the shell makes the
queue wake it when a decision lands, because an approval transition is delivered
through the same `output` event path.

---

## 13. Backward-compatible routes

Old routes still work and delegate to the new ones. New code should use the canonical
paths above.

| Old path | Canonical path |
|----------|----------------|
| `POST /api/sessions/start` | `POST /api/sessions` |
| `GET /api/sessions/{id}/child-shells` | `GET /api/sessions/{id}/shells` (301) |
| `POST /api/sessions/{id}/terminate` | `DELETE /api/sessions/{id}` |
| `POST /api/sessions/{id}/disconnect` | `DELETE /api/sessions/{id}` |
| `POST /api/sessions/{id}/close-shell` | Close the primary shell (`session_id` in path) |
| `POST /api/forwards` | `POST /api/sessions/{id}/forwards` |
| `DELETE /api/sessions/{id}/files/delete` | `DELETE /api/sessions/{id}/files` |
| `POST /api/sessions/{id}/files/rename` | `PUT /api/sessions/{id}/files` |
| `POST /api/sessions/{id}/files/mkdir` | `POST /api/sessions/{id}/files/dir` |

> Note: the legacy `terminate` / `disconnect` only **close** a session (it stays in
> the registry as a DEAD, read-only entry). To erase it for good use
> `DELETE /api/sessions/{id}`.
