package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestEmptyReadEndsOnTheQuietWindow pins issue #307: a read with nothing new returns
// as soon as the channel is quiet, not when the caller's timeout runs out.
func TestEmptyReadEndsOnTheQuietWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// The read under test is the empty-window case, so the start-up output has to be
	// gone first — established by the server's own 204, not by a sleep.
	awaitQuiet(t, s, h)

	start := time.Now()
	got := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(30)})
	elapsed := time.Since(start)
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("an empty read must answer 204:\n%s", got)
	}
	if elapsed > 5*time.Second {
		t.Errorf("an empty read spent %v of a 30s window; it must end when the stream is quiet", elapsed)
	}
	if v, ok := headerValue(t, got, "Completion"); !ok || v != "quiet" {
		t.Errorf("an empty read that ended on quiet must say so (Completion=%q present=%v):\n%s", v, ok, got)
	}
	if !strings.Contains(got, "Content-Range: bytes */") {
		t.Errorf("the 204 must still carry the cursor:\n%s", got)
	}
}

// TestReadWaitSpendsTheWholeWindow pins the escape hatch: --wait keeps the long poll.
func TestReadWaitSpendsTheWholeWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)
	awaitQuiet(t, s, h)

	start := time.Now()
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --wait", "timeout": float64(1.5)})
	elapsed := time.Since(start)
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("an empty long poll must answer 204:\n%s", got)
	}
	if elapsed < time.Second {
		t.Errorf("--wait returned after %v of a 1.5s window; it must spend the window", elapsed)
	}
	if _, ok := headerValue(t, got, "Completion"); ok {
		t.Errorf("a window that ran out did not end on quiet:\n%s", got)
	}
}
