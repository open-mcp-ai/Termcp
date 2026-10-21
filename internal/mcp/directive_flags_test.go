package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// TestNoDirectiveFlagIsDead pins the other half of the flag contract.
//
// directive's TestEveryFlagIsDocumented proves every whitelisted flag is
// DOCUMENTED; this proves every one is READ. Both are needed because the two
// failures look identical to a caller and have opposite fixes: an undocumented
// flag is a capability nobody can find, while an unread flag is a capability the
// caller believes they set and the server silently ignored — `read h --timeout 5`
// parsed, reported success, and waited its default two seconds anyway.
//
// The check is a source scan rather than a table because consumption has no
// single shape: a flag may be read as stmt.Flags["x"], boolFlag(stmt, "x"), or
// copied by setFlagString/setFlagNumber. Requiring its name to appear anywhere in
// the package's non-test source is the cheap test that catches all three.
func TestNoDirectiveFlagIsDead(t *testing.T) {
	src := readPackageSource(t)
	for _, op := range directive.Ops() {
		for _, f := range op.Flags {
			if !strings.Contains(src, `"`+f+`"`) {
				t.Errorf("%s: flag --%s is accepted by the parser but never read anywhere in this package: "+
					"a caller can set it and the server ignores it. Either consume it or drop it from Flags.",
					op.Name, f)
			}
		}
	}
}

// readPackageSource concatenates this package's non-test .go files.
func readPackageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

// TestDocumentedProfileActionsExist pins the action list in help profile to the
// list the handler accepts.
//
// This exists because the first draft of that page documented `export` and
// `import` — plausible, since the page also describes the batch import format —
// and both are refused ("profile needs --action list|create|edit|copy|delete").
// The flag guard above cannot see it: --source IS read, it is the ACTION that was
// invented. A caller following the page would have wasted a call on each.
func TestDocumentedProfileActionsExist(t *testing.T) {
	src := readPackageSource(t)
	// The handler states its own vocabulary in the refusal message; reading it
	// from there means the two cannot drift without this test failing.
	const marker = "profile needs --action "
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("the profile handler no longer states its action list; update this test")
	}
	rest := src[i+len(marker):]
	// The list ends at the comma before "got ", not at the closing quote.
	end := strings.IndexAny(rest, ",\"")
	if end < 0 {
		t.Fatal("unterminated action list in the profile handler")
	}
	accepted := strings.Split(rest[:end], "|")

	op, ok := directive.Lookup("profile")
	if !ok {
		t.Fatal("no profile directive")
	}
	// Every action the handler accepts must be documented, or a caller cannot
	// discover it; every action the page names must be accepted, or it is a
	// capability that does not exist.
	for _, a := range accepted {
		if !strings.Contains(op.Detail, a) {
			t.Errorf("profile accepts --action %s but help profile never mentions it", a)
		}
	}
	for _, word := range []string{"export", "import"} {
		if strings.Contains(op.Detail, "\n  "+word+" ") {
			t.Errorf("help profile lists %q as an action, but the handler accepts only %s", word, strings.Join(accepted, "|"))
		}
	}
}

// TestBindHostReachesTheForwardManager closes the blind spot the scan above has.
//
// That test asks "does this flag's name appear in the package", which --local-host
// passed for years while being read by exactly one handler: it was consumed by the
// REMOTE branch, so the scan saw the name and every local/dynamic call still
// ignored the address the caller set. A name in the source is not a value arriving
// where it is used, so this asserts the wiring itself: both handlers that can bind
// a local listener must pass bind_host on.
func TestBindHostReachesTheForwardManager(t *testing.T) {
	src := readPackageSource(t)
	for _, call := range []struct {
		name   string
		marker string
		arg    string
	}{
		// A local forward binds in this host: the address is the third argument.
		{"CreateLocal", "CreateLocal(sess.ID, sess.Info().Name, ", "bindHost"},
		// A dynamic forward binds in this host too, before the port.
		{"CreateDynamic", "CreateDynamic(sess.ID, info.Name, ", "bindHost"},
	} {
		i := strings.Index(src, call.marker)
		if i < 0 {
			t.Errorf("handle%s no longer calls %s with the shape this test knows; "+
				"update the marker so the bind_host argument stays checked", call.name, call.name)
			continue
		}
		rest := src[i+len(call.marker):]
		if !strings.HasPrefix(rest, call.arg) {
			t.Errorf("%s is called without %s as its bind address: %q. The flag parses, "+
				"the handler runs, and the address the caller set is dropped.",
				call.name, call.arg, firstLine(rest))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
