package mcp

// Dogfood round two's two logic gaps: a dry-run verdict that disagrees with the real call,
// and a reply that printed an observed Exit-Code alongside a Hint saying nothing was known.

import (
	"strings"
	"testing"
)

// DRY-RUN VALIDATES ENUMERATED SETS: an unknown key name is syntactically invalid, and a
// green verdict that the real call refuses turns dry-run into a lie.
func TestDryRunRefusesUnknownKey(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{
		"cmd":     "key s:1 f1",
		"dry_run": true,
	})
	if v, ok := headerValue(t, got, "Verdict"); !ok || v != "invalid" {
		t.Fatalf("dry-run key f1 verdict = %q, want invalid:\n%s", v, firstLine(got))
	}
	if !strings.Contains(got, "unknown key \"f1\"") {
		t.Fatalf("dry-run did not carry the key refusal:\n%s", got)
	}

	// And a real key passes dry-run.
	ok := termText(t, s, map[string]any{
		"cmd":     "key s:1 space",
		"dry_run": true,
	})
	if v, _ := headerValue(t, ok, "Verdict"); v != "valid-syntax" {
		t.Fatalf("dry-run key space verdict = %q, want valid-syntax:\n%s", v, firstLine(ok))
	}
}

// A REPLY THAT SAYS EXIT-CODE SAYS HOW IT ENDED. Printing "this reply says nothing about
// whether the command finished" in the same reply was a contradiction: --until did not
// match OSC 133, but the child exit was observed.
func TestUntilExitReportsCompletionWhenExitCodeIsObserved(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// An exit command ends the shell; ConPTY observes the code.
	// `run` takes a variable ($1), not a direct shell line: the caller who writes
	// `run h exit` is refused at parsing before anything runs.
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " --until exit $1",
		"args":    []any{"exit"},
		"timeout": 5,
	})
	if _, ok := headerValue(t, got, "Hint"); ok {
		t.Fatalf("a reply with an observed exit carried the unmatched Hint:\n%s", firstLine(got))
	}
	if v, ok := headerValue(t, got, "Completion"); !ok || v != "exit" {
		t.Fatalf("Completion = %q, want exit:\n%s", v, firstLine(got))
	}
}
