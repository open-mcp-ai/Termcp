package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// A REFUSAL MUST NOT ARRIVE AFTER THE EFFECT IT DENIES.
//
// `runOne` types the command, presses Enter and only then reads the window — and the
// window is validated where it is read. So a refusal about the window used to arrive
// after the command had already run (issue #179), and a 4xx reads as "nothing
// happened": the caller retries, and the command runs twice. On a mutating command
// the retry is the damage.
//
// The trigger the issue reported was the upper bound on `timeout` (issue #168), which
// is gone — and it was the only rule the READ validator had that the ENTRANCE validator
// did not, which is why the lie was reachable at all. What is pinned here is the ORDER,
// so the next such rule cannot reintroduce it: runOne refuses a window it cannot honor
// before it types anything.
//
// Hence the direct call: through the tool, the entrance refuses a negative window
// before the command exists, and a test of the ordering could not fail.
//
// The proof is a file the command creates, checked by a second command: the reply's
// own text cannot answer this, because the reply is the refusal.
func TestARefusedWindowNeverRanTheCommand(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "marker.txt"))

	stmt := directive.Statement{Op: "run", Target: h, Path: "run " + h + " $1", Payload: "touch " + marker}
	// A negative window, handed straight to runOne so the ENTRANCE validator (which
	// refuses the same value before dispatch) does not answer first. That is the
	// point: runOne must be safe on its own, not because someone upstream happened to
	// check the same numbers.
	slot := s.runOne(context.Background(), stmt, termArgs{TimeoutSeconds: -1, BytesLimit: 200, LinesLimit: 200})
	if slot.Code < 400 {
		t.Fatalf("a negative window must be refused, got %d %s", slot.Code, slot.Body)
	}

	check := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"test -e " + marker + " && echo MARKER-EXISTS || echo NO-MARKER"},
		"timeout": 10,
	})
	if !strings.Contains(check, "NO-MARKER") {
		t.Fatalf("the refused run left its side effect behind: %s", firstLine(check))
	}
}

// A WINDOW ABOVE A MINUTE IS A REQUEST, NOT AN ERROR.
//
// The upper bound was removed from the documentation ("a recommendation, not a
// ceiling") but stayed in the read validator, whose message described only the
// negative case — so `timeout: 90` was answered with "timeout must not be negative,
// got 89.35": a positive number called negative, and no way for the caller to tell
// whether to lower the number or wait (issue #168).
func TestALongWindowIsNotAnError(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, timeout := range []float64{61, 90, 600} {
		marker := fmt.Sprintf("LONG-WINDOW-RAN-%v", timeout)
		got := termText(t, s, map[string]any{
			"cmd":     "run " + h + " $1",
			"args":    []any{"echo " + marker},
			"timeout": timeout,
		})
		if strings.Contains(got, "negative") {
			t.Fatalf("timeout %v was refused as negative: %s", timeout, firstLine(got))
		}
		if !strings.Contains(got, marker) {
			t.Fatalf("timeout %v did not run the command: %s", timeout, firstLine(got))
		}
	}
}
