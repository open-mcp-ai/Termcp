package mcp

// Issue #317: 4xx errors carry a copyable example line.
//
// A refusal sends an AI caller to `help`, which costs a round trip and an essay the
// caller already guessed wrong on. One line it can copy answers the question the
// refusal just asked.

import (
	"strings"
	"testing"
)

func TestBadRequestCarriesCopyableExample(t *testing.T) {
	s := newTestServer(t)

	// A syntax-level failure on a known verb gets that verb's example.
	bad := termText(t, s, map[string]any{"cmd": "run nonsuch --bogus-flag"})
	if !strings.HasPrefix(bad, "TERM/1 4") {
		t.Fatalf("bad run succeeded:\n%s", bad)
	}
	if !strings.Contains(bad, "Example: run a1f3:1 $1") {
		t.Fatalf("bad run did not carry the copyable example line:\n%s", bad)
	}

	// Another verb gets its own example, not the run one.
	badRead := termText(t, s, map[string]any{"cmd": "read nonsuch --bogus"})
	if !strings.Contains(badRead, "Example: read a1f3:1 --grep") {
		t.Fatalf("bad read carried wrong example:\n%s", badRead)
	}
}
