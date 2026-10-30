package auth

import (
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// pageAssets is separate from the application router: anonymous theme requests
// can never fall through to a business handler or browse an asset directory.
type pageAssets struct {
	fs      fs.FS
	handler http.Handler
}

type pageFilesFS struct{ fs.FS }

func (a pageFilesFS) Open(name string) (fs.File, error) {
	f, err := a.FS.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		_ = f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}

const pageAssetsPrefix = "/auth/ui/"

// SetPageAssets supplies the same theme filesystem used by the workspace,
// without coupling authentication to the WebUI package. Call before Wrap.
// A docs-only build or a missing filesystem keeps the built-in page fallback.
func (m *Managed) SetPageAssets(assets fs.FS) {
	if assets == nil {
		return
	}
	for _, name := range []string{"static/theme-boot.js", "static/js/theme.js", "themes/default-light/theme.css"} {
		if info, err := fs.Stat(assets, name); err != nil || info.IsDir() {
			return
		}
	}
	m.pageAssets = &pageAssets{fs: assets, handler: http.StripPrefix(pageAssetsPrefix, http.FileServer(http.FS(pageFilesFS{assets})))}
}

func publicPageAsset(name string) bool {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\:") {
		return false
	}
	switch name {
	case "static/theme-boot.js", "static/js/theme.js", "static/css/theme-boot.css":
		return true
	}
	if strings.HasPrefix(name, "themes/") {
		parts := strings.SplitN(name, "/", 3)
		if len(parts) != 3 || strings.HasPrefix(parts[1], ".") {
			return false
		}
		switch parts[2] {
		case "theme.css", "theme.json", "strings.json":
			return true
		}
		// Custom themes may organize styles, local fonts and images freely.
		return presentationAsset(parts[2])
	}
	// Theme imports and local artwork/fonts are public presentation material.
	// Arbitrary JavaScript, HTML, data files and directory listings are excluded.
	switch {
	case strings.HasPrefix(name, "static/css/"):
		return path.Ext(name) == ".css"
	case strings.HasPrefix(name, "fonts/"), strings.HasPrefix(name, "icons/"):
		return presentationAsset(name)
	}
	return false
}

func presentationAsset(name string) bool {
	switch path.Ext(name) {
	case ".css", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".svg", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".ico", ".avif":
		return true
	}
	return false
}

func (m *Managed) servePageAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// SVG artwork and denied files cannot become an active top-level document.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, pageAssetsPrefix)
	if m.pageAssets == nil {
		http.NotFound(w, r)
		return
	}
	if name == "themes/" {
		m.pageAssets.themes(w, r)
		return
	}
	if !publicPageAsset(name) {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(m.pageAssets.fs, name)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	r = r.Clone(r.Context())
	r.Header.Del("If-Modified-Since")
	r.Header.Del("If-None-Match")
	m.pageAssets.handler.ServeHTTP(w, r)
}

func (a *pageAssets) themes(w http.ResponseWriter, r *http.Request) {
	entries, err := fs.ReadDir(a.fs, "themes")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte("<!doctype html><title>Themes</title>"))
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !publicPageAsset("themes/"+id+"/theme.css") {
			continue
		}
		if info, err := fs.Stat(a.fs, "themes/"+id+"/theme.css"); err != nil || info.IsDir() {
			continue
		}
		_, _ = w.Write([]byte(`<a href="` + html.EscapeString(url.PathEscape(id)) + `/">` + html.EscapeString(id) + "</a>\n"))
	}
}
