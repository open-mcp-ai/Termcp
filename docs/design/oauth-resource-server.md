# OAuth resource server for Claude and Codex

Status: first resource-server slice implemented on 2026-11-07.

This is intentionally not a Termcp-owned identity provider. Termcp validates
access tokens issued by an external OAuth 2.1/OIDC authorization server. Client
registration, CIMD or DCR, browser authorization, refresh-token storage, and
token refresh remain the client/authorization-server responsibilities.

## Configuration

Open **Permissions → OAuth connections** (`/permissions` → `/oauth`) after
signing in and unlocking configuration encryption. Enable OAuth, fill the three
addresses below, and save. No OAuth startup flags or environment variables are
needed. Saving applies immediately; the shared encrypted account state retains
these settings across restarts and in containers using the existing data volume.

| Field | Meaning |
| --- | --- |
| Authorization server URL | Exact issuer supplied by the external authorization service |
| Public Termcp URL | Stable address clients reach, including any reverse-proxy prefix |
| Signing-key URL (JWKS) | Public signing-key document supplied by the authorization service |
| Advanced: Agent identity claim | Claim containing an existing Agent ID; default `termcp_agent_id` |
| Advanced: Required scopes | Space-separated scopes; default `mcp`; empty skips scope enforcement |

Remote addresses require HTTPS; HTTP is accepted only for loopback addresses.
URLs must not contain credentials, queries, or fragments. Termcp never derives
the issuer or public resource from an untrusted `Host` or forwarded header.
The page shows the saved MCP URL for the client once OAuth is enabled.

Changing or disabling the saved configuration ends existing OAuth requests,
background work, and MCP connections. Clients reconnect under the new
configuration; static MCP tokens and browser logins keep working. Disabling
OAuth retains the addresses so it can be enabled again without retyping them.

For administrative API clients, `GET /auth/oauth` returns `settings`; `PUT
/auth/oauth` replaces those settings using the returned `revision`. Both
require an unlocked Admin browser session, and writes require the usual CSRF
header. A stale revision returns 409. Only public configuration values are
returned, never access tokens or private keys.

The authorization server must issue tokens with:

- `iss` exactly equal to the configured issuer;
- `aud` or `resource` equal to the public MCP endpoint or configured resource
  base;
- `exp`, and optionally standard `nbf`/`iat`;
- `scope` containing `mcp`, or the configured scope;
- `termcp_agent_id` containing the stable ID of an enabled Termcp Agent.

The Agent ID is copied from the Agent detail in the permissions page. The
external identity provider must not put a display name or a client-generated
namespace in that claim.

## Discovery contract

Termcp serves RFC 9728 protected-resource metadata at the root and the
path-specific well-known locations:

```text
/.well-known/oauth-protected-resource
/.well-known/oauth-protected-resource/stream
/.well-known/oauth-protected-resource/sse
```

The response points clients to the external issuer and advertises the `mcp`
scope and bearer header method. Unauthorized MCP requests return an RFC 6750
challenge with `resource_metadata`, for example:

```text
WWW-Authenticate: Bearer resource_metadata="https://termcp.example.com/.well-known/oauth-protected-resource/stream", scope="mcp", error="invalid_token"
```

Claude and Codex should be configured with the public Streamable HTTP endpoint:

```text
https://termcp.example.com/stream
```

The external authorization server must support the client registration and
callback flow required by the selected host. Claude and Codex can use DCR or
CIMD depending on their current version and account policy.

## Security boundary

- OAuth access tokens are accepted only at the core MCP surfaces: `/stream`,
  `/sse`, `/message`, and the minimal `/auth/mcp` probe. Extension-stream OAuth
  is deferred and continues to use the existing static/Agent-token policy.
- They do not grant WebUI, REST, authentication-management, or daemon control
  access.
- Agent identity comes only from the verified JWT claim, never from
  `X-Termcp-Namespace`, `clientInfo`, or a request header.
- Issuer, audience/resource, scope, expiry, signature, and enabled-Agent checks
  are fail-closed.
- Raw access tokens and JWKS material are never returned by listing APIs.

## Deliberately deferred

- Termcp-owned `/authorize`, `/token`, refresh-token issuance, and consent UI;
- server-side CIMD or Dynamic Client Registration;
- opaque-token introspection (JWKS-signed JWT is the first supported profile);
- multi-user login, RoleBinding, AgentGrant, and OIDC browser sessions.

## Tests

The focused tests cover metadata discovery, 401 challenges, valid and invalid
JWTs, wrong issuer/audience/Agent/scope, expiry, JWKS key rotation, MCP
initialization, connection attribution, Agent-disable cleanup, persisted UI settings,
configuration revisions, and invalidation during an in-flight JWKS fetch.
