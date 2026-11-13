package mcp

// The dogfood pass found the server claiming more than it observed. This is the one of the
// three that can be pinned without a live transport: `--until prompt` could not match the
// prompts in front of it.

import "testing"

// THE THEMED PROMPTS WERE UNMATCHABLE. starship, oh-my-posh and powerlevel10k end the line
// on ❯ ➜ λ, and usually paint the glyph and then reset the colour, so the bytes are
// `❯ \x1b[m` — the reset comes after the space, which is why `[$#>[ \t]*\r?$` never fired.
// Every `--until prompt` call on such a shell burned its whole timeout and then said, truly
// but uselessly, that it had observed nothing.
func TestUntilPromptPresetMatchesThemedPrompts(t *testing.T) {
	s := newTestServer(t)
	spec, bad := s.untilFrom(stmtWithFlag("prompt"))
	if bad != nil {
		t.Fatalf("the prompt preset did not compile: %s", bad.msg)
	}

	for _, tail := range []string{
		"[12:00:00] /repo on  main ❯ \x1b[m", // starship, reset after the space
		"\x1b[38;2;1;2;3m➜  repo \x1b[m",     // powerlevel10k
		"λ ",                                 // lambda prompts
		"❯ ",                                 // bare glyph, no colour
		"PS C:\\Users\\x> ",                  // PowerShell 5.1
		"user@host:~$ ",
		"root@box:/# ",
		"% ", // csh / zsh with PROMPT_PERCENT
	} {
		if !spec.re.MatchString("\r\n" + tail) {
			t.Errorf("the prompt preset does not match %q (pattern %s)", tail, spec.re)
		}
	}

	// PROSE MUST NOT MATCH, or the preset would fire on the first line of output and
	// report a completion nobody observed.
	for _, prose := range []string{
		"the build finished successfully\n",
		"total 4 files\n",
		"error: could not compile\n",
	} {
		if spec.re.MatchString(prose) {
			t.Errorf("the prompt preset matched ordinary output %q", prose)
		}
	}
}
