# The Termcp Protocol (TERM/1)

> **Terminalization Model Context Protocol** — the wire format and semantics
> exposed by Termcp's single `term` MCP tool and `POST /api/term`.

Termcp collapses what would otherwise be dozens of MCP tool schemas into **one
tool** and carries capability inside the conversation turns. The wire
language it speaks is **`TERM/1`**.

This document is the reference for the protocol as implemented in v0.2.8.
The running instance is always the authoritative source:
- Empty `cmd` → human-readable guide
- `help` → directive table
- `help <directive>` → one directive's full manual

---

## 1. Wire format

Replies follow an HTTP-shaped envelope:

```
TERM/1 <Status-Code> <Reason-Phrase>
<Header-1>: <Value-1>
<Header-2>: <Value-2>

<Payload-Bytes>
```

- **Status line**: `TERM/1` protocol tag, 3-digit numeric code, standard reason phrase.
- **Headers**: colon-separated ASCII key-value pairs, ordered. Values cannot contain unescaped newlines.
- **Blank line**: separates headers from body.
- **Payload**: raw bytes of the response. May be empty, text, or binary stream slice.

### Why HTTP-shaped?

Language models already possess strong priors on HTTP status codes and headers.
Re-using that prior reduces hallucination: an agent that sees `206 Partial Content`
knows there is more to read; an agent that sees `208 Not Run` knows the branch was
skipped by short-circuit evaluation.

---

## 2. Status code vocabulary

The protocol uses a strict subset of HTTP status codes, mapped to execution outcomes:

| Code | Reason | Meaning in TERM/1 |
|:---:|---|---|
| `200` | `OK` | Complete execution; full result delivered in the body. |
| `202` | `Accepted` | Execution deferred; queued for human review or running asynchronously. |
| `204` | `No Content` | Command succeeded and delivered zero bytes. **Body is always empty**; diagnostic reasons (e.g. why no frame matched) ride in the `Hint` header. |
| `206` | `Partial Content` | Successful slice of a larger stream. Accompanied by a `Content-Range` header. |
| `208` | `Not Run` | Statement was skipped due to short-circuit logic (e.g. right side of `&&` after a failure). The slot is preserved so position maps 1:1 to the input AST. |
| `400` | `Bad Request` | Syntax error, unrecognized directive, missing required flag, or invalid flag combination. Refused before execution begins. |
| `403` | `Forbidden` | Operation blocked by session whitelist (`allow` / `paths`) or review policy. |
| `404` | `Not Found` | Target session, channel, file, extension, or host does not exist. |
| `409` | `Conflict` | Resource conflict (e.g. creating an extension or host with a name already taken). |
| `413` | `Payload Too Large` | Single frame exceeds the transport window limit. |
| `500` | `Internal Error` | Unexpected backend or driver failure. |
| `502` | `Bad Gateway` | Remote peer, upstream SSH server, or forwarded port unreachable. |

---

## 3. Headers

Headers provide decision-making metadata for the caller's next step:

### Addressing & Ranges
- `Content-Range: bytes <start>-<end>/<total>` — byte offsets within the stream. The continuation point `<end>+1` is what the next `--cursor` or `--offset` flag accepts.
- `Input-At: <byte-offset>` — the stream offset where this specific command was typed. Re-reading from this address reproduces the command's complete output.
- `Path: <canonical-path>` — the canonical directive invocation that produced this slot.
- `Channel: <id>` / `Session: <id>` — identity of the channel or session that executed the work.

### Explanations & Next Steps
- `Hint: <single-line-text>` — actionable diagnostic on `204` or `4xx` replies (e.g. "no frames arrived within the timeout", "quoted string missing closing quote"). Replaces bodies on `204`.
- `Waited: <duration>` — how long the server waited for output before answering. Differentiates "waited and heard nothing" from "zero timeout peek".

### Window Boundaries & Truncation
- `Truncated: <count> bytes withheld` — output exceeded the caller's `bytes_limit`.
- `Trimmed: <count> lines trimmed` — lines withheld by `lines_limit`.
- `Scan-Window: searched bytes A-B of T` — grep or search stopped early due to byte-budget bounds.
- `Omitted: <count> bytes` — middle section withheld by folding rules.
- `Still-Running: true` — process is still active; re-read will produce more bytes.

---

## 4. The 19 Directives

All operations route through 19 top-level directives:

### Session & Lifecycle
- `connect` — establish connection to a remote SSH host or alias.
- `open` — launch a shell (PTY/Pipe), an extension, a port forward, or a protocol peer.
- `close` — terminate a shell, extension, peer, or connection.
- `rm` — remove a configuration, template, or stored record.

### Inspection & Navigation
- `ls` — list sessions, channels, files, hosts, extensions, forwards, or tasks.
- `get` — inspect detailed configuration of a session, extension, or host.
- `help` — show grammar reference, directive list, or verb manual.

### Terminal & Process Control
- `run` — execute a shell command line (`run <channel> $CMD`). Payload is a variable reference.
- `input` — send raw characters or keystrokes to an interactive PTY.
- `key` — send control characters, function keys, or escape sequences (`Enter`, `Ctrl+C`).
- `resize` — update PTY window dimensions (`cols` / `rows`).
- `span` — mark a logical boundary or bookmark in the terminal stream.
- `reader` — register or unregister an automated output watcher.

### Content & Transfer
- `read` — consume bytes from a channel, inspect files, or fetch peer frames. Supports slicing (`--offset`, `--bytes`, `--cursor`), searching (`--grep`), and filtering (`--stream o|e`).
- `write` — write bytes to a file, edit text in place, or adjust file permissions/metadata.
- `call` — send a JSON-RPC / CDP frame to a connected peer, or invoke an MCP tool.

### Management & Configuration
- `create` — create a host profile, extension template, alias, or file.
- `edit` — modify an existing host profile or extension configuration.
- `notify` — push an alert or confirmation request to the human operator in the WebUI.

---

## 5. Architectural invariants

1. **Parsed, never evaluated.** The `cmd` DSL uses `mvdan.cc/sh/v3/syntax` for lexical analysis. Quotes, escapes, and subshells reach the target execution environment byte-for-byte; Termcp never interpolates variables into the grammar.
2. **Values never become syntax.** `$1`, `$NAME`, and `$SECRETS` can only occupy value slots. If a variable is placed where a verb, flag name, or channel index belongs, it is rejected with a `400` error before any statement runs.
3. **Atomic parsing.** A compound line (`cmd1 ; cmd2 && cmd3`) is parsed in its entirety before statement 1 executes. A syntax error at position 3 prevents position 1 from running.
4. **Slot alignment.** Every statement in a compound command produces exactly one slot in the envelope. Short-circuited statements produce `208 Not Run` slots so slot index matches AST node index.
5. **Constant MCP surface.** Adding, removing, or hot-reloading extensions never changes the `tools/list` schema. The tool definition stays **1 tool, 13 arguments, 2,580 bytes**. Capability expansion happens entirely in the data channel.
6. **No silent parameter drop.** Every accepted flag must change the behavior or be rejected with `400`. The third state (accepted and ignored) is forbidden.

---

## 6. Three-way capability gate

Every operation maps to one of three capability gates (`internal/capability`):

- **GateOperator** — The operator's own surface: the browser terminal, the WebUI's file browser, the REST API called with a deployment token. Never reviewed — review exists to catch an *agent* doing something no human looked at, and asking a human to approve their own keystroke would lock them out of the session they are watching.
- **GateDevice** — "The device was assigned, so its use is authorised." Running a command in an assigned session is not something a rule can judge: what that command installs could be anything, so it is authorised by the assignment ("this session may act as this host's operator"), not by the command text.
- **GateReview** — Operations that **grow the agent's capability surface**, which is the boundary a human can actually see and decide:
  - Hot-loading a new extension (`create ext`)
  - Installing an MCP server on a remote host
  - Opening a port forward / tunnel
  - Writing a file

When review mode is enabled on a session, a GateReview operation yields **`202 Accepted`** and pauses until a human approves the action in the WebUI. Approval and execution are separate events: the approval releases the queued act, it does not re-run it.
