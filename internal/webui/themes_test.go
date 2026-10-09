package webui

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testThemeFS(t *testing.T, themes, overrides string) fs.FS {
	t.Helper()
	embedded := embeddedOnlyFS(t)
	var base fs.FS = embedded
	var external fs.FS
	if overrides != "" {
		external = os.DirFS(overrides)
		base = overrideFS{external: external, embedded: embedded}
	}
	return themeFS{base: base, externalAssets: external, externalThemes: os.DirFS(themes), embedded: embedded}
}

func TestThemesDiscoveredAfterHandlerStarts(t *testing.T) {
	dir := t.TempDir()
	h := noCacheForEmbeddedAssets(http.FileServer(http.FS(testThemeFS(t, dir, ""))))
	listing := func() string {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/themes/", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("theme directory = %d: %s", rr.Code, rr.Body)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("theme discovery must read fresh directory entries")
		}
		return rr.Body.String()
	}
	before := listing()
	for _, id := range []string{"cyberpunk", "default-light", "default-dark"} {
		if !strings.Contains(before, `href="`+id+`/"`) {
			t.Fatalf("missing built-in theme %s", id)
		}
	}
	writeAssetFile(t, dir, "custom/theme.css", `@import url("./assets/static/css/app.css");`)
	writeAssetFile(t, dir, "empty/readme.txt", "not a theme")
	writeAssetFile(t, dir, ".hidden/theme.css", ":root {}")
	writeAssetFile(t, dir, "default-light/theme.css", ":root {}")
	after := listing()
	if !strings.Contains(after, `href="custom/"`) || strings.Contains(after, `href="empty/"`) || strings.Contains(after, `href=".hidden/"`) {
		t.Fatalf("live discovery returned wrong folders: %s", after)
	}
	if strings.Count(after, `href="default-light/"`) != 1 {
		t.Fatal("embedded and external themes must merge by id")
	}
	if err := os.RemoveAll(filepath.Join(dir, "custom")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing(), `href="custom/"`) {
		t.Fatal("a removed theme survived discovery")
	}
}

func TestThemeCSSRefreshesWithUnchangedModificationTime(t *testing.T) {
	dir := t.TempDir()
	writeAssetFile(t, dir, "custom/theme.css", ":root {}")
	palette := "custom/assets/static/css/tokens.css"
	writeAssetFile(t, dir, palette, "old palette")
	filename := filepath.Join(dir, filepath.FromSlash(palette))
	stamp := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := os.Chtimes(filename, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	h := noCacheForEmbeddedAssets(http.FileServer(http.FS(testThemeFS(t, dir, ""))))
	url := "/themes/" + palette
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, url, nil))
	if first.Code != http.StatusOK || first.Body.String() != "old palette" {
		t.Fatalf("initial CSS: %d %q", first.Code, first.Body.String())
	}
	writeAssetFile(t, dir, palette, "new palette")
	if err := os.Chtimes(filename, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	r.Header.Set("If-Modified-Since", first.Header().Get("Last-Modified"))
	second := httptest.NewRecorder()
	h.ServeHTTP(second, r)
	if second.Code != http.StatusOK || second.Body.String() != "new palette" {
		t.Fatalf("updated CSS: %d %q", second.Code, second.Body.String())
	}
	if second.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("theme chunks must not retain an earlier version")
	}
	if r.Header.Get("If-Modified-Since") == "" {
		t.Fatal("the static wrapper must not mutate its caller's request")
	}
}

func TestThemeAssetsFallbackAndLegacyOverrides(t *testing.T) {
	themes, overrides := t.TempDir(), t.TempDir()
	f := testThemeFS(t, themes, overrides)
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(f, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}
	name := "themes/default-light/assets/static/css/tokens.css"
	if !strings.Contains(read(name), "color-scheme: light") {
		t.Fatal("light theme did not resolve its palette")
	}
	writeAssetFile(t, themes, "default-light/assets/static/css/tokens.css", "external palette")
	if read(name) != "external palette" {
		t.Fatal("external theme must precede embedded theme")
	}
	writeAssetFile(t, overrides, "static/css/tokens.css", "global palette")
	if read(name) != "global palette" || read("static/css/tokens.css") != "global palette" {
		t.Fatal("legacy assets must win at original and themed URLs")
	}
	writeAssetFile(t, themes, "custom/theme.css", ":root {}")
	writeAssetFile(t, themes, "custom/theme.json", `{"terminal":{"fontSize":18,"transparent":false}}`)
	if !strings.Contains(read("themes/custom/theme.json"), `"fontSize":18`) {
		t.Fatal("terminal config was not served from its theme")
	}
	writeAssetFile(t, overrides, "themes/custom/theme.json", `{"terminal":{"fontSize":22}}`)
	if !strings.Contains(read("themes/custom/theme.json"), `"fontSize":22`) {
		t.Fatal("assets overrides must retain their priority for theme config")
	}
	writeAssetFile(t, themes, "custom/strings.json", `{"en":{"nav.nethub":"Custom hosts"}}`)
	if !strings.Contains(read("themes/custom/strings.json"), "Custom hosts") {
		t.Fatal("theme-owned UI copy was not served")
	}
	writeAssetFile(t, themes, "custom/strings.json", `{"en":{"nav.nethub":"Updated hosts"}}`)
	if !strings.Contains(read("themes/custom/strings.json"), "Updated hosts") {
		t.Fatal("theme copy updates require a restart")
	}
	writeAssetFile(t, themes, "custom/assets/icons/own.svg", "custom artwork")
	if read("themes/custom/assets/icons/own.svg") != "custom artwork" {
		t.Fatal("theme-owned artwork was not served")
	}
	if read("themes/custom/assets/static/css/base.css") != read("static/css/base.css") {
		t.Fatal("missing custom chunks must use shared assets")
	}
	if _, err := f.Open("themes/unknown/assets/static/css/base.css"); !os.IsNotExist(err) {
		t.Fatalf("unknown theme must not resolve shared assets: %v", err)
	}
	for _, invalid := range []string{"themes/../api.md", "themes/custom/../../api.md", "themes/bad\\name/theme.css"} {
		if file, err := f.Open(invalid); err == nil {
			file.Close()
			t.Fatalf("accepted invalid theme path %q", invalid)
		}
	}
}

func TestBuiltInThemeImportsResolve(t *testing.T) {
	f := testThemeFS(t, t.TempDir(), "")
	for _, id := range []string{"cyberpunk", "default-light", "default-dark"} {
		entry, err := fs.ReadFile(f, "themes/"+id+"/theme.css")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(entry), "./assets/static/css/app.css") {
			t.Fatalf("%s does not load its manifest", id)
		}
		prefix := "themes/" + id + "/assets/static/css/"
		manifest, err := fs.ReadFile(f, prefix+"app.css")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range cssImportRe.FindAllStringSubmatch(string(manifest), -1) {
			if _, err := fs.ReadFile(f, prefix+m[1]); err != nil {
				t.Errorf("%s import %s: %v", id, m[1], err)
			}
		}
	}
}

func TestBuiltInThemeCopyUsesCatalogKeysAndPlaceholders(t *testing.T) {
	f := testThemeFS(t, t.TempDir(), "")
	base := loadCatalogs(t)
	for _, id := range []string{"default-light", "default-dark", "cyberpunk"} {
		data, err := fs.ReadFile(f, "themes/"+id+"/strings.json")
		if err != nil {
			t.Fatal(err)
		}
		var catalog map[string]map[string]string
		if err := json.Unmarshal(data, &catalog); err != nil {
			t.Fatalf("%s copy: %v", id, err)
		}
		for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
			for key, text := range catalog[lang] {
				original, ok := base[lang][key]
				if !ok {
					t.Errorf("%s/%s has unknown copy key %s", id, lang, key)
				}
				if strings.Join(placeholders(text), ",") != strings.Join(placeholders(original), ",") {
					t.Errorf("%s/%s/%s dropped a placeholder", id, lang, key)
				}
			}
		}
		if strings.HasPrefix(id, "default-") {
			for lang, want := range map[string]string{"en": "Connections", "zh-Hans": "连接", "zh-Hant": "連線"} {
				if got := catalog[lang]["nav.nethub"]; got != want {
					t.Errorf("%s/%s title = %q, want %q", id, lang, got, want)
				}
			}
		} else {
			// The cyberpunk skin renames the resource column outright: it is the
			// access bay the whole theme is built around, so "Connections" is the
			// wrong word for it.
			for lang, want := range map[string]string{"en": "NetHub", "zh-Hans": "网络接入仓", "zh-Hant": "網路接入倉"} {
				if got := catalog[lang]["nav.nethub"]; got != want {
					t.Errorf("%s/%s title = %q, want %q", id, lang, got, want)
				}
			}
		}
		// The name lives in TWO keys — the label and the tooltip that expands it —
		// and a rename that touches only one leaves the other naming a control that
		// no longer exists. Asserted as containment (case-insensitive) rather than
		// equality, because the tooltip is a sentence around the name.
		for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
			name, tip := catalog[lang]["nav.nethub"], catalog[lang]["nethub.expand"]
			if name == "" || tip == "" {
				continue // absence is the other test's business
			}
			if !strings.Contains(strings.ToLower(tip), strings.ToLower(name)) {
				t.Errorf("%s/%s: the expand tooltip %q does not name the column %q; renaming one of the two keys leaves the other stale", id, lang, tip, name)
			}
		}
	}
}

func TestBuiltInTerminalThemeSettings(t *testing.T) {
	f := testThemeFS(t, t.TempDir(), "")
	for _, id := range []string{"default-light", "default-dark", "cyberpunk"} {
		data, err := fs.ReadFile(f, "themes/"+id+"/theme.json")
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Terminal struct {
				Transparent *bool   `json:"transparent"`
				Background  string  `json:"background"`
				FontSize    float64 `json:"fontSize"`
			} `json:"terminal"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		settings := config.Terminal
		if settings.Transparent == nil || *settings.Transparent != (id == "cyberpunk") {
			t.Errorf("%s must explicitly select its transparency mode", id)
		}
		if settings.FontSize <= 0 {
			t.Errorf("%s has no valid default font size", id)
		}
		if strings.HasPrefix(id, "default-") && settings.Background != "var(--bg-card)" {
			t.Errorf("%s must use its solid surface as the terminal background", id)
		}
	}
}
