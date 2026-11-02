package mcp

import (
	"strings"
	"testing"
	"time"
)

// A PIPE THAT STAYS ALIVE STILL HAS TO ANSWER, and this is the cost half of that.
//
// `run` on a pipe waited for one thing only: the process exiting. That is the right
// signal for a program the caller started to do one job, and it is no signal at all
// for a resident one — a shell kept open across calls, a REPL, anything that reads a
// line and stays alive. Those never exit, so every `run` burned the caller's whole
// window on a command that had already answered: measured on a resident echo process,
// a 1s window took 1.0065s and a 3s window took 3.0068s, linear in the window and not
// in the command (issue #207).
//
// The wait now also ends when the output stops moving after this call's bytes, which
// is how a resident process reports that it is ready for the next line.
func TestAPipeRunEndsWhenTheCommandStopsTalking(t *testing.T) {
	s := newTestServer(t)
	// `cat` is the resident case in its simplest form: it answers every line and
	// never exits.
	h := openPipe(t, s, "cat")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	start := time.Now()
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"hello-resident"},
		"timeout": 10,
	})
	elapsed := time.Since(start)

	if !strings.Contains(got, "hello-resident") {
		t.Fatalf("the command's answer is missing from the reply:\n%s", firstLine(got))
	}
	// Half the window is the generous line: a tenth of a second of quiet is the
	// expectation, and a wait that went back to sleeping the window would land at 10s.
	if elapsed > 5*time.Second {
		t.Fatalf("a 10s window on a resident pipe that answered at once took %v", elapsed)
	}
}

// AND A COMMAND THAT SAYS NOTHING IS NOT A COMMAND THAT HAS FINISHED.
//
// This is the guard on the other side of the fix, and it is the reason the wait
// watches the stream from a baseline instead of from zero. A resident process is
// silent before it has read anything and silent after a command that prints nothing;
// treating "quiet" as "done" without requiring a byte first would answer every silent
// command instantly with an empty reply — inventing a completion the server cannot
// see, which is the failure this whole wait exists to avoid.
func TestAPipeRunWaitsWhenTheCommandSaysNothing(t *testing.T) {
	s := newTestServer(t)
	// A pipe that never prints and never exits: the wait has nothing to watch.
	h := openPipe(t, s, "sleep 30")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	start := time.Now()
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"ignored"},
		"timeout": 1,
	})
	elapsed := time.Since(start)

	// It must still be running: a silent pipe whose process is alive has nothing to
	// report about completion, which the reply says rather than implies.
	if v, ok := headerValue(t, got, "Still-Running"); !ok || v != "true" {
		t.Fatalf("a pipe that was still running did not say so (Still-Running=%q, present=%v):\n%s", v, ok, firstLine(got))
	}
	// Most of the window, not all of it: a wait that returned early here would land
	// near the quiet window and pass a looser bound while being wrong.
	if elapsed < 700*time.Millisecond {
		t.Fatalf("a silent command returned after %v of a 1s window; quiet before the first byte is not an answer", elapsed)
	}
}
