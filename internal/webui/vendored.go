package webui

import (
	"io/fs"
)

// vendoredFS serves the third-party browser terminal at its historical URLs
// without putting those bytes inside the Web UI asset tree.
//
// The vendored bundle lives in third_party/xterm/ next to the two files that
// explain where it came from (README.md) and whose licence it carries (NOTICE).
// Those two must NOT ship, and a directory //go:embed cannot say that — so the
// package there embeds the two runtime files by name, and this wrapper puts them
// back behind the URLs the page asks for:
//
//	static/xterm/xterm.js   (index.html, unchanged)
//	static/xterm/xterm.css
//
// Everything else still comes from the embedded assets, and an operator's
// --assets override still wins, because this layer sits on top of it.
type vendoredFS struct {
	base     fs.FS
	vendored fs.FS
}

// vendoredPaths maps the URL-shaped path the Web UI serves to the name inside
// the vendored package's filesystem.
var vendoredPaths = map[string]string{
	"static/xterm/xterm.js":  "xterm.js",
	"static/xterm/xterm.css": "xterm.css",
}

const vendoredDir = "static/xterm"

func (v vendoredFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if src, ok := vendoredPaths[name]; ok && v.vendored != nil {
		return v.vendored.Open(src)
	}
	return v.base.Open(name)
}

// ReadDir merges the two layers rather than picking one. It is what makes
// fs.WalkDir(Assets()) — the probe that copies the whole UI to disk for a
// browser — still see the vendored files, and what keeps fs.Glob working on
// static/xterm.
func (v vendoredFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	if v.vendored == nil {
		return fs.ReadDir(v.base, name)
	}
	switch name {
	case vendoredDir:
		return v.vendoredEntries()
	case "static":
		// The parent directory needs the "xterm" entry, or a walk stops one
		// level short and the files are never copied.
		base, err := fs.ReadDir(v.base, name)
		if err != nil {
			return nil, err
		}
		entries := make([]fs.DirEntry, 0, len(base)+1)
		entries = append(entries, base...)
		for _, e := range base {
			if e.Name() == "xterm" {
				return entries, nil
			}
		}
		return append(entries, syntheticDir{"xterm"}), nil
	}
	return fs.ReadDir(v.base, name)
}

func (v vendoredFS) vendoredEntries() ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(v.vendored, ".")
	if err != nil {
		return nil, err
	}
	kept := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		if _, ok := vendoredPaths[vendoredDir+"/"+e.Name()]; ok {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

// syntheticDir is a directory entry with no file behind it: the layer that owns
// "xterm" is a different filesystem, and a walk only needs to know it exists.
type syntheticDir struct{ name string }

func (s syntheticDir) Name() string               { return s.name }
func (s syntheticDir) IsDir() bool                { return true }
func (s syntheticDir) Type() fs.FileMode          { return fs.ModeDir }
func (s syntheticDir) Info() (fs.FileInfo, error) { return nil, fs.ErrInvalid }
