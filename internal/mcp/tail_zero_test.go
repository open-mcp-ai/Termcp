package mcp

import (
	"strings"
	"testing"
)

// TestTailZeroReturnsEmptyWindow pins that `--tail 0` returns zero lines rather than
// the entire buffer, and that `--tail 1` still returns the last line.
func TestTailZeroReturnsEmptyWindow(t *testing.T) {
	s := newTestServer(t)

	// A pipe that prints three lines and exits, so the log is complete and stable
	// by the time we read it.
	text := termText(t, s, map[string]any{
		"cmd": "pipe internal --argv printf 'LINE_ONE\\nLINE_TWO\\nLINE_THREE\\n'",
	})
	h := handleFrom(t, text)

	// WAIT FOR THE PROCESS, NOT FOR A READ.
	//
	// `read` is a snapshot: it returns what is in the buffer at that instant and does
	// not wait for anything. Using it as "wait until the output is complete" worked on
	// a fast machine and failed on a CI runner, where the read landed before the
	// program's bytes did — the reply was `Content-Range: bytes */0` and the assertions
	// below compared against an empty log.
	waitForShellToExit(t, s, shellIDFromHandle(t, s, h))

	// Tail 0: an empty window, not the whole log.
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --tail 0"})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("expected 204 for --tail 0, got:\n%s", got)
	}
	for _, bad := range []string{"LINE_ONE", "LINE_TWO", "LINE_THREE"} {
		if strings.Contains(got, bad) {
			t.Errorf("--tail 0 must not return log content (%s): %s", bad, got)
		}
	}

	// Tail 1: exactly the last line.
	got = termText(t, s, map[string]any{"cmd": "read " + h + " --tail 1"})
	if !strings.Contains(got, "LINE_THREE") {
		t.Errorf("--tail 1 should contain the last line: %s", got)
	}
	if strings.Contains(got, "LINE_ONE") {
		t.Errorf("--tail 1 must not contain the first line: %s", got)
	}
}

// shellIDFromHandle resolves a printed handle to the internal shell id that
// waitForShellToExit polls.
//
// A handle is `<session>:<index>` and the internal id is a bare uuid+index; the
// mapping goes through the same door every directive uses, so a test cannot drift
// from what the code resolves.
func shellIDFromHandle(t *testing.T, s *Server, handle string) string {
	t.Helper()
	cs, bad := s.requireShell(handle)
	if bad != nil {
		t.Fatalf("requireShell(%s): %s", handle, toolResultText(bad))
	}
	return cs.ID
}
