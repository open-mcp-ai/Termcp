package mcp

// The second dogfood round found more of the same three shapes: a preset that cannot match
// the thing in front of it, a flag that is dropped instead of refused, and a value that is
// reinterpreted instead of rejected.

import "testing"

// THE PROMPT PRESET MUST KNOW THE THEMES THAT END ON A POWERLINE SEPARATOR. Round one
// taught it ❯ ➜ λ; the second round found prompts ending `\ue0b0 `, `\uf432  ` and `╰─ `,
// each wasting the whole window and then reporting that nothing was observed.
func TestUntilPromptPresetMatchesPowerlineEndings(t *testing.T) {
	s := newTestServer(t)
	spec, bad := s.untilFrom(stmtWithFlag("prompt"))
	if bad != nil {
		t.Fatalf("prompt preset did not compile: %s", bad.msg)
	}
	for _, tail := range []string{
		"\ue0b0 \x1b[m",     // powerline right arrow, the most common separator
		"\uf432  \x1b[m",    // a nerd-font glyph theme
		"╰─ \x1b[m",         // box drawing
		"╰─❯ \x1b[m",        // box drawing plus a chevron
		"❯ ",                // round one's case, still true
		"user@box:~$ ",      // bash
		"PS C:\\Users\\x> ", // PowerShell
	} {
		if !spec.re.MatchString("\r\n" + tail) {
			t.Errorf("the prompt preset does not match %q", tail)
		}
	}
}

// A SCREEN PAGE IS A PAGE: a page height of zero and a negative row were reinterpreted.
func TestScreenPagingRefusesNonsense(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	for _, cmd := range []string{
		"read " + h + " --screen --top -1",
		"read " + h + " --screen --top abc",
		"read " + h + " --screen --top 0 --rows 0",
	} {
		got := termText(t, s, map[string]any{"cmd": cmd, "timeout": 0})
		if !hasPrefix4xx(got) {
			t.Fatalf("%q was accepted:\n%s", cmd, firstLine(got))
		}
	}
}

// --grep CANNOT APPLY TO A SCREEN, and dropping it read as "no matches".
func TestScreenRefusesGrep(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --screen --grep x", "timeout": 0})
	if !hasPrefix4xx(got) {
		t.Fatalf("--grep with --screen was accepted:\n%s", firstLine(got))
	}
	// And it still works on the log, where lines are real.
	if ok := termText(t, s, map[string]any{"cmd": "read " + h + " --grep x", "timeout": 0}); hasPrefix4xx(ok) {
		t.Fatalf("--grep on the log was refused:\n%s", firstLine(ok))
	}
}

func hasPrefix4xx(text string) bool {
	return len(text) >= 8 && text[:8] == "TERM/1 4"
}
