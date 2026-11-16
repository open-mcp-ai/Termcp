package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExtMCPHandlerHTTPContract pins what the route says to a client that has not
// handed it a session yet (issues #234, #328).
//
// THE ANSWERS CHANGED WHEN THE TRANSPORT MOVED IN. GET used to be 405 with a
// paragraph about how this extension never initiates; now the endpoint HAS a
// server-initiated stream, and a GET with no session is the ordinary "you forgot the
// header" 400 the transport already gives everywhere else. DELETE used to report 204
// while doing nothing; now it is a real end-of-session, so it needs one too.
func TestExtMCPHandlerHTTPContract(t *testing.T) {
	s := &Server{}
	handler := s.ExtMCPHandler()

	do := func(method, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/ext/testext/stream", strings.NewReader(body))
		req.SetPathValue("name", "testext")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	// 1. A GET with no session names the header it wanted.
	if w := do(http.MethodGet, "", nil); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Mcp-Session-Id") {
		t.Errorf("GET without a session: want 400 naming Mcp-Session-Id, got %d %q", w.Code, w.Body.String())
	}

	// 2. A POST with no session cannot be an ordinary frame: only initialize may
	// create one, so the same 400 applies.
	if w := do(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("POST without a session: want 400, got %d %q", w.Code, w.Body.String())
	}

	// 3. DELETE with no session, likewise.
	if w := do(http.MethodDelete, "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("DELETE without a session: want 400, got %d %q", w.Code, w.Body.String())
	}

	// 4. A verb nobody defined still says which verbs exist.
	if w := do(http.MethodPut, "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: want 405, got %d", w.Code)
	}
	if allow := do(http.MethodPut, "", nil).Header().Get("Allow"); !strings.Contains(allow, "POST") || !strings.Contains(allow, "GET") || !strings.Contains(allow, "DELETE") {
		t.Errorf("405 must list the verbs, got %q", allow)
	}
}

// TestExtMCPRouteNeedsAName: the handler is mounted on one pattern for every
// extension, so a request that reached it with no name is a routing mistake worth a
// sentence rather than an empty session table keyed "".
func TestExtMCPRouteNeedsAName(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/ext//stream", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	s.ExtMCPHandler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "{name}") {
		t.Fatalf("a nameless route should be refused by name, got %d %q", w.Code, w.Body.String())
	}
}
