package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

// staticAssetETagHandler adds a strong, content-derived ETag before
// http.FileServer handles the request. FileServer delegates regular files to
// http.ServeContent, which then applies the full HTTP precondition and Range
// rules against that validator.
func staticAssetETagHandler(assets fs.FS, next http.Handler) http.Handler {
	validator := &staticETagValidator{assets: assets}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag, ok := validator.etag(r); ok {
			w.Header().Set("ETag", etag)
		}
		next.ServeHTTP(w, r)
	})
}

type staticETagValidator struct {
	assets  fs.FS
	entries sync.Map // path -> cachedETag
}

type cachedETag struct {
	size int64
	etag string
}

func (v *staticETagValidator) etag(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "", false
	}
	name, ok := staticAssetName(r.URL.Path)
	if !ok {
		return "", false
	}

	f, info, ok := openStaticAsset(v.assets, name, strings.HasSuffix(r.URL.Path, "/"))
	if !ok {
		return "", false
	}
	defer f.Close()

	// Embedded files have no modification time and cannot change for the life
	// of this handler. External --assets files do carry one, so hash those on
	// every request rather than ever answering a stale 304 after an edit.
	if info.ModTime().IsZero() {
		if cached, ok := v.entries.Load(name); ok {
			entry := cached.(cachedETag)
			if entry.size == info.Size() {
				return entry.etag, true
			}
		}
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", false
	}
	sum := hash.Sum(nil)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if info.ModTime().IsZero() {
		v.entries.Store(name, cachedETag{size: info.Size(), etag: etag})
	}
	return etag, true
}

func staticAssetName(urlPath string) (string, bool) {
	if urlPath == "" || !strings.HasPrefix(urlPath, "/") || path.Clean(urlPath) != urlPath {
		return "", false
	}
	if urlPath == "/themes" || strings.HasPrefix(urlPath, "/themes/") {
		return "", false
	}
	// FileServer redirects these canonical index URLs to the directory form.
	if strings.HasSuffix(urlPath, "/index.html") {
		return "", false
	}
	name := strings.TrimPrefix(urlPath, "/")
	if name == "" {
		name = "index.html"
	}
	return name, true
}

func openStaticAsset(assets fs.FS, name string, trailingSlash bool) (fs.File, fs.FileInfo, bool) {
	f, err := assets.Open(name)
	if err != nil {
		return nil, nil, false
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, false
	}
	if info.IsDir() {
		f.Close()
		if !trailingSlash {
			return nil, nil, false
		}
		name = path.Join(name, "index.html")
		f, err = assets.Open(name)
		if err != nil {
			return nil, nil, false
		}
		info, err = f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, false
		}
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, false
	}
	return f, info, true
}
