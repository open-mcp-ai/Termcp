package webui

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The glass degradation has two entrances and one switch, and both halves are
// invisible to a unit test unless the wiring is pinned — which is exactly how the
// cost shipped: a page that is correct in every assertion a Go test can make and
// still spends 200ms per frame on a machine without a GPU.
//
// So this pins the three things that make the fix work, each of which can be
// reverted without breaking anything else:
//
//  1. the stylesheet that neutralises the blur is served AFTER app.css, because
//     the chunks' declarations have the same specificity and a sheet that loses
//     the cascade fixes nothing;
//  2. the JS probe still reads the renderer string and the OS preference, and
//     still runs at boot — a probe that is defined but never called leaves the
//     class off and the page translucent on the slow machines it exists for;
//  3. the class it sets is the one the stylesheet keys off, so the two halves
//     cannot drift apart (a rename on either side is silent otherwise).

// declarationSites returns the selector list of every rule in the concatenated
// chunks that declares tok. A custom property's value is resolved on the element
// that declares it and inherited from there, so a token declared anywhere but
// :root needs its own override — this walker is how the test learns where those
// places are instead of hard-coding a list that goes stale.
func declarationSites(t *testing.T, css, tok string) []string {
	t.Helper()
	withoutComments := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	var sites []string
	for _, rule := range strings.Split(withoutComments, "}") {
		if !strings.Contains(rule, tok) {
			continue
		}
		open := strings.Index(rule, "{")
		if open < 0 {
			continue
		}
		sel := strings.TrimSpace(rule[:open])
		if i := strings.LastIndex(sel, "}"); i >= 0 {
			sel = strings.TrimSpace(sel[i+1:])
		}
		if sel != "" {
			sites = append(sites, sel)
		}
	}
	return sites
}

// glassProbeJS is the section of util.js that decides whether glass is
// affordable: from the probe helper to the boot call that applies it.
func glassProbeJS(t *testing.T) string {
	t.Helper()
	src := readAssetLF(t, "static/js/util.js")
	start := strings.Index(src, "function termcpGlassSignals()")
	if start < 0 {
		t.Fatal("util.js has no termcpGlassSignals probe")
	}
	end := strings.Index(src[start:], "var SVG_COPY_12")
	if end < 0 {
		t.Fatal("the glass probe section does not end where the next declaration begins")
	}
	return src[start : start+end]
}

func TestGlassDegradeNeedsNoImportant(t *testing.T) {
	index := readAssetLF(t, "index.html")
	manifest, err := readAsset("static/css/app.css")
	if err != nil {
		t.Fatal(err)
	}

	// The override has to be the LAST chunk in the manifest. Every blur declaration
	// in the other chunks has the same specificity as the override, so only a later
	// sheet can win — and app.css pulls the chunks in with @import, which the browser
	// expands in place, so manifest order IS cascade order.
	var order []string
	for _, line := range strings.Split(manifest, "\n") {
		if m := cssImportRe.FindStringSubmatch(line); m != nil {
			order = append(order, m[1])
		}
	}
	if len(order) == 0 {
		t.Fatal("app.css imports no chunks; the manifest is unreadable")
	}
	if order[len(order)-1] != "noglass.css" {
		t.Errorf("noglass.css must be the LAST chunk in app.css (order: %v), or the other chunks' backdrop-filter wins the cascade", order)
	}

	// A theme may ship its OWN manifest (cyberpunk does) and the page then loads
	// that one instead, so the chunk has to be last in every manifest — not just in
	// the shared one. Nothing else fetches those files, so a theme manifest that
	// omits the chunk would simply keep its glass on a slow machine, silently.
	themes, err := fs.Glob(Assets(), "themes/*/assets/static/css/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if len(themes) == 0 {
		t.Fatal("no theme ships a manifest; the check below would pass vacuously")
	}
	for _, path := range themes {
		body := readAssetLF(t, path)
		var chunks []string
		for _, line := range strings.Split(body, "\n") {
			if m := cssImportRe.FindStringSubmatch(line); m != nil {
				chunks = append(chunks, m[1])
			}
		}
		if len(chunks) == 0 {
			t.Errorf("%s imports no chunks; the manifest is unreadable", path)
			continue
		}
		if chunks[len(chunks)-1] != "noglass.css" {
			t.Errorf("%s must end with noglass.css (order: %v): the page loads this manifest instead of static/css/app.css, so the glass override never reaches that theme", path, chunks)
		}
	}
	block := readAssetLF(t, "static/css/noglass.css")

	// The stylesheet is fetched separately from the page, so it caches on its own;
	// and it must not live in index.html, whose extraction is asserted elsewhere.
	if strings.Contains(index, "html.no-glass") {
		t.Error("the degradation belongs in a CSS chunk, not inline in index.html")
	}

	// Every blur in the UI must be neutralised, including the two that use the
	// -webkit- prefix (Safari), or the fix is partial on one engine.
	for _, want := range []string{"backdrop-filter: none", "-webkit-backdrop-filter: none"} {
		if !strings.Contains(block, want) {
			t.Errorf("the degradation chunk should declare %q", want)
		}
	}
	// The wildcard has to reach pseudo-elements too: body::after is a real layer
	// in this UI and would keep its own stacking/paint cost otherwise.
	if !strings.Contains(block, "html.no-glass *::after") {
		t.Error("the degradation chunk must cover pseudo-elements, not only elements")
	}
	// The breathing layer is what makes the blur repaint every frame; a degraded
	// page stops it.
	if !strings.Contains(block, "html.no-glass .page-backdrop { animation: none") {
		t.Error("the degradation chunk must stop the backdrop's breathing")
	}

	// The switch must be ONE class on <html>, because both entrances share these
	// values. If a second selector (a body class, a media query with its own
	// copy) appears, the two entrances drift.
	if strings.Contains(block, "@media (prefers-reduced-transparency") {
		t.Error("the OS preference must be read in JS and land on the same class; a CSS copy is a second set of values")
	}
	if !strings.Contains(block, "html.no-glass {") {
		t.Error("the degradation block must set its opaque tokens on the same html.no-glass selector")
	}

	// Opaque means a surface, not the glass tint: every variable the translucent
	// look is built from has to be re-pointed, or the window keeps a see-through
	// plate (and its contrast) while only the blur is gone.
	//
	// A token must be overridden on every element that DECLARES it, and this walk is
	// what keeps the override list honest rather than a copy of an old selector
	// list that goes stale. It shipped broken once by ignoring the rule: the
	// override lived on :root alone while theme.css re-declared the terminal's
	// glass tokens on `.shell-windows-container` and its siblings, so inside every
	// terminal the root value was shadowed. Verified in Edge — the window reported
	// backdrop-filter: none and still painted its rgba(126,176,232,.08) frost —
	// which no amount of source reading would have shown, because both halves
	// looked right on their own. Custom properties are ordinary inherited
	// declarations, so !important on the root one does not win either (verified:
	// the descendant's own value takes it).
	//
	// Today every one of these tokens is declared on :root only (theme.css and
	// tokens.css), which the html.no-glass rule already covers — html IS that
	// element. The walk below is what fails the moment a chunk declares one of
	// them somewhere else again, so the coverage cannot silently regress to the
	// shipped-broken shape.
	var earlier strings.Builder
	for _, name := range order[:len(order)-1] {
		earlier.WriteString(readAssetLF(t, "static/css/"+name))
	}
	css := earlier.String()
	for _, tok := range []string{"--term-glass-frost:", "--term-glass-tint:", "--term-glass-surface:", "--term-pane-72:"} {
		sites := declarationSites(t, css, tok)
		if len(sites) == 0 {
			t.Fatalf("no declaration site found for %s; the walker is stale", tok)
		}
		var missing []string
		for _, site := range sites {
			for _, one := range strings.Split(site, ",") {
				one = strings.TrimSpace(one)
				if one == "" || one == ":root" {
					continue // html.no-glass is the same element as :root
				}
				if !strings.Contains(block, "html.no-glass "+one) {
					missing = append(missing, one)
				}
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s is declared on %s in the chunks, but the degradation block does not re-declare it there: a custom property resolves on the element that declares it, so that subtree keeps its translucent value", tok, strings.Join(missing, ", "))
		}
	}

	// Each token must be re-pointed to an OPAQUE value. The token name alone is
	// not enough: a value like rgba(..., .08) keeps the plate see-through, which is
	// exactly the state the degraded window must not be in.
	// --overlay-modal is intentionally absent from the list below: the modal scrim
	// is alpha blending, not a backdrop-filter, so it is not part of the paint cost
	// this chunk exists to remove — and a hardcoded scrim colour would break the
	// light theme (it is a near-black in cyberpunk and a soft grey in light, by
	// design). Asserted so the omission is a decision, not a dropped entry.
	if strings.Contains(block, "--overlay-modal:") {
		t.Error("the degradation chunk re-points --overlay-modal; the scrim is not a compositor cost and its per-theme colour must stay the theme's")
	}
	stillTranslucent := func(tok string) bool {
		at := strings.Index(block, tok)
		if at < 0 {
			t.Errorf("the degradation block should re-point %s, or that surface stays translucent", tok)
			return false
		}
		val := block[at+len(tok):]
		if i := strings.IndexAny(val, ";\n"); i >= 0 {
			val = val[:i]
		}
		val = strings.TrimSpace(val)
		// transparent is opaque for the tint layer: it is a wash that must stop
		// stacking, not a surface that has to become solid.
		if val == "transparent" {
			return false
		}
		if strings.HasPrefix(val, "rgba(") {
			last := val[strings.LastIndex(val, ",")+1:]
			last = strings.TrimSuffix(strings.TrimSpace(last), ")")
			if alpha, err := strconv.ParseFloat(last, 64); err == nil && alpha < 0.9 {
				t.Errorf("%s is re-pointed to %q, which is still translucent", tok, val)
			}
		}
		return false
	}
	for _, tok := range []string{
		"--glass-plate:", "--glass-plate-strong:", "--glass-tile:", "--glass-archived:",
		"--term-glass-frost:", "--term-glass-tint:", "--term-glass-surface:",
		"--term-pane-72:", "--bg-header:",
	} {
		stillTranslucent(tok)
	}
}

// The probe runs under node against the real source, because the two branches
// that matter are both reachable only from a browser API: the OS preference and
// the WebGL renderer string. A Go test can assert the source mentions them; only
// running the function says whether the class is actually set.
func TestGlassProbeDegradesOnSoftwareRenderer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the glass probe")
	}
	probe := glassProbeJS(t)

	script := `
const fs = require('fs');
const vm = require('vm');

const src = fs.readFileSync(process.argv[2], 'utf8');
let bad = 0;
function check(name, got, want) {
  if (got !== want) { console.log('FAIL ' + name + ': got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want)); bad++; }
  else console.log('PASS ' + name);
}

// A fake document + matchMedia good enough for the probe: the class list is the
// only observable, and it is what the stylesheet keys off.
function run(reduceTransparency, renderer) {
  const classes = new Set();
  const document = {
    documentElement: {
      classList: {
        toggle(name, on) { if (on) classes.add(name); else classes.delete(name); },
        contains: n => classes.has(n),
      },
    },
    createElement() {
      const gl = {
        getExtension: () => ({ UNMASKED_RENDERER_WEBGL: 0x9246 }),
        getParameter: () => renderer,
      };
      return { getContext: () => gl };
    },
  };
  const window = {
    matchMedia: () => ({ matches: reduceTransparency, addEventListener() {} }),
  };
  const sandbox = { window, document, console };
  vm.createContext(sandbox);
  vm.runInContext(src, sandbox);
  return { opaque: sandbox.termcpSyncGlassMode(), classed: classes.has('no-glass') };
}

// A GPU machine with the OS preference off keeps its glass: the degradation must
// not be the default, or every user pays the plain look for a rare machine.
check('gpu + transparency on keeps glass', run(false, 'ANGLE (Intel, Intel(R) UHD Graphics (0x00009A60) Direct3D11 vs_5_0 ps_5_0, D3D11)').classed, false);
// The OS preference is the official entrance.
check('os preference degrades', run(true, 'ANGLE (Intel, Intel(R) UHD Graphics Direct3D11)').classed, true);
// No GPU: Windows' Basic Render Driver, which is what a machine with no display
// driver (or a remote session on one) reports. This is the case that costs 200ms
// per frame at 2x and the reason the probe exists at all.
const basic = run(false, 'ANGLE (Microsoft, Microsoft Basic Render Driver (0x0000008C) Direct3D11 vs_5_0 ps_5_0, D3D11)');
check('basic render driver degrades', basic.classed, true);
check('basic render driver is reported as opaque', basic.opaque, true);
// The other software rasterizers seen in the wild.
check('swiftshader degrades', run(false, 'Google SwiftShader').classed, true);
check('llvmpipe degrades', run(false, 'Mesa/X.org, llvmpipe (LLVM 15.0.7, 256 bits)').classed, true);
// An unknown renderer keeps the glass: the failure that matters is a slow page,
// not a missing decoration, so the probe must not fire on a string it does not
// recognise.
check('unknown renderer keeps glass', run(false, 'Some Future GPU').classed, false);
// A browser that refuses to hand out a GL context must not crash the boot path.
check('no webgl keeps glass', (() => {
  const classes = new Set();
  const document = {
    documentElement: { classList: { toggle(n, on) { if (on) classes.add(n); else classes.delete(n); }, contains: n => classes.has(n) } },
    createElement: () => ({ getContext: () => null }),
  };
  const window = { matchMedia: () => ({ matches: false, addEventListener() {} }) };
  const sandbox = { window, document, console };
  vm.createContext(sandbox);
  vm.runInContext(src, sandbox);
  return sandbox.termcpSyncGlassMode() === false && !classes.has('no-glass');
})(), true);

if (bad) { console.log(bad + ' FAILED'); process.exit(1); }
console.log('all glass-probe assertions passed');
`
	dir := t.TempDir()
	probePath := filepath.Join(dir, "glass-probe.js")
	if err := os.WriteFile(probePath, []byte(probe), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "probe_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, probePath).CombinedOutput()
	if err != nil {
		t.Fatalf("the glass probe failed under node: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "all glass-probe assertions passed") {
		t.Fatalf("the glass probe did not complete:\n%s", out)
	}
}

// The probe must be APPLIED, not merely defined: a function that is never called
// at boot leaves every slow machine translucent while every other test passes.
func TestGlassProbeRunsAtBoot(t *testing.T) {
	src := readAssetLF(t, "static/js/util.js")
	probe := glassProbeJS(t)
	if !strings.Contains(probe, "termcpSyncGlassMode();") {
		t.Error("util.js defines the glass probe but never calls it at boot")
	}
	// util.js loads before every other app script, so the class is on <html>
	// before the first paint of anything that uses it.
	index := readAssetLF(t, "index.html")
	utilAt := strings.Index(index, `src="static/js/util.js"`)
	if utilAt < 0 {
		t.Fatal("index.html does not load util.js")
	}
	for _, later := range []string{`src="static/js/shell-windows.js"`, `src="static/js/ui-socket.js"`, `src="static/js/terminal-window.js"`} {
		if at := strings.Index(index, later); at >= 0 && at < utilAt {
			t.Errorf("%s loads before util.js, so the glass class is set after that script runs", later)
		}
	}
	// And the class name the probe sets is the one the stylesheet keys off.
	if !strings.Contains(src, "'no-glass'") {
		t.Error("the probe no longer sets the no-glass class the stylesheet consumes")
	}
}
