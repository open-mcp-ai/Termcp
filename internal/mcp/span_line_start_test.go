package mcp

import (
	"strings"
	"testing"
)

// TestSpanListsLineStartOffset pins that `span` prints the line's start offset
// separately from the command's start offset, so a caller can address the line
// without landing mid-prompt.
func TestSpanListsLineStartOffset(t *testing.T) {
	s := newTestServer(t)

	// A pipe whose command prints several lines.
	text := termText(t, s, map[string]any{
		"cmd": "pipe internal --argv printf 'HELLO\\nWORLD\\n'",
	})
	h := handleFrom(t, text)

	// Wait for the program to exit so its bytes are all in the log; a read is a
	// snapshot and does not wait (see waitForShellToExit).
	waitForShellToExit(t, s, shellIDFromHandle(t, s, h))

	// Span must include the @ column.
	got := termText(t, s, map[string]any{"cmd": "span " + h})
	if !strings.Contains(got, "@") {
		t.Fatalf("span output must carry the @<line_start> column, got:\n%s", got)
	}

	// Read from line 1: the start of that read should match the @ offset.
	r := termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1"})
	if !strings.Contains(r, "HELLO") {
		t.Errorf("read should contain HELLO, got:\n%s", r)
	}
}
