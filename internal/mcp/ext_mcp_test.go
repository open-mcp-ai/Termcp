package mcp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExtMCPHandlerValidation pins the HTTP-level contracts of /ext/{name}/stream (issue #234).
func TestExtMCPHandlerValidation(t *testing.T) {
	s := &Server{}
	handler := s.ExtMCPHandler()

	// 1. GET is refused with 405 and teaches POST
	req := httptest.NewRequest(http.MethodGet, "/ext/testext/stream", nil)
	req.SetPathValue("name", "testext")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "POST your JSON-RPC frames") {
		t.Errorf("refusal should teach POST, got: %q", w.Body.String())
	}

	// 2. Non-JSON body is refused with 400
	req = httptest.NewRequest(http.MethodPost, "/ext/testext/stream", bytes.NewBufferString("not-json"))
	req.SetPathValue("name", "testext")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on non-JSON, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not JSON") {
		t.Errorf("refusal should mention not JSON, got: %q", w.Body.String())
	}

	// 3. DELETE reports 204
	req = httptest.NewRequest(http.MethodDelete, "/ext/testext/stream", nil)
	req.SetPathValue("name", "testext")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on DELETE, got %d", w.Code)
	}
}
