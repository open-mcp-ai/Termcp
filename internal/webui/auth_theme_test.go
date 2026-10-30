package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

// Exercise the real theme filesystem through managed admission, including CSS
// imports, local fonts and artwork. A login page can look correct in an already
// authenticated tab while all these requests fail for a fresh browser.
func TestManagedThemeImportsArePublic(t *testing.T) {
	if !uiEmbedded {
		t.Skip("docs-only build uses the authentication fallback palette")
	}
	m := &auth.Managed{}
	m.SetPageAssets(Assets())
	h := m.Wrap(http.NotFoundHandler())
	refs := regexp.MustCompile(`url\(\s*["']?([^\s"')]+)["']?\s*\)`)
	seen := map[string]bool{}
	var check func(string)
	check = func(target string) {
		t.Helper()
		if seen[target] {
			return
		}
		seen[target] = true
		r := httptest.NewRequest("GET", target, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Location") != "" || strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("anonymous theme resource %s: %d, %s", target, w.Code, w.Header().Get("Content-Type"))
		}
		if !strings.HasSuffix(target, ".css") {
			return
		}
		base, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range refs.FindAllStringSubmatch(w.Body.String(), -1) {
			ref, err := url.Parse(match[1])
			if err != nil {
				t.Fatal(err)
			}
			if ref.IsAbs() || ref.Fragment != "" {
				continue
			}
			check(base.ResolveReference(ref).String())
		}
	}
	for _, id := range []string{"default-light", "default-dark", "cyberpunk", "matrix"} {
		check("/auth/ui/themes/" + id + "/theme.css")
		check("/auth/ui/themes/" + id + "/theme.json")
		check("/auth/ui/themes/" + id + "/strings.json")
	}
	check("/auth/ui/static/theme-boot.js")
	check("/auth/ui/static/js/theme.js")
	check("/auth/ui/static/css/theme-boot.css")
	if len(seen) < 30 {
		t.Fatalf("theme imports were not traversed: %d", len(seen))
	}
}
