---
name: termcp-http
description: Drive the termcp terminal-platform over its HTTP REST API with curl + jq (no MCP client needed). Always fetch the instance's own <origin>/api.md first for version-exact endpoints, then use the recipes for creating sessions, polling output in chunks, sending input/keys, resizing PTYs, terminating or purging sessions, exporting transcripts/screenshots, transferring files, and managing port forwards. For server sweeps, batch commands, CI, and agents that only have HTTP access.
---

# termcp over HTTP

termcp is a terminal-session platform: one HTTP port serves REST, WebSocket and
MCP. This skill uses curl only — no MCP client required.

The documentation endpoints (`/api.md`, `/skills.md`) are public: they work with
no token. **API calls themselves need the token** whenever the instance runs with
authentication enabled (see section 2).

> Served at `<origin>/skills.md`. To install it as a loadable skill, save it to
> `~/.agents/skills/termcp-http/SKILL.md` (the folder name is what gets discovered).

## 1. Rules

1. **Read the instance's own docs first**: `curl <origin>/api.md`. The instance
   serves its own docs, always matching the running API — this skill deliberately
   does not duplicate endpoint details.
2. **Origin** = the address you got from the user (default `http://localhost:18765`).
3. **Never put credentials in URLs or logs**: use
   `Authorization: Bearer <token>` when auth is enabled.

## 2. Connect & authenticate

```bash
BASE=${TERMCP_BASE:-http://localhost:18765}
AUTH=()                                           # no auth configured
# AUTH=(-H "Authorization: Bearer $TERMCP_TOKEN") # --auth-token on

curl -fsS "${AUTH[@]}" "$BASE/api.md"             # authoritative reference — read before acting
```

## 3. Session lifecycle

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

# Stop but keep history (archive, still readable)
curl -fsS -X POST "${AUTH[@]}" "$BASE/api/sessions/$SID/terminate"
# Permanently delete (archive + on-disk messages, irreversible)
curl -fsS -X DELETE "${AUTH[@]}" "$BASE/api/sessions/$SID"
```

`ssh_config`: `"internal"` (the termcp host) or a configured profile name;
`GET /api/connections` lists names only (never credentials).

## 4. Read output (cursor semantics)

Output is a byte stream: `GET /api/shells/{shell_id}/output-range` returns
`{"start","end","total","d":<base64>}`. Poll with `end` as your cursor until
`end == total` and it stops growing.

```bash
read_range() { # $1=shell_id $2=start
  curl -fsS "${AUTH[@]}" "$BASE/api/shells/$1/output-range?start=$2&max=262144"
}
R=$(read_range "$SHELL_ID" 0)
echo "$R" | jq -r .d | base64 -d            # decoded text
END=$(jq -r .end <<<"$R"); TOTAL=$(jq -r .total <<<"$R")
# Tail only: ?tail=1&max=8192
```

After issuing a command, sleep ~0.3s and re-poll until output stops growing.
Empty reads do NOT mean done — check session/shell status. For REPL/TUI
programs, use `tail=1` to watch the latest screen.

## 5. Write input

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

## 6. Archived sessions, transcript, screenshot

```bash
curl -fsS "${AUTH[@]}" "$BASE/api/history" | jq -r '.sessions[]?.id'    # archived sessions
curl -fsS "${AUTH[@]}" "$BASE/api/history/$OLD/transcript?format=markdown"   # full transcript
curl -fsS "${AUTH[@]}" "$BASE/api/history/$OLD/screenshot?lines=60&cols=160" -o s.png
```

Archived sessions are read-only: write endpoints reject them; use `output-range`.

## 7. Files & port forwards

Bodies are in `/api.md`. Essentials:

- Files: `GET/PUT/DELETE /api/sessions/{sid}/files`,
  `POST /api/sessions/{sid}/files/upload|dir`, paths via the `path=` query
  parameter; auth-protected like everything else.
- Forwards: `POST /api/sessions/{sid}/forwards` (local/remote/dynamic),
  `GET /api/forwards` to list, `DELETE /api/forwards/{id}` to close.

## 8. Pitfalls

- `DELETE /api/sessions/{id}` and `DELETE /api/history/{id}` are **permanent**;
  use `terminate` to keep history.
- `shell_id` ≠ `session_id`: terminal I/O (output-range / input / key / resize)
  takes `shell_id`.
- With auth enabled, every API endpoint needs credentials (`/api.md` and
  `/skills.md` are public and need none); keep tokens in headers only.
- Password/sudo prompts belong to the human — never guess and paste secrets.
- The instance's own `/api.md` is authoritative; this skill provides the
  workflow, not a frozen endpoint list.