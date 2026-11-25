package mcp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/envelope"
)

// envelopeSlot is a directive slot with the two fields these tests read.
func envelopeSlot(code int, body string) envelope.Slot {
	return envelope.Slot{Code: code, Body: body}
}

// TestExtMCPHandlerValidation pins the HTTP-level contracts of /ext/{name}/stream after
// issue #328 moved it onto the shared streamable transport.
//
// WHAT CHANGED, AND WHY THESE ASSERTIONS ARE THE OPPOSITE OF THE OLD ONES. The endpoint
// used to be a hand-rolled copy of the transport, and the copy answered 405 to GET with a
// paragraph explaining that this extension never initiates. That was wrong twice over: a
// client whose request got a 202 (which the spec forbids) waits on that GET stream, and a
// missing session id is the client's mistake, not a wrong verb. Now GET without a session
// is a 400 naming the header, and GET with one is a real stream.
func TestExtMCPHandlerValidation(t *testing.T) {
	s := newExtensionTestServer(t)
	handler := s.ExtMCPHandler()

	// 1. An unknown extension is refused BEFORE the transport sees the request, so the
	//    answer is a fact about the name rather than a protocol-shaped reply.
	req := httptest.NewRequest(http.MethodPost, "/ext/nosuchext/stream", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.SetPathValue("name", "nosuchext")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown extension, got %d: %s", w.Code, w.Body.String())
	}

	// 2. A missing name in the path is a 404 that spells the route.
	req = httptest.NewRequest(http.MethodPost, "/ext//stream", bytes.NewBufferString(`{}`))
	req.SetPathValue("name", "")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a nameless route, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ext/{name}/stream") {
		t.Errorf("the refusal should spell the route, got %q", w.Body.String())
	}
}

// The GET-without-session contract (400 naming Mcp-Session-Id) belongs to the transport
// itself, which is shared with the instance's own /stream — see the dogfood check
// "#328 GET 缺 session 是 400 并点名头" for it on a live instance. Keeping it here would
// need a real extension behind the handler, and that is what dogfood is for.

// TestExtFrameBodyRefusesWhatCannotBeForwarded pins the two shapes that must never reach
// the peer's channel: a body that is not JSON, and a batch (removed in 2025-06-18).
func TestExtFrameBodyRefusesWhatCannotBeForwarded(t *testing.T) {
	if _, why := extFrameBody("not-json"); !strings.Contains(why, "not JSON") {
		t.Fatalf("a non-JSON body must be refused as such, got %q", why)
	}
	if _, why := extFrameBody(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`); !strings.Contains(why, "batch") {
		t.Fatalf("a batch must be refused by name, got %q", why)
	}
	if frame, why := extFrameBody(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); why != "" || frame == "" {
		t.Fatalf("a plain frame must pass through, got %q / %q", frame, why)
	}
}

// TestExtSlotBodyKeepsThePeersBytesVerbatim pins the promise /ext makes to an MCP client:
// what it reads is the EXTENSION's frame, untouched. Anything this project appends for a
// human reader would be a parse error here, so a body that is not a JSON-RPC message is
// refused as a gateway failure instead of forwarded.
func TestExtSlotBodyKeepsThePeersBytesVerbatim(t *testing.T) {
	frame := `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"mock-calc","version":"1.0.0"}}}`
	got, ok := extSlotBody(envelopeSlot(200, frame))
	if !ok || got != frame {
		t.Fatalf("the peer's frame must survive untouched, got %q ok=%v", got, ok)
	}
	if _, ok := extSlotBody(envelopeSlot(200, "python: command not found")); ok {
		t.Fatal("a non-JSON-RPC answer must not be forwarded as a frame")
	}
	if _, ok := extSlotBody(envelopeSlot(204, "")); ok {
		t.Fatal("a window that produced nothing must not be forwarded as a frame")
	}
}
