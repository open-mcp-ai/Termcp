package mcp

import (
	"strings"
	"testing"
)

// TestWatchWithoutTargetIsSyntaxError pins the refusal on `watch` with no target.
//
// TargetOptional: true lets `--list` and `--off` run without a channel. The default
// register form needs one, and before this fix it reached the handler and returned
// a handler-internal "shell_id is required" rather than a syntax-level refusal.
func TestWatchWithoutTargetIsSyntaxError(t *testing.T) {
	s := newTestServer(t)

	// Bare `watch`: syntax refusal with usage.
	got := termText(t, s, map[string]any{"cmd": "watch"})
	if !strings.HasPrefix(got, "TERM/1 400") {
		t.Fatalf("expected 400, got:\n%s", got)
	}
	for _, want := range []string{"watch needs a target", "watch <handle>"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal should mention %q:\n%s", want, got)
		}
	}

	// But `--list` still needs no target.
	got = termText(t, s, map[string]any{"cmd": "watch --list"})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("watch --list should succeed without a target, got:\n%s", got)
	}
}
