package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func contentETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func TestEmbeddedAssetETagReturnsNotModified(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()

	for _, tc := range []struct {
		url   string
		asset string
	}{
		{url: "/", asset: "index.html"},
		{url: "/static/css/app.css", asset: "static/css/app.css"},
	} {
		t.Run(strings.TrimPrefix(tc.url, "/"), func(t *testing.T) {
			body, err := fs.ReadFile(embeddedOnlyFS(t), tc.asset)
			if err != nil {
				t.Fatal(err)
			}
			first := requestAsset(t, h, http.MethodGet, tc.url, nil)
			if first.Code != http.StatusOK {
				t.Fatalf("first GET %s = %d, want 200", tc.url, first.Code)
			}
			wantETag := contentETag(body)
			if got := first.Header().Get("ETag"); got != wantETag {
				t.Fatalf("ETag = %q, want %q", got, wantETag)
			}
			if first.Body.String() != string(body) {
				t.Fatalf("first GET body does not match %s", tc.asset)
			}

			for _, header := range []string{
				wantETag,
				"W/" + wantETag,
				`"not-this-version", ` + wantETag,
				"*",
			} {
				rr := requestAsset(t, h, http.MethodGet, tc.url, map[string]string{"If-None-Match": header})
				if rr.Code != http.StatusNotModified {
					t.Errorf("If-None-Match %q = %d, want 304", header, rr.Code)
				}
				if rr.Body.Len() != 0 {
					t.Errorf("If-None-Match %q returned a body", header)
				}
				if got := rr.Header().Get("ETag"); got != wantETag {
					t.Errorf("304 ETag = %q, want %q", got, wantETag)
				}
				if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
					t.Errorf("304 Cache-Control = %q, want no-cache", got)
				}
			}
		})
	}

	head := requestAsset(t, h, http.MethodHead, "/static/js/util.js", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("ETag") == "" {
		t.Fatalf("HEAD = %d, %d body bytes, ETag %q; want 200 without a body and with an ETag",
			head.Code, head.Body.Len(), head.Header().Get("ETag"))
	}
	notModified := requestAsset(t, h, http.MethodHead, "/static/js/util.js", map[string]string{
		"If-None-Match": head.Header().Get("ETag"),
	})
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional HEAD = %d, %d body bytes; want 304 without a body", notModified.Code, notModified.Body.Len())
	}
}

func TestEmbeddedAssetETagPreservesRangeSemantics(t *testing.T) {
	setAssetsDir(t, "")
	h := embeddedStaticServer()

	first := requestAsset(t, h, http.MethodGet, "/static/css/app.css", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}

	partial := requestAsset(t, h, http.MethodGet, "/static/css/app.css", map[string]string{
		"If-None-Match": `"other"`,
		"Range":         "bytes=0-9",
	})
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("range GET = %d, want 206", partial.Code)
	}
	if partial.Body.Len() != 10 {
		t.Fatalf("range GET returned %d bytes, want 10", partial.Body.Len())
	}
	if got := partial.Header().Get("ETag"); got != etag {
		t.Fatalf("range GET ETag = %q, want %q", got, etag)
	}

	matchedRange := requestAsset(t, h, http.MethodGet, "/static/css/app.css", map[string]string{
		"If-None-Match": etag,
		"Range":         "bytes=0-9",
	})
	if matchedRange.Code != http.StatusNotModified || matchedRange.Body.Len() != 0 {
		t.Fatalf("matched conditional range = %d, %d body bytes; want 304 without a body",
			matchedRange.Code, matchedRange.Body.Len())
	}
}

func TestAssetETagUsesComposedFSContent(t *testing.T) {
	dir := t.TempDir()
	const firstBody = "/* override v1 */\n"
	const secondBody = "/* override v2 */\n"
	writeAssetFile(t, dir, "static/css/app.css", firstBody)
	setAssetsDir(t, dir)
	h := embeddedStaticServer()

	first := requestAsset(t, h, http.MethodGet, "/static/css/app.css", nil)
	if first.Code != http.StatusOK || first.Body.String() != firstBody {
		t.Fatalf("initial override = %d %q", first.Code, first.Body.String())
	}
	firstETag := first.Header().Get("ETag")
	if want := contentETag([]byte(firstBody)); firstETag != want {
		t.Fatalf("override ETag = %q, want %q", firstETag, want)
	}

	sibling := requestAsset(t, h, http.MethodGet, "/static/js/util.js", nil)
	siblingETag := sibling.Header().Get("ETag")
	if siblingETag == "" {
		t.Fatal("embedded sibling has no ETag")
	}

	filename := filepath.Join(dir, "static", "css", "app.css")
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filename, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	writeAssetFile(t, dir, "static/css/app.css", secondBody)
	if err := os.Chtimes(filename, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	updated := requestAsset(t, h, http.MethodGet, "/static/css/app.css", map[string]string{
		"If-None-Match": firstETag,
	})
	if updated.Code != http.StatusOK || updated.Body.String() != secondBody {
		t.Fatalf("updated override = %d %q, want 200 with the new bytes", updated.Code, updated.Body.String())
	}
	if got, want := updated.Header().Get("ETag"), contentETag([]byte(secondBody)); got != want {
		t.Fatalf("updated override ETag = %q, want %q", got, want)
	}

	siblingAgain := requestAsset(t, h, http.MethodGet, "/static/js/util.js", nil)
	if got := siblingAgain.Header().Get("ETag"); got != siblingETag {
		t.Fatalf("sibling ETag changed from %q to %q after one file override", siblingETag, got)
	}
}

func TestAssetETagLeavesThemesNoStoreAndFresh(t *testing.T) {
	dir := t.TempDir()
	writeAssetFile(t, dir, "custom/theme.css", `@import url("./assets/static/css/tokens.css");`)
	palette := "custom/assets/static/css/tokens.css"
	writeAssetFile(t, dir, palette, "old palette")
	filename := filepath.Join(dir, filepath.FromSlash(palette))
	stamp := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := os.Chtimes(filename, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	SetThemesDir(dir)
	t.Cleanup(func() { SetThemesDir("") })

	h := embeddedStaticServer()
	url := "/themes/" + palette
	first := requestAsset(t, h, http.MethodGet, url, nil)
	if first.Code != http.StatusOK || first.Body.String() != "old palette" {
		t.Fatalf("initial theme = %d %q", first.Code, first.Body.String())
	}
	if got := first.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("theme Cache-Control = %q, want no-store", got)
	}
	if got := first.Header().Get("ETag"); got != "" {
		t.Fatalf("theme ETag = %q, want none", got)
	}

	writeAssetFile(t, dir, palette, "new palette")
	if err := os.Chtimes(filename, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	second := requestAsset(t, h, http.MethodGet, url, map[string]string{
		"If-Modified-Since": first.Header().Get("Last-Modified"),
		"If-None-Match":     "*",
	})
	if second.Code != http.StatusOK || second.Body.String() != "new palette" {
		t.Fatalf("updated theme = %d %q, want 200 with fresh bytes", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("updated theme Cache-Control = %q, want no-store", got)
	}
	if got := second.Header().Get("ETag"); got != "" {
		t.Fatalf("updated theme ETag = %q, want none", got)
	}
}

func TestAssetETagMissingFileStays404(t *testing.T) {
	setAssetsDir(t, "")
	rr := requestAsset(t, embeddedStaticServer(), http.MethodGet, "/static/js/no-such-module.js", map[string]string{
		"If-None-Match": "*",
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404", rr.Code)
	}
	if got := rr.Header().Get("ETag"); got != "" {
		t.Fatalf("missing asset ETag = %q, want none", got)
	}
}
