package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/mcp"
)

// TestAnUnknownArgumentIsRefusedByName pins the rule that keeps a deleted parameter
// from becoming a silent no-op.
//
// The tool used to publish `pipe_to` and `allow_escapes`. Both are gone, and the
// failure mode of a removal is the same as the failure mode of the bug `pipe_to`
// itself was: the caller keeps asking for something, gets a 200, and the effect
// simply does not happen. Nothing in the reply says so — the caller has to notice
// the bytes came back inline instead of being piped, or that its Ctrl-C never
// arrived.
//
// So an argument the tool does not have is a 400 that names it and lists what the
// tool does take. A typo (`timeout_seconds`) is the same mistake one letter away
// from a correct call, and it lands in the same place.
func TestAnUnknownArgumentIsRefusedByName(t *testing.T) {
	s := newTestServer(t)

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"the retired pipe_to", map[string]any{"cmd": "ls", "pipe_to": "a1f3:1"}},
		{"the retired allow_escapes", map[string]any{"cmd": "ls", "allow_escapes": true}},
		{"a typo", map[string]any{"cmd": "ls", "timeout_seconds": 5}},
		{"a name from another tool", map[string]any{"cmd": "ls", "shell_id": "a1f3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, tc.args)
			if !strings.HasPrefix(got, "TERM/1 4") {
				t.Fatalf("an unknown argument must be refused with 400:\n%s", got)
			}
			// The refusal names the argument the caller sent...
			bad := tc.name
			_ = bad
			for k := range tc.args {
				if k == "cmd" {
					continue
				}
				if !strings.Contains(got, k) {
					t.Errorf("the refusal does not name %q:\n%s", k, got)
				}
			}
			// ...and lists what the tool takes, from the same table the schema is
			// generated from.
			for _, known := range []string{"timeout", "bytes_limit", "learn_mode", "vars"} {
				if !strings.Contains(got, known) {
					t.Errorf("the refusal does not list %q among the known arguments:\n%s", known, got)
				}
			}
		})
	}

	// The complement: every published argument is still accepted. Without this the
	// refusal above could be "reject everything" and the test would pass.
	for _, d := range termArgDecls {
		if _, err := parseTermArgs(map[string]any{d.Name: sampleValueFor(d.Kind)}); err != nil {
			t.Errorf("declared argument %q is refused by the reader: %s", d.Name, err.Msg)
		}
	}
}

// sampleValueFor builds a legal value for each declared argument kind, so the
// complement above exercises the reader rather than the argument table's types.
func sampleValueFor(kind argKind) any {
	switch kind {
	case argString:
		return "ls"
	case argStringArray:
		return []any{"echo hi"}
	case argMap:
		return map[string]any{"V": "echo hi"}
	case argNumber:
		// A number must be legal for EVERY numeric argument, and the windows now have a
		// floor as well as a ceiling: `1` is a legal timeout and an illegal byte window
		// (a window narrower than one character cannot return one). The sample is the
		// widest floor in the table, so this test measures "is the name accepted"
		// rather than "is this particular value in range".
		return float64(4)
	case argBool:
		return true
	default: // argEnum
		return "truncate"
	}
}

// TestTheToolSaysChannelNotHandle pins the one word the whole surface uses for a
// shell a caller drives.
//
// `handle` was the older word, and it leaked into user-facing text in seventeen
// places — schema descriptions, error messages, docs. Two words for one thing cost
// the caller a lookup the first time ("is a handle the same as a channel?") and
// nothing after that, which is exactly the kind of tax nobody reports and everybody
// pays. The check reads the PUBLISHED schema and the guide, not the source: the
// source may keep its own identifiers.
func TestTheToolSaysChannelNotHandle(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	var term *mcp.Tool
	tools := listTools(t, s, originContext("http://127.0.0.1:18765"))
	for i := range tools {
		if tools[i].Name == termToolName {
			term = &tools[i]
		}
	}
	if term == nil {
		t.Fatalf("%q is not registered", termToolName)
	}
	raw, err := json.Marshal(term)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "handle") {
		t.Errorf("the published schema says \"handle\"; a shell a caller drives is a channel:\n%s", raw)
	}

	// The guide is the other text every caller reads first.
	live := newTestServer(t)
	guide := termText(t, live, map[string]any{"cmd": ""})
	if strings.Contains(strings.ToLower(guide), "handle") {
		t.Errorf("the guide says \"handle\":\n%s", guide)
	}
	if !strings.Contains(guide, "channel") {
		t.Errorf("the guide never says \"channel\", which is the word it should use:\n%s", guide)
	}
}
