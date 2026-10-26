# Capabilities and security model

Everything Termcp can do, and the rules that bound it. The wire language these
capabilities are driven through is documented in
[`protocol.md`](./protocol.md); HTTP endpoints are in [`api.md`](./api.md).

**Summary.** One MCP tool drives many long-lived SSH sessions, each holding
interactive and pipe terminals, files, port forwards, and mounted protocol peers
(Chrome DevTools, another MCP server, a raw JSON-RPC socket). Output is read by
byte cursor and survives a restart. Everything an agent writes can be gated on a
human decision, per session.

---

## Capability surface

| | |
| --- | --- |
| **Sessions** | Many long-lived SSH connections at once — to this host (`ssh_config="internal"`, zero config) or to any remote host; each session holds several shells, forwards and SFTP. |
| **Terminals** | Full `pty` (ConPTY on Windows) and line-oriented `pipe` channels on one connection; arbitrary rows/cols; input, named keys, resize. |
| **Output** | Byte-cursor reads (tail / offset / live reader), paged from disk and replayable after a restart; per-line timeline of who wrote what. |
| **Files** | SFTP browse, read, write, rename, delete, mkdir, permissions, links — plus HTTP URLs with Range resume. |
| **Forwarding** | `-L` local, `-R` remote, `-D` SOCKS5, over ProxyJump bastion chains. |
| **Browser & other protocols** | `open <session> cdp <url>` mounts Chrome DevTools through an existing session, so `:9222` is a port on the target: list tabs, call any protocol method, page a large answer back from the log. The same door mounts another MCP server (`mcp`) or a raw JSON-RPC socket (`jrpc`). |
| **Notifications** | `notify_user` toasts the human with the instance URL, plays a short tone and raises a desktop notification. One-way by design: nothing in termcp wakes an agent, so a caller waits on the reply it already has. |
| **SSH profiles** | A central TOML store with batch import/export, in-memory temporary hosts, connection testing, and edit/copy/rename. |
| **Review mode** | Per-session approval gate: every agent write waits for a human decision. |
| **Extensions** | A stored template for a session whose capabilities are trimmed to what you decided: an MCP server over a pipe channel, a read-only window onto remote files, an interactive terminal with instructions attached. One that declares no listener starts on demand (`open ext <name>`) and closes itself after `idle_timeout`. |
| **Connected clients** | The header lists who is attached: MCP clients with the name they announced and their namespace, plus Web UI tabs with their address and what they are watching. |
| **Update notice** | The header tells you when a newer release exists — only for real `vX.Y.Z` builds, never for a local `dev-<sha>`. |
| **One REST entrance** | `POST /api/term` speaks the same command language as the MCP tool, so an agent drives everything with plain `curl`. |

---

## Security model

- **Credentials stay server-side.** Passwords, private keys and passphrases written
  through `ssh_config` are stored on the host only and never read back; the MCP read
  interface returns profile names. Profile write tools stay off unless
  `--mcp-manage-ssh-configs` is set, and `--no-internal` narrows agents to remote
  hosts.
- **One token protects every surface.** `--auth-token`, or a salted `--auth-hash` so
  the server holds no plaintext — Web UI, REST, both MCP transports and the
  WebSocket. A non-loopback bind **requires** it.
- **Basic auth is Base64.** Terminate TLS in a reverse proxy when serving beyond
  your own machine; the token is never logged and never placed in a URL.
- **🚨 Do not put an agent on a production host unattended. If you must, turn review
  mode on.** Every write it makes over MCP then waits for a human decision. Review
  mode is a real gate, not a guarantee: a careless reviewer can still wave a
  destructive command through.
- **What needs a live connection.** On a closed (DEAD) session, file and forward
  tools return `session_not_running`; output reading keeps working, and the
  session's port forwards close with it.

Review mode is not a setting on top of the capability model — it is one of the three
gates the model already has. [`protocol.md` §6](./protocol.md#6-three-way-capability-gate)
explains which acts fall behind which gate, and why the gate sits on the act that
grows the capability list rather than on individual commands.
