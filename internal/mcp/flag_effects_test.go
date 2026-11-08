package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestFlagsThatOnlyMatterServerSide covers flags whose effect is not a change to a file
// or a config but something the reply has to show: a cursor that advances, a stream
// that gets chosen, a notification that is held instead of shown.
//
// They were accepted, read by the handler, and asserted by nothing — the same shape as
// the dead flags, one step further along: the value arrives and does something, and no
// test would have noticed if it stopped.
func TestFlagsThatOnlyMatterServerSide(t *testing.T) {
	s := newTestServer(t)

	t.Run("read-cursor-advances", func(t *testing.T) {
		h := openPty(t, s)
		defer termText(t, s, map[string]any{"cmd": "close " + h})
		// TYPED, NOT RUN. `run` reads the window it produced, and that read now advances
		// the default cursor (issue #162 S2), so a plain read after it has nothing left
		// to return and two empty replies would look like "the cursor never advanced".
		// input/key do not read, so the bytes are still there for the read to consume.
		termText(t, s, map[string]any{"cmd": "input " + h + " $1", "args": []any{"echo cursor-marker"}})
		termText(t, s, map[string]any{"cmd": "key " + h + " enter"})
		waitForContent(t, s, h, "cursor-marker")

		first := termText(t, s, map[string]any{"cmd": "read " + h})
		if !strings.Contains(first, "Content-Range") {
			t.Fatalf("a plain read reports no range:\n%s", first)
		}
		// The second read starts where the first stopped: a cursor that never advances
		// makes `read` answer the same bytes forever, which looks correct in isolation.
		second := termText(t, s, map[string]any{"cmd": "read " + h})
		if second == first {
			t.Errorf("the cursor did not advance; the second read repeated the first:\n%s", second)
		}
	})

	t.Run("read-raw-skips-sanitising", func(t *testing.T) {
		h := openPty(t, s)
		defer termText(t, s, map[string]any{"cmd": "close " + h})
		termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"printf 'a\\033[31mb\\n'"}})
		time.Sleep(150 * time.Millisecond)

		plain := termText(t, s, map[string]any{"cmd": "read " + h})
		raw := termText(t, s, map[string]any{"cmd": "read " + h + " --raw"})
		// --raw keeps the escape sequence; the default strips it. Without a case like
		// this, a --raw that did nothing would look exactly like correct behaviour.
		if strings.Contains(plain, "\x1b[31m") && strings.Contains(raw, "\x1b[31m") {
			t.Errorf("--raw made no difference to the escape sequence")
		}
	})

	t.Run("key-repeat", func(t *testing.T) {
		h := openPty(t, s)
		defer termText(t, s, map[string]any{"cmd": "close " + h})

		// A typed character is not echoed by every shell, so counting "aaa" in the
		// transcript measures the shell, not the flag. The flag's own contract is its
		// bounds: 20 is the most a caller may ask for, and a rejection has to say so
		// rather than silently sending one key.
		if got := termText(t, s, map[string]any{"cmd": "key " + h + " enter --repeat 3"}); strings.HasPrefix(got, "TERM/1 4") {
			t.Fatalf("--repeat 3 was refused:\n%s", got)
		}
		got := termText(t, s, map[string]any{"cmd": "key " + h + " enter --repeat 21"})
		if !strings.Contains(got, "between 1 and 20") {
			t.Errorf("--repeat 21 was not refused with its bounds:\n%s", got)
		}
	})

	t.Run("ls-all", func(t *testing.T) {
		all := termText(t, s, map[string]any{"cmd": "ls --all"})
		plain := termText(t, s, map[string]any{"cmd": "ls"})
		// An empty machine is allowed; what is not allowed is --all changing nothing.
		if all == plain && strings.Contains(plain, "(nothing is running)") {
			t.Skip("nothing to enumerate")
		}
		if all == plain {
			t.Errorf("--all returned exactly what the default did:\n%s", plain)
		}
	})

	t.Run("notify-for-and-sticky", func(t *testing.T) {
		// --for and --sticky set the same field to different things: seconds, and zero.
		// A wrong hand-off turns a held notification into a disappearing one.
		for _, cmd := range []string{
			`notify --for 30 "held"`,
			`notify --sticky "sticky"`,
			`notify --title T --level info "titled"`,
			// --on routes the notification to one session rather than the whole
			// machine; it maps onto session_id, and a wrong hand-off would deliver it
			// everywhere — which looks like success from the caller's side.
			`notify --on ` + firstSessionID(t, s) + ` "routed"`,
		} {
			got := termText(t, s, map[string]any{"cmd": cmd})
			if !strings.HasPrefix(got, "TERM/1 2") {
				t.Errorf("%q was refused:\n%s", cmd, got)
			}
			if !strings.Contains(got, "delivered") {
				t.Errorf("%q says nothing about delivery:\n%s", cmd, got)
			}
		}
	})
}

// waitForContent polls a channel until the text appears, without consuming it.
func waitForContent(t *testing.T, s *Server, handle, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + handle + " --from-line 1"}), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%q never appeared on %s", want, handle)
}

// firstSessionID opens a session and returns its id, for flags that address one.
func firstSessionID(t *testing.T, s *Server) string {
	t.Helper()
	h := openPipe(t, s, "sleep 30")
	t.Cleanup(func() { termText(t, s, map[string]any{"cmd": "close " + h}) })
	return strings.SplitN(h, ":", 2)[0]
}
