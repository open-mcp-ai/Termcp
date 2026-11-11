package mcp

import (
	"strings"
	"testing"
)

// A COMPOUND LINE IS REFUSED ONLY WHEN SOMETHING IS OBSERVED (#311).
//
// The first version of this guard refused every compound line on a channel without a
// `Completion` header — and that is a default, not a boundary: a pty `run` never produces
// one, so the rule became "compound lines need --force on a pty, forever". Five of this
// repository's own tests had to change to keep passing, which is what a changed product
// default looks like.
//
// The table below is the whole rule, and every row of it is a fact the server has:
// a full-screen program in control (the grid exists because it took the screen), bytes
// still arriving when the last window ended, or nothing known at all — and "nothing known"
// refuses nothing, because a 4xx that rests on "we could not tell" is the guess this
// server does not make.
func TestCompoundVerdictTable(t *testing.T) {
	cases := []struct {
		name                       string
		force, compound            bool
		altScreen, busy, confirmed bool
		known                      bool
		wantRefuse, wantWarn       bool
	}{
		{name: "simple line is never gated", compound: false, known: true, busy: true},
		{name: "force overrides everything", force: true, compound: true, altScreen: true, busy: true, known: true},
		{name: "a TUI holding the screen refuses", compound: true, altScreen: true, known: true, wantRefuse: true, wantWarn: true},
		{name: "a command still producing output refuses", compound: true, busy: true, known: true, wantRefuse: true, wantWarn: true},
		{name: "a confirmed channel allows", compound: true, confirmed: true, known: true},
		{name: "not knowing warns but allows", compound: true, confirmed: false, known: true, wantWarn: true},
		{name: "a fresh channel warns but allows", compound: true, known: false, wantWarn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refuse, warn := compoundVerdict(tc.force, tc.compound, tc.altScreen, tc.busy, tc.confirmed, tc.known)
			if got := refuse != ""; got != tc.wantRefuse {
				t.Fatalf("refuse=%v (%q), want %v", got, refuse, tc.wantRefuse)
			}
			if warn != tc.wantWarn {
				t.Fatalf("warn=%v, want %v", warn, tc.wantWarn)
			}
		})
	}
}

// NOT KNOWING IS NOT A REFUSAL: the ordinary `a; b` on a shell goes through, carrying a
// warning the caller can read. This is the test the five --force edits were standing in
// for.
func TestCompoundLineOnAQuietChannelIsSentWithAWarning(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo FIRST"}, "timeout": 5})
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"echo COMPOUND-A; echo COMPOUND-B"},
		"timeout": 20,
	})
	if strings.Contains(got, "conflict") {
		t.Fatalf("a compound line on a merely quiet channel was refused:\n%s", firstLine(got))
	}
	if !strings.Contains(got, "COMPOUND-B") {
		t.Fatalf("the compound line did not run:\n%s", firstLine(got))
	}
	if v, ok := headerValue(t, got, "Warning"); !ok || !strings.Contains(v, "compound") {
		t.Fatalf("the reply did not warn about the unproven channel (Warning=%q, present=%v)", v, ok)
	}
}

// A CHANNEL THAT IS OBSERVABLY BUSY REFUSES, and nothing is typed: the case where the
// refusal is evidence rather than a guess.
func TestCompoundLineIsRefusedWhileACommandIsStillRunning(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// A command that is still printing when its window ends: Still-Running is the
	// observation that makes the next compound line unsafe.
	first := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{noisyCommand("noise")},
		"timeout": 1,
	})
	if v, ok := headerValue(t, first, "Still-Running"); !ok || v != "true" {
		t.Fatalf("setup: the noisy command did not report Still-Running (got %q, present=%v):\n%s", v, ok, firstLine(first))
	}

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"echo GATED-A; echo GATED-B"},
		"timeout": 5,
	})
	if !strings.Contains(got, "conflict") {
		t.Fatalf("a compound line was typed into a channel that was still running:\n%s", firstLine(got))
	}
	// The Path header carries the refused line, so look at the BODY: nothing was typed,
	// which is the whole point of checking before the bytes go out.
	body := got
	if _, after, ok := strings.Cut(got, "\n\n"); ok {
		body = after
	}
	if strings.Contains(body, "GATED-B") {
		t.Fatalf("the refused line ran anyway:\n%s", firstLine(got))
	}
}

// AND --force STILL TYPES IT, because the caller may know the program has finished.
func TestForceTypesACompoundLineIntoABusyChannel(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{noisyCommand("noise")},
		"timeout": 1,
	})
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " --force $1",
		"args":    []any{"echo FORCED-A; echo FORCED-B"},
		"timeout": 20,
	})
	if strings.Contains(got, "conflict") {
		t.Fatalf("--force did not override the gate:\n%s", firstLine(got))
	}
}
