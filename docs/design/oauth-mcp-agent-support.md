# OAuth-protected remote MCP server support

Research date: 2026-11-07

Scope: mainstream MCP hosts and clients, including OpenAI products. This note
uses only vendor-owned documentation, release notes, source repositories, and
the Model Context Protocol specification. Features not established by those
sources are marked `Unknown`.

Classification:

- `Supported`: the vendor documents an OAuth sign-in flow for remote MCP
  servers.
- `Partial`: OAuth works only on some transports or surfaces, or requires an
  external bridge.
- `Unsupported`: the vendor explicitly says the feature is unavailable.
- `Unknown`: the allowed primary sources do not establish support.

The current stable MCP revision is `2026-07-28`. Several clients still document
OAuth against the earlier `2025-11-25` revision. The feature matrix records what
each client documents or implements; it does not assert full conformance with
the newest revision.

## Termcp current status

Termcp now implements the first OAuth resource-server slice for external
authorization servers: Protected Resource Metadata, an OAuth 401 challenge,
external JWT verification through JWKS, and `termcp_agent_id` claim mapping.
There is still no Termcp-owned OAuth Authorization Server, `/authorize`,
`/token`, refresh-token issuance, CIMD, or DCR endpoint; client registration and
refresh stay with the external authorization server. The managed mode continues
to have a local Admin login, browser JWT sessions, Origin/CSRF checks, static
Agent Bearer tokens, permission groups, and Token rotation/expiry/revocation.
See [auth scope](auth-scope-2026-11-05.md), the OAuth resource-server design in
[oauth-resource-server.md](oauth-resource-server.md), and the current
authentication wrapper in [managed_http.go](../../internal/auth/managed_http.go).

The most important design distinction is that an MCP HTTP server is normally an
OAuth **resource server**: it publishes protected-resource metadata, points
clients at an authorization server, validates an access token, and enforces
resource/audience/scopes. It does not have to implement `/authorize`, `/token`,
or Dynamic Client Registration itself. A separate authorization-server
implementation is only needed if Termcp is intended to be the identity provider.
Termcp also needs a stable external resource URL for discovery; the current
scope deliberately defers canonical-origin and trusted-proxy configuration.

## Comparison

### Support and transports

| Host/client | Classification | Remote transports | Main caveat | Version/date context |
| --- | --- | --- | --- | --- |
| OpenAI ChatGPT / Codex (CLI, IDE, desktop) | **Supported** | Streamable HTTP | ChatGPT web uses plugins/connectors; local Codex configuration is shared by the desktop app, Codex CLI, and IDE extension. The API's `authorization` field takes an already-obtained OAuth access token rather than running the host browser flow. | OpenAI docs updated 2026-11-07. [S41][S42][S43] |
| Anthropic Claude Desktop | **Supported** | Streamable HTTP preferred; legacy HTTP+SSE supported | Remote connectors are brokered through Anthropic's cloud, even in Desktop. Private-network servers need a public tunnel or Anthropic egress allowlisting. | Anthropic connector authentication page updated 2026-11-05; custom-connector help article current in November 2026. [S2][S3][S4] |
| Anthropic Claude Code | **Supported** | Streamable HTTP; legacy SSE; WebSocket has no OAuth | OAuth sign-in requires an interactive session. A non-interactive `claude -p` or Agent SDK run cannot start the browser flow. HTTP-first automatic SSE fallback requires v2.1.265 or later. | Current docs on 2026-11-07 include fixes and behavior through Claude Code v2.1.299. [S5][S6] |
| Cursor | **Supported** | Streamable HTTP and SSE | Cursor Cloud Agents support custom MCP servers over HTTP or stdio but explicitly do not support SSE or `mcp-remote`. Redirect URIs are fixed by surface. | Current docs on 2026-11-07. OAuth support was announced in Cursor 1.0 on 2025-06-04. [S7][S8][S9] |
| VS Code + GitHub Copilot Chat (IDE) | **Supported** | Streamable HTTP preferred; SSE fallback | GitHub Copilot cloud agent and Copilot code review do not support remote MCP servers using OAuth for repository-configured servers. The Copilot harness also does not forward VS Code `${input:...}` secrets into HTTP headers. | VS Code docs current 2026-11-07; source pinned to `microsoft/vscode@61c77e9`. CIMD support was added in VS Code 1.106. [S10][S11][S12][S13][S14][S14a][S14b] |
| Windsurf | **Supported** | Streamable HTTP by default; falls back to SSE on non-auth 4xx errors | The current vendor docs use the new product name Devin Desktop and redirect `docs.windsurf.com` there. Discovery paths and PKCE are not documented publicly. | Devin Desktop documentation updated 2026-11-06. [S15][S16] |
| Cline | **Supported** | `streamableHttp` and legacy SSE | Omitting `type` defaults to legacy SSE. Streamable HTTP must use the exact `streamableHttp` spelling. A static `Authorization` header must be removed before starting OAuth. | Repository snapshot 2026-11-06; VS Code extension 4.1.26 and CLI 3.0.74. [S17][S18][S19][S20] |
| Continue | **Partial** | SSE and Streamable HTTP for connections; native OAuth is wired only for SSE | The IDE path does not attach an OAuth provider to its Streamable HTTP transport. The CLI falls back to `npx -y mcp-remote` after a 401 in interactive mode; headless mode does not use that fallback. | Latest inspected `main` commit is 2026-07-20; VS Code extension package version is 1.3.40. Current user docs do not describe OAuth. [S21][S22][S23][S24][S25][S26] |
| JetBrains AI Assistant (IDE) | **Unknown** | Streamable HTTP and legacy SSE | OAuth, authentication headers, and static bearer setup are absent from the official AI Assistant MCP page. Transport support is explicit; OAuth support is not established. | AI Assistant 2026.2 help. [S27] |
| JetBrains Junie | **Partial** | Junie CLI documents remote HTTP/HTTPS; SSE support is not documented | OAuth is documented for Junie CLI, not for the standalone Junie IDE plugin. The IDE plugin page still says security tokens are not supported in MCP configs. | Junie docs dated 2026-11-06. [S28][S29] |
| Zed | **Supported** | Streamable HTTP | Current source has stdio and HTTP context servers, not a legacy HTTP+SSE client transport. Zed suppresses OAuth when a static `Authorization` header is configured. | Source pinned to `zed-industries/zed@9a8ab54` dated 2026-11-06. Pre-registered OAuth clients were added by stable 0.212.7. [S30][S31][S32][S33] |
| Google Gemini CLI | **Supported** | Streamable HTTP and SSE | OAuth requires a local browser and a loopback callback; it does not work in headless, remote-SSH, or browserless container environments. OAuth discovery to private networks is blocked by default. | Source pinned to `google-gemini/gemini-cli@62bab5b` dated 2026-11-06; package version `0.69.0-nightly.20261104`. [S34][S35][S36][S37] |
| GitHub Copilot CLI | **Supported** | HTTP (`streamable-http` alias) and legacy SSE | Expired OAuth credentials can require `/mcp auth <server>`. Custom HTTP/SSE MCP server URLs cannot contain query parameters. | GitHub Docs current on 2026-11-07; no CLI version is published on the cited pages. [S38][S39][S40] |

### OAuth feature details

| Host/client | Discovery and metadata | Client registration | Authorization code + PKCE | Token refresh | Custom headers and static bearer |
| --- | --- | --- | --- | --- | --- |
| OpenAI ChatGPT / Codex | **Yes.** OpenAI host discovers the MCP protected resource and authorization server; the server must publish protected-resource metadata, normally at `/.well-known/oauth-protected-resource`, and return a `WWW-Authenticate` challenge. | **Yes.** CIMD preferred, DCR supported, and pre-registered client IDs are supported; the exact callback is surface-specific. | **Yes.** The host performs browser authorization-code flow with an OAuth 2.1/PKCE flow; issuer validation uses RFC 9207 when advertised. | **Host-managed.** Stored MCP OAuth credentials are used by Codex for Streamable HTTP; the API caller remains responsible for obtaining and supplying an access token. Exact refresh timing is not documented for every surface. | **Yes.** Codex supports bearer-token environment variables and static HTTP headers; ChatGPT plugins use the host OAuth connection rather than local headers. |
| Claude Desktop | **Yes.** Requires a 401; uses a `WWW-Authenticate` `resource_metadata` pointer when present, then path/root PRM well-known locations. Uses the first PRM `authorization_servers` entry, then RFC 8414 or OIDC metadata. | **Yes.** DCR, CIMD, Anthropic-held client credentials, and customer-entered OAuth client ID/secret. CIMD is selected only when advertised appropriately; otherwise Claude falls back to DCR. | **Yes, S256.** Claude sends a PKCE challenge on every authorization request. | **Yes.** Refreshes proactively up to five minutes before expiry and reactively after a 401. | **Yes.** Fixed API key or bearer token in request headers. Static headers are beta and available to a limited set of organizations; common auth header names work. |
| Claude Code | **Yes.** PRM first, then RFC 8414 metadata fallback. `authServerMetadataUrl` can override discovery with an HTTPS metadata URL. | **Yes.** DCR by default, automatic CIMD, or pre-registered client ID plus optional secret. | **Yes.** Anthropic documents S256 PKCE as the shared connector infrastructure used by Claude Code. | **Yes.** Stored tokens are refreshed automatically; `offline_access` is added when advertised. | **Yes.** Static `headers`, `--header`, and dynamic `headersHelper`. Setting `Authorization` selects static authentication and disables OAuth fallback for that server. |
| Cursor | **Yes.** Reads RFC 9728 PRM to locate the authorization server and resource indicator, and reads authorization-server metadata for scopes. | **Yes.** DCR as a public client, or a static pre-registered client ID and optional secret under `auth`. CIMD is not documented. | **Yes.** Cursor documents authorization code flow with PKCE. | **Yes.** Renews expired access tokens with the issued refresh token. Without a refresh token, the user signs in again. | **Yes.** Arbitrary `headers`, including static `Authorization: Bearer ...`, and environment interpolation. |
| VS Code + GitHub Copilot Chat | **Yes.** PRM, RFC 8414 authorization-server metadata, and OIDC discovery are implemented in VS Code OAuth code. | **Yes.** DCR first; CIMD when supported; pre-registered `oauth.clientId` and secret in VS Code secret storage. | **Yes, S256.** The built-in OAuth implementation sends `code_challenge` and `code_challenge_method=S256`. | **Yes.** Refreshes tokens expiring within five minutes and retries on 401 where credentials remain refreshable. | **Yes.** Arbitrary HTTP `headers` and static bearer values. Environment references work. `${input:...}` and VS Code `SecretStorage` values are not forwarded in Copilot harness sessions. |
| Windsurf | **Unknown.** The current docs require OAuth for remote servers but do not publish PRM/AS metadata paths or ordering. | **Yes.** DCR is automatic for most servers; `oauthClientId` and optional `oauthClientSecret` configure pre-registration. CIMD is not documented. | **Unknown.** The primary docs do not state the authorization-code/PKCE exchange details. | **Yes.** The docs state OAuth tokens are stored locally and refreshed automatically. | **Yes.** `headers` supports fixed credentials. OAuth client ID/secret are separate from request credentials. |
| Cline | **Yes, through the MCP SDK.** Cline persists discovery state and delegates the full flow to the MCP SDK. | **Yes.** DCR when no client is stored, or `oauthClient: { clientId, clientSecret }` pre-registration. CIMD is not documented. | **Yes.** Cline's OAuth provider stores the PKCE verifier and delegates code exchange to the MCP SDK. | **Yes.** The provider returns expired tokens with a refresh token so the MCP SDK refreshes and saves the new token. | **Yes.** `headers`, including `Authorization: Bearer ...`. A static Authorization header is mutually exclusive with starting OAuth. |
| Continue | **Partial / source-level.** The SSE path calls MCP SDK `auth()`, which implements PRM and RFC 8414/OIDC discovery. The host docs do not describe this. | **Partial.** MCP SDK DCR can run when the provider has no client information, but Continue exposes no documented pre-registration configuration. | **Yes.** The provider implements the PKCE code-verifier callbacks and calls MCP SDK `auth()`. | **Unknown.** The provider advertises `refresh_token`, but the SSE connection path reads only the stored access token and the docs do not establish automatic refresh. | **Yes.** `requestOptions.headers` provides arbitrary headers; `apiKey` becomes `Authorization: Bearer <apiKey>`. |
| JetBrains AI Assistant | **Unknown.** No OAuth discovery behavior is documented. | **Unknown.** | **Unknown.** | **Unknown.** | **Unknown.** The official page does not document request-header or static-token authentication. |
| JetBrains Junie | **Unknown.** The CLI docs show OAuth status and browser authorization but do not describe discovery metadata. | **Unknown.** DCR and pre-registration are not documented for Junie. | **Unknown.** PKCE is not stated. | **Unknown.** Expiry results in an authorization-required state; refresh is not documented. | **Yes for Junie CLI.** Remote configs accept `headers`, including `Authorization: Bearer ...`. The standalone IDE plugin page still says security tokens are not supported. |
| Zed | **Yes.** Parses `WWW-Authenticate`, fetches path-specific or root PRM, then RFC 8414 or OIDC metadata. Validates issuer and resource. | **Yes.** CIMD first, DCR fallback, or pre-registered `oauth.client_id` plus optional secret. | **Yes, S256.** Zed generates a verifier and S256 challenge and sends RFC 8707 `resource`. | **Yes.** Persists token expiry, refreshes before use or after a 401, and stores rotated refresh tokens. | **Yes.** Arbitrary `headers`, including static `Authorization: Bearer ...`; a static Authorization header bypasses the OAuth flow. |
| Google Gemini CLI | **Yes.** Handles a 401 `WWW-Authenticate` challenge or PRM well-known discovery, follows the first authorization server, and discovers RFC 8414/OIDC authorization-server metadata. | **Yes.** DCR if `registration_endpoint` is advertised, or configured client ID and optional secret. CIMD is not documented. | **Yes.** Source implements authorization code plus PKCE and sends RFC 8707 `resource`. | **Yes.** Refreshes expired access tokens when a refresh token is available. | **Yes.** Arbitrary `headers`, including bearer or API-key headers. |
| GitHub Copilot CLI | **Partial.** Docs say it reads `WWW-Authenticate` scopes and a discovered token endpoint, but do not name the exact PRM/RFC 8414/OIDC sequence. | **Yes.** DCR by default; `oauthClientId` and optional stored secret skip DCR. CIMD is not documented. | **Yes for the default browser flow.** The docs state that `client_credentials` skips the authorization-code browser flow and PKCE, implying the default grant uses PKCE. | **Unknown.** Docs cover `/mcp auth` after expiry but do not state whether a refresh token is used first. | **Yes.** Arbitrary `headers`, static bearer tokens, and `oidc: true` Bearer injection for remote servers. It also supports a non-interactive `client_credentials` grant. |

## Standards conformance

The MCP `2026-07-28` authorization profile is HTTP-only and remains optional,
but HTTP transports `SHOULD` conform. The largest interoperability differences
against the current specification are:

- **Discovery is normative.** MCP clients must use RFC 9728 Protected Resource
  Metadata and support both RFC 8414 OAuth Authorization Server Metadata and
  OpenID Connect Discovery. Claude Desktop, Claude Code, VS Code, Zed, and
  Gemini CLI document this full chain. GitHub Copilot CLI and Continue document
  or implement only part of it; Windsurf and both JetBrains surfaces do not
  publish enough information to verify it.
- **CIMD is now the preferred client identity.** MCP `2026-07-28` makes Client
  ID Metadata Documents a `SHOULD`, permits pre-registration, and deprecates
  DCR as a compatibility fallback. Claude Desktop, Claude Code, VS Code, and
  Zed document CIMD. Cursor, Windsurf, Cline, Gemini CLI, and GitHub Copilot CLI
  rely primarily on DCR plus pre-registration.
- **Authorization code with S256 PKCE is baseline.** Claude Desktop, Claude
  Code, Cursor, VS Code, Cline, Continue, Zed, Gemini CLI, and the default
  GitHub Copilot CLI flow establish it. Windsurf and JetBrains do not publish
  enough detail.
- **Resource indicators and issuer validation are safety-critical.** Zed and
  Gemini source explicitly send RFC 8707 `resource`; Claude and Cursor document
  it. The current MCP revision also requires issuer validation from RFC 9207.
  Most host docs cite older revisions, so their current conformance level
  should be verified by interoperability tests rather than inferred from
  marketing support.
- **Refresh tokens remain optional.** Claude Desktop, Claude Code, Cursor,
  VS Code, Windsurf, Cline, and Zed document refresh behavior. GitHub Copilot
  CLI, Continue, and Junie do not provide enough evidence; `Unknown` is safer
  than assuming refresh from the presence of an OAuth flow.
- **Streamable HTTP is the portable transport.** SSE support is widespread but
  legacy. Zed is the clearest HTTP-only case; Continue's native OAuth is
  SSE-only; Cline defaults omitted types to SSE; Cursor Cloud Agents exclude
  SSE. A remote MCP implementation should expose Streamable HTTP first and
  treat SSE as a compatibility endpoint.
- **Static bearer support remains the universal escape hatch.** Every client
  except the undocumented JetBrains AI Assistant path provides a documented
  way to set an `Authorization` header. Claude Desktop's fixed-header mode is
  beta and limited by organization.

## Sources

All links are primary sources owned by the named vendor, the MCP project, or
the applicable official SDK project. Source links for moving repositories are
pinned to the commit inspected on 2026-11-07 where possible.

### OpenAI

1. [S41] [OpenAI Docs: Model Context Protocol](https://learn.chatgpt.com/docs/extend/mcp)
2. [S42] [OpenAI Docs: Plugin authentication](https://developers.openai.com/plugins/build/auth)
3. [S43] [OpenAI API: MCP tools and authorization](https://developers.openai.com/api/docs/guides/tools-connectors-mcp)

### MCP

1. [S1] [Model Context Protocol specification 2026-07-28: Authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
2. [S1a] [Model Context Protocol: The 2026-07-28 specification release](https://blog.modelcontextprotocol.io/posts/2026-07-28/)

### Anthropic

3. [S2] [Authentication for Claude connectors](https://claude.com/docs/connectors/building/authentication) - updated 2026-11-05.
4. [S3] [Build an MCP server for Claude](https://claude.com/docs/connectors/building/index) - transport and surface requirements.
5. [S4] [Get started with custom connectors using remote MCP](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp) - Claude Desktop/cloud-brokered connection behavior.
6. [S5] [Connect Claude Code to tools via MCP](https://code.claude.com/docs/en/mcp) - current reference, including Claude Code v2.1.299 behavior.
7. [S6] [Authentication for Claude connectors: Claude Code callback requirements](https://claude.com/docs/connectors/building/authentication#callback-urls)

### Cursor

8. [S7] [Cursor: Model Context Protocol](https://cursor.com/docs/mcp) - current OAuth, registration, refresh, headers, and transport reference.
9. [S8] [Cursor Cloud Agent capabilities](https://cursor.com/docs/cloud-agent/capabilities) - HTTP-only remote limitation for cloud agents.
10. [S9] [Cursor 1.0 changelog: one-click MCP and OAuth](https://cursor.com/changelog/1-0)

### Visual Studio Code and GitHub Copilot

11. [S10] [VS Code MCP configuration reference](https://code.visualstudio.com/docs/agents/reference/mcp-configuration)
12. [S11] [VS Code MCP extension API guide](https://code.visualstudio.com/api/extension-guides/ai/mcp)
13. [S12] [VS Code 1.106 release notes: remote MCP CIMD support](https://code.visualstudio.com/updates/v1_106)
14. [S13] [VS Code OAuth implementation, commit 61c77e9](https://github.com/microsoft/vscode/blob/61c77e9be5dd33cf5faecffca2a021b15013d36e/src/vs/base/common/oauth.ts)
15. [S14] [VS Code dynamic auth provider, commit 61c77e9](https://github.com/microsoft/vscode/blob/61c77e9be5dd33cf5faecffca2a021b15013d36e/src/vs/workbench/api/common/extHostAuthentication.ts)
16. [S14a] [GitHub Docs: MCP behavior across Copilot surfaces](https://docs.github.com/en/copilot/concepts/customizations/mcp)
17. [S14b] [GitHub Docs: Extending Copilot Chat with MCP servers](https://docs.github.com/en/copilot/how-tos/copilot-in-your-ide/customize-copilot/extend-copilot-with-tools-and-context/extend-copilot-chat-with-mcp)

### Windsurf / Devin Desktop

18. [S15] [Devin Local customizations and MCP servers](https://docs.devin.ai/desktop/devin-local/customizations) - page modified 2026-11-06; reached by redirect from the former Windsurf MCP URL.
19. [S16] [Devin CLI MCP configuration](https://docs.devin.ai/cli/extensibility/mcp/configuration) - the current agent harness shared by Devin Local.

### Cline

20. [S17] [Cline MCP overview and configuration](https://github.com/cline/cline/blob/94eecbbe1a2f899c66b118d13656651b0a884e1f/docs/mcp/mcp-overview.mdx)
21. [S18] [Cline OAuth provider integration](https://github.com/cline/cline/blob/94eecbbe1a2f899c66b118d13656651b0a884e1f/apps/vscode/src/services/mcp/McpOAuthManager.ts)
22. [S19] [Cline core OAuth orchestration](https://github.com/cline/cline/blob/94eecbbe1a2f899c66b118d13656651b0a884e1f/sdk/packages/core/src/extensions/mcp/oauth.ts)
23. [S20] [Cline CLI changelog](https://github.com/cline/cline/blob/94eecbbe1a2f899c66b118d13656651b0a884e1f/apps/cli/CHANGELOG.md) - current package context includes 3.0.74.

### Continue

24. [S21] [Continue MCP documentation](https://docs.continue.dev/customize/deep-dives/mcp)
25. [S22] [Continue OAuth provider source](https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/core/context/mcp/MCPOauth.ts)
26. [S23] [Continue MCP connection source](https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/core/context/mcp/MCPConnection.ts)
27. [S24] [Continue CLI MCP fallback source](https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/extensions/cli/src/services/MCPService.ts)
28. [S25] [Continue config reference](https://docs.continue.dev/reference) - `requestOptions.headers` and remote MCP fields.
29. [S26] [MCP TypeScript SDK v1.25.2 OAuth client](https://github.com/modelcontextprotocol/typescript-sdk/blob/v1.25.2/src/client/auth.ts) - pinned dependency used by Continue `core/package.json`.

### JetBrains

30. [S27] [JetBrains AI Assistant MCP documentation](https://www.jetbrains.com/help/ai-assistant/mcp.html) - AI Assistant 2026.2 help.
31. [S28] [Junie CLI MCP configuration and OAuth](https://junie.jetbrains.com/docs/junie-cli-mcp-configuration.html) - dated 2026-11-06.
32. [S29] [Junie IDE plugin MCP configuration](https://junie.jetbrains.com/docs/junie-ide-plugin.html) - IDE-specific authentication caveat.

### Zed

33. [S30] [Zed MCP documentation](https://zed.dev/docs/ai/mcp)
34. [S31] [Zed OAuth implementation, commit 9a8ab54](https://github.com/zed-industries/zed/blob/9a8ab54db4f9f9af4dc6efb42b304c55c6a62bd3/crates/context_server/src/oauth.rs)
35. [S32] [Zed context-server OAuth flow, commit 9a8ab54](https://github.com/zed-industries/zed/blob/9a8ab54db4f9f9af4dc6efb42b304c55c6a62bd3/crates/project/src/context_server_store.rs)
36. [S33] [Zed 0.212.7 release notes: OAuth client pre-registration](https://zed.dev/releases/stable/0.212.7)

### Google Gemini CLI

37. [S34] [Gemini CLI MCP documentation, commit 62bab5b](https://github.com/google-gemini/gemini-cli/blob/62bab5b232c15b925d31e655ce844ab939c882b7/docs/tools/mcp-server.md)
38. [S35] [Gemini CLI OAuth utilities, commit 62bab5b](https://github.com/google-gemini/gemini-cli/blob/62bab5b232c15b925d31e655ce844ab939c882b7/packages/core/src/mcp/oauth-utils.ts)
39. [S36] [Gemini CLI OAuth provider, commit 62bab5b](https://github.com/google-gemini/gemini-cli/blob/62bab5b232c15b925d31e655ce844ab939c882b7/packages/core/src/mcp/oauth-provider.ts)
40. [S37] [Gemini CLI configuration reference, commit 62bab5b](https://github.com/google-gemini/gemini-cli/blob/62bab5b232c15b925d31e655ce844ab939c882b7/docs/reference/configuration.md)

### GitHub Copilot CLI

41. [S38] [Adding MCP servers for GitHub Copilot CLI](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers)
42. [S39] [GitHub Copilot CLI command and MCP configuration reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference)
43. [S40] [GitHub Docs: MCP concepts and surface limitations](https://docs.github.com/en/copilot/concepts/customizations/mcp)
