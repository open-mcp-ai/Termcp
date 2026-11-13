package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// TestChannelCallTakesOnlyRawFrames pins the rule from issue #271:
// `call <channel>` is an unmanaged byte pipe, not a peer. It accepts only raw
// JSON-RPC frames — never a tool-name form.
//
// The help text once promised `call <channel> <tool> {"arg":"val"}` while the
// implementation (mcpStdio) required valid JSON on the entire payload. Passing a
// tool name therefore failed with 400 "the frame is not JSON". This test pins
// both halves of that contract:
//  1. The help and usage lines must never advertise `<tool|frame>` for a channel.
//  2. A tool-name form against a channel is refused with 400 rather than silently
//     sent or misrouted.
//  3. A raw JSON-RPC frame round-trips normally.
func TestChannelCallTakesOnlyRawFrames(t *testing.T) {
	// 1. HELP TEXT CONTRACT: call's usage line and open's help must not promise
	//    a tool-name form for a channel.
	op, ok := directive.Lookup("call")
	if !ok {
		t.Fatal("no directive `call`")
	}
	if strings.Contains(op.Usage, "<tool|frame>") {
		t.Errorf("call usage advertises <tool|frame> for a channel; channels take raw frames only:\n%s", op.Usage)
	}
	openOp, ok := directive.Lookup("open")
	if !ok {
		t.Fatal("no directive `open`")
	}
	if strings.Contains(openOp.Detail, "call <channel> <tool>") {
		t.Errorf("open help advertises `call <channel> <tool>`; channels take raw frames only:\n%s", openOp.Detail)
	}

	// 2. RUNTIME CONTRACT: tool-name form is refused.
	s := newTestServer(t)
	script := writeScript(t, `read line; printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"ok":true}}'`)
	h := openPipe(t, s, "sh "+script)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	bad := termText(t, s, map[string]any{
		"cmd":     `call ` + h + ` team {"cmd":"help"}`,
		"timeout": 5,
	})
	if !strings.HasPrefix(bad, "TERM/1 400") {
		t.Fatalf("tool-name form against a channel must be refused with 400:\n%s", bad)
	}
	if !strings.Contains(bad, "the frame is not JSON") {
		t.Errorf("refusal should explain the frame is not JSON:\n%s", bad)
	}

	// 3. RUNTIME CONTRACT: raw JSON-RPC frame round-trips.
	okReply := termText(t, s, map[string]any{
		"cmd":     `call ` + h + ` {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"team","arguments":{"cmd":"help"}}}`,
		"timeout": 5,
	})
	if !strings.HasPrefix(okReply, "TERM/1 200") {
		t.Fatalf("raw JSON-RPC frame must succeed:\n%s", okReply)
	}
	if !strings.Contains(okReply, `"result"`) {
		t.Errorf("reply missing result payload:\n%s", okReply)
	}
}
