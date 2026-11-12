package mcp

// The dogfood pass found these three; each is a flag or a header that described something
// other than what happened.

import (
	"strings"
	"testing"
)

// SCREEN-TOP MUST NAME THE FIRST ROW RETURNED. It was computed as total-rows, so a caller
// following the documented round trip ("ask again from that --top") landed somewhere else:
// the rows below the cursor are blank and the tail skips them, so the two numbers differ
// exactly when the screen is not full.
func TestScreenTopNamesTheFirstReturnedRow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{`echo TOP""ROW`}, "timeout": 5})
	screenUntil(t, s, h, "TOPROW")

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --screen --tail 3", "timeout": 0})
	topStr, ok := headerValue(t, got, "Screen-Top")
	if !ok {
		t.Fatalf("no Screen-Top header:\n%s", firstLine(got))
	}
	rowsStr, _ := headerValue(t, got, "Screen-Rows")
	totalStr, _ := headerValue(t, got, "Screen-Total")
	if rowsStr == "" || totalStr == "" {
		t.Fatalf("missing Screen-Rows/Screen-Total:\n%s", firstLine(got))
	}

	// Reading again from the reported top must return the same rows, in the same order.
	again := termText(t, s, map[string]any{
		"cmd": "read " + h + " --screen --top " + topStr + " --rows " + rowsStr, "timeout": 0})
	body := func(text string) string {
		_, b, _ := strings.Cut(text, "\n\n")
		return b
	}
	// The paged read keeps blank rows, so compare after trimming them from both sides.
	trim := func(b string) string {
		lines := strings.Split(strings.TrimRight(b, "\n"), "\n")
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		return strings.Join(lines, "\n")
	}
	if trim(body(got)) != trim(body(again)) {
		t.Fatalf("Screen-Top=%s does not name the first returned row; re-reading from it gave different rows:\n--- tail ---\n%s\n--- page ---\n%s",
			topStr, trim(body(got)), trim(body(again)))
	}
}

// --top AND --rows DESCRIBE A SCREEN PAGE. Without --screen they were parsed, ignored and
// answered with the whole log, which is silent wrongness.
func TestReadTopWithoutScreenIsRefused(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	for _, cmd := range []string{"read " + h + " --top 0", "read " + h + " --rows 5"} {
		got := termText(t, s, map[string]any{"cmd": cmd, "timeout": 0})
		if !strings.HasPrefix(got, "TERM/1 4") {
			t.Fatalf("%q was not refused:\n%s", cmd, firstLine(got))
		}
		if !strings.Contains(got, "--screen") {
			t.Fatalf("%q refused without naming --screen:\n%s", cmd, firstLine(got))
		}
	}
}
