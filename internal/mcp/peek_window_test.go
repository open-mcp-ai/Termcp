package mcp

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The window is a PEEK BUDGET, not a wait for completion: a caller looks at what a
// long command has said so far and comes back later. Two things break that contract,
// and both are in the wait rather than in the reply.

// TestAFullWindowEndsTheWait pins the one that wastes the caller's time: when the reply
// cannot grow any further, waiting for the rest of the window buys nothing.
//
// A noisy command (a scan, a brute force) never goes quiet, so the quiet detector never
// fires and the wait ran to the caller's full window — while the reply could never hold
// more than `bytes_limit`. The reported symptom was a client-side timeout on a 320s
// window over a command that had already filled the reply in the first second.
func TestAFullWindowEndsTheWait(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// Prints steadily for longer than the window: never quiet, never done.
	noisy := noisyCommand("line")
	start := time.Now()
	got := termText(t, s, map[string]any{
		"cmd":         "run " + h + " $1",
		"args":        []any{noisy},
		"bytes_limit": 200,
		"timeout":     10,
	})
	elapsed := time.Since(start)

	// THE WINDOW FILLED, and the way to see that is the RANGE, not the body. Two earlier
	// versions of this assertion were both wrong for the same reason — they looked for
	// text — and neither could work:
	//
	//   - "the reply contains `line`" was satisfied by the ECHO of the command line, so it
	//     passed whatever happened to the command.
	//   - "the body is not empty" fails when the 200 bytes land inside a burst of terminal
	//     control sequences: PowerShell's line editor redraws as the command is typed, and
	//     a window of pure cursor movement renders to nothing while being 200 real bytes.
	//
	// A THIRD VERSION ASSERTED 206 AND WAS ALSO WRONG, for a reason worth keeping: this
	// read starts at the reader cursor, so `start_offset` is where the caller left off and
	// not a withheld prefix — the reply holds everything the caller had not seen. Its
	// status is 206 only when the stream grew between the read and the reply, which for a
	// noisy command is a coin toss. The window filling is a fact about the RANGE: the span
	// the reply covers is exactly the caller's byte_limit, which cannot happen by accident
	// and does not depend on what the stream did in the microseconds after the read.
	if !statusLineIs(got, "206") && !statusLineIs(got, "200") {
		t.Fatalf("a reply about bytes has to say how it went:\n%s", got)
	}
	rng := regexp.MustCompile(`Content-Range: bytes (\d+)-(\d+)/(\d+)`).FindStringSubmatch(got)
	if rng == nil {
		t.Fatalf("a partial reply has to say which bytes it covered:\n%s", got)
	}
	lo, _ := strconv.Atoi(rng[1])
	hi, _ := strconv.Atoi(rng[2])
	if span := hi - lo + 1; span != 200 {
		t.Fatalf("the window is the caller's 200 bytes and the reply covers %d (%s):\n%s", span, rng[0], got)
	}
	// The wait has to end on the WINDOW, not on the clock. 10s is the caller's window; a
	// full 200 bytes arrives in about a second. The bound is 7s rather than 3s because a
	// loaded runner spent 3.0s here once, which is the machine and not the behaviour.
	if elapsed > 7*time.Second {
		t.Errorf("a full byte window should end the wait; waited %v of a 10s window", elapsed)
	}
	// And the caller has to be told the command is not done, or "here is a slice" and
	// "here is everything" look the same.
	if !strings.Contains(got, "Still-Running: true") {
		t.Errorf("a window cut short by its own size must say the command is still running:\n%s", got)
	}
}

// TestWaitedCoversTheWholeCall pins the header that reported the wrong number.
//
// `Waited` is how a caller tells "I waited and heard nothing" from "I did not wait at
// all" — the distinction that keeps a peek from looking like a broken channel. It was
// measured from the READ, so the wait before it (which is where the window was spent)
// was missing: a call that spent a full second waiting answered `Waited: 0ms`, and the
// reporting caller read that as "the window never happened".
func TestWaitedCoversTheWholeCall(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// THE ASSERTION IS "THE WAIT IS IN THERE", NOT "IT EQUALS THE WINDOW".
	//
	// A 1s window is no longer a promise of 1000ms: a pty wait ends when the stream goes
	// still (issue #156), and the echo of the typed command is output — so a legal reply
	// can come back at 600ms. Pinning 900ms made this test fail on a loaded machine for
	// doing the right thing. What the header has to satisfy is that it covers the whole
	// CALL: the bug was measuring it from the read alone, which reported ~0ms while the
	// call spent its window waiting. So the yardstick is the call's own duration.
	start := time.Now()
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"sleep 2"},
		"timeout": 1,
	})
	elapsed := time.Since(start)
	waited := headerValueOr(got, "Waited", "")
	if waited == "" {
		t.Fatalf("a call that waited has to say how long:\n%s", got)
	}
	ms, err := strconv.Atoi(strings.TrimSuffix(waited, "ms"))
	if err != nil {
		t.Fatalf("Waited is not a duration: %q", waited)
	}
	// Slack for the transport either side of the server's own clock; the failure this
	// guards against was off by the entire wait, not by a millisecond.
	if slack := int(elapsed.Milliseconds()) - 250; ms < slack {
		t.Errorf("Waited reports %dms for a call that took %dms: the wait is missing from it", ms, elapsed.Milliseconds())
	}
}
