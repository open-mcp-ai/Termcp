package directive

import (
	"strings"
	"testing"
)

// TestKindDetailPagesAreReachable pins #288: a detail page that nothing references is
// a page no caller can read. `forward` was 54 lines of the only text that says which
// flag belongs to which direction, and neither openDetail nor nodeNotes mentioned it.
func TestKindDetailPagesAreReachable(t *testing.T) {
	for _, name := range []string{"forward", "mcp", "cdp", "jrpc"} {
		page, ok := KindDetail(name)
		if !ok || strings.TrimSpace(page) == "" {
			t.Errorf("help %s can never show its page: KindDetail(%q) is empty", name, name)
		}
	}
	if page, ok := KindDetail("nosuchkind"); ok || page != "" {
		t.Error("an unknown kind must not resolve to a page")
	}
}

// TestKeyDetailNamesTheKeyDomain pins #286: `key <channel> <key>` said WHERE, not
// WHAT, and the accepted names lived only in the refusal. The page must name them,
// including both spellings KeyBytes accepts.
func TestKeyDetailNamesTheKeyDomain(t *testing.T) {
	op, ok := Lookup("key")
	if !ok {
		t.Fatal("key is not a directive")
	}
	if strings.TrimSpace(op.Detail) == "" {
		t.Fatal("key has no detail page, so its operand domain is discoverable only by guessing wrong")
	}
	for _, want := range []string{"enter", "tab", "esc", "escape", "ctrl+c", "ctrl+w", "--repeat"} {
		if !strings.Contains(op.Detail, want) {
			t.Errorf("the key page does not mention %q", want)
		}
	}
}
