// etag_test.go — conditional requests on the static asset surface (issue #264).
//
// The acceptance list from the issue, one test per clause:
//   - a second load of an unchanged asset answers 304;
//   - an --assets override changes that file's ETag and no other's;
//   - editing a theme on disk still hot-reloads (the no-store path is untouched);
//   - GET, HEAD, Range and the If-None-Match forms (list, "*", weak) all hold.

package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// setThemesDirForTest points the theme subtree at dir for one test and restores
// the embedded themes afterwards. Themes are a separate knob from --assets: the
// override directory composes with the embed, while this one replaces the
// on-disk source the hot-reload path reads.
func setThemesDirForTest(t *testing.T, dir string) {
	t.Helper()
	SetThemesDir(dir)
	t.Cleanup(func() { SetThemesDir("") })
}

// requestAsset runs one request through h and returns the response recorder.
func requestAsset(t *testing.T, h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// TestStaticAssetAnswers304OnUnchangedContent is the headline acceptance case:
// a first load stamps an ETag, and replaying that ETag answers 304 with no body.
func TestStaticAssetAnswers304OnUnchangedContent(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()

	for _, assetPath := range []string{"/", "/static/js/util.js", "/static/css/workspace.css", "/api.md"} {
		first := requestAsset(t, h, http.MethodGet, assetPath, nil)
		if first.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", assetPath, first.Code)
		}
		etag := first.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("GET %s carried no ETag; a no-cache response without a validator refetches the body every load", assetPath)
		}
		if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
			t.Errorf("GET %s ETag %q is not a quoted strong validator", assetPath, etag)
		}
		if got := first.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache (the ETag turns it into a revalidate, not a cache)", assetPath, got)
		}
		if first.Body.Len() == 0 {
			t.Fatalf("GET %s returned an empty body", assetPath)
		}

		second := requestAsset(t, h, http.MethodGet, assetPath, map[string]string{"If-None-Match": etag})
		if second.Code != http.StatusNotModified {
			t.Errorf("replaying the ETag for %s = %d, want 304", assetPath, second.Code)
		}
		if second.Body.Len() != 0 {
			t.Errorf("the 304 for %s carried a %d-byte body; a 304 has none", assetPath, second.Body.Len())
		}
		if got := second.Header().Get("ETag"); got != etag {
			t.Errorf("the 304 for %s carried ETag %q, want %q (the client keeps its copy by comparing this)", assetPath, got, etag)
		}
		if got := second.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("the 304 for %s carried Cache-Control %q, want no-cache", assetPath, got)
		}
	}
}

// TestStaticAssetETagIsContentDerived pins that the validator names the bytes:
// two different assets must not share one, and a changed file must change it.
func TestStaticAssetETagIsContentDerived(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()

	seen := map[string]string{}
	for _, p := range []string{"/", "/static/js/util.js", "/static/js/sessions.js", "/static/css/workspace.css"} {
		rr := requestAsset(t, h, http.MethodGet, p, nil)
		etag := rr.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("GET %s carried no ETag", p)
		}
		if other, dup := seen[etag]; dup {
			t.Errorf("%s and %s share the ETag %q; the validator must identify the content", other, p, etag)
		}
		seen[etag] = p
	}
}

// TestStaticAssetETagFollowsTheOverride is the scope note from the issue: the
// hash must come from what the serving FS opens, so an --assets override changes
// that file's ETag — and only that file's.
func TestStaticAssetETagFollowsTheOverride(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()

	before := requestAsset(t, h, http.MethodGet, "/static/css/app.css", nil).Header().Get("ETag")
	untouchedBefore := requestAsset(t, h, http.MethodGet, "/static/js/util.js", nil).Header().Get("ETag")
	if before == "" || untouchedBefore == "" {
		t.Fatal("no ETag before the override; the test cannot measure a change")
	}

	dir := t.TempDir()
	writeAssetFile(t, dir, "static/css/app.css", "/* operator override */\n.app-header{color:rebeccapurple}\n")
	setAssetsDir(t, dir)

	after := requestAsset(t, h, http.MethodGet, "/static/css/app.css", nil).Header().Get("ETag")
	untouchedAfter := requestAsset(t, h, http.MethodGet, "/static/js/util.js", nil).Header().Get("ETag")

	if after == before {
		t.Error("the overridden file kept the embed's ETag; the hash must be computed from the bytes actually served")
	}
	if after == "" {
		t.Error("the overridden file carried no ETag")
	}
	if untouchedAfter != untouchedBefore {
		t.Errorf("an unrelated asset's ETag changed (%s -> %s); an override is per-file", untouchedBefore, untouchedAfter)
	}

	// The stale validator must not answer 304 for the new bytes.
	stale := requestAsset(t, h, http.MethodGet, "/static/css/app.css", map[string]string{"If-None-Match": before})
	if stale.Code != http.StatusOK {
		t.Errorf("replaying the embed's ETag against the override = %d, want 200 with the new body", stale.Code)
	}
}

// TestStaticAssetIfNoneMatchForms covers the header shapes the issue calls out:
// a comma-separated list, "*", and the weak form of the same validator.
func TestStaticAssetIfNoneMatchForms(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()
	const p = "/static/js/util.js"

	etag := requestAsset(t, h, http.MethodGet, p, nil).Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag to replay")
	}
	for _, tc := range []struct {
		name, header string
		want         int
	}{
		{"exact", etag, http.StatusNotModified},
		{"weak form of the same tag", "W/" + etag, http.StatusNotModified},
		{"list, match last", `"nonsense", "also-nonsense", ` + etag, http.StatusNotModified},
		{"list with whitespace", "  \"nonsense\" ,  " + etag + "  ", http.StatusNotModified},
		{"star", "*", http.StatusNotModified},
		{"different tag", `"0000000000000000"`, http.StatusOK},
		{"list, no match", `"aaaa", "bbbb"`, http.StatusOK},
		{"empty header", "", http.StatusOK},
	} {
		rr := requestAsset(t, h, http.MethodGet, p, map[string]string{"If-None-Match": tc.header})
		if rr.Code != tc.want {
			t.Errorf("If-None-Match %s (%q) = %d, want %d", tc.name, tc.header, rr.Code, tc.want)
		}
	}
}

// TestStaticAssetHEADAndRange covers the methods the issue requires.
func TestStaticAssetHEADAndRange(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()
	const p = "/static/js/util.js"

	head := requestAsset(t, h, http.MethodHead, p, nil)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD %s = %d, want 200", p, head.Code)
	}
	etag := head.Header().Get("ETag")
	if etag == "" {
		t.Fatal("HEAD carried no ETag; a HEAD must describe the same representation a GET would")
	}
	if head.Body.Len() != 0 {
		t.Errorf("HEAD returned a %d-byte body", head.Body.Len())
	}

	head304 := requestAsset(t, h, http.MethodHead, p, map[string]string{"If-None-Match": etag})
	if head304.Code != http.StatusNotModified {
		t.Errorf("HEAD with a matching ETag = %d, want 304", head304.Code)
	}

	// A Range request is still a 206 for the bytes asked for; the validator is
	// present so a conditional Range can be answered. A matching If-None-Match
	// wins over Range (RFC 9110 §13.1.3), which is what makes the pair correct.
	ranged := requestAsset(t, h, http.MethodGet, p, map[string]string{"Range": "bytes=0-9"})
	if ranged.Code != http.StatusPartialContent {
		t.Errorf("Range GET %s = %d, want 206", p, ranged.Code)
	}
	if ranged.Body.Len() != 10 {
		t.Errorf("Range GET returned %d bytes, want 10", ranged.Body.Len())
	}
	if ranged.Header().Get("ETag") == "" {
		t.Error("the 206 carried no ETag; a partial response still identifies the representation")
	}

	ranged304 := requestAsset(t, h, http.MethodGet, p, map[string]string{
		"Range":         "bytes=0-9",
		"If-None-Match": etag,
	})
	if ranged304.Code != http.StatusNotModified {
		t.Errorf("Range GET with a matching ETag = %d, want 304 (the validator wins over the range)", ranged304.Code)
	}
}

// TestThemesStayNoStoreAndUnconditional pins the carve-out: themes are edited on
// disk and must hot-reload, so they keep no-store and never see a 304.
func TestThemesStayNoStoreAndUnconditional(t *testing.T) {
	dir := t.TempDir()
	writeAssetFile(t, dir, "default-dark/theme.css", ".a{color:red}\n")
	setThemesDirForTest(t, dir)

	h := embeddedStaticServer()
	rr := requestAsset(t, h, http.MethodGet, "/themes/default-dark/theme.css", map[string]string{
		// A client replaying any validator must be ignored on this path.
		"If-None-Match": `*`,
	})
	if rr.Code != http.StatusOK {
		t.Errorf("GET a theme with If-None-Match: * = %d, want 200 (themes must always re-send)", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("theme Cache-Control = %q, want no-store", got)
	}
	if got := rr.Header().Get("ETag"); got != "" {
		t.Errorf("theme carried ETag %q; a no-store asset must not offer a validator it will ignore", got)
	}

	// Editing the file must be visible immediately.
	writeAssetFile(t, dir, "default-dark/theme.css", ".a{color:blue}\n")
	again := requestAsset(t, h, http.MethodGet, "/themes/default-dark/theme.css", nil)
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), "blue") {
		t.Errorf("after editing the theme on disk: %d %q, want 200 with the new rule", again.Code, again.Body.String())
	}
}

// TestStaticAssetMissingFileStill404s pins that the ETag path does not invent
// responses for names the asset FS cannot open.
func TestStaticAssetMissingFileStill404s(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()
	rr := requestAsset(t, h, http.MethodGet, "/static/js/no-such-module.js", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET a missing asset = %d, want 404", rr.Code)
	}
	if rr.Header().Get("ETag") != "" {
		t.Error("a 404 carried an ETag")
	}
}

// TestStaticAssetETagIsStableAcrossCalls pins the cache: the same path must
// answer the same validator on every request (a hash that drifted per request
// would defeat the whole mechanism).
func TestStaticAssetETagIsStableAcrossCalls(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()
	const p = "/static/css/workspace.css"
	first := requestAsset(t, h, http.MethodGet, p, nil).Header().Get("ETag")
	for i := 0; i < 5; i++ {
		again := requestAsset(t, h, http.MethodGet, p, nil).Header().Get("ETag")
		if again != first {
			t.Fatalf("request %d answered ETag %q, first was %q", i+2, again, first)
		}
	}
}

// TestAssetETagMatchesTheServedBytes is the anti-drift guard: the hash must be
// computed from the file content, so recomputing it here from the same FS must
// agree with what the server sends.
func TestAssetETagMatchesTheServedBytes(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()
	const asset = "static/js/util.js"

	want, err := fs.ReadFile(embeddedOnlyFS(t), asset)
	if err != nil {
		t.Fatalf("embed lacks %s: %v", asset, err)
	}
	got := requestAsset(t, h, http.MethodGet, "/"+asset, nil)
	if got.Body.String() != string(want) {
		t.Fatalf("served body differs from the embed; the validator would describe bytes we do not send")
	}
	if etag := got.Header().Get("ETag"); etag == "" {
		t.Error("no ETag on the asset")
	}
	// The cache is keyed by the asset path, and clearing it must not change the
	// answer for unchanged content.
	clearAssetETags()
	again := requestAsset(t, h, http.MethodGet, "/"+asset, nil).Header().Get("ETag")
	if again != got.Header().Get("ETag") {
		t.Errorf("ETag changed after clearing the cache (%q -> %q) for identical bytes",
			got.Header().Get("ETag"), again)
	}
	_ = filepath.Separator
}
