package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcpgo "github.com/open-mcp-ai/termcp/mcp"
)

// TestTheExtensionListCapsItselfAndCountsTheRest is issue #185's last piece.
//
// The byte window cuts forty rows mid-line and reports it in a header about bytes:
// "Truncated: 900 bytes withheld" does not tell a caller whether it is missing one
// extension or thirty, and the one it needs may be in the part it did not see. A count
// plus a command is something it can act on; "and more" is a wall.
//
// The fold is a VIEW: every extension stays addressable by name, and --all lists them.
func TestTheExtensionListCapsItselfAndCountsTheRest(t *testing.T) {
	s := newTestServer(t)
	listing := func(n int, all bool) string {
		t.Helper()
		exts := make([]any, 0, n)
		for i := 0; i < n; i++ {
			exts = append(exts, map[string]any{
				"name":   fmt.Sprintf("ext-%02d", i),
				"status": "available",
				"notes":  fmt.Sprintf("extension %d, whose first note line is its own one-liner", i),
			})
		}
		body, err := json.Marshal(map[string]any{"extensions": exts})
		if err != nil {
			t.Fatal(err)
		}
		return s.extensionListing(mcpgo.NewToolResultText(string(body)), all)
	}

	folded := listing(14, false)
	if got := strings.Count(folded, "\n"); got != 11 {
		t.Errorf("the list must show ten rows plus the count, got %d lines:\n%s", got, folded)
	}
	if !strings.Contains(folded, "(4 more not shown; `ls ext --all` lists every extension)") {
		t.Errorf("a folded list must name the count and how to see the rest:\n%s", folded)
	}
	if strings.Contains(folded, "ext-13") {
		t.Errorf("the folded list still shows the row past the cap:\n%s", folded)
	}

	// A short list is not padded with a zero count — a header that is always present
	// says nothing.
	if short := listing(3, false); strings.Contains(short, "more not shown") {
		t.Errorf("a list under the cap must not claim it withheld anything:\n%s", short)
	}

	// --all is the other half: folded is a view, not a limit on what can be reached.
	all := listing(14, true)
	if !strings.Contains(all, "ext-13") || strings.Contains(all, "more not shown") {
		t.Errorf("--all must list every extension and claim nothing was withheld:\n%s", all)
	}

	// The one-line self-description is the reason a caller runs this at all: it is
	// what says whether this extension is the one it wants.
	if !strings.Contains(folded, "whose first note line is its own one-liner") {
		t.Errorf("each row must carry the extension's own one-liner:\n%s", folded)
	}
}
