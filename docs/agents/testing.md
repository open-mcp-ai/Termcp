# Writing tests that survive CI

Every test lands in `.github/workflows/test.yml`, which runs
`go test ./internal/... -count=1 -timeout 120s` on **ubuntu, macos and windows**
runners, on every PR. A test that passes on your machine and flakes in CI costs
everyone a re-run and teaches people to ignore red. Write for the runner, not
for your checkout.

## Never hardcode a shared path

A fixed absolute path (`/tmp/x`, `C:\tmp\x`, `/etc/hostname`) is shared state:
another test in the same package, a parallel package, a leftover file from a
previous run, or an entirely different program can own it. Two failure modes,
both of which have bitten this repo:

- **Concurrent writers delete each other's file.** Test A removes `/tmp/x` at
  setup while test B is asserting the file it just wrote exists.
- **The parent directory does not exist on the runner.** `os.OpenFile` with
  `O_CREATE` does *not* create parents, so the error is `file does not exist`
  even though the real problem is a missing directory. On Windows `/tmp` resolves
  to `<drive-of-cwd>:\tmp` — a path that is usually absent on a fresh runner.

Use `t.TempDir()` instead. It returns a per-test, uniquely named directory that
the test framework creates and removes, so it is safe under `-count=N`, under
parallel packages, and on an empty runner:

```go
target := filepath.Join(t.TempDir(), "probe.txt")
```

`t.TempDir()` guarantees only *its own* directory exists. If you need a nested
path, create the subdirectory (`MakeDir`) or use `os.MkdirAll` for a local path.

## Prefer in-process to reachable-network

The loopback SSH server used by the `internal/mcp` tests is in-process, so a
"remote" path is a real local path. Do not assume `/tmp` exists, is writable, or
is yours — the same rules as above apply.

## Do not depend on execution order or leftover state

- No test may assume another test ran first, or that a file it wrote still
  exists. Each test builds the state it needs.
- Clean up through `t.Cleanup` rather than a trailing statement, so a failing
  `t.Fatal` still releases handles and directories. Register cleanup *after*
  `t.TempDir()` so LIFO removes the directory last (an open handle makes
  `RemoveAll` fail on Windows).
- Never mutate process-global state (`os.Chdir`, `os.Setenv`) without
  `t.Setenv`/deferring the restore.

## Watch the clock, not just the logic

CI runners are slower and contended. A test that asserts "the value changed
within 100ms" is a flake waiting to happen. Poll with a deadline (`require.Eventually`,
or a loop with a `time.After` bound) instead of sleeping a fixed duration, and
always give the timeout clear headroom over the expected latency.

## Before you push

```bash
go test ./internal/... -count=1 -timeout 120s      # the CI command exactly
go test ./internal/<pkg>/ -count=5 -shuffle=on     # order and repeat independence
```

`-count=5` catches state leaked between runs in one process; `-shuffle=on`
catches a test that only passes after some other test has run. For packages that
touch the filesystem, run two copies concurrently in separate shells — that is
what a second CI job does to your assumptions.
