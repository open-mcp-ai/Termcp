package webui

import (
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// themeFS adds a virtual themes subtree to the existing static file server.
// No active theme is stored here: its id is part of each resource URL.
type themeFS struct {
	base, externalAssets, externalThemes, embedded fs.FS
}

type themeSource struct {
	fs   fs.FS
	name string
}

func validThemeID(id string) bool {
	if id == "" || strings.HasPrefix(id, ".") || strings.ContainsAny(id, "/\\:") {
		return false
	}
	for _, c := range id {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func (t themeFS) entrySources(id string) []themeSource {
	return []themeSource{
		{t.externalAssets, "themes/" + id + "/theme.css"},
		{t.externalThemes, id + "/theme.css"},
		{t.embedded, "themes/" + id + "/theme.css"},
	}
}

func (t themeFS) validTheme(id string) bool {
	if !validThemeID(id) {
		return false
	}
	f, err := openThemeSources(t.entrySources(id))
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	return err == nil && !st.IsDir()
}

func (t themeFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "themes" {
		return t.themeDirectory()
	}
	if !strings.HasPrefix(name, "themes/") {
		return t.base.Open(name)
	}
	parts := strings.SplitN(strings.TrimPrefix(name, "themes/"), "/", 2)
	id := parts[0]
	if !t.validTheme(id) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if len(parts) == 1 {
		return openThemeSources([]themeSource{
			{t.externalAssets, "themes/" + id}, {t.externalThemes, id}, {t.embedded, "themes/" + id},
		})
	}
	rel := parts[1]
	if rel == "theme.css" {
		return openThemeSources(t.entrySources(id))
	}
	// Theme entries historically import ./assets/static/css/app.css. Serving a
	// theme's own manifest lets an old copy reorder or omit chunks that were
	// added later; cyberpunk's stale manifest, for example, reloaded base.css
	// after settings.css and moved the mobile settings panel off-screen. The
	// manifest is structural, not visual. Serve the current shared manifest at
	// the themed URL so its relative imports still resolve theme-owned
	// tokens/theme/workspace files, while every shared chunk remains present in
	// canonical cascade order. Theme files therefore need no update when a new
	// shared chunk is added.
	if rel == "assets/static/css/app.css" {
		return t.sharedThemeManifest()
	}
	sources := []themeSource{
		{t.externalAssets, name}, {t.externalThemes, id + "/" + rel}, {t.embedded, name},
	}
	if strings.HasPrefix(rel, "assets/") {
		logical := strings.TrimPrefix(rel, "assets/")
		// Legacy --assets paths keep their priority, even behind a themed URL.
		sources = append([]themeSource{{t.externalAssets, logical}}, sources...)
		sources = append(sources, themeSource{t.embedded, logical})
	}
	return openThemeSources(sources)
}

func (t themeFS) sharedThemeManifest() (fs.File, error) {
	data, err := fs.ReadFile(t.base, "static/css/app.css")
	if err != nil {
		return nil, err
	}
	text := string(data)
	return &themeDataFile{
		Reader: strings.NewReader(text),
		info: themeDataInfo{
			name: "app.css",
			size: int64(len(text)),
		},
	}, nil
}

func (t themeFS) themeDirectory() (fs.File, error) {
	names := map[string]bool{}
	for _, source := range []themeSource{
		{t.externalAssets, "themes"}, {t.externalThemes, "."}, {t.embedded, "themes"},
	} {
		if source.fs == nil {
			continue
		}
		entries, err := fs.ReadDir(source.fs, source.name)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() && t.validTheme(entry.Name()) {
				names[entry.Name()] = true
			}
		}
	}
	entries := make([]fs.DirEntry, 0, len(names))
	for name := range names {
		entries = append(entries, themeDirInfo{name: name})
	}
	return newThemeDir("themes", entries), nil
}

// Directory reads merge all layers too: an external folder must not hide its
// embedded siblings. Each open has an independent cursor and fresh disk reads.
func openThemeSources(sources []themeSource) (fs.File, error) {
	entries := map[string]fs.DirEntry{}
	directory := false
	name := "themes"
	for _, source := range sources {
		if source.fs == nil {
			continue
		}
		f, err := source.fs.Open(source.name)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			continue
		}
		if !st.IsDir() {
			if !directory {
				return f, nil
			}
			f.Close()
			continue
		}
		directory = true
		name = path.Base(source.name)
		if d, ok := f.(fs.ReadDirFile); ok {
			children, _ := d.ReadDir(-1)
			for _, child := range children {
				if _, exists := entries[child.Name()]; !exists {
					entries[child.Name()] = child
				}
			}
		}
		f.Close()
	}
	if directory {
		list := make([]fs.DirEntry, 0, len(entries))
		for _, entry := range entries {
			list = append(list, entry)
		}
		return newThemeDir(name, list), nil
	}
	return nil, fs.ErrNotExist
}

type themeDirInfo struct{ name string }

func (d themeDirInfo) Name() string               { return d.name }
func (d themeDirInfo) Size() int64                { return 0 }
func (d themeDirInfo) Mode() fs.FileMode          { return fs.ModeDir | 0555 }
func (d themeDirInfo) ModTime() time.Time         { return time.Time{} }
func (d themeDirInfo) IsDir() bool                { return true }
func (d themeDirInfo) Sys() any                   { return nil }
func (d themeDirInfo) Type() fs.FileMode          { return fs.ModeDir }
func (d themeDirInfo) Info() (fs.FileInfo, error) { return d, nil }

type themeDataInfo struct {
	name string
	size int64
}

func (d themeDataInfo) Name() string       { return d.name }
func (d themeDataInfo) Size() int64        { return d.size }
func (d themeDataInfo) Mode() fs.FileMode  { return 0444 }
func (d themeDataInfo) ModTime() time.Time { return time.Time{} }
func (d themeDataInfo) IsDir() bool        { return false }
func (d themeDataInfo) Sys() any           { return nil }

type themeDataFile struct {
	*strings.Reader
	info fs.FileInfo
}

func (f *themeDataFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *themeDataFile) Close() error               { return nil }

type themeDir struct {
	info    themeDirInfo
	entries []fs.DirEntry
	pos     int
	closed  bool
}

func newThemeDir(name string, entries []fs.DirEntry) *themeDir {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return &themeDir{info: themeDirInfo{name}, entries: entries}
}

func (d *themeDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *themeDir) Close() error               { d.closed = true; return nil }
func (d *themeDir) Read([]byte) (int, error)   { return 0, fs.ErrInvalid }
func (d *themeDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, fs.ErrClosed
	}
	if d.pos == len(d.entries) && n > 0 {
		return nil, io.EOF
	}
	end := len(d.entries)
	if n > 0 && end-d.pos > n {
		end = d.pos + n
	}
	list := d.entries[d.pos:end]
	d.pos = end
	return list, nil
}
