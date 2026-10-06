package webui

import (
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The terminal font stack has two failure modes that are invisible on the
// developer's machine and only show up on someone else's, so both are pinned
// here. See docs/design/font-stack.md for the reasoning and the per-OS matrix.

// fontStackRe captures one custom property's value from the CSS.
func fontStackRe(prop string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(prop) + `\s*:\s*([^;]+);`)
}

// collapseWS normalises a declaration the way both a browser's
// getPropertyValue and a hand-written JS string would be compared: leading and
// trailing space gone, every internal run of whitespace reduced to one space.
// tokens.css wraps these stacks across lines, so a raw comparison would fail on
// formatting rather than on content.
func collapseWS(s string) string {
	return strings.TrimSpace(wsRunRe.ReplaceAllString(strings.TrimSpace(s), " "))
}

var wsRunRe = regexp.MustCompile(`\s+`)

// stripCSSComments removes /* ... */ so a name quoted inside the comment above a
// stack cannot be mistaken for an entry of the stack itself.
func stripCSSComments(s string) string {
	return cssCommentRe.ReplaceAllString(s, "")
}

var cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

// splitFontList splits a font stack into its entries, trimming the quotes and
// spaces so comparisons are about font names and order, not punctuation.
func splitFontList(s string) []string {
	var out []string
	for _, part := range strings.Split(collapseWS(s), ",") {
		name := strings.Trim(strings.TrimSpace(part), `"'`)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// TestTerminalFontStackMatchesCSS pins the JS fallback copy to the CSS
// declaration.
//
// xterm takes fontFamily as a JavaScript option and writes it into its own
// stylesheet, so the terminal grid is the one surface that cannot inherit
// --font-mono. util.js therefore keeps a copy of the stack for the no-variable
// case. That copy is the whole bug in miniature: the previous hardcoded value
// was a latin-only list ('Consolas, Monaco, monospace') which every machine
// with Consolas obeyed and every bare Linux box fell through, so the terminal
// lost CJK coverage while the rest of the UI kept it. A silent drift between
// the two lists restores exactly that, and nothing on a developer's machine
// reveals it — hence the entry-by-entry check below. It compares after the two
// normalisations a browser applies anyway (whitespace collapsed, comments
// stripped), so it fails on a moved or renamed face, not on re-wrapping the
// declaration.
func TestTerminalFontStackMatchesCSS(t *testing.T) {
	css := readAppCSS(t)
	m := fontStackRe("--font-mono").FindStringSubmatch(css)
	if m == nil {
		t.Fatal("tokens.css no longer declares --font-mono; the terminal font source is gone")
	}
	cssStack := collapseWS(stripCSSComments(m[1]))

	util := readAssetLF(t, "static/js/util.js")
	jm := regexp.MustCompile(`(?s)TERMCP_MONO_FALLBACK\s*=\s*'([^']*)'`).FindStringSubmatch(util)
	if jm == nil {
		t.Fatal("util.js no longer declares TERMCP_MONO_FALLBACK; termcpMonoFontFamily() has nothing to fall back to")
	}
	jsStack := collapseWS(jm[1])

	if jsStack != cssStack {
		t.Errorf("the terminal font stack has drifted from --font-mono.\n"+
			"  tokens.css: %s\n  util.js:    %s\n"+
			"Both lists must stay identical: xterm cannot read the CSS variable, so the JS copy is\n"+
			"what a terminal actually uses.", cssStack, jsStack)
	}
}

// TestMonoFontStackOrdersProportionalCJCLast pins the one ordering rule that is
// both real and measurable.
//
// Proportional CJK faces must come after every grid-safe face. A font stack
// resolves per glyph, so a proportional CJK face placed early answers for Latin
// text too. Measured at 13px (scripts/fontlab.py): a proportional CJK face
// advances 'W' by ~13.2px with M=12.7 and i=3.5, against 7.1/7.1/7.1 for a
// Latin-only face. xterm derives the cell from 'W' and then corrects every glyph
// with letter-spacing (cells*cellWidth - glyphWidth), so a face that moves the
// cell width by 85% dictates the spacing correction for every glyph on screen —
// and it was only ever meant to supply Han.
//
// Note what is deliberately NOT asserted: that every CJK face follows every
// Latin face. That was the first version of this test and the measurements
// disproved it. A CJK *mono* face covers both scripts at exactly 2:1 (Latin
// 0.5em, Han 1em), so it is a legitimate grid face, and on a Linux box that has
// one it is the better answer for both scripts. Fallback is per glyph, so
// position decides which face *paints* Han, not whether Han is covered at all;
// only the proportional faces have to stay behind the grid-safe ones.
func TestMonoFontStackOrdersProportionalCJCLast(t *testing.T) {
	css := readAppCSS(t)
	m := fontStackRe("--font-mono").FindStringSubmatch(css)
	if m == nil {
		t.Fatal("tokens.css no longer declares --font-mono")
	}
	names := splitFontList(stripCSSComments(m[1]))
	if len(names) < 10 {
		t.Fatalf("--font-mono parsed to only %d entries (%v); the declaration looks truncated", len(names), names)
	}

	// Faces that are safe for the grid: Latin-only, or CJK mono (which is also
	// Latin-capable at a 2:1 ratio). Anything else is a proportional face.
	//
	// The generic `monospace` keyword is excluded on purpose: as a fallback it is
	// "safe" (it never outranks a named face), but as a *position* it is the end
	// of the list, so treating it as grid-safe would make the ordering check
	// meaningless. Everything before it can be classified.
	gridSafe := map[string]bool{
		// Latin-only.
		"Inter": true, "Geist": true, "ui-monospace": true, "SFMono-Regular": true,
		"SF Mono": true, "Menlo": true, "Monaco": true, "Cascadia Mono": true,
		"Consolas": true, "JetBrains Mono": true, "Roboto Mono": true,
		"DejaVu Sans Mono": true, "Liberation Mono": true, "Noto Sans Mono": true,
		"Ubuntu Mono": true, "Droid Sans Mono": true,
		// CJK monospaced: both scripts from one face, Han = 2 x Latin.
		"Sarasa Mono SC": true, "Sarasa Fixed SC": true, "Sarasa Term SC": true,
		"Sarasa Mono TC": true, "Sarasa Fixed TC": true, "Sarasa Term TC": true,
		"Sarasa Mono J": true, "Sarasa Mono K": true,
		"Noto Sans Mono CJK SC": true, "Noto Sans Mono CJK TC": true,
		"Noto Sans Mono CJK JP": true, "Noto Sans Mono CJK KR": true,
		"Source Han Mono SC": true, "Source Han Mono TC": true,
		"Source Han Mono J": true, "Source Han Mono K": true,
		"WenQuanYi Zen Hei Mono": true, "WenQuanYi Micro Hei Mono": true,
		// Windows Simplified Han, monospaced face of the SimSun family: Han is
		// exactly 2x its Latin advance, so it is grid-safe and belongs here rather
		// than in the proportional block below.
		"NSimSun": true,
	}

	lastGridSafe, firstProportional := -1, -1
	cjkMonoCount := 0
	for i, n := range names {
		// Emoji is neither a Latin nor a CJK face: it constrains no ordering.
		if n == "Noto Color Emoji" || n == "Apple Color Emoji" || n == "Segoe UI Emoji" {
			continue
		}
		if n == "monospace" {
			// The generic keyword: last by construction, and it is what every
			// named face above it deliberately outranks.
			continue
		}
		if gridSafe[n] {
			lastGridSafe = i
			if strings.Contains(n, "Mono CJK") || strings.HasPrefix(n, "Sarasa") ||
				strings.Contains(n, "Han Mono") || strings.Contains(n, "Hei Mono") {
				cjkMonoCount++
			}
			continue
		}
		// Everything else is a proportional CJK face; assuming that for unknown
		// names is the safe direction, since a proportional face placed early is
		// exactly the regression.
		if firstProportional == -1 {
			firstProportional = i
		}
	}

	if lastGridSafe == -1 {
		t.Fatalf("--font-mono has no grid-safe face left: %v", names)
	}
	if names[len(names)-1] != "monospace" {
		t.Errorf("--font-mono ends with %q; the generic keyword must stay last so it only "+
			"answers when nothing named above matched", names[len(names)-1])
	}
	if cjkMonoCount == 0 {
		t.Error("--font-mono names no CJK monospaced face, so no face can serve both scripts at a " +
			"2:1 ratio; the grid then depends on a Latin face's width matching an unrelated CJK face")
	}
	if firstProportional != -1 && firstProportional < lastGridSafe {
		t.Errorf("--font-mono puts proportional CJK face %q (at %d) before the grid-safe face %q (at %d).\n"+
			"A proportional CJK face also answers for Latin, and its Latin advances are proportional\n"+
			"(measured ~13.2px vs 7.1px for 'W'), so it would dictate the cell width and therefore the\n"+
			"letter-spacing correction of every glyph. Keep proportional CJK faces last.",
			names[firstProportional], firstProportional, names[lastGridSafe], lastGridSafe)
	}
}

// TestTerminalGridUsesSharedFontStack checks that the terminal is wired to the
// shared stack in both places that need it.
//
// The grid and the column measurement must agree: fitShellTerminal measures a
// span to convert pixels into columns, and if that span paints in a different
// face than the terminal grid, the column count is wrong — a resized terminal
// ends up with wrapped lines that fit fine, or lines cut off early.
func TestTerminalGridUsesSharedFontStack(t *testing.T) {
	js := readTerminalJS(t)
	if !strings.Contains(js, "fontFamily: termcpMonoFontFamily()") {
		t.Error("terminal-view.js no longer builds its Terminal with the shared font stack; " +
			"a literal here is what left the terminal latin-only on bare Linux")
	}
	if strings.Contains(js, "Consolas, Monaco, monospace") {
		t.Error("terminal-view.js hardcodes the old latin-only stack again")
	}

	sock := readAssetLF(t, "static/js/ui-socket.js")
	if !strings.Contains(sock, "termcpMonoFontFamily()") {
		t.Error("ui-socket.js measures terminal columns with a font other than the grid's; " +
			"the pixel-to-column conversion would disagree with what is painted")
	}

	util := readAssetLF(t, "static/js/util.js")
	if !strings.Contains(util, "function termcpMonoFontFamily()") {
		t.Error("util.js lost termcpMonoFontFamily(), the single reader of --font-mono")
	}
	if !strings.Contains(util, "getPropertyValue('--font-mono')") {
		t.Error("termcpMonoFontFamily() no longer reads --font-mono from the stylesheet")
	}
}

// TestNoLatinOnlyMonoStackInAssets scans every served asset except the vendored
// xterm bundle for a hand-written latin-only monospace stack.
//
// These strings are how the bug spread: each one is a surface that silently
// drops CJK, and each was added at a different time. The scan covers the served
// static tree (static/js, static/css) and exempts only tokens.css, where the
// stack is allowed to be spelled out. Two neighbouring files are out of scope
// for reasons rather than by accident: the vendored bundle under static/xterm,
// whose own JS default (`courier-new, courier, monospace`) is overridden by the
// fontFamily option at every construction site; and assets/api.html, a
// standalone English-only page that loads neither app.css nor tokens.css, so
// var(--font-mono) does not exist there and its inline stack covers every
// character it actually contains.
func TestNoLatinOnlyMonoStackInAssets(t *testing.T) {
	bad := []string{
		"ui-monospace, monospace",
		"ui-monospace,monospace",
		"Consolas, Monaco, monospace",
		"monospace, monospace",
		"SFMono-Regular, Menlo, Consolas, monospace",
	}
	// The scanned set comes from the tree, not from a list kept here: the point of
	// the check is that a NEW surface cannot quietly add another stack, and a
	// hand-maintained list is exactly what lets it (each of the files below was
	// added at a different time, and none of them was added by this test).
	var files []string
	for _, pat := range []string{"static/js/*.js", "static/css/*.css"} {
		m, err := fs.Glob(Assets(), pat)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 20 {
		t.Fatalf("the scan found only %d scripts/stylesheets; the asset tree looks unreadable and "+
			"the check would pass vacuously: %v", len(files), files)
	}
	for _, f := range files {
		body := readAssetLF(t, f)
		// tokens.css is the one place allowed to name the typefaces, and it does
		// so in a full stack; only the other files are checked for a short list.
		if strings.HasSuffix(f, "tokens.css") {
			continue
		}
		for _, b := range bad {
			if strings.Contains(body, b) {
				t.Errorf("%s contains the latin-only stack %q; it drops CJK coverage on a "+
					"machine without the named Latin faces. Use var(--font-mono) (CSS) or "+
					"termcpMonoFontFamily() (JS).", f, b)
			}
		}
	}
}

// TestHantSansStackLeadsWithTraditionalFaces covers the Traditional-Chinese
// chrome stack, which carries the same class of ordering invariant as the
// terminal one: glyph *forms* are selected by which font name wins, so if a
// Simplified-first machine matches an SC face first it renders Traditional text
// with Simplified shapes. Nothing else asserted this, and the rule is invisible
// on a machine whose locale happens to match the language being displayed.
func TestHantSansStackLeadsWithTraditionalFaces(t *testing.T) {
	css := readAppCSS(t)
	m := fontStackRe("--font-sans-hant").FindStringSubmatch(css)
	if m == nil {
		t.Fatal("tokens.css no longer declares --font-sans-hant")
	}
	names := splitFontList(stripCSSComments(m[1]))

	firstTC, firstSC := -1, -1
	for i, n := range names {
		switch n {
		case "PingFang TC", "PingFang HK", "Microsoft JhengHei", "Noto Sans CJK TC", "Noto Sans TC":
			if firstTC == -1 {
				firstTC = i
			}
		case "PingFang SC", "Microsoft YaHei", "Hiragino Sans GB", "Heiti SC", "Noto Sans CJK SC", "Noto Sans SC":
			if firstSC == -1 {
				firstSC = i
			}
		}
	}
	if firstTC == -1 {
		t.Fatalf("--font-sans-hant has no Traditional-specific face; Traditional text would take "+
			"Simplified shapes on a Simplified-first machine: %v", names)
	}
	if firstSC != -1 && firstSC < firstTC {
		t.Errorf("--font-sans-hant leads with Simplified face %q (at %d) before Traditional face %q (at %d); "+
			"body.lang-hant would render with Simplified glyph forms.",
			names[firstSC], firstSC, names[firstTC], firstTC)
	}

	// base.css must bind the language class to the variable rather than repeat
	// the list, so the two chrome stacks cannot drift apart.
	base := readAssetLF(t, "static/css/base.css")
	if !strings.Contains(base, "body.lang-hant { font-family: var(--font-sans-hant); }") {
		t.Error("base.css no longer binds body.lang-hant to --font-sans-hant; " +
			"a second hand-written list here is exactly the duplication the variable exists to remove")
	}
}

// TestHantSansStackIsSansReordered pins the other half of what the two chrome
// stacks promise each other: same faces, Traditional ones first.
//
// Only the ordering half was checked before, and the list had already drifted —
// the Hant stack was missing Source Han Sans SC, both WenQuanYi faces and the JP
// and KR names, so a machine whose only Han font was one of those rendered the
// Traditional UI with no CJK coverage at all. A reorder is what the variable
// exists for; dropping faces while "reordering" is a silent coverage loss, and
// it is invisible unless someone switches the UI to 繁中 on that machine.
func TestHantSansStackIsSansReordered(t *testing.T) {
	css := readAppCSS(t)
	var lists [2][]string
	for i, prop := range []string{"--font-sans", "--font-sans-hant"} {
		m := fontStackRe(prop).FindStringSubmatch(css)
		if m == nil {
			t.Fatalf("tokens.css no longer declares %s", prop)
		}
		lists[i] = splitFontList(stripCSSComments(m[1]))
	}
	sans, hant := lists[0], lists[1]

	// PingFang HK is an extra Traditional name with no Simplified counterpart,
	// so it is allowed to exist only on the Hant side.
	inSans := map[string]bool{}
	for _, n := range sans {
		inSans[n] = true
	}
	inHant := map[string]bool{}
	for _, n := range hant {
		inHant[n] = true
	}
	for _, n := range sans {
		if !inHant[n] {
			t.Errorf("--font-sans-hant is missing %q, which --font-sans names; the Traditional stack is "+
				"supposed to be the same list reordered, so this face's coverage is lost in 繁中", n)
		}
	}
	for _, n := range hant {
		if !inSans[n] && n != "PingFang HK" {
			t.Errorf("--font-sans-hant names %q, which --font-sans does not; the two stacks are drifting "+
				"apart instead of sharing one list", n)
		}
	}
}

// TestTerminalFontStackServedOverHTTP is a smoke check that the edited chunks
// are still the ones the server hands out, so these assertions describe what a
// browser actually receives rather than what is on disk.
func TestTerminalFontStackServedOverHTTP(t *testing.T) {
	for _, path := range []string{"/static/css/tokens.css", "/static/css/app.css"} {
		rr := httptest.NewRecorder()
		embeddedStaticServer().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("GET %s = %d, want 200", path, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	embeddedStaticServer().ServeHTTP(rr, httptest.NewRequest("GET", "/static/js/util.js", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /static/js/util.js = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "termcpMonoFontFamily") {
		t.Error("the served util.js does not carry the shared font stack reader")
	}
}
