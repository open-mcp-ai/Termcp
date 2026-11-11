# MCP connection attribution interface testing

Scope: issue #272. This matrix covers the HTTP, SSE, and stdio bridge
interfaces that expose MCP client attribution and activity counts. It uses
temporary data directories and synthetic credentials; no operator credentials
or local data directory are part of the test.

## Test layers

| Layer | Command or setup | Purpose |
| --- | --- | --- |
| Black-box HTTP | temporary `termcp` instance, `curl`, `jq` | Verify public routes, JSON fields, status codes, and headers |
| Transport integration | `go test -race ./internal/mcp ...` | Verify mcp-go session hooks, Streamable HTTP, SSE, and bridge ownership |
| Auth lifecycle | `go test -race ./internal/auth ...` | Verify issue, rotation, expiry, revocation, disable, policy, and persistence |
| WebUI contract | `go test -race ./internal/webui ...` | Verify `/api/clients`, WebSocket payloads, and safe rendering |
| UI smoke | built-in browser, temporary instance | Verify desktop and narrow permission layouts and activity metric |

## Functional matrix

| ID | Case | Expected |
| --- | --- | --- |
| F1 | First-run setup and Admin login | Admin session is created; protected routes remain closed without a session |
| F2 | Create an Agent and issue its current Token | One Agent, one active Token; the secret is returned only by the issue response |
| F3 | Two Streamable HTTP initializations using one Agent Token | Two distinct session IDs; both count as two connections |
| F4 | `GET /api/clients` | MCP rows include `principal_id`, `agent_name`, `credential_id`, `transport`, `client_mode`, `session_id`, and `connected_at` |
| F5 | `GET /auth/principals` after F3 | The Agent's `active_mcp_connections` is 2 and remains separate from `active_requests` |
| F6 | Initialize over `/sse` | The SSE row has the same verified Agent and Token attribution |
| F7 | `termcp stdio` / `daemon stdio` request headers | Requests carry the display-only `X-Termcp-Client-Mode: stdio-bridge` header |
| F8 | Revoke the current Token | The Token stops authenticating; all of its MCP sessions disappear from `/api/clients` and the Agent count returns to 0 |
| F9 | Disable an Agent with a live session | The session is removed and the Token is rejected |
| F10 | Change or disable its permission group | The live request is canceled and the session disappears after the authority change |
| F11 | Rotate a Token | The old Token and its sessions stop working; the replacement Token can initialize a new session |
| F12 | Expire a Token or use `last_used_at` | Expired admission is 401; successful admission updates `last_used_at` metadata |

## Security matrix

| ID | Case | Expected |
| --- | --- | --- |
| S1 | `/api/clients` response | Never contains a raw bearer Token or its digest |
| S2 | Use Agent B's Token with Agent A's session ID | 403; a valid credential cannot adopt another credential's session |
| S3 | Spoof `X-Termcp-Namespace` and `X-Termcp-Client-Mode` | Namespace remains derived from the verified Agent; client mode cannot change authorization |
| S4 | Use an Agent Token on `/api/clients`, `/auth/*`, or a WebUI route | 403; MCP bearer credentials do not grant API or browser authority |
| S5 | Anonymous `/api/clients` | 401 |
| S6 | Duplicate `Authorization` headers | 401; ambiguous credentials are refused |
| S7 | Cookie write without CSRF or with a foreign Origin | 403 |
| S8 | Revoked, rotated, disabled, expired, or deleted credential | Rejected after the authority change and after restart |
| S9 | Token issued by one instance/data directory | Not accepted by another instance |
| S10 | Browser Cookie and explicit invalid Bearer together | Explicit Bearer wins and does not fall back to the Cookie |

## Execution record

Run on 2026-11-07 on macOS with a temporary data directory:

- Black-box HTTP matrix: 26/26 checks passed, including two Streamable HTTP
  sessions, attribution fields, spoofed namespace rejection, cross-Agent
  session rejection, Bearer/Cookie separation, revocation, and Agent disable
  cleanup.
- Black-box SSE check passed: `/sse` returned an endpoint, the initialized
  session was attributed to the correct Agent and Token, and revocation removed
  the row.
- Black-box CLI bridge check passed: `termcp stdio` exposed a live
  `streamable_http` row with `client_mode=stdio-bridge`.
- Cross-instance isolation passed: a Token issued by the first temporary
  instance was rejected with 401 by a second instance with a different data
  directory.
- `go test -race ./internal/mcp -run '^TestManagedMCPTransportCredentials$'
  -count=3 -shuffle=on`: passed 3/3.
- Focused auth lifecycle suite: passed 18/18, covering Token purpose, rotation,
  expiry/last-used metadata, revocation, policy changes, and group identity.
- Focused WebUI client suite: passed 9/9, covering API fields, panel rendering,
  typed text rendering, and the WebSocket `mcp_clients` frame.
- `make test`: full repository race/shuffle suite passed.
- `go vet ./...` and the `no_webui` build/test checks passed.

## Residual risks

- Activity count is process-local; a session that expires while idle is removed
  when the transport exits or an authority-changing mutation runs, not by a
  passive expiry timer. Admission after expiry is still rejected with 401.
- The black-box matrix is loopback-only. TLS termination, reverse-proxy path
  prefixes, and multi-process sticky-session deployments require separate
  deployment testing.
- The browser smoke test is a visual and interaction check, not a substitute
  for the three-platform CI execution.

## 2026-11-08 OAuth and daemon stdio integration record

Run on macOS with `codex-cli 0.166.0`, temporary data directories, and a
synthetic external OAuth authorization server:

- Codex discovered Termcp's protected-resource metadata from the 401 challenge,
  completed a pre-registered-client authorization-code/PKCE login, initialized
  Streamable HTTP, and invoked `term` successfully.
- A separate isolated Codex profile completed Dynamic Client Registration,
  authorization-code/PKCE login, initialization, and `term` against the same
  endpoint.
- `/api/clients` attributed that live session to the expected Agent,
  `transport=streamable_http`, and an `oauth-` credential digest without
  exposing the bearer token.
- Disabling OAuth through `/auth/oauth` removed the live session; re-enabling it
  allowed the next Codex initialization.
- A 20-second access token and a 25-second idle gap exposed an interoperability
  bug: Codex reused its MCP session after refresh, while Termcp rejected the new
  token digest as a foreign credential. The transport now permits an OAuth
  refresh only for the same verified Agent and updates attribution to the
  current digest. A foreign OAuth Agent still receives 403, and static Token
  ownership remains exact-match. The repeated Codex expiry test passed after
  the fix.
- `termcp daemon stdio` with a static Agent Token completed initialization and a
  real `term` call; the live row reported `client_mode=stdio-bridge`. After the
  bridge exited, the auto-spawned daemon followed its 30-second idle countdown
  and stopped with `reason=idle timeout`.
- `termcp daemon stdio sse` also completed a real Codex `term` call, covering
  the legacy SSE side of the same stdio bridge.
- Restarting a password-only vault without automatic unlock made the stdio
  handshake fail until the Admin unlocked configuration encryption. After an
  ENV provider was rebound, auto-spawn succeeded without manual interaction.
- Keep OAuth aliases in separate temporary `CODEX_HOME` directories. With
  `codex-cli 0.166.0`, multiple names pointing at one OAuth URL in the same
  profile produced stale-credential startup failures; an isolated profile was
  deterministic.

### Manual browser walkthrough

The same date's built-in-browser pass used synthetic local passwords and clicked
through first-run setup, login, the workspace, Permissions, Agent creation,
Token issue/rotation, all four Token configuration modes, copy actions, group
binding, OAuth save/disable/re-enable, invalid-URL feedback, and the Permissions
page back-link. The Codex Agent was moved from read-only diagnostics to host
operations through the UI; `term help` remained 200 while `create host` returned
403 under the restricted group.

The walkthrough found that Permissions activity counts did not update when an
external MCP transport attached: `/api/clients` showed one live session while
the page stayed at zero. The page now subscribes to the same authenticated
`/api/ui/ws` sessions frame, refreshes only the volatile principal activity
fields, and updated 0 -> 1 -> 0 when a synthetic OAuth stream attached and was
deleted. The pass also found and fixed a startup console exception caused by the
removed legacy Token form being read unconditionally.

The OAuth page and Agent management view were checked at 390x844 and desktop
width. Controls remained inside their panels, the tab switching and detail
scrolling worked, and no text overlapped. Console checks after the fixes showed
no new warning or error entries.

Focused regression commands:

```sh
go test -race ./internal/mcp \
  -run '^TestOAuthTokenInitializesAttributedMCPConnection$' \
  -count=10 -shuffle=on
go test -race ./internal/mcp \
  -run '^TestManagedMCPTransportCredentials$' \
  -count=5 -shuffle=on
go test -race ./internal/auth \
  -run '^(TestManagedOAuthResourceServer|TestOAuthSettings.*)$' \
  -count=3 -shuffle=on
go test -race ./internal/auth -run '^TestPermissionPage' -count=3 -shuffle=on
```

## 2026-11-10 built-in OAuth integration

The previous 2026-11-08 record and its focused test names describe the removed
external-authorization-server experiment. They are retained as history and are
not compatibility evidence for the current built-in authorization server.

The built-in flow is covered with a temporary, synthetic OIDC provider and a
temporary Termcp data directory. The integration test exercises DCR, browser
authorization with S256 PKCE, OIDC discovery/ID-token/UserInfo validation,
consent, authorization-code exchange, owner-bound Agent creation in the default
group, real mcp-go Streamable HTTP initialization, refresh-token rotation, MCP
session continuation under the same grant/client/resource, and immediate Agent
disable cancellation. Auth-package tests additionally cover CIMD validation and
caching, dynamic loopback callback ports, denied consent, duplicate callbacks,
concurrent code exchange, refresh reuse revocation, dynamic group changes,
default-group isolation, Admin grant/user revocation, restart persistence,
vault locking, non-durable storage failure, and legacy `oauth` key removal.

### Native Codex verification

On 2026-11-11, `codex-cli 0.166.0` was run with an isolated `CODEX_HOME`, a
 temporary Termcp data directory, and the synthetic OIDC provider above:

- `codex mcp add native-e2e --url http://127.0.0.1:18990/stream` was the complete
  client configuration: no bearer token, Agent ID, OAuth client ID, callback or
  registration option was supplied.
- Codex auto-selected CIMD using its public
  `https://chatgpt.com/oauth/codex/client.json` metadata URL. Termcp fetched
  and validated that document, accepted the dynamic loopback callback port,
  completed the OIDC browser transaction, consent and authorization-code/PKCE
  exchange, and Codex reported `Successfully logged in.`
- `codex mcp list --json` reported `transport=streamable_http` and
  `auth_status=o_auth`. Termcp showed one `origin=oauth` Agent named
  `Codex · Native Test User`, owned by the verified OIDC user, bound to
  `preset-no-access`, with one active grant for `client_name=Codex` and the exact
  `/stream` resource.
- A separate isolated run forced `--oauth-client-registration dcr` and completed
  the same login, so both native CIMD and DCR registration paths were exercised.

Automatic access-token refresh inside that Codex version and Claude client
interoperability were not measured in this run. Refresh rotation, MCP session
continuation and disabled-Agent cleanup are covered by the synthetic mcp-go
integration test above.
