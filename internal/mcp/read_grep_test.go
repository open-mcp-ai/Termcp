package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestReadGrepFiltersBeforeTheWindow pins the core contract:
//
//  1. Filtering happens locally on this daemon's transcript (no SSH, no grep on target).
//  2. Each line carries its REAL line number so context is addressable.
//  3. The line window applies to the MATCHES: `--tail 2` means the last two matches,
//     not the last two raw lines of the buffer.
func TestReadGrepFiltersBeforeTheWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	// THE SHELL HAS TO BE READING BEFORE A LINE IS TYPED. A pty that is still starting
	// discards input, and then the search below finds an empty transcript and reports it
	// as the cursor rule being broken. This is the repository's state probe (term_test.go),
	// not a sleep: it asks the shell to build a marker, so a discarded line cannot pass.
	awaitPtyReady(t, s, h)

	// Feed a predictable multi-line transcript:
	//   1: start
	//   2: [INFO] booting
	//   3: [ERROR] disk full
	//   4: [INFO] retry
	//   5: [ERROR] out of memory
	//   6: [INFO] done
	script := writeScript(t, "printf 'start\n[INFO] booting\n[ERROR] disk full\n[INFO] retry\n[ERROR] out of memory\n[INFO] done\n'")
	termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"sh '" + script + "'"},
		"timeout": 2,
	})
	// A FIXED TIMEOUT IS NOT A SYNCHRONISATION POINT. On a loaded runner the shell
	// had not finished printing when the two seconds expired, so the grep ran over a
	// partial transcript and answered 204 for a pattern that was about to appear.
	// Wait for the last line to be readable, which is the fact the assertions need.
	waitForGrepContent(t, s, h, "done")

	// 1. All matches: must find both ERROR lines, each with its real line number.
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --grep '\\[ERROR\\]'"})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("expected 200, got:\n%s", got)
	}
	if !strings.Contains(got, "disk full") || !strings.Contains(got, "out of memory") {
		t.Errorf("expected both error lines:\n%s", got)
	}
	// INFO lines must be filtered out.
	if strings.Contains(got, "booting") || strings.Contains(got, "retry") {
		t.Errorf("INFO lines were not filtered out:\n%s", got)
	}
	// Real line numbers: lines must start with "N:"
	if !strings.Contains(got, ":[ERROR]") {
		t.Errorf("matching lines must carry their real line numbers (e.g. '3:[ERROR]'):\n%s", got)
	}

	// 2. Window applies to the MATCHES: `--tail 1` must return the LAST error,
	// not line 6 ("[INFO] done").
	gotTail := termText(t, s, map[string]any{"cmd": "read " + h + " --grep '\\[ERROR\\]' --tail 1"})
	if !strings.Contains(gotTail, "out of memory") {
		t.Errorf("tail 1 on matches should return the last match:\n%s", gotTail)
	}
	if strings.Contains(gotTail, "disk full") {
		t.Errorf("tail 1 should have excluded the earlier match:\n%s", gotTail)
	}
}

// TestReadGrepNoMatchAnswers204WithStats pins the zero-match path:
// it answers 204 (nothing there) and reports how many lines were scanned so the
// caller knows the pattern was actually checked against something.
func TestReadGrepNoMatchAnswers204WithStats(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	// THE SHELL HAS TO BE READING BEFORE A LINE IS TYPED. A pty that is still starting
	// discards input, and then the search below finds an empty transcript and reports it
	// as the cursor rule being broken. This is the repository's state probe (term_test.go),
	// not a sleep: it asks the shell to build a marker, so a discarded line cannot pass.
	awaitPtyReady(t, s, h)

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo ONLY_ONE_LINE"}, "timeout": 2})

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --grep 'NO_SUCH_PATTERN'"})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("zero matches must answer 204:\n%s", got)
	}
	if !strings.Contains(got, "Lines-Scanned:") {
		t.Errorf("the 204 reply must report how many lines were scanned:\n%s", got)
	}
}

// TestReadGrepBadRegexAnswers400 pins error reporting for an invalid pattern.
func TestReadGrepBadRegexAnswers400(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	// THE SHELL HAS TO BE READING BEFORE A LINE IS TYPED. A pty that is still starting
	// discards input, and then the search below finds an empty transcript and reports it
	// as the cursor rule being broken. This is the repository's state probe (term_test.go),
	// not a sleep: it asks the shell to build a marker, so a discarded line cannot pass.
	awaitPtyReady(t, s, h)

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --grep '[unclosed'"})
	if !strings.HasPrefix(got, "TERM/1 400") {
		t.Fatalf("bad regex must answer 400:\n%s", got)
	}
	if !strings.Contains(got, "regular expression") {
		t.Errorf("error must explain that it is a Go regex:\n%s", got)
	}
}

// TestReadGrepDoesNotConsumeTheTranscript pins the cursor rule: a search is a look,
// not a read. Two searches for different patterns must see the same bytes, and a
// plain read after a search must still see what it would have seen.
//
// Without this, a caller that grepped for "error" would consume the transcript and
// then find nothing when it grepped for "warn" — the classic way a diagnostic tool
// destroys the evidence it was asked to inspect.
func TestReadGrepDoesNotConsumeTheTranscript(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	// THE SHELL HAS TO BE READING BEFORE A LINE IS TYPED. A pty that is still starting
	// discards input, and then the search below finds an empty transcript and reports it
	// as the cursor rule being broken. This is the repository's state probe (term_test.go),
	// not a sleep: it asks the shell to build a marker, so a discarded line cannot pass.
	awaitPtyReady(t, s, h)

	script := writeScript(t, "printf 'AAA\\nBBB\\nCCC\\n'")
	termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"sh '" + script + "'"},
		"timeout": 2,
	})
	// WAIT FOR THE OUTPUT TO LAND, the same way the other grep tests do. Reading
	// immediately raced the shell: under -race on a loaded runner the first grep could
	// arrive before printf had run, and every assertion after it was describing a
	// transcript that did not exist yet.
	waitForGrepContent(t, s, h, "CCC")

	// Two searches, different patterns, both must succeed on the same transcript.
	first := termText(t, s, map[string]any{"cmd": "read " + h + " --grep 'AAA'"})
	if !strings.Contains(first, "AAA") {
		t.Fatalf("first grep found nothing:\n%s", first)
	}
	second := termText(t, s, map[string]any{"cmd": "read " + h + " --grep 'CCC'"})
	if !strings.Contains(second, "CCC") {
		t.Fatalf("second grep found nothing — the first consumed the transcript:\n%s", second)
	}

	// And a positional read still sees everything.
	all := termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1"})
	for _, want := range []string{"AAA", "BBB", "CCC"} {
		if !strings.Contains(all, want) {
			t.Errorf("positional read is missing %q after greps:\n%s", want, all)
		}
	}
}

// waitForGrepContent blocks until marker is readable in the channel's transcript.
//
// The precondition every grep assertion needs, and NOT the same as "the command
// finished": in a pty the command's completion is unobservable, and in a pipe the
// bytes reach the log asynchronously. A fixed timeout stands in for both and is a
// race that only shows up under load — this test passed alone fifteen times and
// failed in the full run.
func waitForGrepContent(t *testing.T, s *Server, handle, marker string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = termText(t, s, map[string]any{"cmd": "read " + handle + " --from-line 1"})
		if strings.Contains(last, marker) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// THE LAST READ IS PART OF THE FAILURE. Without it the message says only that a
	// marker never appeared, which is true of a shell that never ran the command, a
	// transcript that lost the line, and a window that started past it — three different
	// defects that need three different fixes.
	t.Fatalf("the channel never became readable past %q; last read:\n%s", marker, last)
}
