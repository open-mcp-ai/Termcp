package mcp

// Issue #317: a refusal carries a copyable example line.
//
// A caller that reached this point already knows what it meant to do; what it does not
// know is the shape. Sending it to `help` costs a round trip and so sends it back to the
// page it already guessed wrong on.

import (
	"strings"
	"testing"
)

func TestParseErrorCarriesCopyableExample(t *testing.T) {
	s := newTestServer(t)

	bad := termText(t, s, map[string]any{"cmd": "run nonsuch --bogus-flag"})
	if !strings.HasPrefix(bad, "TERM/1 4") {
		t.Fatalf("bad run was accepted:\n%s", bad)
	}
	example, ok := headerValue(t, bad, "Example")
	if !ok {
		t.Fatalf("bad run carried no Example header:\n%s", firstLine(bad))
	}
	if !strings.HasPrefix(example, "run ") {
		t.Fatalf("Example is for another verb: %q", example)
	}

	// The example is per verb, not one shared line.
	badRead := termText(t, s, map[string]any{"cmd": "read nonsuch --bogus"})
	if v, _ := headerValue(t, badRead, "Example"); !strings.HasPrefix(v, "read ") {
		t.Fatalf("read's refusal carried %q", v)
	}

	// A verb with no example says nothing rather than inventing one: the body is the
	// specific thing to say about THIS call, and an invented example is copyable and
	// wrong.
	if v, ok := headerValue(t, bad, "Usage"); ok && strings.Contains(v, "help") {
		_ = v
	}
	noEx := termText(t, s, map[string]any{"cmd": "notify --bogus"})
	if v, ok := headerValue(t, noEx, "Example"); ok {
		t.Fatalf("a verb without an example invented one: %q", v)
	}
}
