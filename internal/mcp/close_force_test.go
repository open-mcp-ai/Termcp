package mcp

import (
	"strings"
	"testing"
)

// TestCloseForceTerminatesSession pins issue #132:
//
//	close <session> --force must terminate the whole session.
//
// The bug: directiveClose always tried `requireShell(target)` first. Because
// requireShell falls back to a session's primary shell when target is a session id,
// the first branch ALWAYS succeeded and routed to `shell_close`. The subsequent
// `session_terminate` branch was dead code, and `--force` was silently dropped.
//
// On an internal session this was a complete silent no-op: closing an internal
// primary shell is deliberately a no-op (the process outlives tabs), so the call
// returned 204 No Content while the session kept running — identical symptom to #131:
// 204 returned, nothing done.
func TestCloseForceTerminatesSession(t *testing.T) {
	s := newTestServer(t)

	// Open an internal pty. Handle is like "sid:1", and session id is "sid".
	h := openPty(t, s)
	sessID := h
	if idx := strings.Index(h, ":"); idx > 0 {
		sessID = h[:idx]
	}

	// 1. Verify the session is initially running in ps.
	psBefore := termText(t, s, map[string]any{"cmd": "ps"})
	if !strings.Contains(psBefore, sessID) {
		t.Fatalf("session %q must be running in ps before close:\n%s", sessID, psBefore)
	}

	// 2. Run `close <session> --force`.
	closeRes := termText(t, s, map[string]any{"cmd": "close " + sessID + " --force"})
	if !strings.Contains(closeRes, "204") {
		t.Fatalf("close --force must return 204:\n%s", closeRes)
	}

	// 3. The session must no longer be running. In ps, it should either be gone
	// or marked exited/DEAD, never "running".
	psAfter := termText(t, s, map[string]any{"cmd": "ps"})
	for _, line := range strings.Split(psAfter, "\n") {
		if strings.Contains(line, sessID) && strings.Contains(line, "running") {
			t.Errorf("session %q is still running after `close %s --force`:\n%s", sessID, sessID, psAfter)
		}
	}

	// 4. info <session> must reflect that it is closed/dead.
	info := termText(t, s, map[string]any{"cmd": "info " + sessID})
	if strings.Contains(info, "status: running") {
		t.Errorf("info %s reports status: running after close --force:\n%s", sessID, info)
	}
}

// TestCloseWithHandleAndForceTerminatesWholeSession verifies that passing `--force`
// with a channel handle (like `sid:1` copied from a terminal) still resolves to
// terminating the container, as documented.
func TestCloseWithHandleAndForceTerminatesWholeSession(t *testing.T) {
	s := newTestServer(t)

	h := openPty(t, s)
	sessID := h
	if idx := strings.Index(h, ":"); idx > 0 {
		sessID = h[:idx]
	}

	// Caller copied `sid:1` directly and typed `close sid:1 --force`
	closeRes := termText(t, s, map[string]any{"cmd": "close " + h + " --force"})
	if !strings.Contains(closeRes, "204") {
		t.Fatalf("close %s --force must return 204:\n%s", h, closeRes)
	}

	// The session itself must be terminated.
	info := termText(t, s, map[string]any{"cmd": "info " + sessID})
	if strings.Contains(info, "status: running") {
		t.Errorf("info %s reports running after `close %s --force`:\n%s", sessID, h, info)
	}
}

// TestCloseNoSuchTargetReturns404 pins that non-existent handles or sessions return
// 404 instead of pretending success with 204.
func TestCloseNoSuchTargetReturns404(t *testing.T) {
	s := newTestServer(t)

	for _, cmd := range []string{
		"close nosuchhandle123",
		"close nosuchsession456 --force",
	} {
		res := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.Contains(res, "404") {
			t.Errorf("%q must return 404, got:\n%s", cmd, res)
		}
	}
}

// TestCloseChannelWithoutForceOnlyClosesThatChannel pins the other half:
//
//	close <handle> (without --force) closes only that channel; sibling channels
//	and the parent session stay alive.
func TestCloseChannelWithoutForceOnlyClosesThatChannel(t *testing.T) {
	s := newTestServer(t)

	// Open a session with its primary channel.
	h1 := openPty(t, s)
	sessID := h1
	if idx := strings.Index(h1, ":"); idx > 0 {
		sessID = h1[:idx]
	}

	// Open a SECOND channel on the same session. A pipe channel must carry a
	// command (a pipe runs a program, not a login shell), so give it one that stays
	// alive long enough for the test to act.
	res2 := termText(t, s, map[string]any{"cmd": "pipe " + sessID + " --name ch2 --argv sleep 60"})
	if !strings.Contains(res2, "200 OK") {
		t.Fatalf("failed to add second channel: %s", res2)
	}
	h2 := handleFrom(t, res2)
	if h2 == "" || h2 == h1 {
		t.Fatalf("expected distinct handle for second channel, got %q (h1=%q)", h2, h1)
	}

	// Close channel 2 without force.
	closeRes := termText(t, s, map[string]any{"cmd": "close " + h2})
	if !strings.Contains(closeRes, "204") {
		t.Fatalf("close %s must return 204:\n%s", h2, closeRes)
	}

	// Channel 2 must be closed/exited, but the session itself must still be running.
	infoSess := termText(t, s, map[string]any{"cmd": "info " + sessID})
	if !strings.Contains(infoSess, "status: running") {
		t.Errorf("parent session %s should still be running after closing secondary channel %s:\n%s", sessID, h2, infoSess)
	}
}

// TestBareShellIDIsNotAnExternalHandle pins the layering decision: the term
// protocol's caller-facing surface contains exactly ONE way to name a channel
// (`<session>:<index>`, what `ps` prints), while the raw shell id stays an
// internal implementation detail.
//
// The two must not be confused. A shell id is what the SSH transport and the log
// files are keyed by; a handle is what a caller types. Exposing the id as a second
// spelling would mean two names for one thing, and every reader would have to know
// both — which is the class of ambiguity that produced #132.
func TestBareShellIDIsNotAnExternalHandle(t *testing.T) {
	s := newTestServer(t)

	h := openPty(t, s)
	sessID := h
	if idx := strings.Index(h, ":"); idx > 0 {
		sessID = h[:idx]
	}
	sess := s.sessMgr.Get(sessID)
	if sess == nil {
		t.Fatalf("session %q missing", sessID)
	}
	rawShellID := sess.PrimaryShellID()
	if rawShellID == "" {
		t.Fatal("primary shell has no id")
	}

	// The handle `ps` prints works.
	if got := termText(t, s, map[string]any{"cmd": "run " + h + " echo via-handle"}); !strings.Contains(got, "via-handle") {
		t.Errorf("run %s did not reach the channel:\n%s", h, got)
	}

	// The raw shell id is NOT a handle at the term layer.
	got := termText(t, s, map[string]any{"cmd": "run " + rawShellID + " echo via-raw-id"})
	if !strings.Contains(got, "400") {
		t.Errorf("a raw shell id must not be accepted as a handle:\n%s", got)
	}
	if strings.Contains(got, "via-raw-id") {
		t.Errorf("the raw shell id was actually written to:\n%s", got)
	}
}
