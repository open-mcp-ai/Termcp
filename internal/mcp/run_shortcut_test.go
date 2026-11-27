package mcp

import (
	"strings"
	"testing"
)

// ONE SHELL LINE, NO DSL (issue #319).
//
// The point of this surface is what it does NOT do: the line is handed to a shell
// verbatim. `;`, `|`, `&&`, quotes, `$(…)` and `%{http_code}` are the shell's business,
// and the DSL's six ambiguity classes (placeholder conflicts, flag capture, polymorphic
// arguments, reserved words, implicit semantics, switch ambiguity) cannot occur because
// nothing is parsed.
//
// The assertion is therefore on SHELL SEMANTICS ARRIVING INTACT, not on a reply shape:
// a pipeline, a quote with a space, and a `%{…}` that the DSL would have had opinions
// about.
func TestRunHandsTheLineToTheShellVerbatim(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)

	got := termText(t, s, map[string]any{
		"run":     `printf '%s\n' "a b" | tr ' ' '_'; echo "%{http_code}"`,
		"session": sid,
		"timeout": 10,
	})
	if !strings.Contains(got, "a_b") {
		t.Fatalf("the pipeline did not reach the shell:\n%s", got)
	}
	if !strings.Contains(got, "%{http_code}") {
		t.Fatalf("a literal the DSL has opinions about was mangled:\n%s", got)
	}
}

// A ONE-SHOT RUN REPORTS THE PROCESS'S EXIT CODE, which is the whole reason it is a
// separate spelling from `shell`: it owns a process that exits.
func TestRunSessionReportsAnExitCode(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)

	ok := termText(t, s, map[string]any{"run": "echo ONE-SHOT", "session": sid, "timeout": 10})
	if !strings.Contains(ok, "ONE-SHOT") {
		t.Fatalf("the one-shot run produced no output:\n%s", ok)
	}
	if !strings.Contains(ok, "Exit-Code: 0") {
		t.Errorf("a one-shot run does not report its exit code:\n%s", ok)
	}

	failed := termText(t, s, map[string]any{"run": "exit 7", "session": sid, "timeout": 10})
	if !strings.Contains(failed, "Exit-Code: 7") {
		t.Errorf("a non-zero exit was not reported:\n%s", failed)
	}
}

// `shell` TYPES INTO AN EXISTING SHELL, so state persists and there is no exit code —
// the difference the two spellings exist to make.
func TestRunShellKeepsStateAndClaimsNoExitCode(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	termText(t, s, map[string]any{"run": "cd /tmp", "shell": h, "timeout": 10})
	got := termText(t, s, map[string]any{"run": "pwd", "shell": h, "timeout": 10})
	if !strings.Contains(got, "/tmp") {
		t.Fatalf("the second line did not run in the shell the first one changed:\n%s", got)
	}
	// A TERMINAL NEVER REPORTS THAT A COMMAND INSIDE IT FINISHED, so claiming an exit code
	// here would be a measurement the server cannot make.
	if strings.Contains(got, "Exit-Code:") {
		t.Errorf("an interactive shell claimed an exit code:\n%s", got)
	}
}

// THE PAIR IS EXACTLY ONE TARGET, and a call that names both or neither is refused with
// the difference spelled out — that refusal is the whole documentation for the choice.
func TestRunRefusesAnAmbiguousTarget(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)

	both := termText(t, s, map[string]any{
		"run": "echo hi", "session": sid, "shell": sid + ":1", "timeout": 5,
	})
	if !strings.Contains(both, "TERM/1 400") || !strings.Contains(both, "alternatives") {
		t.Errorf("session+shell together was not refused:\n%s", both)
	}

	neither := termText(t, s, map[string]any{"run": "echo hi", "timeout": 5})
	if !strings.Contains(neither, "TERM/1 400") || !strings.Contains(neither, "one-shot pipe") {
		t.Errorf("a run with no target was not refused with the difference:\n%s", neither)
	}
}

// TWO COMMAND LINES ARE TWO REQUESTS: `cmd` is the DSL and `run` is a shell line, so a
// call carrying both is refused rather than having one silently win.
func TestRunAndCmdCannotBeMixed(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)

	got := termText(t, s, map[string]any{
		"cmd": "ls", "run": "echo hi", "session": sid, "timeout": 5,
	})
	if !strings.Contains(got, "TERM/1 400") || !strings.Contains(got, "two different command lines") {
		t.Errorf("cmd+run was not refused:\n%s", got)
	}
}

// A CALL WITHOUT `run` IS UNTOUCHED. Every existing caller must keep working, which is
// the property that lets this be an addition rather than a migration.
func TestACallWithoutRunStillUsesTheDSL(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)

	got := termText(t, s, map[string]any{"cmd": "get " + sid, "timeout": 10})
	if !strings.Contains(got, "TERM/1 200") {
		t.Fatalf("a DSL call stopped working:\n%s", got)
	}
}

// A TARGET WITHOUT A LINE IS REFUSED, not silently ignored: `session`/`shell` mean
// something only beside `run`, and ignoring them would answer a request the caller did
// not make (the DSL path would run, or the guide would come back).
func TestATargetWithoutALineIsRefused(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)

	got := termText(t, s, map[string]any{"session": sid, "timeout": 5})
	if !strings.Contains(got, "TERM/1 400") || !strings.Contains(got, "only means something with `run`") {
		t.Errorf("an orphan session argument was silently ignored:\n%s", got)
	}
}
