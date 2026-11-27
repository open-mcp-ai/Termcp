package mcp

import (
	"strings"
	"testing"
)

// TestFoldSaysWhatItHid pins feedback §5: a fold used to answer 206 with an empty
// body and a range, which is exactly what "no new output" looks like too. The caller
// could not tell "folded on purpose" from "nothing there", and had to trust bytes
// existed somewhere it could not name.
func TestFoldSaysWhatItHid(t *testing.T) {
	// A PIPE, for the same reason as the test below: this asserts on a HEADER and on a
	// line count, and it used a pty whose missing completion signal turned "the output has
	// not arrived yet" into "the fold never happened" on a loaded machine.
	s := newTestServer(t)
	h := openPipe(t, s, "sh")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"seq 1 50"}, "timeout": 10})
	waitForGrepContent(t, s, h, "\n50\n")

	got := termText(t, s, map[string]any{
		"cmd":               "read " + h + " --from-line 1",
		"lines_limit":       float64(5),
		"on_lines_overflow": "fold",
		"timeout":           float64(1),
	})
	if !strings.Contains(got, "Folded:") {
		t.Fatalf("a folded reply must say what it withheld:\n%s", got)
	}
	if !strings.Contains(got, "read --cursor") {
		t.Errorf("the Folded header must name the way to fetch the rest:\n%s", got)
	}
}

// TestNoFoldHeaderWhenNothingWasFolded pins the other side: a reply that returned
// everything must not carry the header, or it stops meaning anything.
func TestNoFoldHeaderWhenNothingWasFolded(t *testing.T) {
	// A PIPE, NOT A PTY, and that is the fix for a flake this test kept reproducing.
	//
	// It asserts on BYTES THAT MUST BE THERE and on a header that must not be — neither of
	// which needs a terminal. A pty has no "the command finished" signal, so `run` returns
	// whatever arrived inside its window and a loaded machine could return before the
	// script had printed anything; the wait below then had nothing to find, for a reason
	// that had nothing to do with folding. The comment used to call this "the third
	// instance of the same race in #189" and answer it with a longer wait. A pipe channel
	// reports its process's exit, so the bytes are in the transcript when `run` returns —
	// the race is removed rather than waited out.
	s := newTestServer(t)
	h := openPipe(t, s, "sh")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// THE MARKER IS NOT IN THE ECHO: the script's own text is not on the command line
	// (`sh /tmp/…/probe.sh` is), so a wait for it can only be satisfied by output.
	script := writeScript(t, "printf 'SHORT\\n'")
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"sh '" + script + "'"}, "timeout": 10})
	waitForGrepContent(t, s, h, "SHORT")
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1", "timeout": float64(1)})
	if strings.Contains(got, "Folded:") {
		t.Errorf("nothing was folded, so the header must be absent:\n%s", got)
	}
	if !strings.Contains(got, "SHORT") {
		t.Errorf("the bytes should be there:\n%s", got)
	}
}
