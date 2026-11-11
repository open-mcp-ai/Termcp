package mcp

import (
	"strings"
	"testing"
	"time"
)

// The three defects issue #161 reported that are defects in THIS code (the other
// three are either already fixed, already implemented, or not ours — see the reply on
// the issue). Each is pinned by the claim it makes about one parameter or one reply.

// TestOneTimeoutMeansOneBudget pins P1-3's first half: the caller's window is spent
// ONCE.
//
// The wait before a read (awaitWindow / awaitExit) burns the caller's budget, and the
// read that followed then passed the full timeout again — so `timeout: 60` could block
// for 120 seconds, and the reply said nothing about the second half. The read now gets
// what is LEFT.
func TestOneTimeoutMeansOneBudget(t *testing.T) {
	win := termArgs{TimeoutSeconds: 60}
	if got := remaining(win, 10*time.Second).TimeoutSeconds; got != 50 {
		t.Errorf("after a 10s wait of a 60s window, 50s remain; got %v", got)
	}
	// A wait that used the whole budget leaves nothing: the read must not block again.
	if got := remaining(win, 60*time.Second).TimeoutSeconds; got != 0 {
		t.Errorf("a spent window leaves no budget; got %v", got)
	}
	if got := remaining(win, 90*time.Second).TimeoutSeconds; got != 0 {
		t.Errorf("an overrun must clamp to zero, not go negative; got %v", got)
	}
	// And the cap that made one parameter mean two things is gone: a read is bounded by
	// the caller's window, the same number `run` uses.
	if got := readTimeout(termArgs{TimeoutSeconds: 300}); got != 300 {
		t.Errorf("readTimeout silently capped at 60 before; a 300s window is 300s, got %v", got)
	}
	if got := readTimeout(termArgs{TimeoutSeconds: 0}); got != 0 {
		t.Errorf("timeout 0 stays a non-blocking snapshot, got %v", got)
	}
}

// TestAWindowThatRanOutSaysSo pins P1-3's second half.
//
// A pty has no completion signal, so a `run` whose window expired returns the bytes so
// far with nothing to distinguish "the command finished" from "my window ended". The
// wait already computed which of the two happened and threw it away; it is now a header.
func TestAWindowThatRanOutSaysSo(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// A command that keeps printing for longer than the window: the stream never goes
	// quiet, so the wait can only end because the deadline arrived.
	long := noisyCommand("tick")
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{long},
		"timeout": 1,
	})
	// "tick 1", not "tick": the second is in the echo of the command line, so a reply
	// carrying nothing but the echoed line and a parse error used to satisfy it.
	if !strings.Contains(got, "tick 1") {
		t.Fatalf("the window should have caught some output:\n%s", got)
	}
	if !strings.Contains(got, "Still-Running: true") {
		t.Errorf("a pty run whose window expired must say the command is still running:\n%s", got)
	}
}

// TestAQuietCommandIsNotMarkedStillRunning is the other side of the same header: it
// must mean "the window ran out", not "this is a pty".
//
// The header was kept off the pty path precisely because it would otherwise be
// permanently true — a shell outlives every command it runs, so the channel is always
// alive. Pinning the negative keeps the fix from turning into that.
func TestAQuietCommandIsNotMarkedStillRunning(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// A command that prints and stops: the wait ends on quiet, long before the window.
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"echo zz-quiet-done"},
		"timeout": 20,
	})
	if !strings.Contains(got, "zz-quiet-done") {
		t.Fatalf("the command's output is missing:\n%s", got)
	}
	if strings.Contains(got, "Still-Running: true") {
		t.Errorf("a command that went quiet was reported as still running:\n%s", got)
	}
}

// TestAnEmptyListingSaysWhatToDoNext pins P2-5's one real defect: the empty `ls`
// answered "nothing is running" and stopped, so the caller's first move left it with no
// next move. `ls host` is the command that answers "what could I connect to", and it was
// a guess.
func TestAnEmptyListingSaysWhatToDoNext(t *testing.T) {
	s := newTestServer(t)
	out := termText(t, s, map[string]any{"cmd": "ls"})
	if !strings.Contains(out, "nothing is running") {
		t.Fatalf("this instance is not empty; the test needs a fresh server:\n%s", out)
	}
	if !strings.Contains(out, "ls host") {
		t.Errorf("an empty listing must name the next move:\n%s", out)
	}
}
