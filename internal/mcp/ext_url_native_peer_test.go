package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExtURLMountsAsNativeMCPPeer pins route 2 (issue #271):
// A `create ext --url ...` MCP server is mounted as a real MCP peer by `open ext <name>`.
// The caller gets a real peer handle (mcp:1) and invokes tools by name directly
// (`call <session> mcp:1 <tool> {args}`), without hand-writing initialize,
// tools/list, or raw JSON-RPC envelope frames.
func TestExtURLMountsAsNativeMCPPeer(t *testing.T) {
	// A minimal streamable-HTTP MCP server that answers initialize, tools/list, and tools/call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify custom header passed from ext.Headers:
		if r.Header.Get("Authorization") != "Bearer test-token-123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
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
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"serverInfo":      map[string]any{"name": "mock-mcp", "version": "1.0"},
					"capabilities":    map[string]any{"tools": map[string]any{}},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"tools": []any{
						map[string]any{
							"name":        "echo_tool",
							"description": "echoes the text argument",
							"inputSchema": map[string]any{"type": "object"},
						},
					},
				},
			})
		case "tools/call":
			params, _ := env["params"].(map[string]any)
			args, _ := params["arguments"].(map[string]any)
			text, _ := args["text"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "ECHO: " + text},
					},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"ok": true}})
		}
	}))
	defer srv.Close()

	s := newExtensionTestServer(t)

	// 1. Create the extension with URL and Authorization header:
	createRes := termText(t, s, map[string]any{
		"cmd": `create ext mymcp --target internal --url ` + srv.URL + ` --headers '{"Authorization":"Bearer test-token-123"}'`,
	})
	if !strings.HasPrefix(createRes, "TERM/1 200") {
		t.Fatalf("create ext failed:\n%s", createRes)
	}

	// 2. Open the extension:
	openRes := termText(t, s, map[string]any{"cmd": "open ext mymcp", "timeout": 10})
	if !strings.HasPrefix(openRes, "TERM/1 200") {
		t.Fatalf("open ext failed:\n%s", openRes)
	}
	// Verify that open ext reports tools and tells the caller to use `call`:
	if !strings.Contains(openRes, "echo_tool") {
		t.Errorf("open ext reply did not discover tools:\n%s", openRes)
	}
	if !strings.Contains(openRes, "call ") {
		t.Errorf("open ext reply missing call instruction:\n%s", openRes)
	}

	// Extract the peer handle from the Handle header:
	handle, ok := headerValue(t, openRes, "Handle")
	if !ok || !strings.Contains(handle, "mcp:") {
		t.Fatalf("open ext Handle header is not an mcp peer address: %q\nFull reply:\n%s", handle, openRes)
	}

	// 2.5 AI checks tool discovery via `get`:
	getRes := termText(t, s, map[string]any{"cmd": "get " + handle + " tools"})
	if !strings.HasPrefix(getRes, "TERM/1 200") || !strings.Contains(getRes, "echo_tool") {
		t.Fatalf("get tools failed:\n%s", getRes)
	}

	// 3. AI calls the tool DIRECTLY BY NAME, with NO JSON-RPC ENVELOPE:
	callRes := termText(t, s, map[string]any{
		"cmd":     `call ` + handle + ` echo_tool {"text":"hello world"}`,
		"timeout": 10,
	})
	if !strings.HasPrefix(callRes, "TERM/1 200") {
		t.Fatalf("call tool failed:\n%s", callRes)
	}
	// The result is CLEAN UNPACKED TEXT, not a JSON-RPC envelope:
	if !strings.Contains(callRes, "ECHO: hello world") {
		t.Fatalf("call result missing unpacked text:\n%s", callRes)
	}
	if strings.Contains(callRes, `"jsonrpc"`) {
		t.Fatalf("call result should be unpacked text, not raw JSON-RPC frame:\n%s", callRes)
	}

	// 4. Release cleans up the peer:
	closeRes := termText(t, s, map[string]any{"cmd": "close ext mymcp"})
	if !strings.HasPrefix(closeRes, "TERM/1 200") {
		t.Fatalf("close ext failed:\n%s", closeRes)
	}

	// 5. The released peer is gone, not silently kept alive:
	callClosed := termText(t, s, map[string]any{
		"cmd":     `call ` + handle + ` echo_tool {"text":"hello"}`,
		"timeout": 5,
	})
	if strings.HasPrefix(callClosed, "TERM/1 200") {
		t.Fatalf("peer still answers after close ext:\n%s", callClosed)
	}
}

// TestExtURLExtensionAddressIsRecoverable pins the recovery path a caller needs
// when its context is gone (issue #271).
//
// `ls ext` is where an agent that lost the `open ext` reply looks for an address.
// An endpoint extension has no shell, so the address column used to fall back to
// the bare session id — a handle no `call` accepts. The only way back was to run
// `open ext` again and hope it was idempotent. The peer address belongs there.
func TestExtURLExtensionAddressIsRecoverable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		_ = json.NewDecoder(r.Body).Decode(&env)
		id := env["id"]
		w.Header().Set("Content-Type", "application/json")
		switch env["method"] {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "m", "version": "1"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"tools": []any{map[string]any{"name": "ping", "description": "p", "inputSchema": map[string]any{"type": "object"}}},
			}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}})
		}
	}))
	defer srv.Close()

	s := newExtensionTestServer(t)
	if got := termText(t, s, map[string]any{"cmd": `create ext recov --target internal --url ` + srv.URL}); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("create ext failed:\n%s", got)
	}
	if got := termText(t, s, map[string]any{"cmd": "open ext recov", "timeout": 10}); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("open ext failed:\n%s", got)
	}

	// The listing must name the address a caller can actually type next.
	listing := termText(t, s, map[string]any{"cmd": "ls ext"})
	var addr string
	for _, line := range strings.Split(listing, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) >= 3 && parts[0] == "recov" {
			addr = strings.TrimSpace(parts[2])
		}
	}
	if !strings.Contains(addr, "mcp:") {
		t.Fatalf("ls ext does not name the peer address (got %q):\n%s", addr, listing)
	}
	// And the recovered address works without a second `open ext`.
	call := termText(t, s, map[string]any{"cmd": `call ` + addr + ` ping {}`, "timeout": 10})
	if !strings.HasPrefix(call, "TERM/1 200") || !strings.Contains(call, "pong") {
		t.Fatalf("recovered address does not answer:\n%s", call)
	}
}

// TestOpenExtSummaryFoldsNotesWithoutAll pins that the operating essay is folded
// by default and restored by `--all` (issue #271).
func TestOpenExtSummaryFoldsNotesWithoutAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		_ = json.NewDecoder(r.Body).Decode(&env)
		id := env["id"]
		w.Header().Set("Content-Type", "application/json")
		switch env["method"] {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "m", "version": "1"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": []any{}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		}
	}))
	defer srv.Close()

	s := newExtensionTestServer(t)
	if got := termText(t, s, map[string]any{"cmd": `create ext fold --target internal --url ` + srv.URL}); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("create failed: %s", got)
	}

	// Default `open ext` on a MOUNTED PEER: the template's abstract notes are
	// replaced by the peer's concrete instructions (real handle, real syntax),
	// and the full prose stays behind `--all`.
	openDefault := termText(t, s, map[string]any{"cmd": "open ext fold", "timeout": 10})
	if !strings.Contains(openDefault, "full notes under --all") {
		t.Errorf("default open ext kept the template's notes instead of deferring to the peer note:\n%s", openDefault)
	}
	if strings.Contains(openDefault, "do not write JSON-RPC frames") {
		t.Errorf("default open ext repeated the notes' abstract peer warning:\n%s", openDefault)
	}
	if !strings.Contains(openDefault, "not a pipe: no handshake") {
		t.Errorf("default open ext missing the concrete peer note:\n%s", openDefault)
	}

	// With `--all`: the full notes prose is present.
	openAll := termText(t, s, map[string]any{"cmd": "open ext fold --all", "timeout": 10})
	if !strings.Contains(openAll, "This session is kept warm") {
		t.Errorf("open ext --all missing full notes body:\n%s", openAll)
	}
}
