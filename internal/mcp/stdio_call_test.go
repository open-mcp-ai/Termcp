package mcp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/term/envelope"
)

// A ROUND TRIP IS ONE CALL, WHICHEVER WAY THE PEER WAS STARTED.
//
// `call` against a URL peer waits for the reply (peerChannel sets a deadline for it);
// against a stdio peer it wrote the frame and answered 202, so the same verb behaved
// differently depending on how the peer had been started — and the caller could not
// see which it had. Every round trip cost two calls, and the second one needed the
// caller to correlate replies by hand (issue #175).
//
// The peer here is a script that answers one request: the sleep proves the wait is a
// wait, and the id proves the reply that comes back is the answer to this call.
func TestAStdioCallWaitsForItsOwnReply(t *testing.T) {
	s := newTestServer(t)
	script := writeScript(t, `read line; printf '%s\n' '{"jsonrpc":"2.0","id":7,"result":{"ok":true}}'`)
	h := openPipe(t, s, "sh "+script)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "call " + h + ` {"jsonrpc":"2.0","id":7,"method":"tools/list"}`,
		"timeout": 20,
	})
	if !strings.Contains(got, `"result"`) {
		t.Fatalf("one call did not return the reply:\n%s", got)
	}
	// THE STATUS LINE, NOT THE WHOLE REPLY. This assertion used to be a substring test
	// for "202" over the entire reply, which the command echo satisfied by accident:
	// the random session id in `Path: call 548e1202-666:1 {...}` contains it. The test
	// then failed on CI for the id it happened to draw and passed locally for every
	// other one — this repo's own rule that an assertion must not be satisfiable by
	// the echo.
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("call answered something other than the reply:\n%s", got)
	}
}

// --send KEEPS THE OLD SHAPE, and it is the form that says "do not wait".
//
// Nothing was taken away by making `call` wait: the caller that wants fire-and-forget
// writes --send, and the CDP path already honoured it.
func TestSendDoesNotWait(t *testing.T) {
	s := newTestServer(t)
	script := writeScript(t, `read line; sleep 30`)
	h := openPipe(t, s, "sh "+script)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "call " + h + ` --send {"jsonrpc":"2.0","id":7,"method":"tools/list"}`,
		"timeout": 20,
	})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("--send waited for a reply that was never coming:\n%s", got)
	}
}

// ONE STALE FRAME DOES NOT MAKE THE NEXT CALL ANSWER THE WRONG QUESTION (#301).
//
// A frame that was already on the channel used to be handed to the next `call` with a
// warning attached: a 200 whose body answers a question nobody just asked, while the
// real answer stayed on the stream. The wait is now for the id the call sent; frames
// passed on the way are parked and `read --frame` hands them out, oldest first.
func TestACallWaitsForItsIDAndKeepsWhatItPasses(t *testing.T) {
	s := newTestServer(t)
	// Two frames are on the channel before the call: a notification (no id at all) and
	// a reply to an earlier request. Only the third answers what this call asks.
	script := writeScript(t, `printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/stale"}'; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"stale":true}}'; read line; printf '%s\n' '{"jsonrpc":"2.0","id":99,"result":{"fresh":true}}'`)
	h := openPipe(t, s, "sh "+script)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "call " + h + ` {"jsonrpc":"2.0","id":99,"method":"tools/list"}`,
		"timeout": 20,
	})
	if !strings.HasPrefix(got, "TERM/1 200") || !strings.Contains(got, "fresh") {
		t.Fatalf("the call did not return the reply to its own id:\n%s", got)
	}
	if strings.Contains(got, "stale") {
		t.Fatalf("a frame for another id was returned as this call's answer:\n%s", got)
	}
	// NOTHING WAS LOST. The two frames the call passed are on the parked queue, and the
	// order they come back in is the order they arrived in.
	first := termText(t, s, map[string]any{"cmd": "read " + h + " --frame"})
	if !strings.Contains(first, "notifications/stale") {
		t.Fatalf("the parked notification was not handed out first:\n%s", first)
	}
	second := termText(t, s, map[string]any{"cmd": "read " + h + " --frame"})
	if !strings.Contains(second, `"stale":true`) {
		t.Fatalf("the parked reply was not handed out:\n%s", second)
	}
}

// THE WINDOW IS ONE BUDGET FOR THE WHOLE WAIT, not one per frame it passes.
//
// A peer that never answers the id must end the call at the window the caller named;
// resetting the window per skipped frame would multiply the wait by the number of
// foreign frames on the stream. Twenty buffered frames and half a second: the fixed
// call returns in about the window, the resetting one takes ten seconds.
func TestTheWindowIsOneBudgetNotOnePerSkippedFrame(t *testing.T) {
	s := newTestServer(t)
	script := writeScript(t, strings.Repeat(`printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'; `, 20)+`sleep 30`)
	h := openPipe(t, s, "sh "+script)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	start := time.Now()
	got := termText(t, s, map[string]any{
		"cmd":     "call " + h + ` {"jsonrpc":"2.0","id":99,"method":"tools/list"}`,
		"timeout": 0.5,
	})
	elapsed := time.Since(start)
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("a call whose id never arrived answered something other than nothing-yet:\n%s", got)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the wait (%v) grew past the window: the budget is being reset per skipped frame", elapsed)
	}
	// The nothing-yet answer says where the frames it passed went, and they survive the
	// timeout: read --frame still hands them out.
	if !strings.Contains(got, "parked") {
		t.Errorf("the nothing-yet answer does not say what happened to the skipped frames:\n%s", got)
	}
	first := termText(t, s, map[string]any{"cmd": "read " + h + " --frame"})
	if !strings.Contains(first, `"id":1`) {
		t.Fatalf("the frames skipped before the timeout were lost:\n%s", first)
	}
}

// A WAITER LOOKS IN THE PARKED QUEUE FIRST, BY ID: its reply can have been skipped by
// a waiter that was after a later id, and the queue is the only way back to it.
func TestAParkedFrameIsTakenByIDWithoutDisturbingTheRest(t *testing.T) {
	s := newTestServer(t)
	s.enqueuePendingFrame("h", envelope.Slot{Body: `{"jsonrpc":"2.0","id":5}`})
	s.enqueuePendingFrame("h", envelope.Slot{Body: `{"jsonrpc":"2.0","id":7}`})

	slot, ok := s.takePendingFrame("h", "7")
	if !ok || frameID(slot.Body) != "7" {
		t.Fatalf("the parked reply for id 7 was not found: %v %q", ok, slot.Body)
	}
	slot, ok = s.popPendingFrame("h")
	if !ok || frameID(slot.Body) != "5" {
		t.Fatalf("taking by id disturbed the order of the rest: %v %q", ok, slot.Body)
	}
	if _, ok := s.popPendingFrame("h"); ok {
		t.Fatal("an empty queue answered as if it held a frame")
	}
}

// A NOTIFICATION HAS NO ANSWER, so waiting for one would be a lie. The reply says
// which it is rather than sitting on the caller's window.
func TestANotificationIsNotWaitedFor(t *testing.T) {
	s := newTestServer(t)
	script := writeScript(t, `read line; sleep 30`)
	h := openPipe(t, s, "sh "+script)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "call " + h + ` {"jsonrpc":"2.0","method":"notifications/initialized"}`,
		"timeout": 20,
	})
	if !strings.Contains(got, "notification") {
		t.Fatalf("a frame with no id was waited on:\n%s", got)
	}
}

// THE ID IS COMPARED AS SPELLED. A JSON-RPC id may be a string or a number, and `"1"`
// and `1` are different ids to the peer. A comparison that normalised them would call
// two frames the same one.
func TestTheIDComparisonKeepsTheSpelling(t *testing.T) {
	cases := []struct {
		frame string
		want  string
	}{
		{`{"jsonrpc":"2.0","id":1}`, "1"},
		{`{"jsonrpc":"2.0","id":"1"}`, `"1"`},
		// `id: null` is an error response's id, never a request's, so it is read as "no
		// id": there is nothing to wait for, and saying so beats waiting out the window.
		{`{"jsonrpc":"2.0","id":null}`, ""},
		{`{"jsonrpc":"2.0","method":"x"}`, ""},
		{`not json`, ""},
		{`{"jsonrpc":"2.0","id":{"nested":1}}`, `{"nested":1}`},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%.20s", c.frame), func(t *testing.T) {
			if got := frameID(c.frame); got != c.want {
				t.Fatalf("frameID(%s) = %q, want %q", c.frame, got, c.want)
			}
		})
	}
}
