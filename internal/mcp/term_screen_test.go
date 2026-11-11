package mcp

import (
	"strings"
	"testing"
	"time"
)

// `read --screen` ANSWERS IN THE SCREEN'S COORDINATES, NOT THE LOG'S (#312).
//
// Every other read reports byte offsets into a stream, which is right for "what did this
// channel write" and wrong for "what does it say now": `\r` overwrites, erases remove,
// and a repainted prompt occupies ten byte ranges and one row. The model that decides this
// is the same one the Web UI draws with (internal/termview).
//
// POLLED, NOT TIMED. A pty's shell takes its own time to come up and to answer, and the
// assertion is about what the screen says once the bytes are there — so the loop waits for
// that state and the deadline only bounds how long a failure takes to report.
//
// THE MARKERS ARE SPLIT IN THE COMMAND (`echo NEW""ROW`) so the word the test looks for is
// not in the echoed command line. Without that the echo of the command satisfies the poll
// before the command has produced anything, and the test passes on the wrong row.
func screenUntil(t *testing.T, s *Server, h, want string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = termText(t, s, map[string]any{"cmd": "read " + h + " --screen", "timeout": 0})
		if strings.Contains(got, want) {
			return got
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the screen never showed %q within the deadline:\n%s", want, firstLine(got))
	return got
}

func TestReadScreenReportsTheRowsAndTheCursor(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{`echo SCREEN""ONE`}, "timeout": 5})
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{`echo SCREEN""TWO`}, "timeout": 5})

	got := screenUntil(t, s, h, "SCREENTWO")
	if !strings.Contains(got, "SCREENONE") {
		t.Fatalf("the screen dropped an earlier row:\n%s", firstLine(got))
	}
	if _, ok := headerValue(t, got, "Cursor"); !ok {
		t.Fatalf("a screen read said nothing about where the cursor is:\n%s", firstLine(got))
	}
	// A full-screen program's buffer is not recorded, so the only honest answer about it
	// is whether one is in control. That has to be said either way.
	if v, ok := headerValue(t, got, "Alt-Screen"); !ok || v != "no" {
		t.Fatalf("the reply did not say whether a full-screen program holds the screen (Alt-Screen=%q, present=%v)", v, ok)
	}
}

// --tail IS THE NUMBER OF SCREEN ROWS, not log lines: the caller is asking about what is
// displayed, and the two count different things once anything repaints.
//
// THE COUNT IS WHAT IS ASSERTED, not which row happened to be newest: a shell repaints its
// prompt when a command ends, so "the last row is the command's output" is a coin flip
// decided by how fast this machine is. The count is the contract.
func TestReadScreenTailCountsRows(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{`echo OLD""ROW`}, "timeout": 5})
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{`echo NEW""ROW`}, "timeout": 5})
	screenUntil(t, s, h, "NEWROW")

	one := termText(t, s, map[string]any{"cmd": "read " + h + " --screen --tail 1", "timeout": 0})
	if v, ok := headerValue(t, one, "Screen-Rows"); !ok || v != "1" {
		t.Fatalf("--tail 1 reported Screen-Rows=%q (present=%v), want 1:\n%s", v, ok, firstLine(one))
	}

	wide := termText(t, s, map[string]any{"cmd": "read " + h + " --screen --tail 8", "timeout": 0})
	for _, want := range []string{"OLDROW", "NEWROW"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("--tail 8 lost %s, which is inside the last eight rows:\n%s", want, firstLine(wide))
		}
	}
}
