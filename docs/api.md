# termcp HTTP API

Base URL: `http://localhost:18765`

termcp is a terminal-session platform. One port serves the same sessions to four
kinds of callers:

| Entrance | Path | Caller |
|----------|------|--------|
| Web UI | `/` + `/api.html` | Humans (browser) |
| MCP | `/sse` + `/message` or `/stream` | AI agents with an MCP client |
| Agent Skill + REST | `/skills.md` (install once) + `/api/*` | AI agents driving termcp with `curl` |
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
| HTTP API reference (this file) | `/api.md` | You drive termcp over HTTP yourself (REST/WebSocket): endpoints, bodies, output-cursor semantics |
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
    { "name": "pi", "kind": "remote", "host": "192.168.1.100", "user": "pi", "port": 22 }
  ]
}
```

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

A locator names a termcp object in one string. The Web UI's copy buttons emit
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
| `input` | `id`, `d`(string), `nl` | Send keystrokes |
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
> ConPTY already replaces such sequences before termcp sees them, and on Linux
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

## 12. Backward-compatible routes

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
