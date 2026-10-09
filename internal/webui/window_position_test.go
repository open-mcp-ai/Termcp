package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTerminalWindowPlacementCentresForTheHostDrawer runs the real placement
// function from shell-windows.js under node.
//
// The rule under test is who names the position, and what "no position" means.
// The host drawer is pinned to the left edge while its click is still being
// handled, so a window placed at that click lands against the screen's left wall
// and under the drawer — the clamp that keeps a window on screen is exactly what
// pinned it there. The drawer's entry points therefore pass null, which must
// mean "centre", not "fall back to a hardcoded corner": the fallback this
// replaced was 80px/60px, which is the same left wall in different digits.
func TestTerminalWindowPlacementCentresForTheHostDrawer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the window placement")
	}
	body := readAssetLF(t, "static/js/shell-windows.js")

	start := strings.Index(body, "function positionShellWindowFromClick(")
	if start < 0 {
		t.Fatal("shell-windows.js no longer defines positionShellWindowFromClick; the placement is untested now")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit positionShellWindowFromClick")
	}
	placement := body[start : start+end+2]

	// The placement's floor is the page header's lower edge, measured by this
	// function. Sliced in rather than stubbed so a constant floor (the regression
	// this guards) fails here instead of passing through a hand-written stub.
	hStart := strings.Index(body, "function appHeaderBottom(")
	if hStart < 0 {
		t.Fatal("shell-windows.js no longer defines appHeaderBottom; the header floor is untested now")
	}
	hEnd := strings.Index(body[hStart:], "\n}\n")
	if hEnd < 0 {
		t.Fatal("could not delimit appHeaderBottom")
	}
	headerBottom := body[hStart : hStart+hEnd+2]

	script := `
const fs = require('fs');
const vm = require('vm');

const src = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = {};
vm.createContext(sandbox);
// The only globals the placement reads: the cascade counter, the viewport, the
// measured header depth, and the two mobile helpers it delegates the touch layout
// to (a full-screen window is the stylesheet's job — the placement only has to
// keep its hands off it).
vm.runInContext('var shellWindowCount = 0; var window = { innerWidth: 1440, innerHeight: 900 };\n' +
  'var DOM = { headerBottom: 0 };\n' +
  'var document = { querySelector: function (sel) {\n' +
  '  if (sel !== ".app-header") return null;\n' +
  '  return { getBoundingClientRect: function () { return { bottom: DOM.headerBottom }; } };\n' +
  '} };\n' +
  'function isMobileViewport() { return false; }\n' +
  'function applyWindowViewportMode(win) { win._fullscreen = true; }\n' + src, sandbox);

const W = 640, H = 480;
const CENTRE_L = Math.round((1440 - W) / 2);
const CENTRE_T = Math.round((900 - H) / 2);
let bad = 0;

function check(name, wantL, wantT) {
  const win = { style: {} };
  sandbox.win = win;
  vm.runInContext('positionShellWindowFromClick(win, ' + sandbox.arg + ')', sandbox);
  const gotL = parseFloat(win.style.left), gotT = parseFloat(win.style.top);
  if (gotL !== wantL || gotT !== wantT) {
    console.log('FAIL ' + name + ': left=' + win.style.left + ' top=' + win.style.top +
                ' want ' + wantL + ',' + wantT);
    bad++;
  } else {
    console.log('PASS ' + name);
  }
}

// A drawer click can be anywhere down the left edge; every one of them resolves
// to null and must land in the middle, which is what makes the window
// independent of the drawer's width and of where in a card the user clicked.
sandbox.shellWindowCount = 1;
sandbox.arg = 'null';
check('centred for the drawer click', CENTRE_L, CENTRE_T);

// A second window cascades from the centre instead of stacking exactly on the
// first — the cascade is what decides the top window at equal z-order.
sandbox.shellWindowCount = 2;
check('centred and cascaded', CENTRE_L + 24, CENTRE_T + 24);

// A session card still names its own point: that click is a real target on a
// surface that is not pinned to an edge, so the pointer placement stays (still
// clamped to the viewport, since the window may be taller than the point).
sandbox.shellWindowCount = 3;
sandbox.arg = '{ x: 1000, y: 500 }';
check('pointer placement honoured', 1000 - W / 2, Math.min(500 - 21, 900 - H - 8));

// A click inside the page header must not place the window over the header. The
// header is where the page's own controls live, and a window whose title bar sits
// there covers them; the floor is the measured header depth, not the 8px margin.
sandbox.DOM.headerBottom = 60;
const headerCases = [
  ['clicked inside the header',      '{ x: 1000, y: 20 }', 1000 - W / 2, 60],
  ['clicked just under the header',  '{ x: 1000, y: 90 }', 1000 - W / 2, 69],
  ['centred with a header',          'null',              CENTRE_L + 48, CENTRE_T + 48],
];
for (const [name, arg, wantL, wantT] of headerCases) {
  sandbox.arg = arg;
  check(name, wantL, wantT);
}
// A header that grew (a wrapped nav) moves the floor with it; a constant would
// leave the tallest header states uncovered.
sandbox.DOM.headerBottom = 120;
sandbox.arg = '{ x: 1000, y: 20 }';
check('a wrapped header raises the floor', 1000 - W / 2, 120);
sandbox.DOM.headerBottom = 0;

// A touch device is the other way a drawer click can end: the window is
// full-screen and CSS owns it, so the placement must not write inline geometry
// (an inline width beats the media query).
sandbox.shellWindowCount = 4;
vm.runInContext('isMobileViewport = function () { return true; };', sandbox);
sandbox.arg = 'null';
const mobileWin = { style: {} };
sandbox.mobileWin = mobileWin;
vm.runInContext('positionShellWindowFromClick(mobileWin, null)', sandbox);
if (!mobileWin._fullscreen || mobileWin.style.width || mobileWin.style.left || mobileWin.style.top) {
  console.log('FAIL a touch window was given inline geometry: ' + JSON.stringify(mobileWin.style));
  bad++;
} else {
  console.log('PASS touch window left to the stylesheet');
}

console.log(bad === 0 ? 'PLACEMENT OK' : bad + ' placement failure(s)');
process.exit(bad === 0 ? 0 : 1);
`
	tmp := t.TempDir()
	placementPath := filepath.Join(tmp, "placement.js")
	if err := os.WriteFile(placementPath, []byte(placement+"\n"+headerBottom), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmp, "placement_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, placementPath).CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("placement check failed: %v\n%s", err, got)
	}
	if !strings.Contains(got, "PLACEMENT OK") {
		t.Fatalf("harness did not run to completion:\n%s", got)
	}
}

// TestHostListEntryPointsLeaveThePlacementToTheDrawer pins the wiring the
// placement test cannot see: which callers name a position.
//
// Every session entry point that lives in or behind the host drawer — the card
// body, the launch-options dialog, the selection mode's open action — hands the
// placement to the centred cascade. Passing a click event again would restore
// the reported bug while every placement unit test above still passed, so all
// call sites are pinned here.
func TestHostListEntryPointsLeaveThePlacementToTheDrawer(t *testing.T) {
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	at := strings.Index(dialogs, "startSessionAndOpenShell(")
	if at < 0 {
		t.Fatal("dialogs.js no longer starts a session from a host card")
	}
	cardCall := dialogs[at : at+strings.Index(dialogs[at:], "\n")]
	if !strings.Contains(cardCall, ", null)") {
		t.Errorf("the host card must open its window centred; got %q", cardCall)
	}

	connForm := readAssetLF(t, "static/js/conn-form.js")
	if strings.Contains(connForm, "_startClickEvt") {
		t.Error("the launch-options dialog captures a click position again; the dialog outlives that click")
	}
	for _, at := range indexOfAll(connForm, "startSessionAndOpenShell(") {
		call := connForm[at : at+strings.Index(connForm[at:], "\n")]
		if !strings.Contains(call, ", null") {
			t.Errorf("every drawer-side entry point must open its window centred; got %q", call)
		}
	}
}

// indexOfAll returns the start offset of every non-overlapping occurrence of
// needle in s.
func indexOfAll(s, needle string) []int {
	var out []int
	for i := 0; i+len(needle) <= len(s); {
		at := strings.Index(s[i:], needle)
		if at < 0 {
			break
		}
		out = append(out, i+at)
		i += at + len(needle)
	}
	return out
}
