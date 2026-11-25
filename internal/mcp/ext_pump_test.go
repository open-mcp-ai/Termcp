package mcp

import (
	"testing"
	"time"
)

// TestExtAnswerWindowOutlastsAPlausibleToolCall pins acceptance 3 of issue #328: a
// `tools/call` that legitimately takes 90 seconds must come back with its result, not with
// a protocol error. The window is a backstop for a peer that never answers — if it were
// shorter than a real tool call it would be a tool timeout wearing a protocol hat, and the
// client's own deadline is the authority on "too long".
func TestExtAnswerWindowOutlastsAPlausibleToolCall(t *testing.T) {
	if extPumpAnswerWindow <= 90*time.Second {
		t.Fatalf("the answer window is %s: a 90s tools/call would fail with a protocol error instead of returning its result", extPumpAnswerWindow)
	}
}

// TestSplitFramesCarriesThePartialLine pins the scanner: a read may stop mid-line, and the
// leftover must ride to the next read instead of being parsed as a frame of its own.
func TestSplitFramesCarriesThePartialLine(t *testing.T) {
	frames, rest := splitFrames(nil, []byte("{\"a\":1}\n{\"b\":2"))
	if len(frames) != 1 || string(frames[0]) != `{"a":1}` {
		t.Fatalf("frames = %q", frames)
	}
	if string(rest) != `{"b":2` {
		t.Fatalf("rest = %q", rest)
	}
	frames, rest = splitFrames(rest, []byte("}\n"))
	if len(frames) != 1 || string(frames[0]) != `{"b":2}` || len(rest) != 0 {
		t.Fatalf("frames = %q rest = %q", frames, rest)
	}
}

// TestRewriteFrameIDChangesOnlyTheID pins the promise that a reply is the peer's bytes with
// one field swapped: a client compares answers by id, and re-encoding the rest would make a
// number the peer wrote come back spelled differently.
func TestRewriteFrameIDChangesOnlyTheID(t *testing.T) {
	got := rewriteFrameIDRaw(`{"jsonrpc":"2.0","id":"tc7","result":{"n":1.50}}`, `9`)
	want := `{"jsonrpc":"2.0","id":9,"result":{"n":1.50}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if v := initializeVersion([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`)); v != "2024-11-05" {
		t.Fatalf("initializeVersion = %q", v)
	}
}
