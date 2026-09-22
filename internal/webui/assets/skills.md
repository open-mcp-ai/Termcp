---
name: termcp
description: Drive the termcp terminal-platform over its HTTP REST API with curl + jq (curl only). Use whenever a request mentions termcp, a termcp URL/locator (termcp://rock64, termcp://#<session>, termcp://#<session>:2), "open/connect to <host> through termcp", or termcp sessions/shells. Always fetch the instance's own <origin>/api.md first for version-exact endpoints, then use the recipes for resolving locators, creating sessions, polling output in chunks (live and closed sessions), sending input/keys, resizing PTYs, terminating or purging sessions, transferring files, and managing port forwards. For server sweeps, batch commands, CI, and agents that only have HTTP access.
---

# termcp over HTTP

termcp is a terminal-session platform: one HTTP port serves REST, WebSocket and
MCP. This skill uses curl only.

The documentation endpoints (`/api.md`, `/skills.md`) are public: they work with
no token. **API calls themselves need the token** whenever the instance runs with
authentication enabled (see section 2).

> Served at `<origin>/skills.md`. Install it by saving it into your agent's
> skills directory as `termcp/SKILL.md` (the folder name is what gets discovered):
>
> - **Claude Code**: `~/.claude/skills/termcp/SKILL.md`
> - **Other agents** (shared Agent Skills convention): `~/.agents/skills/termcp/SKILL.md`
> - **Project-scoped**: `<project>/.claude/skills/termcp/SKILL.md`
>
> Restart the agent session afterwards — skills load at session start. Claude Code
> has no per-skill CLI command: install = write the file, remove = delete the folder.

## 1. Rules

1. **Read the instance's own docs first**: `curl <origin>/api.md`. The instance
   serves its own docs, always matching the running API — this skill deliberately
   does not duplicate endpoint details.
2. **Origin** = the address you got from the user (default `http://localhost:18765`).
3. **Never put credentials in URLs or logs**: use
   `Authorization: Bearer <token>` when auth is enabled.
4. **A `termcp://...` string is a locator, not a command and not a web page.**
   Never try to "open" it in a browser or as a shell argument; resolve it with
   `GET /api/resolve` (section 2) and act on the ids it returns.

## 2. Connect & authenticate

```bash
BASE=${TERMCP_BASE:-http://localhost:18765}
AUTH=()                                           # no auth configured
# AUTH=(-H "Authorization: Bearer $TERMCP_TOKEN") # --auth-token on

curl -fsS "${AUTH[@]}" "$BASE/api.md"             # authoritative reference — read before acting
```

## 3. Resource locators (termcp://)

Users copy locators from the termcp Web UI and paste them into chat. A locator is
an **address for a termcp object**, not a URL to open in a browser and not a
shell argument:

| Locator | Names | Resolves to |
|---------|-------|-------------|
| `termcp://<entry>` | a connection profile (ssh_config), e.g. `termcp://rock64` | `ssh_config` name |
| `termcp://#<session>` | a session | `session_id` |
| `termcp://#<session>:<N>` | shell channel N of that session (1 = first tab) | `session_id` + `shell_id` |

Resolve any of them in one call — never parse or guess by hand:

```bash
curl -fsS "${AUTH[@]}" -G --data-urlencode 'url=termcp://rock64' "$BASE/api/resolve"
# {"kind":"entry",  "entry":"rock64", "ssh_config":"rock64"}
# {"kind":"session","session_id":"...","name":"...","status":"running"}
# {"kind":"shell",  "session_id":"...","shell_id":"...","index":2,"name":"shell-2","status":"running"}
```

Then act on the ids (see sections 4–6 for the full recipes):

- `"kind":"entry"` — "open termcp://rock64" means **connect to that profile**:
  `POST /api/sessions -d '{"ssh_config":"rock64"}'` → returns `session_id` +
  `shell_id`. Then drive them like any other session. 404 = no such profile
  (`GET /api/connections` lists them; one must be created first in the Web UI).
- `"kind":"session"` — an existing session: use `session_id` for output/files/
  forwards. `"status":"exited"` means read-only (closed): use
  `output-range`, not input.
- `"kind":"shell"` — use `shell_id` for input/key/output-range/resize; the
  `index` matches the `shell-1`/`shell-2` tabs.

Errors: `400` malformed locator, `404` unknown profile/session/out-of-range shell
index, `409` shell locator on a closed (read-only) session.

Example — "open termcp://rock64 and run uname -a":

```bash
OUT=$(curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"ssh_config":"rock64","name":"rock64"}' "$BASE/api/sessions")
SID=$(jq -r .session_id <<<"$OUT"); SHELL_ID=$(jq -r .shell_id <<<"$OUT")
# then: input section 6, wait, poll output section 5
```

## 4. Session lifecycle

```bash
# List (read-only)
curl -fsS "${AUTH[@]}" "$BASE/api/sessions" | jq -r '.sessions[] | [.id,.name,.status,.mode] | @tsv'

# Create run-and-exit session (one command; good for batch/CI)
OUT=$(curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"ssh_config":"internal","command":"uname","args":["-a"],"mode":"pipe"}' \
  "$BASE/api/sessions")
SID=$(jq -r .session_id <<<"$OUT")

# Create interactive session (no command → login shell)
OUT=$(curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"ssh_config":"internal","name":"investigate"}' "$BASE/api/sessions")
SID=$(jq -r .session_id <<<"$OUT"); SHELL_ID=$(jq -r .shell_id <<<"$OUT")

# Open another shell channel on an existing session
curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"name":"shell-2"}' "$BASE/api/sessions/$SID/shells" | jq .

# Close but keep in registry (DEAD, still readable via output-range)
curl -fsS -X POST "${AUTH[@]}" "$BASE/api/sessions/$SID/terminate"
# Permanently delete (drops from registry, removes its session directory)
curl -fsS -X DELETE "${AUTH[@]}" "$BASE/api/sessions/$SID"
```

`ssh_config`: `"internal"` (the termcp host) or a configured profile name;
`GET /api/connections` lists names only (never credentials).

## 5. Read output (cursor semantics)

Output is a byte stream: `GET /api/shells/{shell_id}/output-range` returns
`{"start","end","total","d":<string>}`. Poll with `end` as your cursor until
`end == total` and it stops growing.

```bash
read_range() { # $1=shell_id $2=start
  curl -fsS "${AUTH[@]}" "$BASE/api/shells/$1/output-range?start=$2&max=262144"
}
R=$(read_range "$SHELL_ID" 0)
echo "$R" | jq -r .d                     # already text; no base64 step
END=$(jq -r .end <<<"$R"); TOTAL=$(jq -r .total <<<"$R")
# Tail only: ?tail=1&max=8192
```

`d` is the raw byte window in a standard JSON string, not base64. Byte sequences
that are not valid UTF-8 cannot survive a JSON string (Windows ConPTY already
replaces them before termcp sees them; a Linux `cat` of binary data may show
U+FFFD) — `log.bin` itself is byte-exact.

After issuing a command, re-poll until output stops growing — the read blocks
until bytes arrive, so no delay is needed between polls.
Empty reads do NOT mean done — check session/shell status. For REPL/TUI
programs, use `tail=1` to watch the latest screen.

## 6. Write input

```bash
# Text, optionally followed by enter (typing into a shell/REPL)
curl -fsS -X POST "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"text":"ls -la","press_enter":true}' "$BASE/api/shells/$SHELL_ID/input"

# Named keys: enter tab esc up down left right backspace delete home end ctrl+c/d/z/l/u
curl -fsS -X POST "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"key":"ctrl+c"}' "$BASE/api/shells/$SHELL_ID/key"

# Resize the PTY (affects TUI layout)
curl -fsS -X POST "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"rows":40,"cols":120}' "$BASE/api/shells/$SHELL_ID/resize"
```

For real-time bidirectional streams (full-screen TUI) use the WebSocket
`GET /api/ui/ws` (`watch_add` to subscribe, `input` to write); curl cannot
speak WebSocket — without `websocat`, prefer REST + `output-range` polling.

## 7. Files & port forwards

Bodies are in `/api.md`. Essentials:

- Files: `GET/PUT/DELETE /api/sessions/{sid}/files`,
  `POST /api/sessions/{sid}/files/upload|dir`, paths via the `path=` query
  parameter; auth-protected like everything else.
- Forwards: `POST /api/sessions/{sid}/forwards` (local/remote/dynamic),
  `GET /api/forwards` to list, `DELETE /api/forwards/{id}` to close.

## 8. Pitfalls

- `DELETE /api/sessions/{id}` is **permanent**;
  use `terminate` to keep the session visible and readable.
- `shell_id` ≠ `session_id`: terminal I/O (output-range / input / key / resize)
  takes `shell_id`.
- With auth enabled, every API endpoint needs credentials (`/api.md` and
  `/skills.md` are public and need none); keep tokens in headers only.
- Password/sudo prompts belong to the human — never guess and paste secrets.
- The instance's own `/api.md` is authoritative; this skill provides the
  workflow, not a frozen endpoint list.