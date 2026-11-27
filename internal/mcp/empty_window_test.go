package mcp

import (
	"strings"
	"testing"
	"time"
)

// AN EMPTY BODY IS NEVER A DELIVERED RANGE (issue #269 #1).
//
// The measured defect, on the local pty, before this was pinned:
//
//	run <h> sleep 0.3        (timeout 0.05)
//	→ 200, body "", Content-Range: bytes 314-335/336
//
// A 200 whose body is empty is indistinguishable from a truncated reply, and the range
// claimed 22 bytes that never arrived — so a caller paging by `Content-Range` skipped the
// very window it was told it had seen. The 22 bytes were real but invisible: the pty's
// echo of the typed line plus its bracketed-paste escape, which the text path strips to
// nothing.
//
// The invariant this pins: a reply claims a byte range only when it either DELIVERED
// those bytes or NAMED them as withheld (`Folded`, `Omitted`). Anything else is "nothing
// yet" — a 204, and a position that did not move.
func TestAnEmptyBodyIsNotADeliveredRange(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	// Establish the prompt first: an unread startup banner IS delivered bytes, and what
	// this test isolates is a command that prints nothing of its own.
	awaitPtyReady(t, s, h)

	// A command that prints nothing inside a window far shorter than its runtime: the
	// window covers the echo and nothing else.
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"sleep 1"},
		"timeout": 0.05,
	})
	if strings.Contains(got, "TERM/1 200") {
		t.Fatalf("a reply with no delivered bytes answered 200:\n%s", got)
	}
	if !strings.Contains(got, "TERM/1 204") {
		t.Fatalf("an empty window is not a 204:\n%s", got)
	}
	// THE RANGE MUST SAY "NOTHING DELIVERED", not name a span: `*/N` is the unsatisfied
	// form, and `bytes A-B/N` is the claim that was wrong.
	if strings.Contains(got, "Content-Range: bytes ") && !strings.Contains(got, "Content-Range: bytes */") {
		t.Errorf("the empty reply claims a delivered range:\n%s", got)
	}
	// AND THE POSITION IS NAMED, so the caller can read the same window again.
	if !strings.Contains(got, "Input-At:") {
		t.Errorf("the reply does not say where the window started:\n%s", got)
	}
}

// NOTHING IS LOST BY BEING HONEST ABOUT IT: the output that arrives after the window is
// still there for the next read, which is the whole reason the reply must not pretend the
// window was delivered.
func TestOutputArrivingAfterTheWindowIsStillReadable(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"sleep 0.4; echo LATE-269"},
		"timeout": 0.05,
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": 1})
		if strings.Contains(got, "LATE-269") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the output that arrived after the window never became readable:\n%s", got)
		}
	}
}

// A FOLDED WINDOW IS STILL A RANGE, because the reply NAMES the span it withheld and the
// flag that fetches it: that is the difference between "withheld on purpose" and "nothing
// there", and collapsing the two would make `on_bytes_overflow: fold` unusable.
func TestAFoldedWindowStillReportsItsRange(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sh")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// A pipe that prints more than one byte window, with the fold policy on.
	termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"printf 'aaaaaaaaaa\\nbbbbbbbbbb\\n'"},
		"timeout": 2,
	})
	got := termText(t, s, map[string]any{
		"cmd":              "read " + h + " --offset 0",
		"bytes_limit":      5,
		"on_bytes_overflow": "fold",
		"timeout":          2,
	})
	if !strings.Contains(got, "Folded:") {
		t.Fatalf("a folded window does not name the span it withheld:\n%s", got)
	}
	if !strings.Contains(got, "Content-Range: bytes ") {
		t.Errorf("a folded window lost its range, so the withheld bytes have no address:\n%s", got)
	}
}
