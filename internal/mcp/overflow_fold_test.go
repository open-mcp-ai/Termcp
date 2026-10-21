package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// fold has to empty the body when the window ended before the stream did.
//
// The old test was on the body's SIZE (`len(body) > win.BytesLimit`), but the
// handler had already applied that limit, so the comparison was false by
// construction and the policy never ran: `on_bytes_overflow: fold` returned the
// truncated window anyway. The parameter was parsed, validated, and ignored.
func TestByteFoldEmptiesAnIncompleteWindow(t *testing.T) {
	s := newTestServer(t)
	stmt := mustStatement(t, "read a1f3:1")
	win := defaultTermArgs()
	win.BytesLimit = 20
	win.OnBytesOverflow = "fold"

	doc := map[string]any{
		"output":       strings.Repeat("x", 20),
		"start_offset": float64(0),
		"end_offset":   float64(20),
		"total_bytes":  float64(500),
		"has_more":     true,
	}
	slot := s.windowSlot(stmt, win, doc)
	if slot.Body != "" {
		t.Fatalf("fold kept %d bytes of an incomplete window", len(slot.Body))
	}
	if slot.Code != 206 {
		t.Errorf("code = %d, want 206 (a partial read)", slot.Code)
	}
}

// The same window with truncate keeps what fits — fold must not be the only path.
func TestByteTruncateKeepsTheWindow(t *testing.T) {
	s := newTestServer(t)
	stmt := mustStatement(t, "read a1f3:1")
	win := defaultTermArgs()
	win.BytesLimit = 20
	win.OnBytesOverflow = "truncate"

	doc := map[string]any{
		"output":       strings.Repeat("x", 20),
		"start_offset": float64(0),
		"end_offset":   float64(20),
		"total_bytes":  float64(500),
		"has_more":     true,
	}
	slot := s.windowSlot(stmt, win, doc)
	if len(slot.Body) == 0 {
		t.Fatal("truncate dropped a window it should have kept")
	}
}

// A window that is NOT short must be returned whole: folding a complete read would
// turn "nothing more to read" into "nothing to read".
func TestFoldDoesNothingWhenTheWindowIsComplete(t *testing.T) {
	s := newTestServer(t)
	stmt := mustStatement(t, "read a1f3:1")
	win := defaultTermArgs()
	win.BytesLimit = 20
	win.OnBytesOverflow = "fold"

	doc := map[string]any{
		"output":       "short\n",
		"start_offset": float64(0),
		"end_offset":   float64(6),
		"total_bytes":  float64(6),
		"has_more":     false,
	}
	slot := s.windowSlot(stmt, win, doc)
	if slot.Body == "" {
		t.Fatal("a complete window was folded away")
	}
}

func mustStatement(t *testing.T, cmd string) directive.Statement {
	t.Helper()
	f, err := directive.ParseWithSubst(cmd, directive.Subst{})
	if err != nil {
		t.Fatalf("%q: %v", cmd, err)
	}
	return f.Statements[0]
}
