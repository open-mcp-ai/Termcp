package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestAPtyOpenWaitsForTheShellToSpeak is issue #156.
//
// The handle used to be handed out as soon as the process existed, and the shell's line
// editor — PSReadLine, readline, zle — takes over the terminal a moment later. Input that
// arrives in between is DISCARDED rather than buffered, and nothing says so: `run` answers
// 200 and the command never happens, on every channel of that session, forever. On a
// machine whose PowerShell draws a themed prompt this was reproducible roughly one time in
// three.
//
// The fix is the smallest one that can be correct: a pty open waits for the shell to say
// something before it answers. A shell that has printed its prompt has installed its editor,
// and that is the only signal it gives.
func TestAPtyOpenWaitsForTheShellToSpeak(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// THE WRITE HAPPENS IMMEDIATELY, which is the whole point: no sleep, no wait for a
	// prompt, because the open already waited.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo ready-marker"}})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1", "timeout": 0}), "ready-marker") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a command written immediately after the open never reached the shell")
}

// TestAPipeOpenDoesNotWait pins the other half of the rule: a pipe's bytes go into the
// process's own stdin, which the OS buffers, so there is nothing to wait for and waiting
// would be a lie — `--argv sleep 30` prints nothing for thirty seconds by design.
func TestAPipeOpenDoesNotWait(t *testing.T) {
	s := newTestServer(t)

	// THE BOUND IS RELATIVE, NOT ABSOLUTE (see docs/agents/testing.md).
	//
	// An absolute budget here measures the machine, not the rule: `connect --argv`
	// has to create a session and start a process, which is ~220ms on a laptop and
	// can be several times that on a loaded runner — so a fixed 300ms says "the
	// pipe open waited" when what it really measured was a slow machine. The rule
	// is that a pipe open does not wait for OUTPUT, and that is what the comparison
	// below isolates: the same open is performed twice, once in each mode, and the
	// pipe must not be the slower of the two.
	//
	// `cat` prints nothing until it is written to, so if the open waited for output
	// it would sit out the whole ready budget. A pty open, which legitimately does
	// wait for its shell to answer, is the yardstick.
	ptyStart := time.Now()
	pty := openPty(t, s)
	ptyElapsed := time.Since(ptyStart)
	defer termText(t, s, map[string]any{"cmd": "close " + pty})

	start := time.Now()
	h := openPipe(t, s, "cat")
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	elapsed := time.Since(start)

	// Generous slack: process startup dominates both numbers and varies run to run,
	// so this asks only that the pipe did not wait out a ready budget ON TOP of the
	// same startup the pty paid for.
	if elapsed > ptyElapsed+shellReadyBudget {
		t.Fatalf("a pipe open waited %v where the same pty open took %v: a pipe does not wait for output",
			elapsed, ptyElapsed)
	}

	// And its input still arrives: a pipe buffers, so the bytes are there when the
	// process reads them.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"hello-pipe"}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// A zero timeout makes the read a peek: without it every poll would wait out the
		// tool's blocking window, and the loop would take three seconds per look.
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1", "timeout": 0}), "hello-pipe") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("input written to a pipe channel never appeared in its echo")
}

// TestAChildChannelIsAlsoReadyWhenItAnswers pins that the wait covers `open` as well as
// `connect`: a second channel starts a second process, with its own editor to install.
func TestAChildChannelIsAlsoReadyWhenItAnswers(t *testing.T) {
	s := newTestServer(t)
	first := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + first})
	sid, _, _ := strings.Cut(first, ":")

	second := handleFrom(t, termText(t, s, map[string]any{"cmd": "open " + sid + " --name s2"}))
	defer termText(t, s, map[string]any{"cmd": "close " + second})

	termText(t, s, map[string]any{"cmd": "run " + second + " $1", "args": []any{"echo second-marker"}})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + second + " --from-line 1", "timeout": 0}), "second-marker") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a command written immediately after a child channel opened never reached its shell")
}
