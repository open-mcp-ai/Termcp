package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestVersionEndpoint pins that GET /api/version reports the build string the
// handler was constructed with — the Web UI header labels the page with it.
func TestVersionEndpoint(t *testing.T) {
	h := &Handler{Version: "v0.0.0-test"}
	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/version = %d, want 200", rr.Code)
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode version body: %v (%s)", err, rr.Body.String())
	}
	if body.Version != "v0.0.0-test" {
		t.Errorf("version = %q, want %q", body.Version, "v0.0.0-test")
	}
}
