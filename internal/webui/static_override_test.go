package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The static asset surface can be pointed at an external directory (--assets).
// These tests pin the composition contract: files present there win, everything
// else -- including the whole directory being absent -- falls back to the copy
// embedded in the binary, so an override can never turn a working page into a
// 404.

// setAssetsDir points the package at dir for the duration of the test and
// restores the embedded-only default afterwards.
func setAssetsDir(t *testing.T, dir string) {
	t.Helper()
	SetAssetsDir(dir)
	t.Cleanup(func() { SetAssetsDir("") })
}

// embeddedOnlyFS returns the pristine embed, bypassing any override, so a test
// can assert what the fallback is supposed to serve.
func embeddedOnlyFS(t *testing.T) fs.FS {
	t.Helper()
	root, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// writeAssetFile creates name (slash-separated) under root with body.
func writeAssetFile(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// serveAsset runs one GET through h without following redirects.
func serveAsset(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr.Code, rr.Body.String()
}

// TestAssetsMissingDirServesEmbedEntirely is the acceptance case for a stock
// install: ~/.termcp/assets does not exist, so every request is served from the
// embed and nothing fails. A missing override directory is a normal state, not
// an error path.
func TestAssetsMissingDirServesEmbedEntirely(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created")
	setAssetsDir(t, missing)

	h := embeddedStaticServer()
	for _, tc := range []struct{ url, asset string }{
		{"/", "index.html"},
		{"/api.md", "api.md"},
		{"/skills.md", "skills.md"},
		{"/static/css/app.css", "static/css/app.css"},
		{"/static/js/util.js", "static/js/util.js"},
	} {
		want, err := fs.ReadFile(embeddedOnlyFS(t), tc.asset)
		if err != nil {
			t.Fatalf("embed lacks %s: %v", tc.asset, err)
		}
		code, body := serveAsset(t, h, tc.url)
		if code != http.StatusOK {
			t.Errorf("GET %s with a missing --assets dir = %d, want 200", tc.url, code)
			continue
		}
		if body != string(want) {
			t.Errorf("GET %s served %d bytes, want the %d embedded bytes", tc.url, len(body), len(want))
		}
	}
}

// TestAssetsNoOverrideIsByteIdenticalToEmbed pins the no-flag behaviour: with no
// external directory configured, responses are byte-for-byte the embedded files.
func TestAssetsNoOverrideIsByteIdenticalToEmbed(t *testing.T) {
	setAssetsDir(t, "")

	h := embeddedStaticServer()
	for _, tc := range []struct{ url, asset string }{
		{"/", "index.html"},
		{"/api.md", "api.md"},
		{"/skills.md", "skills.md"},
	} {
		want, err := fs.ReadFile(embeddedOnlyFS(t), tc.asset)
		if err != nil {
			t.Fatalf("embed lacks %s: %v", tc.asset, err)
		}
		code, body := serveAsset(t, h, tc.url)
		if code != http.StatusOK || body != string(want) {
			t.Errorf("GET %s = %d, %d bytes; want 200 with the %d embedded bytes", tc.url, code, len(body), len(want))
		}
	}
}

// TestAssetsOverrideWinsAndFallsBackPerFile covers both halves of the override:
// a file that exists externally is served from there, and a file that does not
// is served from the embed with 200 -- not a 404. The override is selective, so
// replacing one stylesheet leaves the rest of the UI working.
func TestAssetsOverrideWinsAndFallsBackPerFile(t *testing.T) {
	dir := t.TempDir()
	const css = "/* operator override */\n.app-header { color: rebeccapurple; }\n"
	writeAssetFile(t, dir, "static/css/app.css", css)
	setAssetsDir(t, dir)

	h := embeddedStaticServer()

	code, body := serveAsset(t, h, "/static/css/app.css")
	if code != http.StatusOK {
		t.Fatalf("GET overridden app.css = %d, want 200", code)
	}
	if body != css {
		t.Errorf("app.css body = %q, want the external override %q", body, css)
	}

	// A sibling asset the override directory does not contain must come from the
	// embed, otherwise a one-file override would blank the page.
	wantJS, err := fs.ReadFile(embeddedOnlyFS(t), "static/js/util.js")
	if err != nil {
		t.Fatal(err)
	}
	code, body = serveAsset(t, h, "/static/js/util.js")
	if code != http.StatusOK {
		t.Fatalf("GET util.js = %d, want 200 from the embed", code)
	}
	if body != string(wantJS) {
		t.Error("util.js was not served from the embed; a partial override must not hide the rest of the UI")
	}

	// The override directory holds no index.html, so the page itself and the
	// agent docs stay on the embed.
	wantIndex, err := fs.ReadFile(embeddedOnlyFS(t), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := serveAsset(t, h, "/"); code != http.StatusOK || body != string(wantIndex) {
		t.Errorf("GET / = %d, %d bytes; want 200 with the embedded page", code, len(body))
	}
}

// TestAssetsMissingEverywhereStays404 keeps the fallback honest: falling back to
// the embed must not invent a 200 for a path that exists in neither place.
func TestAssetsMissingEverywhereStays404(t *testing.T) {
	dir := t.TempDir()
	writeAssetFile(t, dir, "static/css/app.css", "/* override */\n")
	setAssetsDir(t, dir)

	code, _ := serveAsset(t, embeddedStaticServer(), "/static/js/no-such-module.js")
	if code != http.StatusNotFound {
		t.Errorf("GET a path absent from both the override and the embed = %d, want 404", code)
	}
}

// TestAssetsOverrideAppliesToDocs pins that the browser UI and the agent-facing
// documents read one FS: main wires mcpSrv.SetDocsFS(webui.Assets()), so an
// overridden api.md is the same bytes over HTTP (/api.md) and as an MCP
// resource, while the untouched skills.md still falls back to the embed.
func TestAssetsOverrideAppliesToDocs(t *testing.T) {
	dir := t.TempDir()
	const api = "# operator api reference\n\ncustom deployment notes\n"
	writeAssetFile(t, dir, "api.md", api)
	setAssetsDir(t, dir)

	// This is exactly the FS main hands to the MCP server.
	b, err := fs.ReadFile(Assets(), "api.md")
	if err != nil {
		t.Fatalf("read overridden api.md: %v", err)
	}
	if string(b) != api {
		t.Errorf("Assets() api.md = %q, want the external override", string(b))
	}

	mux := http.NewServeMux()
	(&Handler{}).Register(mux)
	code, body := serveAsset(t, mux, "/api.md")
	if code != http.StatusOK {
		t.Fatalf("GET /api.md = %d, want 200", code)
	}
	if body != string(b) {
		t.Error("HTTP /api.md and webui.Assets() disagree; the UI and the MCP docs must show the same bytes")
	}

	wantSkill, err := fs.ReadFile(embeddedOnlyFS(t), "skills.md")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := serveAsset(t, mux, "/skills.md"); code != http.StatusOK || body != string(wantSkill) {
		t.Errorf("GET /skills.md = %d, %d bytes; want 200 with the embedded skill", code, len(body))
	}
}

// TestAssetsRejectsEscapingPaths pins that the override directory cannot be read
// outside itself. The guard lives in the composed FS, the single entry point for
// both http.FileServer and direct reads, so an escaping name is refused before it
// can reach the external filesystem.
func TestAssetsRejectsEscapingPaths(t *testing.T) {
	parent := t.TempDir()
	const secret = "TOP-SECRET-SIBLING\n"
	writeAssetFile(t, parent, "secret.txt", secret)
	dir := filepath.Join(parent, "assets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setAssetsDir(t, dir)

	for _, name := range []string{"../secret.txt", "..", "../../secret.txt", "/secret.txt"} {
		if _, err := fs.ReadFile(Assets(), name); err == nil {
			t.Errorf("Assets().Open(%q) succeeded; the override root must not be escapable", name)
		}
	}

	// Serve it the way a hostile client would: the raw static handler, without
	// the net/http ServeMux path cleaning in front of it.
	for _, path := range []string{"/../secret.txt", "/..%2fsecret.txt", "/static/../../secret.txt"} {
		code, body := serveAsset(t, embeddedStaticServer(), path)
		if strings.Contains(body, "TOP-SECRET-SIBLING") {
			t.Errorf("GET %s leaked a file outside the override root", path)
		}
		if code == http.StatusOK {
			t.Errorf("GET %s = 200, want the escape refused", path)
		}
	}
}

// TestAssetsRelativeDirResolvesAgainstCwd pins that a relative --assets value is
// interpreted from the working directory the server was started in (os.DirFS
// resolves it against the process cwd, not against the module directory).
func TestAssetsRelativeDirResolvesAgainstCwd(t *testing.T) {
	root := t.TempDir()
	const override = "<!doctype html><title>from cwd</title>\n"
	writeAssetFile(t, root, "assets/index.html", override)
	t.Chdir(root)
	setAssetsDir(t, "assets")

	code, body := serveAsset(t, embeddedStaticServer(), "/")
	if code != http.StatusOK {
		t.Fatalf("GET / with a relative --assets = %d, want 200", code)
	}
	if body != override {
		t.Errorf("relative --assets was not resolved against the working directory: got %q", body)
	}
}
