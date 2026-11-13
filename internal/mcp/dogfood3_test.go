package mcp

// The third dogfood round's three findings, each a place where the product told the caller
// to do something it had already done or described a shape it did not accept.

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// THE WARNING'S OWN REMEDY SILENCES IT: it closes by advising "pass `--until` so the channel
// has evidence", so a call that already carries `--until` must not be told that.
func TestUntilSilencesTheCompoundWarning(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " --until prompt $1",
		"args":    []any{"echo a; echo b"},
		"timeout": 10,
	})
	if w, ok := headerValue(t, got, "Warning"); ok && strings.Contains(w, "pass `--until`") {
		t.Fatalf("a call that passed --until was advised to pass --until:\n%s", w)
	}
}

// A GLYPH IN THE MIDDLE OF THE LINE IS STILL A PROMPT. Themes label it: the live shell this
// was found on reads `@anonymous ➜ repo git(main) ` — the line ends on `)`, so requiring the
// glyph at either edge recognised only stripped-down themes and the whole window was spent.
func TestPromptPresetRecognisesALabelledGlyph(t *testing.T) {
	for _, line := range []string{
		"@anonymous ➜ repo git(daily/x) ",
		"\x1b[38;5;1m@box \x1b[m➜  src \x1b[32mmain\x1b[m) ",
		"user@box:~$ ",
		"➜  repo ",
		"╰─ ",
		"λ ",
	} {
		if !mustPrompt(t, line) {
			t.Errorf("the prompt preset does not recognise %q", line)
		}
	}
	// And a line without a prompt glyph is still not a prompt: widening a guess must not
	// turn into matching everything.
	for _, line := range []string{
		"tick 1",
		"Press any key to continue . . .",
		"error: 3 files changed",
	} {
		if mustPrompt(t, line) {
			t.Errorf("the prompt preset matched a non-prompt %q", line)
		}
	}
}

func mustPrompt(t *testing.T, line string) bool {
	t.Helper()
	s := newTestServer(t)
	spec, bad := s.untilFrom(stmtWithFlag("prompt"))
	if bad != nil {
		t.Fatalf("--until prompt no longer compiles: %s", bad.msg)
	}
	return spec.re.MatchString(line)
}

// THE USAGE LINE IS WHAT THE PARSER ACCEPTS. `open <session> [--argv <cmd> [--pty]]` read as
// "argv first, then pty", and the parser refuses that order because everything after
// `--argv` is the command.
func TestOpenUsageOrdersFlagsTheWayTheParserDoes(t *testing.T) {
	op, ok := directive.Lookup("open")
	if !ok {
		t.Fatal("open is not declared")
	}
	up := strings.Index(op.Usage, "--pty")
	av := strings.Index(op.Usage, "--argv")
	if up < 0 || av < 0 {
		t.Fatalf("the usage line stopped mentioning both flags: %s", op.Usage)
	}
	if up > av {
		t.Fatalf("the usage line still puts --argv before --pty, which the parser refuses: %s", op.Usage)
	}
}
