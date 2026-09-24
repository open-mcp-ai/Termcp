package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Web UI ships three message catalogs (en, zh-Hans, zh-Hant) as a plain
// synchronous <script>, and every module looks its copy up through t(). These
// tests keep the three catalogs in lockstep and catch the two failure modes
// that are invisible in the Go build and in a browser smoke test: a key that
// exists in one language only (renders as the raw key for everyone else) and a
// key that is referenced by no markup and no code (dead weight that silently
// drifts out of date).
//
// Nothing here executes JavaScript: the catalog is a flat literal and is parsed
// with regexes on purpose, so the repo stays free of node/npm.

// catalogLangRe matches the three top-level language blocks, in file order.
var catalogLangRe = regexp.MustCompile(`(?m)^  (en|'zh-Hans'|'zh-Hant'): \{$`)

// catalogEntryRe matches one 'key': 'value' line inside a language block.
// Values are single-quoted and contain no apostrophes, which is what makes this
// parse possible; i18n-catalog.js documents that constraint.
var catalogEntryRe = regexp.MustCompile(`(?m)^    '([^']+)': '([^']*)',?$`)

// i18nKeyRe is the allowed key shape (domain.element.property, camelCase
// segments). It also rejects a Chinese sentence used as a key.
var i18nKeyRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)+$`)

// i18nAttrRe matches the four supported markup hooks.
var i18nAttrRe = regexp.MustCompile(`data-i18n(?:-title|-placeholder|-aria)?="([^"]+)"`)

// tCallRe matches the key arguments of t('key') and of
// tCount('key.one', 'key.other', ...). Anchoring on the call rather than
// scanning every quoted literal matters: comment prose with an apostrophe
// ("the token's id") leaves an odd number of quotes on a line and desynchronises
// naive quote pairing for the rest of the file.
var tCallRe = regexp.MustCompile(`\bt(?:Count)?\(\s*'([^']+)'(?:\s*,\s*'([^']+)')?`)

// i18nSetAttrRe matches the programmatic form of the same hook, which the
// modules use for nodes they build by hand (a dynamically created <select>,
// a button whose title depends on state).
var i18nSetAttrRe = regexp.MustCompile(`setAttribute\(\s*'data-i18n(?:-title|-placeholder|-aria)?'\s*,\s*'([^']+)'`)

// placeholderRe extracts {name} interpolation slots.
var placeholderRe = regexp.MustCompile(`\{(\w+)\}`)

// loadCatalogs parses i18n-catalog.js into lang -> key -> value.
func loadCatalogs(t *testing.T) map[string]map[string]string {
	t.Helper()
	body, err := readAsset("static/js/i18n-catalog.js")
	if err != nil {
		t.Fatalf("i18n-catalog.js must exist and be embedded: %v", err)
	}
	locs := catalogLangRe.FindAllStringSubmatchIndex(body, -1)
	if len(locs) != 3 {
		t.Fatalf("found %d language blocks in i18n-catalog.js, want 3 (en, zh-Hans, zh-Hant)", len(locs))
	}
	out := map[string]map[string]string{}
	for i, loc := range locs {
		lang := strings.Trim(body[loc[2]:loc[3]], "'")
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0] // the block runs to the next language header
		}
		entries := map[string]string{}
		for _, m := range catalogEntryRe.FindAllStringSubmatch(body[loc[1]:end], -1) {
			if _, dup := entries[m[1]]; dup {
				t.Errorf("catalog %s declares %q twice", lang, m[1])
			}
			entries[m[1]] = m[2]
		}
		if len(entries) == 0 {
			t.Fatalf("catalog %s parsed to zero entries; the file format changed", lang)
		}
		out[lang] = entries
	}
	return out
}

// TestCatalogsHaveIdenticalKeys is the core invariant: en is the authoritative
// set, and both Chinese catalogs must cover exactly the same keys.
func TestCatalogsHaveIdenticalKeys(t *testing.T) {
	cat := loadCatalogs(t)
	for _, key := range sortedKeys(cat["en"]) {
		if !i18nKeyRe.MatchString(key) {
			t.Errorf("catalog key %q does not match %s; keys are dotted lowercase paths, never a sentence", key, i18nKeyRe)
		}
		if strings.TrimSpace(cat["en"][key]) == "" {
			t.Errorf("catalog key %q has an empty en value", key)
		}
	}
	for _, lang := range []string{"zh-Hans", "zh-Hant"} {
		for _, key := range sortedKeys(cat[lang]) {
			if _, ok := cat["en"][key]; !ok {
				t.Errorf("%s has key %q that en does not; the catalogs must be identical", lang, key)
			}
		}
		for _, key := range sortedKeys(cat["en"]) {
			if _, ok := cat[lang][key]; !ok {
				t.Errorf("%s is missing key %q", lang, key)
			}
			if strings.TrimSpace(cat[lang][key]) == "" {
				t.Errorf("%s has an empty value for %q", lang, key)
			}
		}
	}
	t.Logf("%d keys x %d languages", len(cat["en"]), len(cat))
}

// TestIndexHTMLKeysExist catches a typo in a data-i18n* attribute: the engine
// falls back to rendering the key itself, so a misspelling shows up as
// "modal.foward.title" on screen instead of failing anything.
func TestIndexHTMLKeysExist(t *testing.T) {
	cat := loadCatalogs(t)
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := i18nAttrRe.FindAllStringSubmatch(index, -1)
	if len(refs) < 50 {
		t.Fatalf("index.html references only %d i18n keys; the static markup lost its markers", len(refs))
	}
	for _, m := range refs {
		if _, ok := cat["en"][m[1]]; !ok {
			t.Errorf("index.html references unknown i18n key %q", m[1])
		}
	}
	t.Logf("index.html marks %d static strings", len(refs))
}

// jsModules returns every UI module under static/js, excluding the catalog
// itself (its values are translations, not references).
func jsModules(t *testing.T) []string {
	t.Helper()
	all, err := fs.Glob(Assets(), "static/js/*.js")
	if err != nil {
		t.Fatal(err)
	}
	out := all[:0]
	for _, f := range all {
		if f == "static/js/i18n-catalog.js" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// TestNoOrphanKeys fails on a catalog entry no markup and no code ever asks
// for: it is either a leftover after a refactor or a key that was added "for
// later" and is already stale.
func TestNoOrphanKeys(t *testing.T) {
	cat := loadCatalogs(t)
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	referenced := map[string]bool{}
	for _, m := range i18nAttrRe.FindAllStringSubmatch(index, -1) {
		referenced[m[1]] = true
	}
	mods := jsModules(t)
	calls := 0
	for _, f := range mods {
		body, err := readAsset(f)
		if err != nil {
			t.Fatal(err)
		}
		// The modules also generate markup, so the data-i18n* markers they embed
		// in their template strings are references too.
		for _, m := range i18nAttrRe.FindAllStringSubmatch(body, -1) {
			referenced[m[1]] = true
			if _, ok := cat["en"][m[1]]; !ok {
				t.Errorf("%s embeds data-i18n marker for unknown key %q", f, m[1])
			}
		}
		for _, m := range i18nSetAttrRe.FindAllStringSubmatch(body, -1) {
			referenced[m[1]] = true
			if _, ok := cat["en"][m[1]]; !ok {
				t.Errorf("%s sets an unknown data-i18n key %q", f, m[1])
			}
		}
		for _, m := range tCallRe.FindAllStringSubmatch(body, -1) {
			for _, key := range m[1:] {
				if key == "" {
					continue
				}
				calls++
				referenced[key] = true
				// A key-shaped literal that is not in the catalog is a typo at
				// the call site; t() would render it verbatim on screen.
				if _, ok := cat["en"][key]; !ok && i18nKeyRe.MatchString(key) {
					t.Errorf("%s calls t(%q), which is not a catalog key", f, key)
				}
			}
		}
	}
	t.Logf("scanned %d modules, %d keyed t()/tCount() arguments", len(mods), calls)
	for _, key := range sortedKeys(cat["en"]) {
		if !referenced[key] {
			t.Errorf("catalog key %q is referenced by no data-i18n* attribute and no t() call", key)
		}
	}
}

// TestPlaceholdersMatchAcrossLanguages catches a translation that dropped a
// {name} slot: the value would render with a stale name baked in, or lose the
// dynamic part entirely.
func TestPlaceholdersMatchAcrossLanguages(t *testing.T) {
	cat := loadCatalogs(t)
	for _, key := range sortedKeys(cat["en"]) {
		want := placeholders(cat["en"][key])
		for _, lang := range []string{"zh-Hans", "zh-Hant"} {
			got := placeholders(cat[lang][key])
			if strings.Join(want, ",") != strings.Join(got, ",") {
				t.Errorf("key %q: en has {%s} but %s has {%s}", key, strings.Join(want, ","), lang, strings.Join(got, ","))
			}
		}
	}
}

// TestCoreKeysTranslated samples the keys a user meets first and asserts each
// language carries a real translation for them. Without executing t(), the
// check is that the value exists in all three catalogs and is not the key
// itself, which is exactly what t() falls back to.
func TestCoreKeysTranslated(t *testing.T) {
	cat := loadCatalogs(t)
	core := []string{
		"sec.entries", "sec.sessions",
		"session.delete.title", "session.delete.message", "session.status.running",
		"modal.forward.title", "modal.conn.titleAdd", "modal.start.title",
		"common.cancel", "common.delete", "common.close", "common.confirm",
		"fw.panel.title", "win.files.tab", "win.notify.tab", "win.term.tab",
		"common.download", "common.upload", "banner.dismiss", "lang.auto",
		"modal.forward.dir.local", "modal.forward.dir.remote",
	}
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, key := range core {
			v, ok := cat[lang][key]
			if !ok {
				t.Errorf("%s: core key %q is missing", lang, key)
				continue
			}
			if v == key {
				t.Errorf("%s: core key %q is not translated (value equals the key)", lang, key)
			}
		}
	}
	// The two Chinese catalogs must actually differ from English; a copy/paste
	// slip that left English in place is otherwise invisible.
	for _, lang := range []string{"zh-Hans", "zh-Hant"} {
		same := 0
		for _, key := range core {
			if cat[lang][key] == cat["en"][key] {
				same++
				t.Errorf("%s: core key %q still holds the English string", lang, key)
			}
		}
		if same > 0 {
			t.Logf("%s: %d core keys identical to English", lang, same)
		}
	}
}

// TestLanguageSelectorIsWired pins the three user-facing requirements to the
// markup: the switch offers auto plus all three languages, picking one calls
// the engine's entry point, and the dictionaries load before the modules that
// read them at load time.
func TestLanguageSelectorIsWired(t *testing.T) {
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(index, `class="lang-icon"`) {
		t.Error("index.html no longer shows the language glyph")
	}
	// The glyph is painted through a CSS mask so that it takes the label's own
	// colour, which means its reference lives in the stylesheet rather than in
	// index.html; check it there and check that the file is really served, since
	// the markup no longer points at it.
	css, err := readAsset("static/css/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(css, "icons/language.svg") {
		t.Error("app.css no longer masks the language glyph in; the icon would not render")
	}
	rr := httptest.NewRecorder()
	embeddedStaticServer().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/icons/language.svg", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("GET /icons/language.svg = %d, want 200", rr.Code)
	}
	// A trigger that names the language in use, and one button per choice behind
	// it. Both are real <button>s, so Tab/Enter/Space are the browser's job and
	// only the highlight is CSS.
	if !strings.Contains(index, `id="lang-btn"`) || !strings.Contains(index, `aria-haspopup="true"`) {
		t.Error("index.html has no language trigger button")
	}
	if !strings.Contains(index, `onclick="toggleLangMenu()"`) {
		t.Error("the language trigger does not open the menu")
	}
	if !strings.Contains(index, `id="lang-current"`) {
		t.Error("the trigger does not show the language in use")
	}
	if !strings.Contains(index, `id="lang-menu"`) {
		t.Error("index.html has no language menu")
	}
	for _, want := range []string{
		`data-lang="auto" onclick="chooseLang('auto')"`,
		`data-lang="en" onclick="chooseLang('en')"`,
		`data-lang="zh-Hans" onclick="chooseLang('zh-Hans')"`,
		`data-lang="zh-Hant" onclick="chooseLang('zh-Hant')"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("the language menu is missing the choice %s", want)
		}
	}
	if strings.Contains(index, "lang-select") || strings.Contains(index, `name="termcp-lang"`) {
		t.Error("a <select> or the old radio group is back; the switch is a trigger + menu now")
	}
	catalogAt := strings.Index(index, "static/js/i18n-catalog.js")
	engineAt := strings.Index(index, "static/js/i18n.js")
	utilAt := strings.Index(index, "static/js/util.js")
	xtermAt := strings.Index(index, "static/xterm/xterm.js")
	if catalogAt == -1 || engineAt == -1 {
		t.Fatal("index.html does not load i18n-catalog.js and i18n.js")
	}
	if !(xtermAt < catalogAt && catalogAt < engineAt && engineAt < utilAt) {
		t.Error("i18n-catalog.js and i18n.js must load after xterm.js and before util.js: " +
			"the engine fills the static markup at load time, and later modules translate during load")
	}
}

// TestUntranslatedAudit lists string literals that still look like UI copy.
//
// This is an audit, not a gate: a heuristic cannot tell "application/json" from
// "Open terminal", and turning it into t.Errorf would push the next person to
// grow a whitelist longer than the code. Run it with:
//
//	go test ./internal/webui/ -run TestUntranslatedAudit -v
func TestUntranslatedAudit(t *testing.T) {
	// Capitalised word + space + more text, which is what an English sentence
	// fragment looks like in this codebase.
	auditRe := regexp.MustCompile(`'([A-Z][a-z]+ [^']*)'`)
	// Lines that are already translated, or that hold a protocol/CSS/identifier
	// value rather than copy.
	skipLineRe := regexp.MustCompile(`data-i18n|\bt\(|\btCount\(`)
	skipTextRe := regexp.MustCompile(`application/json|text/plain|text/html|charset=|Content-Type|GET |POST |PUT |DELETE |HTTP |://|Monaco|Consolas|px|vh|dvw|calc\(|rgba\(|termcp|[Ss]erver response|session_id|shell_id`)
	found := 0
	for _, f := range jsModules(t) {
		body, err := readAsset(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(body, "\n") {
			if skipLineRe.MatchString(line) {
				continue
			}
			for _, m := range auditRe.FindAllStringSubmatch(line, -1) {
				if skipTextRe.MatchString(m[1]) {
					continue
				}
				found++
				t.Logf("%s:%d: %s", f, i+1, m[1])
			}
		}
	}
	t.Logf("%d candidate untranslated strings (review by hand; diagnostics shown to the user inside a translated sentence are intentional)", found)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func placeholders(s string) []string {
	var out []string
	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}
