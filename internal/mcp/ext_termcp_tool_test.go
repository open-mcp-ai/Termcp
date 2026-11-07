package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// termcpDeclServer is a stub downstream whose tools/list answer is supplied by
// the test, so each case can say exactly which shape is being refused.
func termcpDeclServer(t *testing.T, tools []any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		id := env["id"]
		method, _ := env["method"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"serverInfo":      map[string]any{"name": "stub", "version": "1"},
					"capabilities":    map[string]any{"tools": map[string]any{}},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"tools": tools},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		}
	}))
}

// TestATermcpToolDeclarationIsVerifiedNotTrusted pins the three outcomes of
// `termcp_tool = "…"` (issue #274), and the rule behind them: a declaration is
// checked against the peer's own tool list, and a wrong one degrades the
// extension instead of breaking it.
//
// The shape requirement is the load-bearing part. TERM/1 is a line of text, so a
// downstream that takes `{query, top_k}` cannot be handed one, and one that takes
// `{cmd, timeout}` would silently force this side to implement the downstream's
// window semantics. Exactly one string argument means the recursion has one
// meaning.
func TestATermcpToolDeclarationIsVerifiedNotTrusted(t *testing.T) {
	oneString := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cmd": map[string]any{"type": "string"},
		},
		"required": []any{"cmd"},
	}

	cases := []struct {
		name   string
		tools  []any
		tool   string
		wantOK bool
		want   string // substring the refusal must explain
	}{
		{
			name:   "declared tool speaks TERM/1",
			tools:  []any{map[string]any{"name": "term", "inputSchema": oneString}},
			tool:   "term",
			wantOK: true,
		},
		{
			name:  "no such tool",
			tools: []any{map[string]any{"name": "other", "inputSchema": oneString}},
			tool:  "term",
			want:  "no tool named",
		},
		{
			name: "takes two arguments",
			tools: []any{map[string]any{"name": "term", "inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd":   map[string]any{"type": "string"},
					"limit": map[string]any{"type": "number"},
				},
				"required": []any{"cmd", "limit"},
			}}},
			tool: "term",
			want: "requires 2 arguments",
		},
		{
			name: "takes no arguments at all",
			tools: []any{map[string]any{"name": "term", "inputSchema": map[string]any{
				"type": "object", "properties": map[string]any{},
			}}},
			tool: "term",
			want: "takes no arguments",
		},
		{
			name:  "no schema to check",
			tools: []any{map[string]any{"name": "term"}},
			tool:  "term",
			want:  "no input schema",
		},
		{
			name:   "no declaration means an ordinary peer",
			tools:  []any{map[string]any{"name": "term", "inputSchema": oneString}},
			tool:   "",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := termcpDeclServer(t, tc.tools)
			defer upstream.Close()
			s := newExtensionTestServer(t)

			create := `create ext downstream --target internal --url ` + upstream.URL
			if tc.tool != "" {
				create += ` --termcp-tool ` + tc.tool
			}
			if got := termText(t, s, map[string]any{"cmd": create}); !strings.HasPrefix(got, "TERM/1 200") {
				t.Fatalf("create ext failed:\n%s", got)
			}
			opened := termText(t, s, map[string]any{"cmd": "open ext downstream", "timeout": 15})
			if !strings.HasPrefix(opened, "TERM/1 200") {
				t.Fatalf("open ext failed:\n%s", opened)
			}
			// THE MOUNT MUST SUCCEED EITHER WAY. A declaration this side does not
			// accept is a typo in a template, not a reason for the instance to
			// refuse to serve it.
			if !strings.Contains(opened, "call ") {
				t.Errorf("a refused declaration must leave the peer usable, so the reply still teaches `call`:\n%s", opened)
			}

			if tc.wantOK {
				if !strings.Contains(opened, "speaks TERM/1") {
					t.Errorf("an accepted declaration must be reported:\n%s", opened)
				}
				return
			}
			if tc.tool == "" {
				if strings.Contains(opened, "termcp_tool") {
					t.Errorf("an extension with no declaration must not mention one:\n%s", opened)
				}
				return
			}
			if !strings.Contains(opened, tc.want) {
				t.Errorf("the refusal must explain itself (%q):\n%s", tc.want, opened)
			}
		})
	}
}
