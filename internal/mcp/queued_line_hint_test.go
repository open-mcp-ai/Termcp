package mcp

import (
	"runtime"
	"strings"
	"testing"
	"time"
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
		argv = "powershell -NoLogo -Command Start-Sleep -Seconds 10"
	}
	opened := termText(t, s, map[string]any{"cmd": "connect internal", "timeout": float64(2)})
	sid := strings.Split(firstHandle(opened), ":")[0]
	out := termText(t, s, map[string]any{"cmd": "open " + sid + " --pty --argv " + argv, "timeout": float64(2)})
	h := firstHandle(out)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Wait for start-up banner (e.g. powershell theme) to finish printing.
	time.Sleep(3 * time.Second)
	for i := 0; i < 30; i++ {
		got := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(1)})
		if strings.HasPrefix(got, "TERM/1 204") {
			break
		}
	}

	// Send a command while the foreground sleep is holding the terminal.
	got := termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo test-input"}, "timeout": float64(1)})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("expected 204 No Content for a queued command:\n%s", got)
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
