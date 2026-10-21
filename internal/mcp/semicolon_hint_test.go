package mcp

import (
	"strings"
	"testing"
)

// TestSemicolonAfterPayloadGetsAHint pins the hint that names the real mistake.
//
// `run h:1 pwd; whoami` fails with "unknown directive: whoami", which is true and
// useless: the caller wrote shell commands and the `;` separated DIRECTIVES. The
// reply already carries the directive table for a misspelled name, and this is not
// that case — no table entry would have helped, because the caller was not writing
// termcp statements at all.
func TestSemicolonAfterPayloadGetsAHint(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "run a:1 pwd; whoami"})

	if !strings.Contains(got, "unknown directive: whoami") {
		t.Fatalf("the failure itself must still be reported:\n%s", got)
	}
	for _, want := range []string{"HINT", "args", "&&"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint does not mention %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "a `run`") {
		t.Errorf("hint does not name the directive that was cut short:\n%s", got)
	}
}

// TestNoHintWhenNothingWasCutShort pins the negative: a typo in a command that has
// no payload has no `;` to blame, so the reply must not suggest args.
func TestNoHintWhenNothingWasCutShort(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "ps; bogus"})
	if !strings.Contains(got, "unknown directive: bogus") {
		t.Fatalf("the failure itself must be reported:\n%s", got)
	}
	if strings.Contains(got, "HINT") {
		t.Errorf("a typo after `ps` has nothing to do with a payload:\n%s", got)
	}
}

// TestHelpRunDocumentsArgs pins that `help run` teaches the args channel.
//
// The directive is the one place a caller looks up how to run something, and before
// this it described the payload as if a semicolon were legal in it — so a caller
// following the help page wrote a `;` and learned about the split from an error.
func TestHelpRunDocumentsArgs(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "help run"})
	for _, want := range []string{"args", "$1", "VERBATIM", "vars"} {
		if !strings.Contains(got, want) {
			t.Errorf("help run does not mention %q:\n%s", want, got)
		}
	}
}

// TestHintSuppressesTheDirectiveTable pins that the two replies do not compete.
//
// The directive table rides along on an unknown-but-directive-looking name, which is
// right for a typo and wrong for a shell command that a `;` cut loose: there the
// caller does not need one of 25 directives, they need a different argument. Showing
// both put 25 names between the caller and the one line that helps.
func TestHintSuppressesTheDirectiveTable(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "run a:1 pwd; whoami"})
	if !strings.Contains(got, "HINT") {
		t.Fatalf("expected a hint:\n%s", got)
	}
	if strings.Contains(got, "run | type | key") {
		t.Errorf("the directive table competes with the hint:\n%s", got)
	}

	// A plain typo still gets the table: that caller DOES need the name list.
	got = termText(t, s, map[string]any{"cmd": "ps; bogus"})
	if !strings.Contains(got, "run | type | key") {
		t.Errorf("a bare typo should still carry the table:\n%s", got)
	}
}
