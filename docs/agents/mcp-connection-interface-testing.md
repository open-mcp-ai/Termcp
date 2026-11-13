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

### 2026-11-11 consent redirect regression

The native CLI run above forwarded the final loopback callback through a local
protocol verifier; it did not prove that a browser could follow the complete
consent redirect chain unaided. A later isolated browser test reproduced the
missing step: the consent POST succeeded with HTTP 302, but `form-action 'self'`
blocked the redirect to the client's different loopback port. The consumed
transaction then produced "OAuth transaction is missing or expired" on retry.

The consent policy now also permits the origin of the already validated
callback for this authorization, using the actual requested port. Query values
and hosts containing CSP separators or wildcards are excluded from the policy. Browser,
CSRF, PKCE, redirect validation and single-use transaction checks still apply.

The built-in browser was used with a temporary encrypted auth store, a synthetic
OIDC provider and an ephemeral client callback listener. Before the fix, the
listener received no callback; after the fix, a single confirmation click
returned an authorization code and the original client state to that listener.
No Google account or production client configuration was used in this test.

`TestBuiltinOAuthRenderedConsentSubmission` parses the real consent HTML,
submits its hidden fields over HTTP with a cookie jar, verifies the callback
policy and authorization response, and rejects a repeated confirmation.
`TestOAuthConsentCSPCallbackOrigin` covers loopback, IPv6 and HTTPS callback
origins, query exclusion, and attempts to inject CSP directives or wildcards.

macOS validation passed: `make test`, `make test-permissions`, focused auth and
oauthserver `go vet`, `go build -tags no_webui ./...`, and
`TestNoWebUIBuildCarriesDocsOnly` with race detection. OAuth-focused stress in
`internal/auth` and `internal/oauthserver` passed with `-race -count=2
-cpu=1,2,4 -shuffle=on`, selecting `TestBuiltinOAuth|TestOAuthConsent|TestCIMD|
TestFetcher|TestRedirect`. The broader auth-package stress run was interrupted
and replaced with that focused run; neither a completed full-package stress
run nor Linux/Windows execution is claimed for this fix. Google login and
OpenCode's real callback still require a fresh user-side authorization after
restarting the updated service.

### 2026-11-11 Agent direct-host authorization regression

An Agent with all-host scope and the required action grants could still receive
"session is outside this Agent's host scope" for `run use:internal`. Admission
treated a borrowed host locator as an existing shell/session handle. After that
was corrected, borrowed command containers also needed the caller's namespace;
an operator-owned or another Agent's container must not be reused.

`TestAgentBorrowedRunWithAllHosts` drives real Streamable HTTP with synthetic
Agent credentials and verifies actual command output for explicit, bare and
quoted host locators, reusing one container per caller while separating the
operator and two Agents. Scope-denial and missing composite grants are checked
before execution. A profile changed after admission is rejected before dialing
for both direct commands and borrowed file transport.

`TestBuiltinOAuthStreamableHTTPInitializesAndRefreshes` now also runs a real
command through the synthetic OIDC/OAuth flow: the default group denies it,
Admin moves that same Agent to an all-host/all-actions group, and the original
OAuth access token succeeds after an MCP reconnect. This path issues no static
Agent token and performs no new browser authorization or refresh after the
group change. Disabling the Agent still rejects the existing OAuth credential.

macOS validation passed: `make test`, `make test-permissions`, a final full
`internal/mcp`/`internal/session` race-and-shuffle run after the binding change,
focused `go vet`, the `no_webui` build and docs-only test, and the release binary
build. MCP-focused stress passed with `-race -count=2 -cpu=1,2,4 -shuffle=on`
for `TestAgentBorrowed|TestBuiltinOAuthStreamableHTTPInitializesAndRefreshes|
TestBorrowedContainerIsPerHostSingleton|TestAuthorizedProfileBindingRejectsContentAndNameReplacement`.
The final rename/binding case was repeated separately with those stress flags.
The integration tests use synthetic identities and local SSH only; no real
Google/OpenCode session or production SSH host was used for this verification.

## 2026-11-11 Issue #315 Agent origins and reconnect

This record uses only synthetic OIDC identities, a temporary encrypted data
directory, an isolated OpenCode config/home and loopback endpoints. It does not
touch the operator's normal OpenCode credentials or Termcp data.

### Browser and Agent management

- A real built-in-browser OIDC login and consent created an OAuth Agent owned by
  the synthetic user. The same page also showed the server-created
  `daemon_stdio` registration and a manual Agent, with no client-supplied field
  used for classification.
- OAuth detail showed owner, avatar fallback, issuer, client, grant/resource,
  validity and live connection state without a static-Token count or issue
  action. daemon detail showed the trusted registration and managed credential
  status without a copy action. Manual detail retained Token issue/rotation.
- The origin filter changed both the list and selected detail when the previous
  selection was filtered out. Search covered Agent name, owner, client and
  identity source.
- Chinese and English were checked at desktop width. `default-light`,
  `default-dark`, `cyberpunk` and `matrix` all rendered the full Agent detail.
  A real Chrome CDP viewport at `390x844` reported
  `scrollWidth - innerWidth = 0`; the stacked list/detail layout was usable.
  Browser console logs were empty after the page interactions.
- A follow-up mobile regression at `390x844` selected a permission group and an
  Agent and asserted that `.detail` / `#agent-detail` were DOM descendants of the
  corresponding expanded list entry. Both entries expanded inline, remained
  scrollable, and kept page overflow at zero. The regression is pinned by
  `TestPermissionMobileDetailsExpandInsideTheirSelectedListEntry`.
- At the same mobile width, Agent search and creation now share one toolbar row:
  the 44px-tall `新增` control is 72px wide beside the search field instead of
  consuming a full-width row. With the previously added notice and filters, the
  visible toolbar reduced to one search/create row and the first Agent began
  sooner in the viewport.

### OpenCode 1.18.30

- The complete config was a remote MCP entry containing only the URL
  `http://127.0.0.1:18990/stream`.
- `opencode mcp auth termcp315` performed DCR, opened the native authorization
  URL, completed the synthetic OIDC login and Termcp consent, and ended with
  `Authentication successful!`.
- `opencode mcp list` reported `termcp315` connected. `opencode mcp debug
  termcp315` reported authenticated, an opaque access token, a refresh token and
  `Connection successful`.
- The access token stored by OpenCode initialized MCP and invoked `term` with
  `help`, receiving `TERM/1 200 OK`.
- Termcp was restarted, its vault unlocked, and a request using OpenCode's token
  plus the old `Mcp-Session-Id` returned 404 with
  `initialize a new session`. A new initialize with the same token returned 200,
  and a new isolated `opencode mcp list` process reported connected without URL
  or credential edits.
- Admin moved the same OAuth Agent to the host-operator group; the original MCP
  session continued. Clearing that group's business policy kept the session but
  made the same `term` call return `TERM/1 403`.
- A refresh using OpenCode's stored refresh token rotated the credential, and
  the new access token continued the same MCP session. The OpenCode CLI process
  itself was not left running long enough to observe its automatic refresh
  timer.

### Automated and stress checks

- `make test-permissions`: passed auth, authorization, capability, fileop, MCP
  and WebUI with `-count=2 -shuffle=on -race`.
- `make test`: full repository passed with `-shuffle=on -race`.
- Focused Issue #315 stress passed auth (150 runs), MCP (84 runs), and the full
  mcpbridge package (240 runs) with `-count=2 -cpu=1,2,4 -shuffle=on -race`.
- Full `configstore` (444 runs) and `sshconfig` (198 runs) stress passed with the
  same flags.
- A single make invocation covering auth, MCP, configstore, sshconfig and bridge
  hit the command's 30-minute timeout while auth was still in CPU-1 Argon2 test
  setup. The MCP half also exposed the pre-existing
  `TestDeadSessionFileAndForwardRejected` retained-tail flake once. That test
  then passed 10 consecutive race/shuffle runs by itself. The package-scoped
  stress runs above are the completed evidence; the single-command run is not
  claimed as passed.
- `go build -tags no_webui ./...`,
  `go test -tags no_webui -count=1 -race -run TestNoWebUIBuildCarriesDocsOnly
  ./internal/webui/`, `go vet ./...` and `git diff --check` passed.
- Windows/amd64 auth, MCP and mcpbridge test binaries cross-compiled
  successfully. Native Windows/Linux execution remains assigned to CI and was
  not run on this macOS host.

### Residual external check

No isolated model/provider credential was available, so `opencode run` could
not be driven end-to-end through an LLM turn. Native OpenCode registration,
consent, credential storage, initialization, `term` invocation with the
OpenCode-acquired token, reconnect after Termcp restart, policy changes and
manual refresh were exercised. Automatic refresh inside a still-running
OpenCode process remains an external client-timing check.
