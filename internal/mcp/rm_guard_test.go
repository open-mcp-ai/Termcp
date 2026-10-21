package mcp

import (
	"strings"
	"testing"
)

// TestRmRunningSessionNeedsForce pins the guard against accidental erasure.
//
// `close` and `rm` look interchangeable and are not: close stops a session and keeps
// its transcript readable, while rm erases the record permanently. Accepting a
// running session without comment answered 204 for an action that wiped work in
// flight.
func TestRmRunningSessionNeedsForce(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "pty internal --name live"})
	h := handleFrom(t, text)
	sid := strings.Split(h, ":")[0]

	// 1. Without --force on a running session: 409 Conflict.
	got := termText(t, s, map[string]any{"cmd": "rm " + sid})
	if !strings.HasPrefix(got, "TERM/1 409") {
		t.Fatalf("expected 409 on a running session, got:\n%s", got)
	}
	for _, want := range []string{"is running", "--force", "close " + sid} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, got)
		}
	}

	// 2. The session survived the refusal.
	got = termText(t, s, map[string]any{"cmd": "info " + sid})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("the session should still exist after refusal:\n%s", got)
	}

	// 3. With --force: erased immediately, 204.
	got = termText(t, s, map[string]any{"cmd": "rm " + sid + " --force"})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("rm --force should succeed, got:\n%s", got)
	}

	// 4. Gone.
	got = termText(t, s, map[string]any{"cmd": "info " + sid})
	if !strings.HasPrefix(got, "TERM/1 404") {
		t.Fatalf("the session should be gone after rm --force:\n%s", got)
	}
}

// TestRmStoppedSessionDoesNotNeedForce pins that a stopped session has nothing left
// to lose, so ceremony is not added where no danger exists.
func TestRmStoppedSessionDoesNotNeedForce(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "pty internal --name dead"})
	h := handleFrom(t, text)
	sid := strings.Split(h, ":")[0]

	// Stop it first.
	termText(t, s, map[string]any{"cmd": "close " + sid + " --force"})

	// rm without --force: succeeds because it is already stopped.
	got := termText(t, s, map[string]any{"cmd": "rm " + sid})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("rm on a stopped session should succeed without --force, got:\n%s", got)
	}
}

// TestRmRefusesAChannelHandle pins the TargetKind declaration.
//
// `rm <session>:1` used to resolve the locator, find the session, and erase the whole
// container. A caller who named a channel asked about that channel, and erasing
// everything is a bigger answer than the question.
func TestRmRefusesAChannelHandle(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "rm a1f3:1"})
	if !strings.HasPrefix(got, "TERM/1 400") {
		t.Fatalf("expected 400 on a channel handle, got:\n%s", got)
	}
	for _, want := range []string{"needs a SESSION", "not the channel", "close a1f3:1"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, got)
		}
	}
}
