package mcp

import (
	"strings"
	"testing"
)

// TestAliasesRunEveryShapeTheyAllow pins #284.
//
// An alias was resolved out of the parse error, and that recovery path could only
// ever hand back ONE statement — so every shape a command line can take beyond
// "one directive, no arguments" failed, and it failed with the wrong error:
// `unknown directive: <alias>`, about a name that was defined.
func TestAliasesRunEveryShapeTheyAllow(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	define := func(name, body string) {
		t.Helper()
		// THE BODY IS QUOTED WHEN IT HOLDS A SEPARATOR. `create alias a b; c` splits
		// the line into two statements before `create` ever sees it, so the alias would
		// hold `b` and `c` would run right there — which is how the first version of this
		// test passed while testing nothing.
		if strings.ContainsAny(body, ";&") {
			body = "'" + body + "'"
		}
		if got := termText(t, s, map[string]any{"cmd": "create alias " + name + " " + body}); !strings.HasPrefix(got, "TERM/1 2") {
			t.Fatalf("create alias %s failed:\n%s", name, got)
		}
	}

	t.Run("multi-statement", func(t *testing.T) {
		// `;` gives the parser two statements, and the recovery path refused
		// anything that was not exactly one. BOTH legs have to run: a reply from the
		// first alone looks like success.
		//
		// The markers are the two Path headers the alias body produces, counted: a
		// host name is whatever this machine has (`windows` passed here, failed on
		// macOS), and an alias listed by `ls alias` is whatever earlier tests left
		// behind. Counting `Path: ls alias` and `Path: ls host` says exactly one
		// thing — both legs ran — and nothing about the machine.
		define("two", "ls alias; ls host")
		got := termText(t, s, map[string]any{"cmd": "two"})
		if !strings.HasPrefix(got, "TERM/1 2") {
			t.Fatalf("a two-statement alias must run both legs:\n%s", got)
		}
		for _, leg := range []string{"Path: ls alias", "Path: ls host"} {
			if n := strings.Count(got, leg); n != 1 {
				t.Fatalf("only one leg of a two-statement alias ran (%q appears %d times):\n%s", leg, n, got)
			}
		}
	})

	t.Run("arguments", func(t *testing.T) {
		// The caller's substitution table was not passed to the recovery path, so an
		// alias body referencing $1 could never be filled in.
		define("say", "run "+h+" $1")
		got := termText(t, s, map[string]any{
			"cmd":  "say",
			"args": []any{"echo alias-arg-ok"},
		})
		if !strings.Contains(got, "alias-arg-ok") {
			t.Fatalf("an alias body's $1 must take the caller's arg:\n%s", got)
		}
	})

	t.Run("expansion-error-is-reported-not-hidden", func(t *testing.T) {
		// The body is a shape the parser refuses. The caller must be told what the
		// body got wrong; saying "unknown directive: broken" sends them looking for a
		// typo in a name that exists.
		define("broken", "run "+h+" true")
		got := termText(t, s, map[string]any{"cmd": "broken"})
		if !strings.HasPrefix(got, "TERM/1 4") {
			t.Fatalf("a malformed expansion must fail:\n%s", got)
		}
		if strings.Contains(got, "unknown directive: broken") {
			t.Errorf("the alias name is defined; the refusal must describe the body:\n%s", got)
		}
		if !strings.Contains(got, "VARIABLE") {
			t.Errorf("the body's own error must survive:\n%s", got)
		}
	})
}
