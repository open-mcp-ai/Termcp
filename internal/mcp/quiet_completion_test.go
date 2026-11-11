package mcp

import (
	"strings"
	"testing"
	"time"
)

// A WAIT THAT ENDED ON QUIETNESS MAKES NO CLAIM.
//
// A pty has no completion signal, so a `run` on one ends its wait one of two ways: the
// window runs out, or the stream goes quiet (half a second with no new byte). The quiet
// case is the ordinary one — it is how a finished command looks — and the reply used to
// say `Completion: quiet` about it.
//
// THAT HEADER WAS A MEASUREMENT DRESSED AS A VERDICT, and callers acted on it: reading
// `Completion: quiet` and moving on is how the next command got typed into a program
// that still held the terminal. Standing still is not evidence of anything — a finished
// command and a thinking one are identical on a pty — so the reply now says nothing
// about completion at all. `Completion` appears only when there is evidence to name
// (`matched` from the caller's own `--until`, `exit` from the remote shell), and a
// missing header means exactly what its absence says: not known.
func TestAQuietPtyRunClaimsNothing(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// The command prints, then works silently for longer than the quiet window. The
	// wait ends on the quietness, not on the window.
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"echo FIRST-LINE; sleep 4; echo LATE-LINE"},
		"timeout": 30,
	})
	if v, ok := headerValue(t, got, "Completion"); ok {
		t.Fatalf("a wait that ended on quietness claimed Completion=%q; a quiet stream is not evidence of anything:\n%s", v, firstLine(got))
	}
	// And it must not claim the other thing either: nothing here proves the command is
	// still running.
	if v, ok := headerValue(t, got, "Still-Running"); ok {
		t.Fatalf("a quiet stream claimed Still-Running=%q, which is evidence this reply does not have", v)
	}

	// The late output is still in the stream, which is what makes the silence harmless:
	// the command kept working and its bytes arrived after the reply did. Polled,
	// because the point is that they arrive LATER.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + h + " --tail 20", "timeout": 0}), "LATE-LINE") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the bytes the quiet wait left behind never became readable")
}

// A PIPE STILL REPORTS THE ONE THING A PIPE CAN: its process exiting.
//
// Completion is a pty statement. On a pipe the observable fact is the exit, and a
// quiet stream there means the process is waiting for input — a different sentence
// entirely, and one this header must not overwrite.
func TestAPipeRunDoesNotClaimQuiet(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "cat")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"hello-pipe"},
		"timeout": 10,
	})
	if v, ok := headerValue(t, got, "Completion"); ok {
		t.Fatalf("a pipe reply carried Completion=%q; a pipe's completion is its exit, which is reported as a status", v)
	}
}
