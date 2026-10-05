package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBlackwallReactionIsDerivedFromPaintedState runs the real blackwallState()
// from ui-socket.js under node.
//
// The reaction is derived from what is already on screen (the channel chips, the
// approval counts, the link flag) rather than pushed by each writer. That is what
// keeps it honest: a writer that forgets to announce itself cannot leave the page
// in a state the Blackwall disagrees with — it is reading the same DOM the user
// is. The precedence is the rule worth pinning: a lost link outranks everything
// (every terminal is frozen), a queue waiting on a human outranks agent activity
// (the human is the blocker), and an agent's line outranks a lap of ordinary
// typing.
func TestBlackwallReactionIsDerivedFromPaintedState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the Blackwall state")
	}
	body := readAssetLF(t, "static/js/ui-socket.js")
	start := strings.Index(body, "function blackwallState()")
	if start < 0 {
		t.Fatal("ui-socket.js no longer defines blackwallState; the Blackwall cannot react to the workbench")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit blackwallState")
	}
	state := body[start : start+end+2]

	script := `
const fs = require('fs');
const vm = require('vm');

const src = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = {};
vm.createContext(sandbox);
// Only the two globals the derivation reads: the link flag and the counts map.
vm.runInContext('var _uiLinkDown = false; var window = {};', sandbox);
vm.runInContext(src, sandbox);

let bad = 0;
function check(name, want) {
  const got = vm.runInContext('blackwallState()', sandbox);
  if (got !== want) { console.log('FAIL ' + name + ': got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want)); bad++; }
  else console.log('PASS ' + name);
}
// A window carrying one channel state, which is all the derivation looks at.
function setWin(chipState) {
  const ch = {}; if (chipState) ch.statusState = chipState;
  sandbox.win = { _channels: { s1: ch } };
  vm.runInContext('document = { querySelectorAll: function () { return [win]; } };', sandbox);
}

vm.runInContext('document = { querySelectorAll: function () { return []; } };', sandbox);
check('an idle page has no reaction', '');

setWin('typing');
check('typing warms the layer', 'session-active');
setWin('running');
check('a command\'s output warms the layer', 'session-active');
setWin('done');
check('a quiet channel is not activity', '');
setWin('ended');
check('an ended channel is not activity', '');
setWin('ai');
check('an agent outranks a human keystroke', 'ai-active');

// A queue waiting on a human is the one state where the page is blocked on the
// person, so it outranks everything except the broken link itself.
setWin('ai');
vm.runInContext('window._approvalCounts = { other: 2 };', sandbox);
check('a pending review outranks agent activity', 'review-pending');
vm.runInContext('window._approvalCounts = { other: 0 };', sandbox);
check('an empty queue is not pending', 'ai-active');

vm.runInContext('_uiLinkDown = true;', sandbox);
check('a lost link outranks every work state', 'error');
vm.runInContext('_uiLinkDown = false; window._approvalCounts = {};', sandbox);
setWin('');
check('recovering the link returns to idle', '');

console.log(bad === 0 ? 'BLACKWALL OK' : bad + ' state failure(s)');
process.exit(bad === 0 ? 0 : 1);
`
	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "state.js")
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmp, "state_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, statePath).CombinedOutput()
	if err != nil {
		t.Fatalf("blackwall state probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "BLACKWALL OK") {
		t.Fatalf("blackwall state probe did not pass:\n%s", out)
	}
}

// TestConsoleStateIsPaintedOnTheWindow pins the three window-level facts a
// reader needs at a glance and cannot get from the tab bar alone: which window is
// the active one, whether its link is up, and that the state markers are actually
// wired to the window templates rather than only styled.
//
// The active class is asserted on the ONE writer (refreshSessionTabbar) because
// the depth ramp only works if the marker is exclusive: two writers toggling it
// would leave two windows claiming the front slot.
func TestConsoleStateIsPaintedOnTheWindow(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	util := readAssetLF(t, "static/js/util.js")
	tv := readAssetLF(t, "static/js/terminal-view.js")
	css := readAssetLF(t, "static/css/app.css")

	tabbar := between(t, shell, "function refreshSessionTabbar() {", "\n}\n")
	if !strings.Contains(tabbar, "paintSessionTileWindowMarkers()") {
		t.Error("the tab bar refresh no longer mirrors open windows onto the session cards; the grid would not show which session is already open")
	}
	if !strings.Contains(util, "function paintSessionTileWindowMarkers()") {
		t.Error("util.js no longer defines paintSessionTileWindowMarkers")
	}
	// The marker is the tab bar's, and only the tab bar's: it runs on every open,
	// close and raise, which is why the paint hangs off that one function.
	if n := strings.Count(shell, "classList.toggle('win-active'"); n != 1 {
		t.Errorf("win-active has %d writer(s); one exclusive writer is what makes the front window unambiguous", n)
	}
	for _, want := range []string{
		".shell-window.win-active:not(.win-fullscreen) {",
		".shell-link-state.is-connecting {",
		".shell-link-state.is-live {",
		".shell-link-state.is-dead {",
		".conn-tile.sess-tile.has-open-window",
		".sess-plate-empty {",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lost %q; the state has nowhere to show", want)
		}
	}

	// Both window templates carry the marker, and both seed it: a window born
	// from a pending dial is connecting, one restored from a dead session is dead.
	for _, tmpl := range []string{"openPendingShellWindow", "openShellWindow"} {
		if n := strings.Count(tv, `'<span class="shell-link-state"></span>'`); n < 2 {
			t.Fatalf("only %d window template(s) carry the link-state marker; both need it (%s)", n, tmpl)
		}
	}
	for _, want := range []string{
		"function setWindowLinkState(",
		"setWindowLinkState(win, 'is-connecting')",
		"setWindowLinkState(win, 'is-live')",
		"setWindowLinkState(win, readOnly ? 'is-dead' : 'is-live')",
	} {
		if !strings.Contains(tv, want) {
			t.Errorf("terminal-view.js misses %q; the marker would keep its template default forever", want)
		}
	}
	// A session that dies while its window is open must flip the marker too, or a
	// frozen terminal keeps claiming a live link. That transition is owned by
	// setWindowDeadBadge, the one path lockWindowReadonly already goes through.
	sessions := readAssetLF(t, "static/js/sessions.js")
	dead := between(t, sessions, "function setWindowDeadBadge(win) {", "\n}\n")
	if !strings.Contains(dead, "setWindowLinkState(win, 'is-dead')") {
		t.Error("setWindowDeadBadge no longer flips the link marker; a dead window would keep a live lamp")
	}

	// The Blackwall transition is triggered from the states it reads, and from the
	// link's own open/close. Guarded calls, because the status machine is executed
	// on its own by TestChannelStatusRule.
	ui := readAssetLF(t, "static/js/ui-socket.js")
	ts := []struct{ msg, needle string }{
		{"paint on the chip transition", "if (typeof blackwallPaint === 'function') blackwallPaint();"},
		{"clear the lost-link flag when the socket opens", "_uiLinkDown = false;"},
		{"raise it when the socket closes", "_uiLinkDown = true;"},
	}
	for _, c := range ts {
		if !strings.Contains(ui, c.needle) {
			t.Errorf("ui-socket.js does not %s (%q)", c.msg, c.needle)
		}
	}
	if !strings.Contains(readAssetLF(t, "static/js/approval.js"), "blackwallPaint()") {
		t.Error("approval.js no longer repaints the Blackwall; a queued command would not disturb the backdrop")
	}
}

// TestEmptyPlatesSpeakThroughTheCatalog keeps the rowless plates from inventing
// their own copy: the sentence is a catalog entry rendered by sessions.js, so the
// stylesheet must not bake one in and the three languages must stay in lockstep
// (TestCatalogsHaveIdenticalKeys covers the last part).
func TestEmptyPlatesSpeakThroughTheCatalog(t *testing.T) {
	sessions := readAssetLF(t, "static/js/sessions.js")
	css := readAssetLF(t, "static/css/app.css")
	for _, want := range []string{"t('plate.sessions.empty')", "t('plate.archive.empty')", "'sess-plate-empty'"} {
		if !strings.Contains(sessions, want) {
			t.Errorf("sessions.js misses %q; an empty plate would say nothing", want)
		}
	}
	// A sentence in the stylesheet is a sentence no catalog can translate. The
	// pseudo-elements may carry the two decorative chevrons, but no words.
	if strings.Contains(css, "content: 'NO ACTIVE") || strings.Contains(css, "content: 'ARCHIVE CLEAR") {
		t.Error("the empty plate bakes copy into the stylesheet; empty text belongs in the catalogs")
	}
	// Before the first server frame every plate is trivially empty; claiming "no
	// live sessions" then would be a statement about a list the page has not read.
	if !strings.Contains(sessions, "rows.length === 0 && window._lastSessionsSnapshot") {
		t.Error("the empty state must wait for the first snapshot, not announce emptiness on a page that has not loaded yet")
	}

	// The plate's "+" card is gone, and the empty readout is why: with no live
	// sessions the grid showed a dashed creation card AND this line, two answers
	// to the same question — one saying "nothing here", the other offering a
	// second door to the host drawer that NetHub already is. The card's removal
	// has to hold on all four surfaces (markup, stylesheet, its wiring, the
	// re-append that kept it alive across re-renders), so they are pinned one by
	// one: any one of them left behind is a half-deleted control.
	index := readAssetLF(t, "index.html")
	forms := readAssetLF(t, "static/js/conn-form.js")
	for _, gone := range []struct{ file, body, sel string }{
		{"index.html", index, "sess-add-card"},
		{"index.html", index, "id=\"session-add\""},
		{"app.css", css, ".sess-add-card"},
		{"conn-form.js", forms, "getElementById('session-add')"},
		{"sessions.js", sessions, "sess-add-card"},
		{"sessions.js", sessions, "addCard"},
	} {
		if strings.Contains(gone.body, gone.sel) {
			t.Errorf("%s still carries %q; the add card must not come back beside the empty readout", gone.file, gone.sel)
		}
	}
	// NetHub is the remaining door to the hosts, so it keeps its own key and
	// wiring while the card's key could go.
	if !strings.Contains(index, "open-host-drawer") || !strings.Contains(forms, "getElementById('open-host-drawer')") {
		t.Error("NetHub must stay wired: it is the only remaining way to the host list from the sessions plate")
	}
}
