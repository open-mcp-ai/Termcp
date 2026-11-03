package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// TestExtMCPForwardsThePeersFrameVerbatim pins issue #234's central promise: the
// client that connects to /ext/{name}/stream talks to the EXTENSION, so what it
// reads is the peer's own frame — its serverInfo, its tools, its answers.
//
// Two halves of that promise are checked here, because both have a tempting wrong
// answer that looks fine until a real client is on the other end:
//
//  1. A JSON-RPC body is returned unchanged. Anything this project appends to a
//     reply for a human reader (a Hint header, a note after the body) would be a
//     parse error for an MCP client, so the body has to survive untouched.
//  2. A body that is NOT a JSON-RPC message is refused as a gateway error rather
//     than forwarded. Sending it would hand the client a "response" it cannot
//     parse, and the failure would surface later as a mysterious protocol error
//     instead of as "this extension did not answer with a frame".
func TestExtMCPForwardsThePeersFrameVerbatim(t *testing.T) {
	s := &Server{}

	// 1. A peer's frame comes back byte for byte, as JSON.
	frame := `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"mock-calc","version":"1.0.0"}}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ext/any/stream", nil)
	s.extMCPWriteSlot(rec, req, envelope.Slot{Code: 200, Body: frame})
	if rec.Code != http.StatusOK {
		t.Fatalf("a JSON-RPC body should be returned 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != frame {
		t.Fatalf("the peer's frame must be returned verbatim.\n want: %s\n  got: %s", frame, got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("a client accepting JSON should get application/json, got %q", ct)
	}

	// 2. A non-frame answer is a 502, not a body the client cannot parse.
	rec = httptest.NewRecorder()
	s.extMCPWriteSlot(rec, req, envelope.Slot{Code: 200, Body: "python: command not found"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("a non-JSON-RPC answer should be a 502, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not a JSON-RPC message") {
		t.Errorf("the 502 should say what went wrong, got: %q", rec.Body.String())
	}

	// 3. A client that accepts only an event stream gets one, wrapping the same frame.
	rec = httptest.NewRecorder()
	sseReq := httptest.NewRequest(http.MethodPost, "/ext/any/stream", nil)
	sseReq.Header.Set("Accept", "text/event-stream")
	s.extMCPWriteSlot(rec, sseReq, envelope.Slot{Code: 200, Body: frame})
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("an event-stream client should get text/event-stream, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: "+frame) {
		t.Errorf("the frame should ride in an SSE data line, got: %q", rec.Body.String())
	}
}
