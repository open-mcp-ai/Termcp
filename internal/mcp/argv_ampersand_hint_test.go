package mcp

import (
	"strings"
	"testing"
)

// TestAnAmpersandAfterArgvNamesTheOperatorAndTheFix pins issue #208.3.
//
// `open internal --argv cmd.exe /c echo OUT & exit 7` was split at the `&`, which is
// termcp's background operator, so `exit` was read as the next statement's directive
// and the answer was `unknown directive: exit`. That reads like a typo in a directive
// name rather than a command line cut in half, and nothing said which character did it.
//
// The refusal now names the operator, the slot that lost its words, and the spelling
// that works.
func TestAnAmpersandAfterArgvNamesTheOperatorAndTheFix(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)

	out := termText(t, s, map[string]any{"cmd": `open internal --argv cmd.exe /c echo OUT & exit 7`})
	if !strings.Contains(out, "--argv") {
		t.Errorf("the refusal does not say which slot lost its words:\n%s", out)
	}
	if !strings.Contains(out, "`&`") {
		t.Errorf("the refusal does not name the operator that cut the line:\n%s", firstLine(out))
	}
	// The form that works, not just the diagnosis (docs/agents/hints.md).
	if !strings.Contains(out, "sh -c $1") {
		t.Errorf("the refusal names no way to write the line:\n%s", firstLine(out))
	}
}
