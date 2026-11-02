package mcp

import (
	"log/slog"
	"strings"
	"testing"
)

// A SECRET IS DECLARED, NOT DETECTED.
//
// Issue #161 P0-1: a credential passed as a value landed verbatim in the server log
// (`~/.termcp/termcp.log`), which is the one place the caller never intended it to go.
// The tempting fix is a redactor — match `-p '…'`, `--password=`, `://user:pass@` — and
// it is the wrong one: a pattern list that recognises `-p` and misses `--password`
// reports a safety the code does not have, and it silently rewrites legitimate text on
// the way in.
//
// So the caller says it, by where the value goes. `secrets` is the same substitution
// table as `vars` with a different RECORD: the log names which secrets were passed and
// never writes their values, while `vars` stays verbatim because a log that hides what
// was asked is not a record.
//
// This test is the whole contract, and it has to write a real log line to mean
// anything: asserting that a value is absent from a log nothing wrote to proves nothing.
func TestASecretIsNotWrittenToTheLog(t *testing.T) {
	const secret = "HUNTER2-SECRET-VALUE"
	const plain = "PLAIN-VALUE"
	s := newTestServer(t)
	cap := withCapturedLogger(t)

	h := openPipe(t, s, "sh")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// THE REFERENCE IS THE WHOLE PAYLOAD, and that is not a detail of this test: cmd is
	// DSL, so `$PW` standing where the shell line goes is the only place a value from
	// these tables is expanded. Text inside an `args` value is data — it reaches the
	// shell verbatim, which is what makes `args: ["echo $HOME"]` print the remote's
	// HOME and not this process's.
	termText(t, s, map[string]any{
		"cmd":     "run " + h + " $PW ; run " + h + " $PLAIN",
		"vars":    map[string]any{"PLAIN": "echo " + plain},
		"secrets": map[string]any{"PW": ": " + secret},
	})

	recs := cap.snapshot()
	if len(recs) == 0 {
		t.Fatal("no log record was written, so this test proves nothing about redaction")
	}
	var joined strings.Builder
	for _, r := range recs {
		joined.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			joined.WriteString(" " + a.Key + "=" + a.Value.String())
			return true
		})
		joined.WriteString("\n")
	}
	got := joined.String()

	// ANTI-SPIN GUARD: the same call must have logged its OTHER value, or "the secret is
	// absent" would pass on a log that records no values at all. The vars table is the
	// control — same mechanism, same call, logged verbatim.
	if !strings.Contains(got, "vars=PLAIN=echo "+plain) {
		t.Fatalf("the log does not carry the vars value, so the log is no longer a faithful "+
			"record of the call and the secret's absence below would prove nothing:\n%s", got)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("the secret reached the log, which is the one place the caller did not "+
			"send it:\n%s", got)
	}
	// The NAME is the fact worth keeping: it says the call carried a credential
	// without saying which one.
	if !strings.Contains(got, "secrets=PW") {
		t.Fatalf("the log does not name the secret that was passed, so a support report "+
			"cannot tell whether a credential was involved:\n%s", got)
	}
}

// SECRETS AND VARS EXPAND THE SAME WAY, because they are one table.
//
// If the two diverged in substitution — quoting, escaping, the unused-name refusal —
// the caller would have to know which table a value came from to predict what reaches
// the shell, and that is exactly the kind of difference nobody remembers.
func TestSecretsExpandLikeVars(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "cat")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $A ; run " + h + " $B",
		"vars":    map[string]any{"A": "echo one"},
		"secrets": map[string]any{"B": "echo two"},
	})
	if strings.Contains(got, "$A") || strings.Contains(got, "$B") {
		t.Fatalf("a reference was left unexpanded:\n%s", got)
	}
	// cat echoes what it is given, so both values are in the reply — the point is that
	// the substitution happened, not which table named the value.
	if !strings.Contains(got, "echo one") || !strings.Contains(got, "echo two") {
		t.Fatalf("the two tables did not expand the same way:\n%s", got)
	}
}

// ONE NAME, ONE VALUE.
//
// A name in both tables is two values for one reference, and any precedence rule this
// code picked would be a silent choice between them.
func TestANameInBothTablesIsRefused(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "cat")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $PW",
		"vars":    map[string]any{"PW": "echo from-vars"},
		"secrets": map[string]any{"PW": "echo from-secrets"},
	})
	if !strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("defining one name in both tables was accepted:\n%s", got)
	}
	if !strings.Contains(got, "both vars and secrets") {
		t.Fatalf("the refusal does not say what is wrong:\n%s", got)
	}
}

// A SECRET THAT CMD NEVER REFERENCES IS REFUSED LIKE ANY OTHER VALUE.
//
// The unused-name check is what keeps a dropped argument from being invisible, and it
// has to cover the table that carries credentials: a secret nobody referenced is a
// value the caller believes reached the shell and did not.
func TestAnUnusedSecretIsRefused(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "cat")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " echo hello",
		"secrets": map[string]any{"UNUSED": "x"},
	})
	if !strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("an unreferenced secret was accepted:\n%s", got)
	}
}

// THE FIRST SCREEN HAS TO NAME THE PASSWORD, NOT JUST THE MECHANISM.
//
// "A value that must not be written to the server log goes in secrets" is true and does
// not say the thing a caller is about to do: type a password. The rule is only useful if
// it arrives BEFORE the value does — a caller that learns about `secrets` after putting
// the password in `args` has already written it down.
//
// The negative half mattered as much: `cmd` is not a safe carrier just because it is the
// DSL. A payload (`run h:1 <the whole line>`) and a flag value (`--data …`) are part of
// the command line and are logged with it. The guide now states the mechanism — which
// channels the log records verbatim — so the conclusion is drawn from where the bytes go,
// not from a prohibition. The comment above is retained because it names the failure mode.
func TestTheGuideSaysWhereAPasswordGoes(t *testing.T) {
	s := newTestServer(t)
	out := termText(t, s, map[string]any{})

	if !strings.Contains(out, "secrets") {
		t.Fatalf("the guide never names secrets, so a caller learns about it after the "+
			"password is already in args:\n%s", out)
	}
	if !strings.Contains(out, "secrets, which records names only") {
		t.Errorf("the guide does not say what secrets does, which is the only reason a password goes there:\n%s", out)
	}
	if !strings.Contains(out, "cmd, args and vars are") || !strings.Contains(out, "verbatim") {
		t.Errorf("the guide does not say where a credential gets written down. cmd looks safe because it is the DSL:\n%s", out)
	}
	// And the limitation, on the same screen: the terminal echoes what it runs.
	if !strings.Contains(out, "echoes what it runs") {
		t.Errorf("the guide does not say the remote shell echoes the line, which is the one "+
			"thing secrets does not protect:\n%s", out)
	}
}
