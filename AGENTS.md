## Security & Privacy

禁止在项目中出现以下敏感信息和敏感文件：

- 本机 hostname、本地用户名称
- 真实密钥、API keys、tokens、passwords
- 邮箱地址、手机号等个人信息
- .env 文件、私钥文件（PEM、SSH key 等）
- 禁止出现 `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>` 相关字样
- 只有在人工同意后才能push和commit。在多次commit后，人工要求push再进行push，绝对不要主动push。

## Repo shape

Single Go module (`github.com/open-mcp-ai/termcp`, Go 1.25), pure Go / no CGO
except that `-race` needs cgo. `package main` at the repo root is the whole
binary (subcommands in `subcommand.go` / `daemoncli.go` / `authcli.go`); all
logic lives in `internal/` (`session` is the kernel; `mcp`, `webui`,
`sshserver`, `storage`, `forward`, `sftp` are the main surfaces). `pkg/api`
holds only shared types that `internal/` packages must agree on.

There is no linter or formatter config — `go build` / `go vet` via `go test` is
the whole gate, so keep to gofmt-default style.

## Commands

- `make` (or `make build`) — release binary to `dist/`, stamped with
  version/commit/date via ldflags.
- `make test` — byte-for-byte the CI command
  (`go test ./... -count=1 -shuffle=on -race -timeout 240s`); if you change the
  flags, change `.github/workflows/test.yml` too. `-race` needs a C toolchain;
  on Windows without one use `RACE=0` (default `RACE=auto` probes and degrades).
- `make test-stress` — scheduling perturbation (`-cpu=1,2,4 -count=2`) for
  filesystem/lifecycle races `-race` cannot see. Run it before pushing changes
  that touch the filesystem or session lifecycle. Scope it with
  `STRESS_PKGS=./internal/session/`.
- One test: `go test -run TestX ./internal/<pkg>/ -count=1`. A `-shuffle=on`
  failure replays with `go test -shuffle=<seed>`.

## Two build variants — changes must compile under both

`make build-api` builds with `-tags no_webui`: no embedded Web UI, no
`/`/`/api.html`/`/static/*` routes, only REST/MCP/WS plus `/api.md` and
`/skills.md`. CI compiles *and* tests the `no_webui` tag, so any code you touch
must build under both variants.

## Docs that are executable truth

- `docs/api.md` is the single source for the served `/api.md`; after editing it
  run `make sync-assets` (copies to `internal/webui/assets/api.md`) —
  `TestSyncedDocsMatchSource` fails CI on drift. `docs/mcp-tools.md` is *not*
  synced; MCP tools self-describe via `tools/list` schema.
- Tests must survive `.github/workflows/test.yml` on ubuntu + macos + windows:
  no shared absolute paths (`t.TempDir()` is the default), no execution-order
  assumptions, poll with deadlines instead of fixed sleeps, and a session
  created against a real store must be *deleted* (not just terminated) in
  cleanup — see `docs/agents/testing.md` for the full rules.

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues via `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Project docs live under `docs/` (`docs/api.md`, `docs/architecture.md`, `docs/design/`).
Consumption rules for `CONTEXT.md` / ADRs (created lazily, may not exist yet): `docs/agents/domain.md`.

### Releases

`dev` is the rebased development line; `main` only receives release PRs. Use
the process in `docs/releasing.md` before tagging. Write the `## Unreleased`
notes **before** cutting the release — one short paragraph per user-visible
change since the last release, not a design note; `make prepare-release` only
moves them. Give the PR a `release/vX.Y.Z` head branch or `release: vX.Y.Z`
title, run `make prepare-release RELEASE_VERSION=vX.Y.Z`, and review the
generated changelog and `server.json` changes. CI checks both files before
merge, and `release.yml` re-checks `server.json` against the tag on release.

A `vX.Y.Z-rc.N` tag is an internal test build: it adds a dated changelog
section, **leaves `server.json` alone** (no registry entry, no `:latest` move)
and is marked as a GitHub pre-release. Its branch and PR name the rc itself
(`release/v0.2.8-rc.1`), because CI requires the release signal to match the
files exactly — the final release is its own PR (`release/v0.2.8`).
`docs/releasing.md` has the details.
### Tests run in CI on three OSes

Whenever you write or modify a test, remember it must pass in
`.github/workflows/test.yml` — ubuntu, macos and windows, via
`go test ./... -count=1 -shuffle=on -race -timeout 240s` (identical to
`make test`). For races the detector cannot see — filesystem and lifecycle
races — run `make test-stress` locally before pushing. Assume concurrency, a
fresh runner, and dirty state from other tests or processes. Never hardcode a
shared absolute path; `t.TempDir()` is the default. See
`docs/agents/testing.md`.
## Driving Termcp's MCP tools

Working in this repo usually means driving Termcp itself. The reference is the
running instance, not a file: `help` lists every directive, `help <directive>`
teaches one, and an empty `term()` call returns the guide. The REST entrance
`POST /api/term` speaks the same command language, and `GET /api.md` publishes
the transport-level reference. Three invariants are cheap to keep in mind without being told:

- **Turns are the scarce resource.** An MCP call costs 2–10 ms; an assistant
  turn costs 6–9 s — ~1000×. Minimise *turns*, not calls: whatever one turn can
  hold, keep it in one turn.
- **One command = one batch.** `shell_input` (types) → `shell_key(enter)`
  (executes) → `shell_output` (reads) is *tool semantics, not turn boundaries* —
  run all three inside a single `mcpScript` turn, and let
  `shell_output(timeout≤3)` do the only waiting there is. The loop is
  input → execute → read → next input, with nothing in between.
- **`shell_id` is the I/O target, `session_id` is the connection.** Every I/O
  tool takes `shell_id`. One task gets one session (give it a `name`).

Split across turns only when a human must act first (credential prompt → read
it, then `notify_user(level="warn", duration_seconds=0, session_id=...)`), a
destructive action needs confirmation, or you must inspect intermediate state
to decide the next step.
