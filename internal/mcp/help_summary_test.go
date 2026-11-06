package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// The summary is what a caller reads instead of the page, so it has to be the thing
// that is cheap — and the page has to be unchanged for the caller who asks for it.
func TestHelpSummaryIsCheaperThanThePage(t *testing.T) {
	s := newTestServer(t)

	// Both windows are raised: the page is over the default lines_limit of 200, and a
	// cut page is not the price the summary quotes.
	summary := termText(t, s, map[string]any{"cmd": "help read", "bytes_limit": 32768, "lines_limit": 100000})
	page := termText(t, s, map[string]any{"cmd": "help read --all", "bytes_limit": 32768, "lines_limit": 100000})

	if len(summary) >= len(page)/2 {
		t.Errorf("summary is %d bytes and the page is %d: not a smaller question", len(summary), len(page))
	}
	// The page is still reachable, and reachable from the summary.
	if !strings.Contains(summary, "help read --all") {
		t.Error("the summary must name the page, or the page is lost to a caller who does not guess --all")
	}
	if !strings.Contains(page, "READ IS A CURSOR") {
		t.Error("--all must still return the page, not a summary of it")
	}
	// The template and the flag list are in both: they are what a caller scans.
	for _, want := range []string{"read <session> <index>", "Flags: --"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q", want)
		}
	}
}

// A verb that has not migrated keeps its page, so the surface can move one verb at a
// time without a half-migrated table anywhere.
func TestUnmigratedVerbStillReturnsItsPage(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "help run", "bytes_limit": 32768})
	if !strings.Contains(got, "THIS TYPES INTO A TERMINAL") {
		t.Error("a verb with no summary must return its page, unchanged")
	}
	if strings.Contains(got, "--all`") {
		t.Error("a verb with no summary must not advertise a page it already returned")
	}
}

// The table walks the declaration, so a flag cannot go unmentioned — the failure mode
// of a hand-kept list is silence, and silence is what a caller cannot notice.
func TestEveryDeclaredFlagOfAMigratedVerbHasALine(t *testing.T) {
	op, ok := directive.Lookup("read")
	if !ok {
		t.Fatal("read is not declared")
	}
	if !directive.HasSummaries("read") {
		t.Fatal("read should be migrated")
	}
	for _, flag := range op.FlagNames() {
		if _, ok := directive.FlagSummary("read", flag); !ok {
			t.Errorf("--%s is declared by read but has no summary line", flag)
		}
	}
}

// The summary quotes a price for the page. It has to be the bill: a caller deciding
// whether to spend a byte window on it is reading that number.
func TestSummaryQuotesTheRealPageSize(t *testing.T) {
	s := newTestServer(t)

	// Both windows are raised: the page is over the default lines_limit of 200, and a
	// cut page is not the price the summary quotes.
	summary := termText(t, s, map[string]any{"cmd": "help read", "bytes_limit": 32768, "lines_limit": 100000})
	page := termText(t, s, map[string]any{"cmd": "help read --all", "bytes_limit": 32768, "lines_limit": 100000})

	const marker = "The full page is "
	i := strings.Index(summary, marker)
	if i < 0 {
		t.Fatal("the summary does not name the page's size")
	}
	rest := summary[i+len(marker):]
	j := strings.Index(rest, " bytes")
	if j < 0 {
		t.Fatal("the quoted size has no unit")
	}
	quoted := rest[:j]

	// The rendered text is headers, a blank line, then the body; the price is the body.
	if sep := strings.Index(page, "\n\n"); sep >= 0 {
		page = page[sep+2:]
	}
	if got := fmt.Sprintf("%d", len(page)); got != quoted {
		t.Errorf("summary quotes %s bytes but `--all` returns %s", quoted, got)
	}
}

// TestNoSurfaceRendersADeclaration pins the boundary between the table and the caller.
//
// A flag's value domain is written into the flag's own entry (`offset:number`), which is
// what keeps one list true instead of two — and it is the table's business, not the
// caller's. Every surface that prints a flag name walks that same list, so the
// annotation leaked: `help read` rendered a column of `--cursor:number`, and the summary
// table — keyed by name — had nothing to put beside it. Two upstream tests caught the
// consequence (an empty description, a name `help` never mentioned); this one names the
// cause, so the next surface that walks op.Flags fails here instead of in the reply.
func TestNoSurfaceRendersADeclaration(t *testing.T) {
	s := newTestServer(t)
	for _, op := range directive.Ops() {
		for _, f := range op.Flags {
			name, domain, ok := strings.Cut(f, ":")
			if !ok || domain == "" {
				continue
			}
			// Only the line window is raised. bytes_limit has to stay under
			// total_bytes_limit (the cap for the whole reply), and the default byte
			// window is already larger than any page this walks.
			body := termText(t, s, map[string]any{"cmd": "help " + op.Name, "lines_limit": 100000})
			if strings.Contains(body, "--"+f) {
				t.Errorf("help %s renders the declaration --%s; the caller can only pass --%s", op.Name, f, name)
			}
			if !strings.Contains(body, "--"+name) {
				t.Errorf("help %s does not mention --%s", op.Name, name)
			}
		}
	}
}
