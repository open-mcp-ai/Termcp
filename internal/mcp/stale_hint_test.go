package mcp

import (
	"strings"
	"testing"
)

// TestADirectoryHintNamesAFlagThatExists pins issue #146.
//
// A byte read of a directory is refused, but the refusal pointed at
// `--action stat` — a flag of the `file` directive, which the command-surface
// rewrite deleted. `--action` is not accepted anywhere now, so the hint sent the
// caller straight into a second error:
//
//	read s file /tmp
//	  -> 400  /tmp: is a directory; use --action stat to list its contents
//	read s file /tmp --action stat
//	  -> 400  unknown flag for read: --action
//
// The message is the only thing a caller has to go on, so it has to name a
// spelling this build actually answers. `--op stat` does (verified below by
// running it), and the shell can list a directory too — which the hint says, so
// a caller without a session-scoped file read still has a way forward.
func TestADirectoryHintNamesAFlagThatExists(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 5")

	dir := t.TempDir()
	refused := termText(t, s, map[string]any{"cmd": "read " + h + " file " + dir + " --mode hex"})
	if !strings.Contains(refused, "is a directory") {
		t.Fatalf("reading a directory was not refused as one:\n%s", refused)
	}
	// The removed spelling must not come back: it is the one word in this message
	// that a caller cannot act on.
	if strings.Contains(refused, "--action") {
		t.Errorf("the hint still names --action, which no directive accepts:\n%s", refused)
	}
	if !strings.Contains(refused, "--op stat") {
		t.Errorf("the hint does not name the flag that lists a directory:\n%s", refused)
	}

	// And the spelling it recommends has to work, or the hint is a second trap.
	listed := termText(t, s, map[string]any{"cmd": "get " + h + " file " + dir + " --op stat"})
	if code := statusCode(t, listed); code >= 400 {
		t.Errorf("the recommended `--op stat` answered %d; the hint points at nothing:\n%s", code, listed)
	}
}
