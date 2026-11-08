package mcp

import (
	"strings"
	"testing"
)

// TestAnArgvValueWithADollarReachesTheProgramUnchanged pins issue #292 end to end.
//
// The report came from a Windows target: `--argv powershell.exe -NoProfile -Command
// "$1"` with a script containing `$t` reached PowerShell as `foreach(\$t ...)`, a
// syntax error, while the same call with a `$`-free script worked. The backslash was
// injected at SUBSTITUTION time, and nothing downstream removes it — the caller's `"`
// is taken off by the word-list reader, which copies what is inside it byte for byte.
//
// The assertion is on the Path header first: it is what the caller reads back, and
// seeing `foreach(\$t ...)` there is what made the defect visible. Then on the
// program's own output, because a Path that reads right is still a claim about text,
// not about bytes.
//
// The reference is quoted because a word list is whitespace-separated: the quotes are
// what hold a script with spaces together, and they are the caller's, not termcp's.
// The unquoted spelling of the same value is a different word list on purpose, and
// TestArgvValueWithDollarArrivesUnchanged covers both at the byte level.
func TestAnArgvValueWithADollarReachesTheProgramUnchanged(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)

	const script = `foreach($t in @('a','b')) { $t }`
	t.Run("script", func(t *testing.T) {
		out := termText(t, s, map[string]any{
			"cmd":  `open internal --argv powershell.exe -NoProfile -Command "$1"`,
			"args": []any{script},
		})
		path, ok := headerValue(t, out, "Path")
		if !ok {
			t.Fatalf("no Path header:\n%s", out)
		}
		if strings.Contains(path, `\$`) {
			t.Errorf("the value was escaped on its way into a word list:\n%s", path)
		}
		if !strings.Contains(path, script) {
			t.Errorf("Path does not echo the caller's bytes:\n got %q\nwant it to contain %q", path, script)
		}

		handle, ok := headerValue(t, out, "Handle")
		if !ok {
			t.Fatalf("no Handle header:\n%s", out)
		}
		got := termText(t, s, map[string]any{"cmd": "read " + handle, "timeout": 10})
		// The script prints the array's elements one per line; a mangled `\$t`
		// never runs at all, so this is the difference between "ran" and "did not
		// parse".
		for _, want := range []string{"a", "b"} {
			if !strings.Contains(got, want) {
				t.Errorf("script did not run (missing %q):\n%s", want, got)
			}
		}
	})
}
