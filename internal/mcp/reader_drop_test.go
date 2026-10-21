package mcp

import (
	"strings"
	"testing"
)

// TestReaderDropWithoutValueFailsCleanly pins the refusal on `reader <handle> --drop`.
//
// Before the fix, `--drop` with no value failed the numeric conversion and fell
// through to the registration branch, answering 200 with a newly minted reader id.
func TestReaderDropWithoutValueFailsCleanly(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "pty internal --name live"})
	h := handleFrom(t, text)

	// Bare --drop must be refused at 400.
	got := termText(t, s, map[string]any{"cmd": "reader " + h + " --drop"})
	if !strings.HasPrefix(got, "TERM/1 400") {
		t.Fatalf("expected 400, got:\n%s", got)
	}
	if !strings.Contains(got, "needs a reader id") {
		t.Errorf("refusal should mention reader id: %s", got)
	}

	// Register a real reader.
	text = termText(t, s, map[string]any{"cmd": "reader " + h})
	if !strings.Contains(text, "reader 1") {
		t.Fatalf("expected reader 1, got:\n%s", text)
	}

	// Drop with a valid id: 204.
	got = termText(t, s, map[string]any{"cmd": "reader " + h + " --drop 1"})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("drop should succeed with 204, got:\n%s", got)
	}
}
