package mcp

// The five judgment calls, each of which needed a definition before it could be fixed.

import (
	"strings"
	"testing"
	"time"
)

// A SESSION IS A CONTAINER, SO ITS STATUS IS THE CONTAINER'S — and when every channel has
// exited the container is still usable (`open` adds one). The listing now says both halves
// instead of leaving `ls` and `get` looking like they disagree about the same object.
func TestSessionRowSaysWhenNoChannelIsLive(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	sid, _, _ := strings.Cut(h, ":")

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"exit"}, "timeout": 10})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got := termText(t, s, map[string]any{"cmd": "ls", "timeout": 0})
		if strings.Contains(got, sid) && strings.Contains(got, "no live channel") {
			// And the session is still addressable: `open` on it works, which is why the
			// status is `running` and not `exited`.
			reopen := termText(t, s, map[string]any{"cmd": "open " + sid + " --argv " + testShell(), "timeout": 10})
			if !strings.HasPrefix(reopen, "TERM/1 2") {
				t.Fatalf("a container with no live channel refused `open`:\n%s", firstLine(reopen))
			}
			termText(t, s, map[string]any{"cmd": "close " + sid, "timeout": 5})
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("`ls` never said the session had no live channel")
}

// ASKING PAST THE END OF THE MODEL IS NOT AN ERROR, but an empty page with no explanation
// reads as "the screen is empty".
func TestScreenPagePastTheEndExplainsItself(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --screen --top 999 --rows 5", "timeout": 0})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("a page past the end was an error:\n%s", firstLine(got))
	}
	hint, ok := headerValue(t, got, "Hint")
	if !ok || !strings.Contains(hint, "past the end") {
		t.Fatalf("a page past the end said nothing about it (Hint=%q):\n%s", hint, firstLine(got))
	}
	if total, _ := headerValue(t, got, "Screen-Total"); !strings.Contains(hint, total) {
		t.Fatalf("the hint does not name the model's size (%q vs Screen-Total %s)", hint, total)
	}
}

// THE GATE'S WARNING MUST NOT CLAIM IT OBSERVED NOTHING. When the last line of the channel
// is not a shell prompt, that IS an observation and the warning names it.
func TestCompoundWarningNamesWhatItSaw(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// Put the channel into a program that holds the terminal with a prompt of its own.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"cat"}, "timeout": 2})

	got := termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo a; echo b"}, "timeout": 3})
	warn, ok := headerValue(t, got, "Warning")
	if !ok {
		// A refusal is also acceptable; what is not acceptable is a warning that says it
		// observed nothing while a prompt-less line sits on screen.
		if strings.HasPrefix(got, "TERM/1 409") {
			return
		}
		t.Fatalf("a compound line into a held terminal neither warned nor refused:\n%s", firstLine(got))
	}
	if strings.Contains(warn, "Nothing observed") {
		t.Fatalf("the warning still claims it observed nothing:\n%s", warn)
	}
	if !strings.Contains(warn, "not a shell prompt") && !strings.Contains(warn, "looks like a shell prompt") {
		t.Fatalf("the warning does not say what the last line was:\n%s", warn)
	}
}
