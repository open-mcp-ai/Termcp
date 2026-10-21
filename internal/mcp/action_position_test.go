package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// The action of profile/ext can be spelled as a flag or as the first positional
// word, and the NAME moves between the two shapes. Reading the name as a fixed
// positional index is what broke `profile <name> --action edit` and
// `ext <name> --action update`: the name was in Args[0] and the code read Args[1].
func TestResolveActionConsumesTheFlagOrTheFirstWord(t *testing.T) {
	cases := []struct {
		cmd        string
		wantAction string
		wantRest   []string
	}{
		// Action as a flag: every positional word is data.
		{"profile --action edit --name demo", "edit", nil},
		{"profile --action create", "create", nil},
		// Action as the first positional word: it is consumed.
		{"profile create demo", "create", []string{"demo"}},
		{"profile demo --action edit", "edit", []string{"demo"}},
		{"profile demo", "demo", nil},
		// Nothing at all: the fallback stands.
		{"profile", "list", nil},
	}
	for _, tc := range cases {
		f, err := directive.Parse(tc.cmd)
		if err != nil {
			t.Errorf("%q: %v", tc.cmd, err)
			continue
		}
		gotAction, gotRest := resolveAction(f.Statements[0], "list")
		if gotAction != tc.wantAction {
			t.Errorf("%q: action = %q, want %q", tc.cmd, gotAction, tc.wantAction)
		}
		if strings.Join(gotRest, ",") != strings.Join(tc.wantRest, ",") {
			t.Errorf("%q: rest = %v, want %v", tc.cmd, gotRest, tc.wantRest)
		}
	}
}

// A name given by --name wins, and the positional word is then not a name. This is
// what keeps `profile create --name demo` (the form the tests already used) working.
func TestResolveActionLeavesNameFlagsAlone(t *testing.T) {
	f, err := directive.Parse("profile create demo --name other")
	if err != nil {
		t.Fatal(err)
	}
	action, rest := resolveAction(f.Statements[0], "list")
	if action != "create" {
		t.Fatalf("action = %q", action)
	}
	if len(rest) != 1 || rest[0] != "demo" {
		t.Fatalf("rest = %v, want [demo]", rest)
	}
}
