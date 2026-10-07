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

const sandbox = {};
vm.createContext(sandbox);
// isTiledWin lives in the same module; panes are laid out by the grid and must
// never be repositioned by a drag clamp.
vm.runInContext('function isTiledWin(w){return !!(w && w._tiled);}\n' + src.slice(start, end), sandbox);

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
