//go:build no_webui

package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The pure-API build carries the two agent documents and nothing else: no
// index.html, no static tree, no UI routes. A regression that re-embeds the
// browser UI would show up here.
func TestNoWebUIBuildCarriesDocsOnly(t *testing.T) {
	fsys := Assets()
	for _, want := range []string{"api.md", "skills.md"} {
		if _, err := fs.Stat(fsys, want); err != nil {
			t.Errorf("Assets() is missing %s: %v", want, err)
		}
	}
	if _, err := fs.Stat(fsys, "index.html"); err == nil {
		t.Error("the headless build must not embed the browser UI (found index.html)")
	}
	if uiEmbedded {
		t.Error("uiEmbedded is true in a -tags headless build")
	}

	mux := http.NewServeMux()
	h := &Handler{Version: "v0.0.0-test"}
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET / in the headless build = %d, want 404 (no UI routes)", rr.Code)
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api.md", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("GET /api.md in the headless build = %d, want 200", rr.Code)
	}
}
