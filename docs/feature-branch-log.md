# Termcp feature branch log: encryption to Issue #315

Snapshot date: 2026-11-13.

This log covers the work after the `encryption-feature` tip at `99ce8f8`
through the current Issue #315 commit `2af17fb`.

| Item | Value |
| --- | --- |
| Encryption branch tip | `99ce8f8 fix(webui): refine permission and workspace navigation` |
| Current end point | `2af17fb feat(auth): separate Agent origins and preserve MCP sessions (#315)` |
| Net commits | 4 |
| Net diff | 93 files, 13,144 insertions, 339 deletions |

The diff size includes one OAuth implementation that was later deleted and
replaced. It is therefore not a count of current feature files.

## Starting point at encryption-feature

The branch already contained the encryption and permission foundation:

- opt-in profile encryption and password-derived configuration data;
- encrypted Admin login state, local-state keyring/EDK handling and independent
  login/configuration passwords;
- Agent, permission-group and static MCP Token lifecycle records;
- explicit allow/deny/ask operation policies, host scopes, approvals and audit;
- encrypted persistence that preserves Admin, Agents, groups, Tokens and hosts.

The later OAuth work builds on that state. It does not clear `auth`, recreate the
Admin, rotate host encryption or require a new data directory.

## Commit timeline

### `aba8052` — external OAuth experiment

Added discovery and JWT verification for an externally hosted OAuth
authorization server, encrypted settings in the Admin UI, Agent claims, scopes,
transport ownership and cleanup on configuration changes.

This is historical only. The external-JWT admission path, custom Agent claim,
old settings and compatibility wrappers were removed by `aa5d941`. Current
Termcp does not accept those tokens or expose an external/builtin mode switch.

### `77d1f6c` — client attribution and refresh continuity

Completed the connection and attribution layer:

- verified principal, credential, transport, client mode and connection time are
  exposed without bearer secrets or digests;
- real Streamable HTTP, SSE and stdio-bridge sessions are counted separately
  from requests and background work;
- OAuth refresh can continue a session for the same identity instead of being
  mistaken for a foreign credential;
- Agent management and connection-interface tests were expanded.

The attribution and connection accounting remain. The earlier rule that an
ordinary permission-group change invalidates a session was later replaced by
Issue #315.

### `aa5d941` — built-in OAuth authorization server and OIDC login (#281)

Replaced the unpublished external OAuth experiment with the current built-in
model:

- Termcp is both the MCP resource server and its own OAuth authorization server;
- one Admin-configured standard OIDC provider handles browser identity and
  profile data;
- CIMD and public-client DCR are supported, together with authorization code,
  PKCE S256, exact resource binding, opaque access tokens and refresh rotation;
- the first complete authorization atomically creates an owner-bound Agent in
  the current default group; refresh/reconnect reuses that Agent;
- OAuth users, clients, grants, credentials and revocations are stored in the
  existing encrypted auth state;
- the old external-JWT `oauth` model was removed in favor of `oauth_server`;
- Admin grant/user controls and OAuth consent were added, while existing Admin,
  Agents, groups, static Tokens, hosts and encrypted data were retained.

### `2af17fb` — Agent origins and MCP session lifecycle (#315)

Completed the current Agent-management and session model:

- creation origin is now explicitly `oauth`, `daemon_stdio` or `manual`;
- creation origin, credential kind and current connection method are separate
  dimensions in the data model and UI;
- the local server creates a trusted daemon registration and an owner-only
  credential handoff for `daemon stdio`; it is not accepted from an
  unauthenticated HTTP caller or inferred from headers/`clientInfo`;
- OAuth views show owner, avatar fallback, issuer, client, grants and resource
  without implying that a static Token is required;
- daemon views show the managed registration and credential state without
  exposing a copyable Token;
- manual Agents retain static Token issuance, rotation, history and revocation;
- an OAuth Agent with an additional static Token keeps that real secondary
  credential visible;
- ordinary group/action/constraint/host changes retain the credential, grant and
  MCP transport; the next operation evaluates the current policy;
- lost background work is canceled by its registered operation/host authority,
  rather than every activity of the Agent indiscriminately;
- Streamable HTTP now distinguishes missing sessions (404), real ownership
  conflicts (403) and invalid Bearer credentials (401);
- the stdio bridge reinitializes lost Streamable HTTP and SSE sessions with
  bounded cancellation, but reports requests whose result is unknown without
  replaying commands, keystrokes, writes or deletes;
- OAuth consent callback-origin CSP and borrowed-host authorization fixes are
  included in the same tested delivery;
- mobile permission groups and Agents expand their details inside the selected
  list entry, and the Agent create action shares the search row instead of
  consuming a full-width mobile row.

## Current behavior

### Identity and authorization

- The local Admin remains separate from OAuth Login Users.
- A Login User is identified by stable `(issuer, subject)`; display name/avatar
  are presentation data only.
- An Agent is the execution identity, a Login User is the applicant, and an
  OAuth grant represents one client/resource authorization.
- Agent business permissions come from its explicitly bound permission group.
- Admin group changes affect the next business operation without invalidating a
  still-valid MCP credential/session.

### Credentials

- A manual/Agent static Token remains a separate credential with one current
  Token per Agent and retained history.
- OAuth access is an opaque token backed by encrypted server state; refresh
  tokens rotate and remain bound to Agent, owner, grant, client and resource.
- The local daemon credential is stored in an owner-only data-directory handoff,
  while encrypted auth state stores only the token digest and registration
  relationship.
- OAuth credentials never use the static-Token rotation history or become MCP
  bearer tokens from the upstream OIDC provider.

### Connections and recovery

- Streamable HTTP session ownership is checked against verified
  principal/credential authority.
- OAuth refresh can continue the same grant/client/resource session.
- An unknown session is recoverable with a new initialize; a real session
  ownership mismatch is not.
- Revocation, disabled users/Agents, static-Token replacement and vault locking
  still terminate affected authority.
- The bridge's recovery is bounded and does not replay an operation with unclear
  side effects.

### Management interface

- The Agent page supports source/status filtering, name/user/client search,
  owner and identity-source display, grant inspection, connection counts,
  in-flight requests and background-work counts.
- Empty OAuth views do not claim that a static Token must be issued.
- Manual Token controls remain available where they are meaningful.
- Desktop, narrow mobile navigation and all built-in themes use the existing
  permission-management visual system.

## Removed or superseded behavior

- External JWT access tokens, custom `termcp_agent_id` claims and the external
  OAuth verifier are gone.
- `external`/`builtin` mode selection is gone.
- The legacy `oauth` settings object is not read as current configuration and is
  not written during normal saves.
- Ordinary group changes no longer revoke grants or force clients to
  reinitialize/restart.
- Mobile detail panels no longer detach to the bottom of the page.

## Data and compatibility

- No migration clears the auth object or data directory.
- Admin identity, Login Users, existing Agents, groups, default group, static
  Tokens/history, hosts, keyring and encrypted objects are preserved.
- Old Agents without a recorded origin remain manual; no source is inferred from
  name, namespace, header or `clientInfo`.
- The daemon registration is additive and uses a new registration/token kind.
- The replaced OAuth experiment has no token/configuration compatibility path
  because it was never released.

## Validation summary

- `make test-permissions` passed with auth, authorization, capability, fileop,
  MCP and WebUI under race/shuffle repetition.
- `make test` passed the complete repository with race and shuffle.
- Focused #315 stress passed for auth, MCP and the complete mcpbridge package;
  full configstore and sshconfig stress also passed.
- `go vet ./...`, the `no_webui` build and the no-WebUI docs-only test passed.
- Windows/amd64 auth, MCP and mcpbridge test binaries cross-compiled; native
  Linux/Windows execution remains CI evidence.
- Browser checks covered the three Agent origins, source filtering, inline mobile
  details, compact mobile create/search controls, Chinese/English, built-in
  themes and a 390x844 viewport without page overflow.
- Native `codex-cli 0.166.0` completed URL-only CIMD and DCR OAuth login.
- Native `opencode 1.18.30` completed URL-only DCR/browser authorization,
  connected by URL, invoked `term`, reinitialized after Termcp restart with the
  same credential, retained the session across policy changes, and continued the
  same session after refresh-token rotation.

## Known limits and external checks

- Automated refresh inside a long-running OpenCode process was not observed;
  refresh rotation and same-session continuation were exercised with the
  OpenCode-acquired refresh token.
- No isolated model/provider credential was available for a complete
  `opencode run` LLM tool-call loop. OpenCode's actual stored credential was used
  for real MCP initialize and `term` calls.
- A single combined all-package stress command hit its 30-minute command timeout
  in the Auth CPU-1 Argon2 setup. Package-scoped stress runs above completed;
  the combined command is not reported as a pass.
- Native Windows/Linux execution is delegated to the repository's three-platform
  CI; only cross-compilation was performed locally.

For implementation-level verification details, see
[`agents/mcp-connection-interface-testing.md`](agents/mcp-connection-interface-testing.md)
and [`agents/permissions-testing.md`](agents/permissions-testing.md).
