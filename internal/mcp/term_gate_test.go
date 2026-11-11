package mcp

import (
	"strings"
	"testing"
)

// A COMPOUND LINE DOES NOT SURVIVE A HALF DELIVERY.
//
// `ping host; cd ~/work; rm -rf *` is one meaning made of three. Lose the `cd` and the
// `rm` does not fail — it runs somewhere else, silently, because `;` does not stop on
// failure either. So an unconfirmed channel refuses the compound spelling, and the next
// test pins the other half: a forked channel full of refusals is a tool people route
// around with `--force`, which would remove the guard exactly where it is needed.
func TestCompoundLineIsRefusedOnAnUnconfirmedChannel(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// A plain run ends without evidence, because nothing on a pty can prove completion.
	first := termText(t, s, map[string]any{
		"cmd": "run " + h + " $1", "args": []any{"echo FIRST"}, "timeout": 5,
	})
	if v, ok := headerValue(t, first, "Completion"); ok {
		t.Fatalf("setup: a plain pty run claimed Completion=%q", v)
	}

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"echo COMPOUND-MARKER; echo SECOND"},
		"timeout": 5,
	})
	if !strings.Contains(got, "conflict") && !strings.Contains(got, "--until") {
		t.Fatalf("a compound line on an unconfirmed channel was not refused:\n%s", firstLine(got))
	}
	// AND NOTHING WAS TYPED: the refusal has to arrive before the bytes, or it is a
	// report of damage rather than a prevention of it.
	seen := termText(t, s, map[string]any{"cmd": "read " + h + " --tail 40", "timeout": 0})
	if strings.Contains(seen, "COMPOUND-MARKER") {
		t.Fatalf("the refused line was typed into the channel anyway:\n%s", firstLine(seen))
	}
}

// A SIMPLE COMMAND STILL PASSES: if it is eaten, it is eaten, and nothing downstream is
// re-bound to a mistake. Refusing it would refuse the ordinary case.
func TestSimpleLinePassesOnAnUnconfirmedChannel(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo FIRST"}, "timeout": 5})
	got := termText(t, s, map[string]any{
		"cmd": "run " + h + " $1", "args": []any{"echo SIMPLE-MARKER"}, "timeout": 5,
	})
	if strings.Contains(got, "conflict") {
		t.Fatalf("a simple command was refused on an unconfirmed channel:\n%s", firstLine(got))
	}
	if !strings.Contains(got, "SIMPLE-MARKER") {
		t.Fatalf("a simple command did not run:\n%s", firstLine(got))
	}
}

// EVIDENCE IS THE KEY: once a run has it, the compound line goes through. The gate is
// about what is known, not about refusing compound lines.
func TestCompoundLinePassesOnceTheChannelHasEvidence(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	first := termText(t, s, map[string]any{
		"cmd":     "run " + h + " --until EVIDENCE-DONE $1",
		"args":    []any{"echo EVIDENCE-DONE"},
		"timeout": 20,
	})
	if v, ok := headerValue(t, first, "Completion"); !ok || v != "matched" {
		t.Fatalf("setup: --until did not produce evidence (Completion=%q, present=%v)", v, ok)
	}

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"echo COMPOUND-A; echo COMPOUND-B"},
		"timeout": 20,
	})
	if strings.Contains(got, "conflict") {
		t.Fatalf("a compound line was refused on a channel that had evidence:\n%s", firstLine(got))
	}
}

// --force IS THE ESCAPE HATCH, and it is the caller's to use knowingly.
func TestForceTypesACompoundLineOnAnUnconfirmedChannel(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo FIRST"}, "timeout": 5})
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " --force $1",
		"args":    []any{"echo FORCED-A; echo FORCED-B"},
		"timeout": 20,
	})
	if strings.Contains(got, "conflict") {
		t.Fatalf("--force did not override the gate:\n%s", firstLine(got))
	}
	if !strings.Contains(got, "FORCED-B") {
		t.Fatalf("--force did not type the line:\n%s", firstLine(got))
	}
}
