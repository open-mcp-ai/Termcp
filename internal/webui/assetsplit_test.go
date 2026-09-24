package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Web UI is served as a thin index.html plus independently cacheable
// modules under static/js and static/css. These tests guard the split: a
// dropped <script>, a module that 404s, or a declaration that moved out of
// the shared global scope all break the page in the browser while leaving the
// Go build green, so they are asserted here instead.

// assetRefRe matches src=/href= targets in index.html.
var assetRefRe = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// moduleScriptRe matches the module <script src> tags, in document order.
var moduleScriptRe = regexp.MustCompile(`<script src="(static/js/[^"]+)"></script>`)

// TestIndexHTMLReferencesOnlyExistingAssets catches a renamed or missing
// module: a 404 on any referenced asset yields a blank page with no Go error.
func TestIndexHTMLReferencesOnlyExistingAssets(t *testing.T) {
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(assetRefRe.FindAllStringSubmatch(index, -1)) == 0 {
		t.Fatal("index.html references no assets at all; the split was reverted?")
	}
	for _, m := range assetRefRe.FindAllStringSubmatch(index, -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "/") || strings.Contains(ref, "://") {
			continue // absolute or external
		}
		// Serve it through the same handler the browser hits, so a path that
		// exists on disk but is unreachable over HTTP still fails.
		rr := httptest.NewRecorder()
		embeddedStaticServer().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/"+ref, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("index.html references %s but GET /%s = %d", ref, ref, rr.Code)
		}
	}
}

// TestModulesLoadedInSourceOrder pins the load order. The modules rely on
// script-execution order for `var` state that is initialised and read at load
// time (e.g. forward-modal's cached DOM refs before its wiring statements run),
// so a shuffled order can break the page even though every name exists.
func TestModulesLoadedInSourceOrder(t *testing.T) {
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	got := moduleScriptRe.FindAllStringSubmatch(index, -1)
	if len(got) < 2 {
		t.Fatalf("expected several module scripts in index.html, found %d", len(got))
	}
	// Every module file under static/js must be referenced by index.html: an
	// unreferenced module is dead code, and a referenced-but-missing one 404s.
	onDisk, err := fs.Glob(Assets(), "static/js/*.js")
	if err != nil {
		t.Fatal(err)
	}
	referenced := map[string]bool{}
	for _, m := range got {
		referenced[m[1]] = true
	}
	for _, f := range onDisk {
		if !referenced[f] {
			t.Errorf("%s exists but no <script src=%q> in index.html loads it", f, f)
		}
	}
	if len(onDisk) != len(got) {
		t.Errorf("index.html loads %d modules but %d exist under static/js", len(got), len(onDisk))
	}
	// Every module except the last must appear before the following one, and
	// the whole block must sit after the xterm vendor script so `Terminal`
	// exists when the modules are evaluated.
	xtermAt := strings.Index(index, "static/xterm/xterm.js")
	firstModuleAt := strings.Index(index, got[0][1])
	if xtermAt == -1 || firstModuleAt == -1 || xtermAt > firstModuleAt {
		t.Error("xterm.js must be loaded before the UI modules")
	}
	last := -1
	for _, m := range got {
		at := strings.Index(index, m[1])
		if at < last {
			t.Errorf("module %s appears out of order in index.html", m[1])
		}
		last = at
	}
}

// TestModuleDeclarationsReachSharedScope verifies the split's core invariant:
// the modules are plain scripts (no per-file IIFE and no `let`/`const` at top
// level), so their top-level `var`/`function` declarations land in the shared
// global scope and stay visible to every other module. Wrapping one module in
// its own IIFE, or switching a declaration to `let`, silently hides it from
// the others at runtime.
func TestModuleDeclarationsReachSharedScope(t *testing.T) {
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	mods := moduleScriptRe.FindAllStringSubmatch(index, -1)
	if len(mods) == 0 {
		t.Fatal("no module scripts found")
	}

	declRe := regexp.MustCompile(`(?m)^(?:var|let|const|function)\s+([A-Za-z_$][\w$]*)`)
	seen := map[string]string{} // name -> module that declares it
	total := 0

	for _, m := range mods {
		path := m[1] // e.g. static/js/util.js, relative to the assets root
		body, err := readAsset(path)
		if err != nil {
			t.Errorf("module %s not embedded: %v", m[1], err)
			continue
		}

		// No module may wrap itself in an IIFE: that would re-privatise scope.
		// The original single-file UI did exactly this, which is why the split
		// drops the wrapper.
		head := strings.TrimSpace(firstNonEmptyLine(body))
		if head == "(function () {" || strings.HasPrefix(head, "(() =>") {
			t.Errorf("%s starts with an IIFE wrapper (%q); modules must share the global scope", m[1], head)
		}

		for _, d := range declRe.FindAllStringSubmatch(body, -1) {
			name := d[1]
			full := strings.TrimSpace(strings.SplitN(body[strings.Index(body, d[0]):], "\n", 2)[0])
			if strings.HasPrefix(full, "let ") || strings.HasPrefix(full, "const ") {
				t.Errorf("%s declares %q with let/const; script-scoped bindings are invisible to the other modules — use var", m[1], name)
			}
			if prev, dup := seen[name]; dup {
				t.Errorf("top-level %q declared in both %s and %s; one module silently shadows the other", name, prev, m[1])
			}
			seen[name] = m[1]
			total++
		}
	}
	if total < 50 {
		t.Errorf("only %d top-level declarations found across modules; the split looks truncated", total)
	}
	t.Logf("%d top-level declarations across %d modules share the global scope", total, len(mods))
}

func firstNonEmptyLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// TestCSSIsExtractedAndServable checks the stylesheet left index.html for its
// own file, so it caches independently of the page markup.
func TestCSSIsExtractedAndServable(t *testing.T) {
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(index, "<style>") {
		t.Error("index.html still inlines a <style> block; it should live in static/css/app.css")
	}
	rr := httptest.NewRecorder()
	embeddedStaticServer().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /static/css/app.css = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), ".app-header") {
		t.Error("app.css does not contain the app's own rules; the extraction looks wrong")
	}
}

// TestIndexHTMLIsThin documents the point of the split: the page itself must
// stay small enough to re-fetch on every load while the modules cache.
func TestIndexHTMLIsThin(t *testing.T) {
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	const maxBytes = 64 * 1024
	if len(index) > maxBytes {
		t.Errorf("index.html is %d bytes, over the %d-byte budget; markup or logic crept back in", len(index), maxBytes)
	}
	mods := moduleScriptRe.FindAllStringSubmatch(index, -1)
	biggest, bigName := 0, ""
	for _, m := range mods {
		fi, err := fs.Stat(Assets(), m[1])
		if err != nil {
			continue
		}
		if int(fi.Size()) > biggest {
			biggest, bigName = int(fi.Size()), m[1]
		}
	}
	if biggest == 0 {
		t.Fatal("could not stat any module")
	}
	t.Logf("index.html=%d B, largest module %s=%d B, %d modules", len(index), bigName, biggest, len(mods))
}

// TestNoUnreachableUI pins the removal of the floating tools window
// (#panel-tools) and its module. That window shipped with no way to open it:
// openToolPanel had no caller in any commit, so the whole surface was markup,
// CSS and a 350-line module that no user could ever reach. The panel's two
// tabs duplicated what every terminal window already offers (the term/fw/file
// tabs in .shell-tab-bar), so the fix was deletion, not wiring up a trigger.
//
// This guards against the panel creeping back: a re-added #panel-tools with no
// caller would be invisible in the Go build and in every unit test, which is
// exactly how it survived this long.
func TestNoUnreachableUI(t *testing.T) {
	index, err := readAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(index, "panel-tools") {
		t.Error("index.html declares #panel-tools again; it had no trigger and was removed")
	}
	if _, err := fs.Stat(Assets(), "static/js/tools-panel.js"); err == nil {
		t.Error("tools-panel.js is back; the reachable part of it lives in forward-modal.js")
	}
	// The forward modal is the part that was reachable, so it must still be wired.
	fm, err := readAsset("static/js/forward-modal.js")
	if err != nil {
		t.Fatalf("forward-modal.js must exist: %v", err)
	}
	for _, want := range []string{
		"function openForwardModal(",
		"function createForward(",
		"function deleteForward(",
		"function forwardMatchesConfig(",
	} {
		if !strings.Contains(fm, want) {
			t.Errorf("forward-modal.js lost %q; the forward modal needs it", want)
		}
	}
	// The two formatters moved to util.js because the per-window file tab uses
	// them and forward-modal no longer has a file panel. Losing them breaks the
	// file tab at runtime only, so pin the new home.
	util, err := readAsset("static/js/util.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"function formatSize(", "function fmtTime("} {
		if !strings.Contains(util, want) {
			t.Errorf("util.js should declare %q for the per-window file tab", want)
		}
	}
	// And no module may re-declare them: two top-level definitions silently
	// shadow each other (TestModuleDeclarationsReachSharedScope also covers this,
	// but a targeted check says why).
	mods, _ := fs.Glob(Assets(), "static/js/*.js")
	for _, f := range mods {
		if f == "static/js/util.js" {
			continue
		}
		body, err := readAsset(f)
		if err != nil {
			continue
		}
		for _, fn := range []string{"function formatSize(", "function fmtTime("} {
			if strings.Contains(body, fn) {
				t.Errorf("%s re-declares %q; it belongs to util.js only", f, fn)
			}
		}
	}
}

// TestModulesParse asserts every served JS module is a syntactically valid
// script. Nothing else here can see a parse error: the assertions are string
// matches on the source, so a module can lose one comment marker and stop
// parsing entirely while every test stays green. That is not hypothetical —
// a `//` dropped from a comment in terminal-view.js made the whole file fail to
// parse, and every function it declares (openShellWindow among them) vanished
// from the page, so clicking a session did nothing.
//
// node is used when present because it is the real parser; without it the test
// skips rather than reporting a false pass.
func TestModulesParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot check module syntax")
	}
	mods, err := fs.Glob(Assets(), "static/js/*.js")
	if err != nil || len(mods) == 0 {
		t.Fatalf("no modules to check: %v", err)
	}
	for _, m := range mods {
		// node --check needs a real file; the embedded copy is written to a temp
		// file so the check runs against exactly what the browser is served.
		body, err := fs.ReadFile(Assets(), m)
		if err != nil {
			t.Errorf("module %s not embedded: %v", m, err)
			continue
		}
		tmp := filepath.Join(t.TempDir(), filepath.Base(m))
		if err := os.WriteFile(tmp, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", tmp, err)
		}
		if out, err := exec.Command(node, "--check", tmp).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", m, err, out)
		}
	}
}
