package mcp

import (
	"strings"
	"testing"
)

// AN EXTENSION'S STDIO CHANNEL IS A PEER, NOT A BARE PIPE (issue #316).
//
// The HTTP form had `call <session> mcp:1 <tool> {…}` — a handshake, a tool table, and
// the tool's text as the answer. The same MCP server reached over a pipe was handed back
// as a plain channel and the caller was left to write `initialize`,
// `notifications/initialized` and a `tools/call` envelope by hand, then read frames off a
// byte stream and pair ids itself. Same protocol, two very different amounts of work, and
// nothing in the address said which one you had.
//
// The stub below is a real stdio MCP server: it answers `initialize` on the id it was
// given, lists two tools, returns text from one, reports failure inside the result from
// the other, and answers an unknown tool with a JSON-RPC error.
const stdioMCPStub = `
# THE ID IS ECHOED AS THE BYTES THAT ARRIVED, quotes included: the handshake sends a
# STRING id, and a stub that re-encodes it (a JSON round trip in a shell) turns
# "termcp-init" into termcp-init, which is not JSON — the reply is then unmatched and the
# caller waits out its whole window. Extracting the raw text is also the honest model of
# what a real MCP server does with the id it was handed.
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([^,}]*\).*/\1/p')
  [ -z "$id" ] && continue
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  case "$method" in
    initialize)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"stub316","version":"1"}}}\n' "$id" ;;
    tools/list)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping316","description":"returns the arguments as text","inputSchema":{"type":"object","properties":{"echo":{"type":"string"}}}},{"name":"fail316","description":"reports failure inside the result","inputSchema":{"type":"object"}}]}}\n' "$id" ;;
    tools/call)
      tool=$(printf '%s' "$line" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p')
      case "$tool" in
        ping316)
          args=$(printf '%s' "$line" | sed -n 's/.*"arguments":\({[^}]*}\).*/\1/p' | sed 's/"/\\"/g')
          printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"pong316 %s"}]}}\n' "$id" "$args" ;;
        fail316)
          printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"boom316"}],"isError":true}}\n' "$id" ;;
        *)
          printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"no tool %s"}}\n' "$id" "$tool" ;;
      esac ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
  esac
done
`

// stdioMCPExtension creates and opens an extension whose channel runs the stub, and
// returns the channel handle a caller holds.
func stdioMCPExtension(t *testing.T, s *Server, name string) string {
	t.Helper()
	script := writeScript(t, stdioMCPStub)
	callExtension(t, s, map[string]any{
		"action": "create", "name": name, "target": "internal",
		"command": "sh", "args": []any{script}, "enabled": true,
	})
	got := useExtension(t, s, name)
	shell, _ := got["shell_id"].(string)
	if shell == "" {
		t.Fatalf("extension %q has no channel: %v", name, got)
	}
	t.Cleanup(func() { termText(t, s, map[string]any{"cmd": "close " + shell}) })
	// THE SHELL'S OWN ID IS THE HANDLE, not a guessed `sid:1`: which index a channel gets
	// depends on the connection it was opened on, and a test that assumes 1 asserts about
	// the fixture rather than about the feature.
	return shell
}

// TestAnExtensionStdioChannelAnswersAToolName pins the whole surface in one place: the
// handshake happens by itself, the tool table is readable, a tool call returns the tool's
// text rather than the envelope, and a tool that failed is a failure.
func TestAnExtensionStdioChannelAnswersAToolName(t *testing.T) {
	s := newExtensionTestServer(t)
	h := stdioMCPExtension(t, s, "stub316")

	// 1. THE TABLE, WITH NO HANDSHAKE WRITTEN BY THE CALLER. `get <channel> tools` used to
	//    be a channel description with the word ignored: the caller asked for a tool table
	//    and got `mode: pipe`, which reads as "this is not a peer".
	list := termText(t, s, map[string]any{"cmd": "get " + h + " tools", "timeout": 20})
	if !strings.HasPrefix(list, "TERM/1 200") {
		t.Fatalf("`get <channel> tools` did not answer:\n%s", list)
	}
	for _, want := range []string{"ping316", "fail316", "call " + h + " <tool> {…}"} {
		if !strings.Contains(list, want) {
			t.Errorf("the tool table is missing %q:\n%s", want, list)
		}
	}

	// 2. ONE TOOL'S SCHEMA, named the same way the HTTP peer's is.
	schema := termText(t, s, map[string]any{"cmd": "get " + h + " tools ping316", "timeout": 20})
	if !strings.Contains(schema, `"echo"`) || !strings.Contains(schema, "call "+h+" ping316 {…}") {
		t.Errorf("one tool's schema did not come back:\n%s", schema)
	}

	// 3. A CALL IS A ROUND TRIP THAT RETURNS THE TOOL'S TEXT. The envelope is noise: a
	//    caller in a shell has no JSON parser to walk content[0].text with.
	out := termText(t, s, map[string]any{"cmd": `call ` + h + ` ping316 {"echo":"hi"}`, "timeout": 20})
	if !strings.HasPrefix(out, "TERM/1 200") {
		t.Fatalf("the tool call did not succeed:\n%s", out)
	}
	if !strings.Contains(out, `pong316 {"echo":"hi"}`) {
		t.Errorf("the reply is not the tool's text:\n%s", out)
	}
	if strings.Contains(out, `"jsonrpc"`) {
		t.Errorf("the reply still carries the envelope:\n%s", out)
	}

	// 4. A TOOL THAT REPORTED FAILURE DID NOT SUCCEED (#159): MCP carries that in the
	//    result rather than in an error object, so a status-only caller would read 200.
	failed := termText(t, s, map[string]any{"cmd": "call " + h + " fail316 {}", "timeout": 20})
	if !strings.HasPrefix(failed, "TERM/1 500") {
		t.Fatalf("a tool error read as success:\n%s", failed)
	}
	if !strings.Contains(failed, "boom316") {
		t.Errorf("the failure lost the tool's own words:\n%s", failed)
	}

	// 5. THE PEER'S OWN ERROR OBJECT is a failure carrying its message, not a 200 with a
	//    frame the caller has to interpret.
	refused := termText(t, s, map[string]any{"cmd": "call " + h + " nosuch {}", "timeout": 20})
	if !strings.HasPrefix(refused, "TERM/1 500") || !strings.Contains(refused, "no tool nosuch") {
		t.Errorf("a peer error did not name itself:\n%s", refused)
	}

	// 6. --json IS THE WAY BACK TO THE ENVELOPE, so nothing is unreachable.
	raw := termText(t, s, map[string]any{"cmd": "call " + h + " --json ping316 {}", "timeout": 20})
	if !strings.Contains(raw, `"jsonrpc"`) || !strings.Contains(raw, "pong316") {
		t.Errorf("--json did not return the frame:\n%s", raw)
	}
}

// AN UNMANAGED PIPE STILL TAKES ONLY RAW FRAMES (issue #271).
//
// The tool form is legal on an extension's stdio channel because the reconciler owns the
// channel and the template says the process speaks MCP — the shape of the conversation is
// a fact. A plain `open` channel is a byte pipe: anything at all may be running on it, so
// `call <channel> <tool> {…}` there would be this layer inventing a protocol for someone
// else's process.
func TestAPlainPipeStillRefusesAToolName(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sh")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{"cmd": "call " + h + " ping316 {}", "timeout": 20})
	if !strings.HasPrefix(got, "TERM/1 400") {
		t.Fatalf("an unmanaged pipe accepted a tool name:\n%s", got)
	}
	if !strings.Contains(got, "the frame is not JSON") {
		t.Errorf("the refusal no longer explains the frame rule:\n%s", got)
	}
}

// THE HANDSHAKE IS SENT ONCE, and a later call joins it rather than starting another.
//
// A stdio MCP server has ONE session: a second `initialize` is a second state transition
// in a process that has already made it. The stub answers every request, so the evidence
// is the channel's own bytes — the handshake's reply appears exactly once.
func TestTheStdioHandshakeHappensOncePerChannel(t *testing.T) {
	s := newExtensionTestServer(t)
	h := stdioMCPExtension(t, s, "once316")

	for i := 0; i < 2; i++ {
		out := termText(t, s, map[string]any{"cmd": "call " + h + " ping316 {}", "timeout": 20})
		if !strings.HasPrefix(out, "TERM/1 200") || !strings.Contains(out, "pong316") {
			t.Fatalf("call %d did not answer:\n%s", i+1, out)
		}
	}
	// A SHORT WINDOW, because the bytes are already on the channel: `read` with no
	// `--until` waits its whole window for more, and a test that passes 20 spends 20
	// seconds proving the stream is not growing.
	raw := termText(t, s, map[string]any{"cmd": "read " + h + " --tail 400", "timeout": 0.5})
	if n := strings.Count(raw, `"id":"termcp-init"`); n != 1 {
		t.Errorf("the handshake answered %d times on the channel, want 1:\n%s", n, raw)
	}
}
