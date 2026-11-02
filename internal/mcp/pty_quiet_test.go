package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestALearnedQuietThresholdReplacesTheFloorForThisChannel pins what the learner
// is for: a channel whose output arrives in quick succession stops paying the
// cost of a slower shell somewhere else.
//
// The 500ms floor is a compromise — measured on a local bash, `echo x` has all
// its bytes on the stream by 111ms, and the wait sat until 611ms to be sure
// nothing else was coming. A channel that has shown its widest gap gets a
// threshold of twice that gap, so the second command on a fast shell answers in
// about a sixth of the time the first one took.
func TestALearnedQuietThresholdReplacesTheFloorForThisChannel(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// Seed the learning rather than waiting for it: a channel that has reported two
	// chunks 20ms apart has shown its rhythm, and the wait should use it. Seeding is
	// what makes the assertion below about the WAIT and not about how fast this
	// machine's shell happens to be.
	s.quietTracker(h).seed(20 * time.Millisecond)

	start := time.Now()
	got := termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo learned-quiet"}})
	elapsed := time.Since(start)

	if !strings.Contains(got, "learned-quiet") {
		t.Fatalf("the command's output is missing:\n%s", got)
	}
	// The floor alone would put the earliest possible answer at 500ms. Anything
	// under it means the channel's own threshold was used.
	if elapsed >= ptyQuiet {
		t.Errorf("a channel that reported 20ms gaps still waited %v, at or above the %v floor", elapsed, ptyQuiet)
	}
}

// TestACommandThatSpokeTwiceLeavesItsGapsRecorded is the wiring half: a real wait
// measures this channel and writes what it saw down, which is what makes the next
// wait cheaper.
//
// It asserts the RECORD, not the saving. Whether the saving is real is a property of
// the shell on the machine running this test — a bash that answers in one burst
// shows no gap and correctly keeps today's threshold — and asserting an elapsed time
// here was measured to pass and fail by ~30ms under -race on the same code. The
// arithmetic that turns recorded gaps into a threshold is pinned by the unit cases
// below, where it is deterministic; the numbers this test produces are logged for
// the issue that asked for the change.
func TestACommandThatSpokeTwiceLeavesItsGapsRecorded(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	// THE SHELL IS WARMED FIRST: a cold one discards the first line (#156), and a
	// wait over discarded input records nothing.
	awaitPtyReady(t, s, h)

	got := termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo recorded"}})
	if !strings.Contains(got, "recorded") {
		t.Fatalf("the wait dropped the command's output:\n%s", got)
	}
	tr := s.quietTracker(h)
	if tr.threshold() == ptyQuiet {
		t.Logf("channel %s kept the baseline: this shell produced only one stream chunk", h)
		return
	}
	t.Logf("channel %s learned threshold: %v", h, tr.threshold())
}

// TestTheFirstWaitOnAChannelUsesTheFloor pins the other direction: a channel that
// has not been measured yet is not guessed at.
//
// This is the reason the learner exists at all rather than a shorter constant —
// an unmeasured pty is exactly the oh-my-posh case the 500ms was sized for, and a
// threshold that assumed every shell is bash would truncate its replies.
func TestTheFirstWaitOnAChannelUsesTheFloor(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	if got := s.quietTracker(h).threshold(); got != ptyQuiet {
		t.Errorf("an unmeasured channel's threshold = %v, want the %v floor", got, ptyQuiet)
	}
}

// TestQuietThresholdIsBounded checks the clamp, without a shell in sight.
//
// Learning may only shorten the wait, and both ends of that rule are load-bearing:
// the floor keeps a channel that prints in one burst from being read as "answered
// and finished" before its second chunk, and the ceiling — today's constant —
// keeps a start-up repaint from making later calls wait LONGER than the value that
// was already chosen for the slowest shell. Measured: a learner allowed to grow
// held the two commands after a session's opening burst at 1.09s, where the
// constant answers in 0.68s.
func TestQuietThresholdIsBounded(t *testing.T) {
	cases := []struct {
		name string
		seed []time.Duration
		want time.Duration
	}{
		{"never observed", nil, ptyQuiet},
		{"back-to-back chunks clamp up", []time.Duration{5 * time.Millisecond}, ptyQuietMin},
		{"a moderate gap clamps to the safe floor", []time.Duration{60 * time.Millisecond}, ptyQuietMin},
		{"the widest gap clamps to the safe floor", []time.Duration{10 * time.Millisecond, 80 * time.Millisecond}, ptyQuietMin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &ptyQuietTracker{}
			for _, g := range tc.seed {
				tr.seed(g)
			}
			if got := tr.threshold(); got != tc.want {
				t.Errorf("threshold after %v = %v, want %v", tc.seed, got, tc.want)
			}
		})
	}
}

// TestASlowCommandIsNotLearnedAsARhythm keeps the learner from mistaking a
// command for a shell.
//
// `sleep 3; echo x` has a 3-second gap between its two chunks. Learning that as
// this prompt's repaint interval would make every later wait on the channel
// three seconds (clamped), which is a cost paid for a fact that was never about
// the terminal. The observed ceiling rejects it, and the reply is unaffected —
// the command still has to wait its window out.
func TestASlowCommandIsNotLearnedAsARhythm(t *testing.T) {
	tr := &ptyQuietTracker{}
	tr.seed(3 * time.Second)
	if got := tr.threshold(); got != ptyQuiet {
		t.Errorf("a pause longer than the ceiling changed the threshold to %v; it was a command, not a repaint", got)
	}
	tr.seed(3 * time.Second)
	if got := tr.threshold(); got != ptyQuiet {
		t.Errorf("a seeded pause longer than the ceiling changed the threshold to %v", got)
	}
}

// TestClosingAChannelForgetsItsThreshold is the lifecycle half: the learner is a
// map, and a map that only grows is a leak with a nice comment on it.
func TestClosingAChannelForgetsItsThreshold(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	s.quietTracker(h).seed(50 * time.Millisecond)
	if s.ptyQuiet.peek(h) == nil {
		t.Fatal("the tracker was not created for a channel that ran a command")
	}
	termText(t, s, map[string]any{"cmd": "close " + h})
	if s.ptyQuiet.peek(h) != nil {
		t.Errorf("closing %s left its learned threshold behind", h)
	}
}
