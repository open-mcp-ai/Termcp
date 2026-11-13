package mcp

import (
	"runtime"
	"strings"
	"testing"
)

// TestSilentPtyRunNamesTheQueuedPossibility pins issue #308.
//
// The reporter read `Still-Running: true` on a run whose window expired with no output
// and concluded the new command was executing silently. It was not: a pty queues input
// until the foreground process reads it, so nothing in that reply could tell "my command
// is running silently" from "my command has not been read yet" — and the header asserted
// the first. `run` now keeps Still-Running for the case it is evidence of (bytes were
// arriving when the window ended) and names the ambiguity, plus the way out of it, when
// nothing arrived.
func TestSilentPtyRunNamesTheQueuedPossibility(t *testing.T) {
	s := newTestServer(t)
	argv := "sleep 10"
	if runtime.GOOS == "windows" {
		// -NoProfile, because a themed PowerShell prints its banner a second after it
		// starts: a pause longer than any quiet window, so a drain reports "quiet" while
		// the banner is still on its way and the next run's window catches it instead.
		argv = "powershell -NoLogo -NoProfile -Command Start-Sleep -Seconds 10"
	}
	opened := termText(t, s, map[string]any{"cmd": "connect internal", "timeout": float64(2)})
	sid := strings.Split(firstHandle(opened), ":")[0]
	out := termText(t, s, map[string]any{"cmd": "open " + sid + " --pty --argv " + argv, "timeout": float64(2)})
	h := firstHandle(out)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// NO awaitPtyReady HERE: this channel's foreground process is the sleep, so a probe
	// line would never be read. What has to be established is only that the nested
	// shell's start-up banner has stopped printing, which the drain does.
	awaitQuiet(t, s, h)

	// Send a command while the foreground sleep is holding the terminal.
	got := termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo test-input"}, "timeout": float64(1)})
	// 200 IS THE HONEST STATUS: the pty echoed the line, so the window is not empty. What
	// must not be there is a claim about work — and what must be there is the sentence
	// naming the two possibilities. Asserting 204 made a status code stand in for a
	// meaning, and it failed the moment a runner's tty echoed at a different moment.
	if strings.HasPrefix(got, "TERM/1 204") {
		if !strings.Contains(got, "Hint:") {
			t.Fatalf("a 204 for a queued command explained nothing:\n%s", got)
		}
	} else if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("a queued command answered neither 200 nor 204:\n%s", firstLine(got))
	}
	if strings.Contains(got, "Still-Running: true") {
		t.Errorf("an empty window is not evidence that the command is running:\n%s", got)
	}
	hint, ok := headerValue(t, got, "Hint")
	if !ok {
		t.Fatalf("a silent run must explain what silence does and does not mean:\n%s", got)
	}
	if !strings.Contains(hint, "ctrl+c") {
		t.Errorf("the hint must name the way to take the terminal back: %q", hint)
	}
}

// TestRunningPtyRunStillSaysStillRunning is the other side: when bytes were arriving and
// the window ended mid-stream, that IS evidence of work, and the header must stay.
func TestRunningPtyRunStillSaysStillRunning(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)
	awaitQuiet(t, s, h)

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{noisyCommand("tick")},
		"timeout": 1,
	})
	if !strings.Contains(got, "Still-Running: true") {
		t.Errorf("a window that expired while output was arriving must still report Still-Running:\n%s", got)
	}
	if _, ok := headerValue(t, got, "Hint"); ok {
		t.Errorf("a run with evidence of work must not carry the queued disclaimer:\n%s", got)
	}
}
