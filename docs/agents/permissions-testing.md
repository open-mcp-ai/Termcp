# Permission lifecycle test matrix

`#232` uses the normal three-platform CI matrix and adds a focused repeated run
for permission state. The focused command is:

MCP connection attribution and Token lifecycle interface coverage is listed
separately in [mcp-connection-interface-testing.md](mcp-connection-interface-testing.md).

```sh
make test-permissions
```

It runs on Ubuntu, macOS and Windows with `-race`, `-shuffle=on` and
`-count=2`. The focused packages are:

| Package | Required behavior |
| --- | --- |
| `internal/auth` | Login lifetime, group revisions, principal binding, token revocation, audit events and inactive authorization state |
| `internal/authorization` | Explicit allow/deny, constraints, recipes and catalog metadata |
| `internal/capability` | Effective tool arguments and composite operation plans |
| `internal/fileop` | Shared execution/authorization byte evidence |
| `internal/mcp` | Real Streamable HTTP and SSE admission, MCP/DSL equivalence, resource bindings, previews and background cancellation |
| `internal/webui` | Shared themes, authenticated page assets, browser rendering probes and no-WebUI behavior |

The same workflow also runs:

```sh
go test ./... -count=1 -shuffle=on -race -timeout 1800s
go build -tags no_webui ./...
go test -tags no_webui -count=1 -race -run TestNoWebUIBuildCarriesDocsOnly ./internal/webui/
```

Browser checks use a temporary data directory and synthetic credentials. They
must not use the operator's real data directory or account. The browser contract
covers setup/login, `/permissions`, all built-in themes, language switching,
desktop and narrow layouts, preview rendering, and the absence of CSP or console
errors.

Before a release, also run on an idle local machine:

```sh
make test-stress STRESS_PKGS="./internal/auth ./internal/mcp ./internal/configstore ./internal/sshconfig"
```

CI execution and a local run are separate evidence. A platform not actually run
must be reported as unverified; compilation alone is not a substitute for that
platform's tests.

## 2026-11-02 verification record

- macOS: `make test` passed for the complete repository with race and shuffle.
- macOS: `make test-permissions` passed twice for auth, authorization, capability, fileop, MCP, and WebUI.
- macOS: single-CPU stress passed for auth, MCP, configstore, sshconfig, and extensions.
- macOS: the real `/permissions` page was exercised in the built-in browser with a temporary encrypted data directory and synthetic Admin credentials. The preview reported missing `shell.input`, `shell.key`, and `shell.output`, and did not create a session.
- Linux/amd64 and Windows/amd64: permission-package test binaries cross-compiled with `CGO_ENABLED=0`. Native execution is assigned to the three-platform CI matrix and was not available in the local macOS run.

## 2026-11-11 Issue #315 verification record

- macOS: `make test-permissions` passed with
  `-count=2 -shuffle=on -race` for auth, authorization, capability, fileop, MCP
  and WebUI.
- macOS: `make test` passed the full repository with race and shuffle.
- The ordinary group/action/constraint/host tests now assert that the admitted
  request and MCP transport survive while the live policy resolver changes.
  daemon and static Token transports are covered alongside OAuth.
- Background work records its operations and stable hosts; a test removes an
  unrelated operation and verifies that work continues, then removes the
  operation it actually exercises and verifies cancellation.
- Focused auth/MCP Issue #315 stress passed with `-count=2 -cpu=1,2,4
  -shuffle=on -race`. Full mcpbridge, configstore and sshconfig stress also
  passed with those flags. A combined all-package stress command reached its
  30-minute timeout in CPU-1 Argon2 setup; it is not reported as a pass.
- Streamable HTTP and SSE bridge recovery tests verify bounded reinitialization
  and that the request which observed the lost session returns
  `result=unknown` with `request_was_replayed=false`.
- Windows/amd64 auth, MCP and mcpbridge test binaries cross-compiled. Native
  Windows and Linux execution is delegated to the normal three-platform CI
  matrix.
