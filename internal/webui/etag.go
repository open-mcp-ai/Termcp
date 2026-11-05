// etag.go — strong ETags for the embedded static asset surface (issue #264).
//
// The embedded UI is ~1.1 MB of JS/CSS served with `Cache-Control: no-cache`
// and, before this file, no validator at all: embedded files carry no mtime, so
// http.FileServer sent neither Last-Modified nor ETag. no-cache without a
// validator is a re-fetch order, and over a remote link (daemon on one machine,
// browser on another) every reload re-downloaded the whole payload.
//
// The fix is the standard conditional-request pair: a strong ETag derived from
// the content (first 16 hex chars of its sha256), compared against the request's
// If-None-Match. A match answers 304 without touching the FileServer; a miss
// stamps the ETag on the 200 and serves normally.

package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
)

// assetETags caches one ETag per asset path. The embed cannot change while the
// process runs, and an --assets override directory is read at startup, so a
// hash computed once stays valid for the process's life; SetAssetsDir and
// SetThemesDir clear the cache so a directory change (tests swap them freely)
// cannot answer 304 with the previous directory's bytes.
//
// sync.Map, per the concurrency rules: keyed lookups with no cross-key
// invariant. LoadOrStore is not needed — a recomputed duplicate hash is
// identical, so a plain Load/Store race costs one wasted hash, not correctness.
var assetETags sync.Map

// clearAssetETags drops every cached hash. Called when the asset or theme
// directory is (re)pointed: the cache is keyed by path only, so a directory
// change would otherwise keep answering with the old directory's hashes.
func clearAssetETags() {
	assetETags.Range(func(key, _ any) bool {
		assetETags.Delete(key)
		return true
	})
}

// assetETag returns the strong ETag for the file the serving FS opens at
// fsPath, computing it from what Assets() actually serves — never from the
// embed alone, or an overridden file would answer 304 with the wrong body.
// ok is false when fsPath is not a readable file (directories, missing files),
// which the caller answers by falling through to the FileServer.
func assetETag(fsPath string) (etag string, ok bool) {
	if cached, hit := assetETags.Load(fsPath); hit {
		return cached.(string), true
	}
	f, err := Assets().Open(fsPath)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return "", false
	}
	content, err := io.ReadAll(f)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(content)
	etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	assetETags.Store(fsPath, etag)
	return etag, true
}

// urlToAssetPath maps a request path onto the asset FS: "/" is index.html, a
// directory-shaped path gets index.html inside it, everything else strips the
// leading slash. ok is false for anything that cannot name a file (an escaped
// path), which the caller answers by falling through to the FileServer.
func urlToAssetPath(urlPath string) (fsPath string, ok bool) {
	clean := path.Clean("/" + urlPath)
	if clean == "/." {
		clean = "/"
	}
	if strings.Contains(clean, "\x00") {
		return "", false
	}
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" {
		return "index.html", true
	}
	if strings.HasSuffix(clean, "/") {
		return rel + "index.html", true
	}
	return rel, true
}

// etagMatches reports whether an If-None-Match header value matches etag.
//
// Weak comparison, per RFC 9110 §13.1.2: If-None-Match ignores the W/ prefix,
// so a client that echoes a validator back as weak still hits. The header can
// carry a comma-separated list or "*", which matches any current entity.
func etagMatches(ifNoneMatch, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}
	for _, raw := range strings.Split(ifNoneMatch, ",") {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if tag == etag || tag == "W/"+etag {
			return true
		}
	}
	return false
}

// serveWithETag is the conditional-request front door for the static surface.
//
// GET and HEAD only: other methods have no cached representation to compare
// against. /themes/ is answered by the caller before this runs (no-store, hot
// reload — a theme edited twice in one second must still refresh, which a
// content hash would prevent).
//
// A matching If-None-Match answers 304 carrying ETag and Cache-Control and no
// body — the FileServer is never invoked, so the reload costs a stat-free cache
// hit and zero bytes. A miss stamps the ETag onto the 200 and lets the
// FileServer serve; the embed's zero ModTime means the FileServer adds no
// competing validator, so the stamped ETag is the only one on the wire.
func serveWithETag(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		next.ServeHTTP(w, r)
		return
	}
	fsPath, ok := urlToAssetPath(r.URL.Path)
	if !ok {
		next.ServeHTTP(w, r)
		return
	}
	etag, ok := assetETag(fsPath)
	if !ok {
		// Not a file the asset FS can open (a directory listing, a missing
		// name): the FileServer owns that answer, whatever it is.
		next.ServeHTTP(w, r)
		return
	}
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		// RFC 9110 §15.4.5: the 304 carries the validator and the caching
		// directive so the client keeps what it has; no body, no content type.
		w.WriteHeader(http.StatusNotModified)
		return
	}
	next.ServeHTTP(w, r)
}
