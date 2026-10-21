package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// TestTokenBudgetGuard prevents regression on the one payload every client pays
// for on every turn.
//
// Issue #117 replaced a 32-tool, ~24 KB surface with a single `term` entry, and
// the point of that redesign is that the surface stops growing with the capability
// count. If a directive table, a file format or a usage page ever migrates back
// into the tool description, this fails.
//
// The two limits guard DIFFERENT costs and are not interchangeable. tools/list is
// paid once per session (plus on every client that re-lists), so it has the looser
// 2500 B. `instructions` is re-injected into every model turn by clients that honour
// it, so it is held much tighter: at 1600 B it holds the four facts that must be in
// context BEFORE the first call (one tool, handle shape, the human boundary, a held
// reply) and nothing else. Anything a model can discover by acting — the command
// space via `help`, a reply's shape via the reply — is excluded here and paid for
// only when it is used.
func TestTokenBudgetGuard(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")

	tools := listTools(t, s, originContext("http://127.0.0.1:18765"))
	if len(tools) != 1 {
		names := make([]string, 0, len(tools))
		for _, st := range tools {
			names = append(names, st.Name)
		}
		t.Fatalf("the tool surface must be exactly one tool, got %d: %v", len(tools), names)
	}
	if tools[0].Name != termToolName {
		t.Fatalf("the single tool is %q, want %q", tools[0].Name, termToolName)
	}

	b, err := json.Marshal(tools[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tools/list total: %d B", len(b))
	t.Logf("instructions: %d B", len(mcpServerInstructions))

	if len(b) >= 2500 {
		t.Errorf("the %s schema serialises to %d B, over the 2500 B budget", tools[0].Name, len(b))
	}
	if n := len(mcpServerInstructions); n > 1600 {
		t.Errorf("instructions %d B exceeds 1600 B budget", n)
	}

	// The description must not enumerate directives (issue #117 33d): the
	// semantics live in `cmd`'s syntax and in help/learn_mode, which is what lets
	// the command space grow without the tool growing. The check is about the
	// SHAPE of the text rather than the presence of an English word — "read its
	// result" is not the `read` directive, and asserting on bare words would fail
	// on the prose that makes the blurb readable.
	desc := tools[0].Description
	for _, op := range directive.Ops() {
		if strings.Contains(desc, "`"+op.Name+"`") {
			t.Errorf("the description formats %q as a directive; that is the start of a directive list:\n%s", op.Name, desc)
		}
	}
	if len(desc) > 400 {
		t.Errorf("the description is %d B; a blurb that stays short cannot be enumerating directives:\n%s", len(desc), desc)
	}
	// It must still point somewhere, or "do not list them" becomes "hide them".
	if !strings.Contains(desc, "cmd") {
		t.Errorf("the description does not mention cmd, so a caller does not know where the syntax lives:\n%s", desc)
	}
}
