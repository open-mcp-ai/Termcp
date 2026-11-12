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

// --top AND --rows PAGE THROUGH THE SCREEN IN THE MODEL'S ROW NUMBERING (#312).
//
// `--tail` only looks down; a caller reading output that has already scrolled off needs
// to walk back without having to switch to byte reads.
func TestReadScreenPagingWalksPastRows(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{`echo ROW""ZERO`}, "timeout": 5})
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{`echo ROW""ONE`}, "timeout": 5})
	screenUntil(t, s, h, "ROWONE")

	banner := termText(t, s, map[string]any{"cmd": "read " + h + " --screen --top 0 --rows 2", "timeout": 0})
	if v, ok := headerValue(t, banner, "Screen-Top"); !ok || v != "0" {
		t.Fatalf("banner page reported Screen-Top=%q, want 0:\n%s", v, firstLine(banner))
	}
	if v, ok := headerValue(t, banner, "Screen-Rows"); !ok || v != "2" {
		t.Fatalf("banner page reported Screen-Rows=%q, want 2:\n%s", v, firstLine(banner))
	}

	// ASK FOR THE WHOLE MODEL rather than a guessed window: how many rows the buffer
	// holds depends on how far the prompt theme wrapped, so "--top 8" describes a
	// position this test has no way to know. The paging this asserts is that a window
	// can be named at all; where the newest row happens to sit is the shell's business.
	newest := termText(t, s, map[string]any{"cmd": "read " + h + " --screen --top 0 --rows 100", "timeout": 0})
	if !strings.Contains(newest, "ROWONE") {
		t.Fatalf("a page covering the whole model is missing the last command:\n%s", firstLine(newest))
	}
	for _, hname := range []string{"Cursor-Visible", "Paste-Mode", "Screen-Total"} {
		if _, ok := headerValue(t, newest, hname); !ok {
			t.Fatalf("paged screen is missing header %s:\n%s", hname, firstLine(newest))
		}
	}
}

// connect --cols and --rows MUST TAKE EFFECT (#312).
//
// Both were declared on `connect` and `open` and neither was read: only `--size`
// took effect, so a caller who passed `--cols 40` got 200 OK and an 80-wide screen.
func TestConnectColsAndRowsTakeEffect(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "connect internal --cols 40 --rows 12"})
	if !strings.HasPrefix(text, "TERM/1 200 OK") {
		t.Fatalf("connect --cols 40 --rows 12 failed:\n%s", text)
	}
	h := handleFrom(t, text)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	sc := termText(t, s, map[string]any{"cmd": "read " + h + " --screen", "timeout": 0})
	if v, _ := headerValue(t, sc, "Cols"); v != "40" {
		t.Fatalf("Cols=%q, want 40:\n%s", v, firstLine(sc))
	}
	if v, _ := headerValue(t, sc, "Rows"); v != "12" {
		t.Fatalf("Rows=%q, want 12:\n%s", v, firstLine(sc))
	}
}

// --rows PAST THE END OF THE MODEL IS CLAMPED.
//
// Paging past the end of the buffer must not return padding that pretends to be
// real terminal rows. Screen-Rows describes what the screen has, not what was asked.
func TestReadScreenRowsPastTheEndIsClamped(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	sc := termText(t, s, map[string]any{"cmd": "read " + h + " --screen --top 0 --rows 500", "timeout": 0})
	totalStr, ok := headerValue(t, sc, "Screen-Total")
	if !ok {
		t.Fatalf("missing Screen-Total:\n%s", firstLine(sc))
	}
	rowsStr, _ := headerValue(t, sc, "Screen-Rows")
	if rowsStr != totalStr {
		t.Fatalf("Screen-Rows=%q, want it clamped to Screen-Total=%s:\n%s", rowsStr, totalStr, firstLine(sc))
	}
}
