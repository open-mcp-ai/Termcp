package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/term"
	"github.com/open-mcp-ai/termcp/term/directive"
)

// termcpDownstream is a stub that speaks TERM/1 through one MCP tool, and
// records every command line it was handed.
type termcpDownstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	lines []string
	// answer decides the reply. It sees the command line and the hop header, so a
	// case can make the downstream refuse, echo, or report the depth it saw.
	answer func(line string, hops string) string
}

func newTermcpDownstream(t *testing.T, answer func(line, hops string) string) *termcpDownstream {
	t.Helper()
	d := &termcpDownstream{answer: answer}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					"serverInfo":      map[string]any{"name": "downstream", "version": "1"},
					"capabilities":    map[string]any{"tools": map[string]any{}},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"tools": []any{map[string]any{
					"name": "term",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
						"required":   []any{"cmd"},
					},
				}}},
			})
		case "tools/call":
			params, _ := env["params"].(map[string]any)
			args, _ := params["arguments"].(map[string]any)
			line, _ := args["cmd"].(string)
			d.mu.Lock()
			d.lines = append(d.lines, line)
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": d.answer(line, "")},
				}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *termcpDownstream) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.lines...)
}

// TestRunRoutesToADeclaredDownstream pins the core of #274: `run <ext> <words>`
// on an extension that declared `termcp_tool` sends the words as ONE command
// line, and the downstream's TERM/1 envelope comes back as this call's answer.
//
// The alternative — inferring from the shape of the arguments — is what this
// design refuses, so the test also pins the other half: an extension with no
// declaration is never routed to, however command-like its arguments look.
func TestRunRoutesToADeclaredDownstream(t *testing.T) {
	d := newTermcpDownstream(t, func(line, _ string) string {
		return "TERM/1 200 OK\nPath: " + line + "\n\nran " + line
	})
	s := newExtensionTestServer(t)

	if got := termText(t, s, map[string]any{
		"cmd": `create ext gw --target internal --url ` + d.srv.URL + ` --termcp-tool term`,
	}); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("create ext failed:\n%s", got)
	}
	if got := termText(t, s, map[string]any{"cmd": "open ext gw", "timeout": 15}); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("open ext failed:\n%s", got)
	}

	// THE VARIABLE SHAPE IS THE PROTOCOL'S, not an inconvenience: `run` refuses a
	// literal line because the payload would then be read by two languages at once.
	// A recursive call uses the same shape, and the downstream receives the value.
	reply := termText(t, s, map[string]any{
		"cmd": "run gw $1", "args": []any{"recall napcat 部署"}, "timeout": 20,
	})
	if !strings.HasPrefix(reply, "TERM/1 200") {
		t.Fatalf("run through a declared downstream failed:\n%s", reply)
	}
	// ONE LINE, WHOLE: the recursion's unit is the command line, not a word.
	got := d.got()
	if len(got) != 1 || got[0] != "recall napcat 部署" {
		t.Fatalf("the downstream was handed %q, want one line \"recall napcat 部署\"", got)
	}
	if !strings.Contains(reply, "ran recall napcat 部署") {
		t.Errorf("the downstream's body did not come back:\n%s", reply)
	}
	// The depth is declared on every reply, success included.
	if hops, ok := headerValue(t, reply, hopsHeader); !ok || hops != "1" {
		t.Errorf("%s = %q, want 1:\n%s", hopsHeader, hops, reply)
	}
}

// TestADownstreams208ShortCircuitsTheOuterBranch pins the short-circuit contract:
// when the downstream says 208 Not Run, this side must pass that through with the
// slot preserved, so a caller reading slot i of the answer is reading the same
// statement it wrote.
func TestADownstreams208ShortCircuitsTheOuterBranch(t *testing.T) {
	d := newTermcpDownstream(t, func(line, _ string) string {
		return "TERM/1 208 Not Run\nError-Code: short_circuited\nHint: the branch was not taken\n\nnot run"
	})
	s := newExtensionTestServer(t)
	termText(t, s, map[string]any{"cmd": `create ext gw --target internal --url ` + d.srv.URL + ` --termcp-tool term`})
	termText(t, s, map[string]any{"cmd": "open ext gw", "timeout": 15})

	reply := termText(t, s, map[string]any{"cmd": "run gw $1", "args": []any{"go"}, "timeout": 20})
	if !strings.HasPrefix(reply, "TERM/1 208") {
		t.Fatalf("a downstream 208 must be forwarded as 208:\n%s", reply)
	}
	if hint, ok := headerValue(t, reply, "Hint"); !ok || !strings.Contains(hint, "branch was not taken") {
		t.Errorf("the downstream's Hint must reach the caller:\n%s", reply)
	}
}

// TestAnUndeclaredPeerIsNeverRoutedTo pins the regression the issue calls out:
// behaviour for an extension that declares nothing must be exactly what it was.
// `run` there is a shell verb, so an extension target is not a command line to
// forward — and the downstream must not receive one.
func TestAnUndeclaredPeerIsNeverRoutedTo(t *testing.T) {
	d := newTermcpDownstream(t, func(line, _ string) string { return "TERM/1 200 OK\n\nran " + line })
	s := newExtensionTestServer(t)
	termText(t, s, map[string]any{"cmd": `create ext plain --target internal --url ` + d.srv.URL})
	termText(t, s, map[string]any{"cmd": "open ext plain", "timeout": 15})

	termText(t, s, map[string]any{"cmd": "run plain $1", "args": []any{"recall napcat"}, "timeout": 20})
	if got := d.got(); len(got) != 0 {
		t.Fatalf("an undeclared peer was routed to with %q; only a declared downstream may be", got)
	}
}

// TestALoopStopsAtTheHopLimit pins that a cycle terminates at a declared place
// rather than at a timeout, and that the refusal names the limit it hit.
//
// The chain here is one hop deep on this side (a stub cannot call back), so the
// loop is simulated by handing the router a context that already carries the
// limit — which is exactly what the header does on the wire.
func TestALoopStopsAtTheHopLimit(t *testing.T) {
	d := newTermcpDownstream(t, func(line, _ string) string { return "TERM/1 200 OK\n\nok" })
	s := newExtensionTestServer(t)
	termText(t, s, map[string]any{"cmd": `create ext gw --target internal --url ` + d.srv.URL + ` --termcp-tool term`})
	termText(t, s, map[string]any{"cmd": "open ext gw", "timeout": 15})

	ctx := withHops(t.Context(), maxTermcpHops)
	file, err := term.New(directive.DefaultGrammar).Parse("run gw $1", term.Subst{Pos: []string{"anything"}})
	if err != nil {
		t.Fatal(err)
	}
	stmt := file.Statements[0]
	slots, routed := s.termcpRoute(ctx, stmt, termArgs{})
	if !routed {
		t.Fatal("the router did not claim a declared downstream")
	}
	if len(slots) != 1 || slots[0].Code != 400 {
		t.Fatalf("a chain at the hop limit must be refused, got %+v", slots)
	}
	if code := slots[0].Headers["Error-Code"]; code != "termcp_hop_limit" {
		t.Errorf("Error-Code = %q, want termcp_hop_limit", code)
	}
	if !strings.Contains(slots[0].Body, fmt.Sprint(maxTermcpHops)) {
		t.Errorf("the refusal must name the limit it hit:\n%s", slots[0].Body)
	}
	if got := d.got(); len(got) != 0 {
		t.Errorf("nothing may be sent once the limit is reached, got %q", got)
	}
}

// TestANonEnvelopeAnswerIsForwardedNotParsed pins the boundary with a downstream
// that speaks MCP but not TERM/1 in the reply. Its words must reach the caller
// unchanged: a gateway that reinterprets them has changed what the downstream
// said.
func TestANonEnvelopeAnswerIsForwardedNotParsed(t *testing.T) {
	d := newTermcpDownstream(t, func(line, _ string) string {
		return "plain words, not an envelope"
	})
	s := newExtensionTestServer(t)
	termText(t, s, map[string]any{"cmd": `create ext gw --target internal --url ` + d.srv.URL + ` --termcp-tool term`})
	termText(t, s, map[string]any{"cmd": "open ext gw", "timeout": 15})

	reply := termText(t, s, map[string]any{"cmd": "run gw $1", "args": []any{"hello"}, "timeout": 20})
	if !strings.Contains(reply, "plain words, not an envelope") {
		t.Fatalf("the downstream's text did not reach the caller:\n%s", reply)
	}
	if !strings.Contains(reply, "Downstream: gw") {
		t.Errorf("the reply must say it came from a downstream rather than being parsed:\n%s", reply)
	}
}
