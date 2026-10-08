package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDroppedWindowIsClampedIntoTheContainer runs the real clamp function from
// shell-windows.js against a stub DOM.
//
// A floating window is dragged by translating the pointer with no bound, so the
// window can be released past the edge of the screen. That is recoverable as
// long as part of the window stays reachable, but a window released fully
// outside has no header left to grab — the user sees the window vanish. The
// clamp is the only thing preventing that, and it is pure geometry: a sign error
// in it would look plausible in review and still hide windows off-screen, which
// is exactly the class of bug a runnable check catches.
func TestDroppedWindowIsClampedIntoTheContainer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the clamp")
	}
	body, err := readAsset("static/js/shell-windows.js")
	if err != nil {
		t.Fatal(err)
	}

	script := `
const fs = require('fs');
const vm = require('vm');

const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function clampShellWindowIntoContainer');
if (start < 0) throw new Error('clampShellWindowIntoContainer not found');
const end = src.indexOf('\n}\n', start) + 3;

// The clamp's floor is the page header's lower edge, so the real measuring
// function is part of this test rather than stubbed: if it ever started
// returning a constant, the clamp would pin windows under the header again and
// no geometry case below would notice.
const hStart = src.indexOf('function appHeaderBottom(');
if (hStart < 0) throw new Error('appHeaderBottom not found');
const hEnd = src.indexOf('\n}\n', hStart) + 3;

const sandbox = {};
vm.createContext(sandbox);
// A header whose depth the test drives. Its bottom is read on every measurement,
// exactly as a live getBoundingClientRect would report a wrapped nav.
sandbox.DOM = { headerBottom: 0 };
vm.runInContext('function isTiledWin(w){return !!(w && w._tiled);}\n' +
  'var document = { querySelector: function (sel) {\n' +
  '  if (sel !== ".app-header") return null;\n' +
  '  return { getBoundingClientRect: function () { return { bottom: DOM.headerBottom }; } };\n' +
  '} };\n' +
  src.slice(hStart, hEnd) + src.slice(start, end), sandbox);

const host = { x: 0, y: 0, w: 1000, h: 800 };
function mkWin(rect) {
  return {
    style: {},
    parentNode: {
      clientWidth: host.w, clientHeight: host.h,
      getBoundingClientRect: () => ({ left: host.x, top: host.y, width: host.w, height: host.h })
    },
    getBoundingClientRect: () => ({ left: rect.x, top: rect.y, width: rect.w, height: rect.h })
  };
}

// A window wider/taller than the container is pinned to the leading edge: the
// title and its close button stay on screen even though the far edge cannot fit.
const cases = [
  ['released past the right edge', { x: 900, y: 100, w: 400, h: 300 },  600, 100],
  ['released past the bottom edge', { x: 100, y: 700, w: 400, h: 300 }, 100, 500],
  ['released past the left edge',   { x: -120, y: 100, w: 400, h: 300 }, 0,  100],
  ['released past the top edge',    { x: 100, y: -90, w: 400, h: 300 }, 100,   0],
  ['already inside is untouched',   { x: 200, y: 150, w: 400, h: 300 }, 200, 150],
  ['exactly at the far corner',     { x: 600, y: 500, w: 400, h: 300 }, 600, 500],
  ['larger than the container',     { x: -500, y: -500, w: 1400, h: 1200 }, 0, 0],
];

let bad = 0;
for (const [name, rect, wantL, wantT] of cases) {
  const win = mkWin(rect);
  sandbox.win = win;
  vm.runInContext('clampShellWindowIntoContainer(win)', sandbox);
  const gotL = win.style.left === undefined ? rect.x : parseFloat(win.style.left);
  const gotT = win.style.top === undefined ? rect.y : parseFloat(win.style.top);
  // An untouched window must not be given an inline position at all: writing the
  // value it already had would drop a CSS-owned position (the touch full-screen
  // rule) for an inline one.
  if (gotL !== wantL || gotT !== wantT) {
    console.log('FAIL ' + name + ': left=' + gotL + ' top=' + gotT + ' want ' + wantL + ',' + wantT);
    bad++;
  }
}

// A tiled pane is positioned by the grid; the clamp must leave it alone.
const tiled = mkWin({ x: -50, y: -50, w: 300, h: 200 });
tiled._tiled = true;
sandbox.tiled = tiled;
vm.runInContext('clampShellWindowIntoContainer(tiled)', sandbox);
if (tiled.style.left !== undefined || tiled.style.top !== undefined) {
  console.log('FAIL a tiled pane was repositioned');
  bad++;
}

// The page header is chrome the user needs (the wordmark, the docs and language
// links). A window released over it must settle BELOW it, even though that is
// further down than the cosmetic margin — the container's top is the viewport
// top, so nothing but the measured header edge stops the window there.
sandbox.DOM.headerBottom = 60;
const belowHeader = [
  ['released over the header',       { x: 100, y: -90, w: 400, h: 300 }, 100,  60],
  ['released into the header band',  { x: 100, y:  20, w: 400, h: 300 }, 100,  60],
  ['clear of the header is untouched', { x: 100, y: 200, w: 400, h: 300 }, 100, 200],
  ['taller than the space below',    { x: -500, y: -500, w: 1400, h: 1200 }, 0, 60],
];
for (const [name, rect, wantL, wantT] of belowHeader) {
  const win = mkWin(rect);
  sandbox.win = win;
  vm.runInContext('clampShellWindowIntoContainer(win)', sandbox);
  const gotL = win.style.left === undefined ? rect.x : parseFloat(win.style.left);
  const gotT = win.style.top === undefined ? rect.y : parseFloat(win.style.top);
  if (gotL !== wantL || gotT !== wantT) {
    console.log('FAIL ' + name + ': left=' + gotL + ' top=' + gotT + ' want ' + wantL + ',' + wantT);
    bad++;
  }
}

// A header that has grown (a wrapped nav, a longer label) moves the floor with
// it: a constant here would leave the tallest header states uncovered.
sandbox.DOM.headerBottom = 120;
const grown = mkWin({ x: 100, y: -90, w: 400, h: 300 });
sandbox.grown = grown;
vm.runInContext('clampShellWindowIntoContainer(grown)', sandbox);
if (parseFloat(grown.style.top) !== 120) {
  console.log('FAIL a wrapped header did not raise the floor: top=' + grown.style.top);
  bad++;
}

console.log(bad === 0 ? 'CLAMP OK' : bad + ' clamp failure(s)');
process.exit(bad === 0 ? 0 : 1);
`
	tmp := filepath.Join(t.TempDir(), "shell-windows.js")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(t.TempDir(), "clamp.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, tmp).CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("clamp check failed: %v\n%s", err, got)
	}
	if !strings.Contains(got, "CLAMP OK") {
		t.Fatalf("harness did not run to completion:\n%s", got)
	}
}

// TestTiledWorkspaceAndDragShareTheHeaderFloor pins the wiring the two node
// harnesses cannot see: which variable names the header floor, and whether the
// height behind it is measured rather than assumed.
//
// The clamp above proves the JS floor moves with the header; the stylesheet's
// inset for the tiled layer is a separate declaration that a copy-paste or a
// rename can leave behind at 0 — and then the tiled grid covers the header again
// while every geometry test still passes. Both sides must name the SAME property,
// and the publisher must read the live header box, since the header's height
// depends on whether its nav wraps.
func TestTiledWorkspaceAndDragShareTheHeaderFloor(t *testing.T) {
	css := readAppCSS(t)
	js := readAssetLF(t, "static/js/shell-windows.js")

	ws := cssRule(t, css, ".pane-workspace")
	if !strings.Contains(ws, "inset: var(--app-header-h, 0px)") {
		t.Errorf("the tiled workspace must start below the measured header; got %q", ws)
	}

	publish := between(t, js, "function syncAppHeaderDepth()", "\n}\n")
	if !strings.Contains(publish, "setProperty('--app-header-h'") {
		t.Errorf("syncAppHeaderDepth no longer publishes --app-header-h; the CSS fallback would pin the workspace to the viewport top: %q", publish)
	}
	if !strings.Contains(publish, "appHeaderBottom()") {
		t.Errorf("the published height must come from the measured header, not a constant: %q", publish)
	}

	measure := between(t, js, "function appHeaderBottom()", "\n}\n")
	if !strings.Contains(measure, ".app-header") || !strings.Contains(measure, "getBoundingClientRect") {
		t.Errorf("appHeaderBottom must measure the live header box; a hardcoded height breaks on a wrapped nav: %q", measure)
	}

	// The published value has to survive the header changing size after first
	// paint (a wrap, a language switch, a late web font), which is why an observer
	// on the header exists rather than a one-shot call.
	if !strings.Contains(js, "new ResizeObserver(syncAppHeaderDepth)") {
		t.Error("nothing republishes --app-header-h when the header changes size; the tiled floor would go stale")
	}
}
