package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// toolListPeer is an MCP-ish peer that answers `tools/list` with a real tool list,
// so a test can see what `mcp open` discovered.
func toolListPeer(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var f map[string]any
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			switch f["method"] {
			case "tools/list":
				_ = conn.WriteJSON(map[string]any{
					"jsonrpc": "2.0", "id": f["id"],
					"result": map[string]any{"tools": []any{
						map[string]any{"name": "read_file", "description": "Read a file from the workspace"},
						map[string]any{"name": "run_query", "description": "Execute a SQL query"},
					}},
				})
			default:
				_ = conn.WriteJSON(map[string]any{"id": f["id"], "result": map[string]any{"ok": true}})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMcpOpenReportsDiscoveredTools pins design review §3.3② and its acceptance
// criterion: "can an agent that has never read the MCP spec make the next call
// correctly from this reply alone?"
//
// Before this, `mcp open` answered with a handle and a transport name, and the next
// step — an `initialize` handshake, then `tools/list`, then a call addressed by tool
// name — was unguessable without the spec. The adapter exists to carry that state
// machine, so it should hand back the state machine's result, not a receipt.
func TestMcpOpenReportsDiscoveredTools(t *testing.T) {
	srv := toolListPeer(t)
	s := newTestServer(t)
	internalSession(t, s) // gateways need a session to tunnel through

	out := termText(t, s, map[string]any{"cmd": "open internal mcp ws" + strings.TrimPrefix(srv.URL, "http")})
	h := handleFrom(t, out)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	if !strings.Contains(out, "read_file") {
		t.Errorf("open must report the peer's tools:\n%s", out)
	}
	if !strings.Contains(out, "run_query") {
		t.Errorf("every tool should be listed:\n%s", out)
	}
	// The next call's shape is the unguessable part, and it is now the HIGH-LEVEL one:
	// a tool name and an arguments object, not a hand-written envelope (#159). The
	// raw frame is still there for everything that is not a tool call, but it is no
	// longer what a caller should be taught first.
	if !strings.Contains(out, "call "+h+" <tool>") {
		t.Errorf("open must name the next call's shape, since that is the unguessable part:\n%s", out)
	}
	if !strings.Contains(out, "get "+h+" tools") {
		t.Errorf("open should point at the call that lists the required arguments:\n%s", out)
	}
	// The handle must appear in the suggested command, or the caller still has to
	// join two pieces of the reply by hand.
	if !strings.Contains(out, "call "+h) {
		t.Errorf("the suggested call must use this handle:\n%s", out)
	}
}

// TestMcpOpenSucceedsEvenWhenDiscoveryFails pins the best-effort rule: a peer that
// opens but cannot list tools is still an open connection, and failing the whole
// open over a discovery query would turn a partial success into a total failure.
func TestMcpOpenSucceedsEvenWhenDiscoveryFails(t *testing.T) {
	srv, _ := countingPeer(t, nil) // answers initialize, but tools/list returns no tools array
	s := newTestServer(t)
	internalSession(t, s) // gateways need a session to tunnel through

	out := termText(t, s, map[string]any{"cmd": "open internal mcp ws" + strings.TrimPrefix(srv.URL, "http")})
	if !strings.HasPrefix(out, "TERM/1 200") {
		t.Fatalf("open must succeed even when discovery cannot list tools:\n%s", out)
	}
	h := handleFrom(t, out)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
}

// TestJrpcOpenDoesNotClaimTools pins that the three adapters keep their own shapes:
// only MCP has a tool list, so only MCP reports one.
func TestJrpcOpenDoesNotClaimTools(t *testing.T) {
	srv, _ := countingPeer(t, nil)
	s := newTestServer(t)
	internalSession(t, s) // gateways need a session to tunnel through

	out := termText(t, s, map[string]any{"cmd": "open internal jrpc ws" + strings.TrimPrefix(srv.URL, "http")})
	h := handleFrom(t, out)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	if strings.Contains(out, "tools/call") {
		t.Errorf("jrpc is a bare frame channel and must not advertise tool discovery:\n%s", out)
	}
}

// TestMcpOpenRefusesAPeerThatCannotHandshake pins the other half of "report what
// the connection is for": when the MCP handshake fails, the open FAILED.
//
// It used to answer 200 with a handle and the failure in the body, which meant every
// later call repeated a fact the open already knew — and reported it as 500
// connection_failed, blaming the transport for what is a wrong URL (found by
// dogfooding: `open … mcp http://plain-http-server/` answered 200, and the next
// `call` answered Internal Server Error).
func TestMcpOpenRefusesAPeerThatCannotHandshake(t *testing.T) {
	// A plain HTTP server: it answers, but it is not an MCP endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	s := newTestServer(t)
	internalSession(t, s)

	// The registry is package-global, so the assertion is "this call added nothing"
	// rather than "the registry is empty": another test's live peer is not this
	// test's leak.
	countMCP := func() int {
		n := 0
		jrpcChannels.Range(func(_, v any) bool {
			if h, _ := v.(*jrpcHandle); h != nil && h.kind == "mcp" {
				n++
			}
			return true
		})
		return n
	}
	before := countMCP()

	out := termText(t, s, map[string]any{"cmd": "open internal mcp " + srv.URL})
	if !strings.HasPrefix(out, "TERM/1 502") {
		t.Fatalf("an open whose handshake failed must not report success:\n%s", out)
	}
	if _, hasHandle := headerValue(t, out, "Handle"); hasHandle {
		t.Errorf("a refused open handed back a handle:\n%s", out)
	}
	if !strings.Contains(out, "not an MCP endpoint") {
		t.Errorf("the refusal does not say what the URL is:\n%s", out)
	}
	// A PORT THAT IS NOT UP IS STILL AN OPEN: an HTTP channel dials on its first frame,
	// so a server that has not started yet is a handle worth having. The two cases have
	// to stay distinguishable, or the lazy-connect contract this door has always had
	// would be replaced by "every open needs a running server".
	dead := termText(t, s, map[string]any{"cmd": "open internal mcp http://127.0.0.1:1"})
	if !strings.HasPrefix(dead, "TERM/1 200") {
		t.Fatalf("an unreachable peer should still open (it dials lazily):\n%s", dead)
	}
	if h, ok := headerValue(t, dead, "Handle"); ok {
		termText(t, s, map[string]any{"cmd": "close " + h})
	}

	// No channel left behind: a handle that cannot carry a tool call has no use but
	// `close`, so the refused open must not add one to the registry.
	if after := countMCP(); after != before {
		t.Errorf("the refused peer was left in the registry (%d → %d)", before, after)
	}
}
