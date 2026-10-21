package mcp

import (
	"strings"
	"testing"
)

// `mcp send` must refuse a frame that is not JSON, the way `cdp send` already
// does. A stdio peer reads one frame per line, so anything unparseable is at best
// ignored and answered with a parse error — and the caller was told 202 "sent"
// either way, which claims the frame landed when it was never a frame.
func TestMCPSendRefusesANonJSONFrame(t *testing.T) {
	s := newTestServer(t)
	// The handle only has to name a channel; the gate runs before any write.
	handle := startPipeSession(t, s) + ":1"

	text := termText(t, s, map[string]any{"cmd": "mcp send " + handle + " 'not json'"})
	if !strings.Contains(text, "not JSON") {
		t.Fatalf("a non-JSON frame was accepted:\n%s", text)
	}
}

// A valid frame must get PAST the gate: the session here runs `echo batch` and is
// already gone, so the write fails — but with the transport's reason, not the
// validator's. That is what proves the check did not simply close the door.
func TestMCPSendAcceptsAValidFrame(t *testing.T) {
	s := newTestServer(t)
	handle := startPipeSession(t, s) + ":1"

	text := termText(t, s, map[string]any{"cmd": `mcp send ` + handle + ` '{"jsonrpc":"2.0","id":1,"method":"ping"}'`})
	if strings.Contains(text, "not JSON") {
		t.Fatalf("a valid frame was refused as non-JSON:\n%s", text)
	}
}
