package webui

import (
	"io/fs"
	"os"
	"sync"
)

// assetsDir is the operator-supplied override directory for the static asset
// surface ("" = embedded copy only). It is set once, before serving starts, via
// SetAssetsDir; the mutex keeps that one write from racing an early reader.
var (
	assetsDirMu sync.RWMutex
	assetsDir   string
	themesDir   string
)

// SetAssetsDir points the static asset surface at an external directory whose
// files take precedence over the copies embedded in the binary. A file missing
// from that directory — or the directory itself missing — is not an error: the
// embedded asset serves instead, so an empty or absent override changes nothing
// and never fails a request. Both the browser UI and the MCP document resources
// read through Assets(), so they always show the same bytes. Call before any
// consumer (HTTP server, MCP server) reads the FS.
func SetAssetsDir(dir string) {
	assetsDirMu.Lock()
	defer assetsDirMu.Unlock()
	assetsDir = dir
}

// SetThemesDir configures application resources independently of the data dir.
// Only the path is captured; theme files and directory entries are read on demand.
func SetThemesDir(dir string) {
	assetsDirMu.Lock()
	defer assetsDirMu.Unlock()
	themesDir = dir
}

// Assets returns the asset FS rooted at the assets directory. Besides the
// browser UI it holds the agent-facing documents (api.md, skills.md) that the
// static server publishes
// and that the MCP server registers as resources.
func Assets() fs.FS {
	root, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		panic("webui: embed assets: " + err.Error())
	}
	assetsDirMu.RLock()
	dir := assetsDir
	themeDir := themesDir
	assetsDirMu.RUnlock()
	var base fs.FS = root
	var external fs.FS
	if dir != "" {
		external = os.DirFS(dir)
		base = overrideFS{external: external, embedded: root}
	}
	var themes fs.FS
	if themeDir != "" {
		themes = os.DirFS(themeDir)
	}
	return themeFS{base: base, externalAssets: external, externalThemes: themes, embedded: root}
}

// overrideFS composes an external directory with the embedded assets, file by
// file: a file that exists externally is served from there, everything else
// falls back to the embedded copy. Overrides are therefore selective — replacing
// one CSS file leaves the rest of the UI on the embedded version.
type overrideFS struct {
	external fs.FS
	embedded fs.FS
}

// Open refuses any name that could escape the override directory ("..", a
// leading slash, an absolute Windows path): every caller routes through here —
// including the HTTP server, which hands over the request path — so the guard
// sits at the single point of entry. os.DirFS applies its own second line of
// defence: on Windows it rejects ':' and absolute paths outright).
func (o overrideFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	// Any failure on the external copy — no such file, no such directory,
	// unreadable directory — means "not overridden". An override directory is an
	// operator convenience and must never break a page the embedded copy can
	// serve, so it cannot turn a 200 into a 404.
	if f, err := o.external.Open(name); err == nil {
		return f, nil
	}
	return o.embedded.Open(name)
}
