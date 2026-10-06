# Writing tests that survive CI

Every test lands in `.github/workflows/test.yml`, which runs
`go test ./... -count=1 -shuffle=on -race -timeout 240s` on **ubuntu, macos and
windows** runners, on every PR — byte-for-byte the command `make test` runs, so
the two must change together. A test that passes on your machine and flakes in
CI costs everyone a re-run and teaches people to ignore red. Write for the
runner, not for your checkout.

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

## Terminate leaves a background writer; Delete joins it

A test that terminates a session created against a real store
(`storage.New(t.TempDir())`) must register a cleanup that *deletes* the session,
not merely close the store. `Terminate` deliberately does not wait for the
per-shell exit watcher — joining it there would self-deadlock the natural-exit
path, where the watcher itself drives the DEAD transition — so the watcher's
final manifest persist lands milliseconds *after* `Terminate` returns. A test
that ends right after Terminate then hands a still-live writer to `t.TempDir`'s
`RemoveAll`: a `.tmp-*` (or renamed manifest) appears between the directory
listing and the `rmdir`, and cleanup fails with "directory not empty". This is
invisible to `-race` (no memory is touched) and it shipped to CI twice before it
was caught.

`Delete → finalize` is the join point: it waits for the watcher before
`DeleteSession` removes the directory, and the store's `deleted` set refuses any
straggling append afterwards. So register, right after Create (before `t.TempDir`
in LIFO order — see the cleanup rule above):

```go
t.Cleanup(func() { _ = m.Delete(id) })
```

For restart-style tests that build a second manager over the same store, delete
through the *first* manager — it owns the session object with the watcher; the
restored copy has none.

## Before you push

```bash
make test         # the CI command exactly; -race is on wherever the toolchain supports it
make test-stress  # scheduling perturbation: -cpu=1,2,4 × -count=2 × -shuffle=on
```

The two targets cover the two sources of test-owned races:

- **Memory races** — `-race` (CI always; locally wherever cgo and a C compiler
  exist, which `make test` probes and prints when it has to skip).
- **Filesystem/lifecycle races** — invisible to `-race`. The stress target's
  `-cpu=1` leg squeezes goroutines onto one scheduler, which interleaves
  teardown with background writers the way a loaded CI runner does;
  `-count` repeats, `-shuffle` reorders. This is the harness that reproduced
  `internal/session`'s TempDir race 28 runs out of 30, on a machine where
  plain `-count=15` had never caught it.

`-count` also catches state leaked between runs in one process; `-shuffle=on`
catches a test that only passes after some other test has run (failures print a
seed — rerun with `go test -shuffle=<seed>` to confirm). For packages that touch
the filesystem, run two copies concurrently in separate shells — that is what a
second CI job does to your assumptions.
