# Security Policy

## Reporting a Vulnerability

If you find a security issue in termcp, please report it privately:

- **GitHub Security Advisory** (preferred):
  https://github.com/open-mcp-ai/termcp/security/advisories/new
- We acknowledge receipt within 48 hours and aim to provide a fix or mitigation
  timeline within 7 days.
- Please do not open a public issue until the report has been acknowledged and
  a coordinated disclosure timeline has been agreed upon.

## Supported Versions

| Version | Supported |
|---|---|
| latest release | :white_check_mark: |
| older releases | :x: (please upgrade) |

## Environment Variables & Secrets Handling

Termcp reads five environment variables at startup. None of them are logged,
serialized, persisted to disk, or sent across the network.

| Variable | Classification | Purpose & Scope |
|---|---|---|
| `TERMCP_AUTH_TOKEN` | **Secret** | Plaintext HTTP bearer token guarding the HTTP/WS surface (`Authorization: Bearer <token>`). Kept in memory only; never written to any file or log. Checked in constant time (`crypto/subtle.ConstantTimeCompare`). Mutually exclusive with `TERMCP_AUTH_HASH`. |
| `TERMCP_AUTH_HASH` | **Config** | Salted SHA-256 verifier (`sha256-<salt>-<digest>`) generated via `termcp --gen-auth-hash`. Storing the hash lets deployments avoid cleartext tokens in configuration. Accepting the hash string itself as a credential allows operators holding only the hash to drive `termcp daemon` and `termcp stdio`. Mutually exclusive with `TERMCP_AUTH_TOKEN`. |
| `TERMCP_DISABLE_AUTH_TOKEN` | **Config** | Explicit escape hatch (`1`/`true`/`yes`/`on`) to disable HTTP authentication. Intended strictly for loopback-only deployments where reachability is already the security boundary (local single-user machines, isolated dev containers). When set, non-loopback bindings are rejected unless explicitly allowed. **Deliberate failure mode**: setting both a credential (`TERMCP_AUTH_TOKEN` or `TERMCP_AUTH_HASH`) and `TERMCP_DISABLE_AUTH_TOKEN` is treated as a configuration error and refuses to start — credentials never silently lose to a disable flag. |
| `TERMCP_DATA_DIR` | **Config** | Filesystem path overriding `~/.termcp` for persistent state (sessions, SSH host profiles, logs). Stays local to the machine. |
| `TERMCP_ASSETS_DIR` | **Config** | Filesystem path overriding embedded Web UI assets (`~/.termcp/assets`). Stays local to the machine. |

## Subprocess & Credential Isolation

- When running the background daemon (`termcp daemon start`), auth credentials
  are passed via process environment (`EnvAuthToken` / `EnvAuthHash` /
  `EnvDisableAuth`) to the spawned child, never via command-line arguments,
  so they do not appear in `ps` output.
- SSH private keys configured for host profiles are read locally and held in
  memory only during active connections; they are never relayed over the MCP
  channel.
- Public documentation endpoints (`/api.md`, curl skill) do not require
  authentication and carry no data or credentials.

## Vendored Dependencies

`third_party/xterm/` contains upstream xterm.js build artifacts needed for the
self-contained Web UI: two runtime files, embedded by name from
`third_party/xterm/embed.go`, plus the provenance (`README.md`) and licence
(`NOTICE`) that stay in the repository. See [`docs/xterm.md`](docs/xterm.md) for
upstream versions and SHA-256 checksums to verify they are unmodified, and
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) for the attribution.
