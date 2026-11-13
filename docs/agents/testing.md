# Writing tests that survive CI

Every test lands in `.github/workflows/test.yml`, which runs
`go test ./... -count=1 -shuffle=on -race` on **ubuntu, macos and
windows** runners, on every PR — the same flags `make test` runs, so
the two must change together. The one exception is `-timeout`: locally it is 600s,
on CI 1800s, because a shared runner under `-race` is several times slower and a
run killed by `-timeout` panics with no failing test named. Both are hang
detectors, not budgets. A test that passes on your machine and flakes in
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
make test-linux   # Docker-based cross-test: runs internal/mcp on a real Linux kernel
```

The three targets cover the three sources of test-owned divergence:

- **Memory races** — `-race` (CI always; locally wherever cgo and a C compiler
  exist, which `make test` probes and prints when it has to skip).
- **Filesystem/lifecycle races** — invisible to `-race`. The stress target's
  `-cpu=1` leg squeezes goroutines onto one scheduler, which interleaves
  teardown with background writers the way a loaded CI runner does;
  `-count` repeats, `-shuffle` reorders. This is the harness that reproduced
  `internal/session`'s TempDir race 28 runs out of 30, on a machine where
  plain `-count=15` had never caught it.
- **OS-specific pty timing** — Windows ConPTY, macOS BSD ttys, and Linux POSIX
  ptys echo bytes and schedule wakeups at different moments relative to the
  calling goroutine. `make test-linux` cross-compiles `internal/mcp` and runs
  it in an `ubuntu:24.04` container: that catches the Linux-only pty bugs
  (echo mistaken for output, a quiet channel that never took the slow path)
  before pushing to dev.

`-count` also catches state leaked between runs in one process; `-shuffle=on`
catches a test that only passes after some other test has run (failures print a
seed — rerun with `go test -shuffle=<seed>` to confirm). For packages that touch
the filesystem, run two copies concurrently in separate shells — that is what a
second CI job does to your assumptions.

## Waiting is a state, not a duration

The two flaky failures this section was written for both came from the same
mistake, and it is worth naming before the rules: **a test waited for a duration
and called it a state.**

```
--- FAIL: TestARefusedReadKeepsTheBytesItRefused   (windows)
    the read should have been refused for its charset, got:
    TERM/1 204 No Content
    Content-Range: bytes */0
    Waited: 2000ms
```

The test started PowerShell, asked it to write four bytes, and read with
`timeout: 2`. On a loaded Windows runner PowerShell needed more than two seconds
to get to the write, so `read` correctly answered "nothing yet" — and the test
read that as "the charset refusal is broken".

```
--- FAIL: TestAClosedChannelExplainsItself   (ubuntu)
    a closed channel should refuse the read
```

That one polls for the extension to drop its channel, then polls the channel
itself. The second loop treats *any* `IsError` as success and *every* non-error
as "not yet" — but a `read` on a channel that is closing answers `204 No
Content`, which is not an error either. The loop cannot tell "the channel is
still open" from "the channel is closed and has nothing to say", so it spins
until its deadline.

Neither is a product bug. Both are a test asserting on a **return value that
means something different from what the test assumes**, and using a **fixed
duration as a proxy for a state**.

### Rule 1: a small `timeout` is a request, not a guarantee

`term(..., "timeout": N)` bounds how long the server waits. It does not promise
the thing you are waiting for happens inside N. On a shared runner, a process
starting up (PowerShell, a login shell, a cold `sh`) can take seconds before it
produces its first byte.

So never put two things in one call:

```go
// WRONG: "the bytes arrived" and "the encoding was refused" are different claims,
// and the first one is not guaranteed inside the timeout.
first := read(ch, timeout=2)
if !strings.Contains(first, "not valid UTF-8") { t.Fatal(...) }

// RIGHT: wait for the bytes as its own step, then assert on what they were.
require.Eventually(t, func() bool {
    return !strings.Contains(read(ch, timeout=1), "204 No Content")
}, 30*time.Second, 100*time.Millisecond, "the command never produced output")
first := read(ch, timeout=5)
if !strings.Contains(first, "not valid UTF-8") { t.Fatal(...) }
```

The deadline belongs in the poll, and the `timeout` in the call should be
small, because it is how long a *single* probe waits, not how long the test is
willing to.

### The deadline is a hang detector, not a budget

This distinction is what makes a poll survive both a laptop and a shared runner,
and it is worth stating because "60 seconds" looks like a performance guess:

- **A budget** says "this should finish within N". It encodes an assumption about
  machine speed, so every faster or slower machine is a bug report waiting.
- **A hang detector** says "if this has not happened in N, something is broken
  rather than slow". It only bounds how long a *failure* takes to report.

A poll loop is insensitive to machine speed **by construction**: it returns on the
first iteration that observes the state, so a slow machine costs extra iterations
and nothing else. The deadline only decides how long to wait before declaring the
state unreachable — 60s for a subprocess to print four bytes is not a budget, it
is the point past which "slow" stops being a credible explanation.

That property is testable, and proving it is cheaper than arguing about it. Slow
the producer down deliberately and watch the test still pass at its normal speed:

```go
// Temporarily: make the command take 4 seconds to produce its first byte.
write := "sh -c " + `"sleep 4; printf 'ÖÐÎÄ'; sleep 30"`
```

With the poll, the test finishes in ~0.5s (it returns the moment the byte
arrives). With the old single `read` + `timeout: 2`, the same test fails with the
exact CI symptom — `204 No Content`, `Waited: 2000ms`. If slowing the producer
makes your test slower rather than just differently-timed, it is waiting on a
duration; if it fails, it never had a way to wait at all.

Absolute timeouts are the same trade. `-timeout 1800s` on CI and `600s` in
`make test` are not "CI is three times slower" as a fact about the world; they
are two different points past which a run is assumed wedged, chosen wide enough
that a legitimately slow runner is not mistaken for a hang.

### Rule 2: know what each status means before polling on it

A poll loop needs a predicate that is true for exactly one state. Before writing
one, check the meanings:

| what you get | what it means | is it "done"? |
|---|---|---|
| `204 No Content` | nothing was available in the window | **no** — the channel may be open or closed |
| `404` + `idle`/`closed` | the channel is gone | yes |
| `IsError` | the tool refused | depends on *why* |

`TestAClosedChannelExplainsItself` polled on "not an error", which `204` also
satisfies. Poll on the state you actually want (`get ext <name>` reports whether
the channel is still assigned), or match the status text, and let the deadline
prove the failure.

### Rule 3: sleep only between probes, never as the wait itself

`time.Sleep` inside a polling loop is a backoff. `time.Sleep` standing in for
"and by now it will have happened" is the flake. The repo has 22 sites sleeping
50ms and 14 sleeping 20ms; most are backoff and fine, but any of them that is
followed by a bare assertion rather than a check is a bug waiting for a slow
runner. If you cannot write the check, you do not have a test — you have a
stopwatch.

## A source-text assertion is not a test of behaviour

Four test files under `internal/webui` read their own package's `.go` files and
match substrings (`readGoSource`). They exist because some invariants are about
*structure*: "the pump accumulates before it sends", "this lock is not held
across a send". A behavioural test cannot see those.

They are still fragile in a specific way, and it bit this repo during a
refactor:

```
marker "case !shell.HasMoreOutput(rid)" not found; lines that do:
    case !shell.HasMoreOutput(ent.rid) && ...
```

The behaviour was identical — the variable holding the reader id was renamed
from a parameter to a field on the entry — and the test failed anyway.

So when you write one:

- Match on **semantics, not spelling**. Prefer a marker that names the decision
  (`"HasMoreOutput("`, `"flush()"`) over one that includes a local variable.
- Say in the failure message **what invariant is being protected and why**, so
  the next person can tell "I broke it" from "I renamed something".
- Never use a source-text assertion where a behavioural one is possible. These
  are for the cases where the property is genuinely about code shape.

## A flake is a defect in the test, and it is in scope

CI runs this suite on three operating systems on every PR. A test that fails
one run in twenty does not cost one re-run: it teaches everyone to re-run red,
which is how a real failure gets merged.

So when a test flakes:

1. **Reproduce it locally first** — `-count=10`, then `-cpu=1,2,4 -count=2`
   (see `make test-stress`). A flake you cannot reproduce still needs the log
   line that would have told you what the state was; add it.
2. **Find which assumption was wrong**, and fix that. Adding a retry around a
   broken assumption hides it and moves the failure to a slower machine.
3. **Do not raise a timeout to make a flake go away.** If the only way a test
   passes is by waiting longer, it is asserting on timing rather than on state —
   see Rule 1.
