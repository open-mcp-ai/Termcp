package mcp

import (
	"runtime"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// `--until` IS THE CALLER NAMING THE EVIDENCE THAT ENDS THE WAIT.
//
// The server cannot see whether a command finished on a pty, and it used to say
// `Completion: quiet` when the stream merely stopped talking. A caller that knows what
// "finished" looks like on its own host can say so instead, and then the reply reports
// an observation rather than an inference.

// THE PRESETS ARE UNIT-TESTED BECAUSE THE MARKER IS NOT TYPABLE BY HAND.
//
// Nobody writes `ESC ] 133 ; D ; <status> ST` from memory, and the two terminators
// (`ESC \` and BEL) differ between shells. `--until exit` exists so the caller does not
// have to, and these are the two things it must get right: the marker, and the status
// it carries.
func TestUntilExitPresetReadsTheShellsOwnReport(t *testing.T) {
	s := newTestServer(t)
	spec, bad := s.untilFrom(stmtWithFlag("exit"))
	if bad != nil {
		t.Fatalf("the exit preset did not compile: %s", bad.msg)
	}
	if !spec.verdict {
		t.Fatalf("the exit preset must carry a verdict: its capture group is the exit status")
	}
	for _, marker := range []string{
		"\x1b]133;D;0\x1b\\", // ST-terminated, what kitty and WezTerm send
		"\x1b]133;D;7\x07",   // BEL-terminated, what older scripts send
	} {
		loc := spec.re.FindStringSubmatch(marker)
		if loc == nil {
			t.Fatalf("the exit preset did not match %q", marker)
		}
		if loc[1] == "" {
			t.Fatalf("the exit preset matched %q but captured no status", marker)
		}
	}
	if got := spec.re.FindStringSubmatch("\x1b]133;D;7\x07"); got[1] != "7" {
		t.Fatalf("the exit preset captured %q, want 7", got[1])
	}
}

func TestUntilPromptPresetMatchesAShellPrompt(t *testing.T) {
	s := newTestServer(t)
	spec, bad := s.untilFrom(stmtWithFlag("prompt"))
	if bad != nil {
		t.Fatalf("the prompt preset did not compile: %s", bad.msg)
	}
	for _, line := range []string{"user@host:~$ ", "PS C:\\Users\\Administrator> ", "root@box:/# "} {
		if !spec.re.MatchString(line) {
			t.Fatalf("the prompt preset did not match %q", line)
		}
	}
	if spec.re.MatchString("the build finished successfully") {
		t.Fatalf("the prompt preset matched a line that is not a prompt")
	}
}

// A MATCH IS REPORTED AS A MATCH, and the pattern that produced it is named: a caller
// paging through several runs needs to know which evidence the reply is resting on.
func TestUntilReportsAMatch(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " --until SECOND-LINE $1",
		"args":    []any{"echo FIRST-LINE; echo SECOND-LINE"},
		"timeout": 20,
	})
	if v, ok := headerValue(t, got, "Completion"); !ok || v != "matched" {
		t.Fatalf("a matched --until did not say so (Completion=%q, present=%v):\n%s", v, ok, firstLine(got))
	}
	if v, ok := headerValue(t, got, "Until"); !ok || v != "SECOND-LINE" {
		t.Fatalf("the reply did not name the evidence it matched (Until=%q, present=%v)", v, ok)
	}
}

// THE ECHO OF THE CALLER'S OWN COMMAND IS NOT THE EVIDENCE.
//
// `run ch "echo hi # MARKER"` echoes the text `MARKER` into the same stream it is about
// to run in. A matcher that read the whole window would answer "matched" before the
// command produced anything — which is the classic expect bug, and it makes a `--until`
// that looks right on a quiet command silently wrong on a busy one.
func TestUntilIgnoresTheEchoedCommandLine(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " --until MARKER-ONLY-IN-THE-ECHO $1",
		"args":    []any{"echo hi # MARKER-ONLY-IN-THE-ECHO"},
		"timeout": 1,
	})
	if v, ok := headerValue(t, got, "Completion"); ok {
		// THE WHOLE REPLY, because this assertion is about BYTES: "the echo matched"
		// names no byte, and the runner that echoes differently is exactly the machine
		// whose window text is the evidence. A one-line message sent the Windows
		// failure back unreadable.
		t.Fatalf("the echo of the caller's own command matched (Completion=%q); the echo is not evidence:\n%s", v, got)
	}
	// AND THE MISS SAYS SOMETHING USEFUL. A refusal to claim completion must not leave
	// the caller guessing what to do next.
	if v, ok := headerValue(t, got, "Hint"); !ok || !strings.Contains(v, "--until") {
		t.Fatalf("a missed --until gave no usable hint (Hint=%q, present=%v)", v, ok)
	}
}

func stmtWithFlag(value string) directive.Statement {
	v := value
	return directive.Statement{Flags: map[string]*string{"until": &v}}
}

// A READ CAN WAIT FOR THE SAME EVIDENCE, which is the verb that has no other way to
// wait at all: `read` returns what is there now, and a caller watching a long command
// had to poll it. With `--until` it is a wait primitive — the same predicate, applied
// to the bytes that arrive next.
func TestReadUntilWaitsForTheEvidence(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Start a command that prints late, then wait for its output in a separate call.
	termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"sleep 2; echo ARRIVED-LATER"},
		"timeout": 0,
	})
	got := termText(t, s, map[string]any{
		"cmd":     "read " + h + " --until ARRIVED-LATER",
		"timeout": 20,
	})
	if v, ok := headerValue(t, got, "Completion"); !ok || v != "matched" {
		t.Fatalf("a read whose --until matched did not say so (Completion=%q, present=%v):\n%s", v, ok, firstLine(got))
	}
	if !strings.Contains(got, "ARRIVED-LATER") {
		t.Fatalf("the read matched the evidence but did not return it:\n%s", firstLine(got))
	}
}

// AND IT IS OPTIONAL: without `--until` a read still returns at once and claims nothing.
func TestReadWithoutUntilDoesNotWait(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// A command that sleeps before printing, in the syntax of whichever shell was given.
	//
	// THE MARKER IS BUILT BY THE SHELL, NOT SPELLED IN THE LINE: the pty echoes what was
	// typed, so a line containing the literal marker puts it in the read's window and the
	// assertion starts testing the echo instead of the wait. Quoting splits it only in the
	// echoed text — the shell still prints one word.
	cmd := `sleep 5; echo NEVER-"WAITED"-FOR`
	if runtime.GOOS == "windows" {
		cmd = `Start-Sleep -Seconds 5; echo ("NEVER-" + "WAITED-FOR")`
	}
	termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{cmd},
		"timeout": 0,
	})
	got := termText(t, s, map[string]any{"cmd": "read " + h, "timeout": 0})
	if v, ok := headerValue(t, got, "Completion"); ok {
		t.Fatalf("a plain read claimed Completion=%q; only evidence may produce it", v)
	}
	if strings.Contains(got, "NEVER-WAITED-FOR") {
		t.Fatalf("a read without --until waited for later output:\n%s", got)
	}
}
