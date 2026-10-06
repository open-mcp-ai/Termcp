package webui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The session switcher and the session tiles must go through one entry point.
//
// The bug this guards against: each window built its own switcher menu at
// creation time, so two windows could disagree about which sessions exist, and
// the menu was parented inside the window — where an ancestor's backdrop-filter
// turns it into the containing block for position:fixed, so the menu rendered in
// the wrong place (or was clipped away) instead of over the viewport.
//
// The minimize bug is the same family: "-" called closeShellWindow, which
// removes the window from the DOM, so a minimized session could not be listed or
// restored by anything.
func TestSessionSwitcherUsesOneDataSource(t *testing.T) {
	shellWins := readAssetLF(t, "static/js/shell-windows.js")
	termView := readTerminalJS(t)
	sessions := readAssetLF(t, "static/js/sessions.js")
	css := readAppCSS(t)

	// One menu singleton, rebuilt per open, rather than a node per window.
	if !strings.Contains(termView, "var _sessionSwitchMenu = null;") {
		t.Error("terminal-view.js should hold the switcher menu in a module-level singleton")
	}
	// Parented to <body>, not into the window: see the backdrop-filter note.
	if !strings.Contains(termView, "document.body.appendChild(el);") {
		t.Error("the switcher menu must be appended to <body>, not into the window")
	}
	if strings.Contains(termView, "win.appendChild(menu)") {
		t.Error("the switcher menu must not be parented inside the window")
	}
	// Every entry point routes through focusSessionWindow.
	for _, tc := range []struct {
		name, body, want string
	}{
		{"session tile", sessions, "focusSessionWindow("},
		{"switcher item", termView, "focusSessionWindow("},
		{"session tab", shellWins, "focusSessionWindow("},
	} {
		if !strings.Contains(tc.body, tc.want) {
			t.Errorf("%s should call %s", tc.name, tc.want)
		}
	}
	if strings.Contains(sessions, "openOrFocusShellWindow(") {
		t.Error("sessions.js should use focusSessionWindow, not the old alias")
	}
	// The switcher lists live sessions; it must not also editorialise about
	// window state. Picking a minimized session restores it, nothing more.
	if strings.Contains(termView, "win-minimized") {
		t.Error("the switcher should not branch on minimized state")
	}
	if strings.Contains(css, ".shell-switch-dead") {
		t.Error("the ended/min badge is unused: the switcher lists live sessions only")
	}
}

// The switch menu hangs off the monitor glyph in the terminal header, and only
// on touch devices: on desktop the session tab bar already switches sessions, so
// clicking the glyph must do nothing there.
func TestSwitcherHangsOffTheHeaderIconOnTouchOnly(t *testing.T) {
	termView := readTerminalJS(t)
	setup := between(t, termView, "function setupMobileSessionSwitcher", "\n}\n")

	if !strings.Contains(setup, ".shell-header-icon") {
		t.Error("the switcher should bind to the existing header monitor glyph")
	}
	// Every activation path must be gated on touch, or desktop gets a menu it did
	// not ask for (and a tab bar it already has).
	if strings.Count(setup, "isMobileViewport()") < 2 {
		t.Error("both the affordance and the click handler must check isMobileViewport()")
	}

	// The old standalone hamburger must be gone.
	css := readAppCSS(t)
	if strings.Contains(css, ".shell-window-switch-btn") || strings.Contains(termView, "shell-window-switch-btn") {
		t.Error("the standalone switch button should be replaced by the header glyph")
	}

	// And the freed-up slot opens the connections drawer instead.
	if !strings.Contains(setup, "termcpToggleEntriesDrawer") {
		t.Error("a header button should open the entries drawer")
	}
	connForm := readAssetLF(t, "static/js/conn-form.js")
	if !strings.Contains(connForm, "window.termcpToggleEntriesDrawer") {
		t.Error("conn-form.js should expose the drawer toggle for the terminal header")
	}
}

func TestMinimizeKeepsTheWindowAlive(t *testing.T) {
	shellWins := readAssetLF(t, "static/js/shell-windows.js")

	// The "-" button must minimize, not destroy.
	bind := between(t, shellWins, "function bindShellWindowMinButton", "\n}\n")
	if !strings.Contains(bind, "minimizeShellWindow(win)") {
		t.Error(`the "-" button should call minimizeShellWindow`)
	}
	if strings.Contains(bind, "closeShellWindow(win)") {
		t.Error(`the "-" button must not close (destroy) the window: a removed window cannot be listed or restored`)
	}

	// Minimize hides; it does not remove. Restore is the inverse.
	min := between(t, shellWins, "function minimizeShellWindow", "\n}\n")
	if !strings.Contains(min, "win.classList.add('win-minimized')") {
		t.Error("minimizeShellWindow should add .win-minimized")
	}
	if strings.Contains(min, ".remove()") {
		t.Error("minimizeShellWindow must not remove the node from the DOM")
	}
	restore := between(t, shellWins, "function restoreShellWindow", "\n}\n")
	if !strings.Contains(restore, "win.classList.remove('win-minimized')") {
		t.Error("restoreShellWindow should drop .win-minimized")
	}

	// A minimized window is not on screen, so it must not win "which tab is
	// active" nor be counted as a visible pane.
	refresh := between(t, shellWins, "function refreshSessionTabbar", "\n  // Reuse existing")
	if !strings.Contains(refresh, "win-minimized") {
		t.Error("refreshSessionTabbar should skip minimized windows when picking the active tab")
	}
	grid := between(t, shellWins, "function applyPaneGrid", "\n  var n = wins.length")
	if !strings.Contains(grid, "win-minimized") {
		t.Error("applyPaneGrid should not count minimized panes as grid cells")
	}
}

func TestFocusSessionWindowNormalisesState(t *testing.T) {
	shellWins := readAssetLF(t, "static/js/shell-windows.js")
	body := between(t, shellWins, "function focusSessionWindow", "\n}\n")

	// Picking a session has to undo whatever state the window was left in.
	for _, want := range []string{"restoreShellWindow(ex)", "expandShellWindow(ex)", "bringShellWindowToFront(ex)"} {
		if !strings.Contains(body, want) {
			t.Errorf("focusSessionWindow should call %s", want)
		}
	}
	// An unknown session still opens a window.
	if !strings.Contains(body, "openShellWindow(") {
		t.Error("focusSessionWindow should open a window when none exists for that session")
	}
}

func TestMinimizedCSSCannotBeConfusedWithHidden(t *testing.T) {
	css := readAppCSS(t)
	if !strings.Contains(css, ".shell-window.win-minimized") {
		t.Error("css should define .shell-window.win-minimized")
	}
	// Both must exist as separate rules: .win-hidden is the tab bar's single
	// global flag, .win-minimized is per-window state.
	if !strings.Contains(css, ".shell-window.win-hidden") || !strings.Contains(css, ".shell-window.win-minimized") {
		t.Error(".win-hidden and .win-minimized must stay separate rules")
	}
}

// Full-screen windows all share the stylesheet's z-index, so raising one needs an
// inline value. It must be handed over rather than incremented: a counter
// climbing past the drawer's 24000 would put terminals over the entries drawer.
func TestFullscreenZIndexIsHandedOverNotIncremented(t *testing.T) {
	util := readAssetLF(t, "static/js/util.js")
	body := between(t, util, "function bringShellWindowToFront", "\n}\n")

	for _, want := range []string{"FULLSCREEN_Z", "win-fullscreen"} {
		if !strings.Contains(body, want) {
			t.Errorf("bringShellWindowToFront should reference %s", want)
		}
	}
	if !strings.Contains(util, "var FULLSCREEN_Z =") {
		t.Error("the full-screen z-index should be a single named constant")
	}
	// No increment-and-store counter for the full-screen path.
	if strings.Contains(body, "_shellFullZSeq") {
		t.Error("the full-screen z-index must not be an incrementing counter")
	}

	// The constant has to sit between the tab bar and the drawer, or the
	// terminal hides the drawer (or sinks behind the tab bar).
	css := readAppCSS(t)
	full := between(t, css, ".shell-window.win-fullscreen {", "}")
	if !strings.Contains(full, "z-index: 21000") {
		t.Errorf("the stylesheet full-screen z-index should be 21000; got %q", strings.TrimSpace(full))
	}
	tabbar := between(t, css, ".session-tabbar {", "}")
	if !strings.Contains(tabbar, "z-index: 20000") {
		t.Error("the tab bar should stay below the full-screen window")
	}
}

// The header controls must be usable while a connection is still being
// established. A slow or failing dial is exactly when a user wants to switch to
// another session or reach another profile, so the pending window needs the same
// wiring as a connected one.
func TestPendingWindowHasWorkingHeaderControls(t *testing.T) {
	termView := readTerminalJS(t)
	pending := between(t, termView, "function openPendingShellWindow", "\n}\n")

	for _, want := range []string{
		"setupMobileSessionSwitcher(win)",
		"bindShellWindowMinButton(win, minBtn)",
		"setupShellWindowDrag(win, header)",
	} {
		if !strings.Contains(pending, want) {
			t.Errorf("the connecting window should call %s", want)
		}
	}

	// Calling setup twice (pending, then again on finalize) must not double-bind
	// or insert a second drawer button.
	setup := between(t, termView, "function setupMobileSessionSwitcher", "\n}\n")
	guard := "if (!win || win._termcpSwitcherBound) return;"
	if !strings.Contains(setup, guard) {
		t.Error("setupMobileSessionSwitcher must stay idempotent: finalize calls it again")
	}
	if i := strings.Index(setup, guard); i >= 0 {
		if j := strings.Index(setup, "insertBefore"); j >= 0 && j < i {
			t.Error("the idempotence guard must come before any DOM insertion")
		}
	}
}

// readAssetLF reads one embedded asset with line endings normalised to LF.
//
// The asset files are LF in git, but a checkout on Windows (CI runs there with
// core.autocrlf=true) hands them back as CRLF. Any test that slices a function
// body by a "\n}\n" terminator would then never find it and silently get the
// rest of the file instead — a false pass, or a confusing failure. Normalising
// once here keeps the tests reading the same text on every platform.
func readAssetLF(t *testing.T, p string) string {
	t.Helper()
	s, err := readAsset(p)
	if err != nil {
		t.Fatal(err)
	}
	return normalizeNL(s)
}

// TestNetHubAttributesHostsByProfileNotDisplayName runs the real attribution
// predicate and the real state derivation from dialogs.js under node.
//
// Two bugs live here. The first: the card derived its state by comparing a
// session's NAME to the profile name, so a renamed session — or one started by
// an agent or another tab with its own name — matched nothing and the host read
// "offline" while a live session was running on it. Only the server-stamped
// profile is allowed to decide, and a session that carries none (a record
// written before the field existed) falls back to the old spellings.
//
// The second: a profile with no session was called "offline", which claims the
// machine is down on the strength of a connection Termcp never attempted. The
// two absences are now separate states — "idle" (never dialled, unknown) and
// "offline" (a session was held here and none is running now) — and this pins
// the boundary: a RETAINED dead session is what makes the word "offline"
// earned.
func TestNetHubAttributesHostsByProfileNotDisplayName(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the host attribution")
	}
	body := readAssetLF(t, "static/js/dialogs.js")
	// sessionBelongsToNode, runningSessionsForNode and nodeState are contiguous:
	// the slice runs from the first to the comment that opens the repaint helper.
	state := between(t, body, "function sessionBelongsToNode(", "\n/** Repaint the node cards")

	script := `
const fs = require('fs');
const vm = require('vm');

const src = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = {};
vm.createContext(sandbox);
vm.runInContext('var window = {};\nfunction allShellWins() { return window.wins || []; }\n' + src, sandbox);

let bad = 0;
function check(name, got, want) {
  if (got !== want) { console.log('FAIL ' + name + ': got ' + got + ' want ' + want); bad++; }
  else console.log('PASS ' + name);
}

function belongs(sess, node, wins) {
  sandbox.sess = sess; sandbox.node = node;
  sandbox.wins = wins || [];
  return vm.runInContext('sessionBelongsToNode(sess, node, wins)', sandbox);
}

// The profile decides, whatever the session is called.
check('renamed session still belongs',
  belongs({ id: 'a', name: 'my-scratch-box', ssh_config: 'prod' }, 'prod'), true);
check('another host is not mine',
  belongs({ id: 'a', name: 'prod', ssh_config: 'staging' }, 'prod'), false);

// A pre-field record keeps the old evidence: its name, or a window that names
// the profile it was opened from.
check('legacy record falls back to its name',
  belongs({ id: 'b', name: 'prod' }, 'prod'), true);
check('legacy record matches an open window',
  belongs({ id: 'c', name: 'session-c' }, 'prod', [{ _connName: 'prod', _sid: 'c' }]), true);
check('a window for another session does not match',
  belongs({ id: 'd', name: 'session-d' }, 'prod', [{ _connName: 'prod', _sid: 'someone-else' }]), false);
check('a stamped session ignores a foreign window',
  belongs({ id: 'e', name: 'session-e', ssh_config: 'staging' }, 'prod', [{ _connName: 'prod', _sid: 'e' }]), false);

function stateFor(sessions, wins) {
  sandbox.conns = [{ name: 'prod' }];
  sandbox.sessions = sessions;
  sandbox.wins = wins || [];
  vm.runInContext('window._lastConnections = conns; window._lastSessionsSnapshot = sessions; window.wins = wins;', sandbox);
  return vm.runInContext('nodeState(\'prod\')', sandbox);
}
function prime(connections, sessions) {
  sandbox.connections = connections; sandbox.sessions = sessions;
  vm.runInContext('window._lastConnections = connections; window._lastSessionsSnapshot = sessions; window.wins = [];', sandbox);
}

prime(null, null);
check('before either list arrives the readout is reading',
  vm.runInContext('nodeState(\'prod\')', sandbox), 'loading');

// Never dialled: no evidence either way — NOT a claim that the machine is down.
prime([{ name: 'prod' }], []);
check('a profile with no session at all is not called offline', stateFor([], []), 'idle');

// A retained DEAD session is evidence Termcp did reach it and no longer does.
prime([{ name: 'prod' }], [{ id: 'z', name: 'anything', ssh_config: 'prod', status: 'exited' }]);
check('a host Termcp has connected to before reads disconnected', stateFor([{ id: 'z', name: 'anything', ssh_config: 'prod', status: 'exited' }], []), 'offline');

// Live work, and the dial that precedes it, still win.
const live = [{ id: 'y', name: 'renamed-live', ssh_config: 'prod', status: 'running' }];
prime([{ name: 'prod' }], live);
check('a running session is online', stateFor(live, []), 'online');
check('an open dial is connecting',
  stateFor([], [{ _placeholder: true, _pendingConnName: 'prod' }]), 'connecting');

// A session on another host must not colour this card either way.
prime([{ name: 'prod' }], [{ id: 'w', name: 'prod', ssh_config: 'staging', status: 'exited' }]);
check('another host\'s history does not make this one disconnected',
  stateFor([{ id: 'w', name: 'prod', ssh_config: 'staging', status: 'exited' }], []), 'idle');

console.log(bad === 0 ? 'ATTRIBUTION OK' : bad + ' attribution failure(s)');
process.exit(bad === 0 ? 0 : 1);
`
	tmp := t.TempDir()
	predicatePath := filepath.Join(tmp, "predicate.js")
	if err := os.WriteFile(predicatePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmp, "predicate_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, predicatePath).CombinedOutput()
	if err != nil {
		t.Fatalf("host attribution probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ATTRIBUTION OK") {
		t.Fatalf("host attribution probe did not pass:\n%s", out)
	}
}

// between returns the slice of src from the marker up to (but not including) end.
// It fails the test when either is absent: a missing terminator used to return
// the whole remainder, which quietly widened the slice and made assertions test
// code outside the function they named.
func between(t *testing.T, src, marker, end string) string {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	rest := src[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("terminator %q not found after marker %q", end, marker)
	}
	return rest[:j]
}

// The load banner shows a dismiss button, so it has to be a flex row; and the
// inline display value has to agree with the stylesheet, or the button lands
// under the message instead of beside it.
//
// setLoadBanner is the only writer of that inline value, so the visible state
// belongs there. A stylesheet-only `display:flex` loses to the inline
// declaration, and a rule that keys off the inline string breaks the moment the
// string changes — assert the two agree instead of pinning either spelling.
func TestLoadBannerShowsDismissButtonInAFlexRow(t *testing.T) {
	util := readAssetLF(t, "static/js/util.js")
	css := readAppCSS(t)

	banner := between(t, util, "function setLoadBanner", "\n}\n")
	if !strings.Contains(banner, "conn-load-banner-close") {
		t.Error("setLoadBanner should add the dismiss button: it is the only path every caller shares")
	}

	// The display value setLoadBanner writes must be the flex one the stylesheet
	// expects, otherwise the row collapses and the button wraps to its own line.
	if !strings.Contains(banner, "style.display = msg ? 'flex' : 'none';") {
		t.Error("the visible banner must be laid out with display:flex, not block")
	}
	base := between(t, css, ".conn-load-banner {", "}")
	if !strings.Contains(base, "display: none") || !strings.Contains(base, "align-items: flex-start") {
		t.Errorf("the banner should default to hidden and be a flex row when shown; got %q", base)
	}

	// Neither banner may live inside a section body. Both sections can be collapsed
	// (a state persisted in localStorage, so it can stay collapsed forever), which
	// is display:none — and on touch the entries body is additionally a fixed
	// drawer that slides off-screen. A "Connection failed" or a dropped-WebSocket
	// notice written into either one would be hidden exactly when it matters.
	//
	// Bounds are the next element, not "</div>": each section body's own closing
	// tag comes after nested divs, so a "</div>" bound stops inside #conn-grid and
	// the assertion would only ever see empty markup.
	index := readAssetLF(t, "index.html")
	for _, tc := range []struct{ name, banner, from, to string }{
		{"connections", "conn-load-banner", `id="sec-entries-body"`, `id="sec-sessions"`},
		{"sessions", "session-load-banner", `id="sec-sessions-body"`, `id="modal-forward"`},
	} {
		if !strings.Contains(index, `id="`+tc.banner+`"`) {
			t.Fatalf("index.html should still contain the %s banner", tc.name)
		}
		section := between(t, index, tc.from, tc.to)
		if strings.Contains(section, tc.banner) {
			t.Errorf("the %s banner must not sit inside its section body: a collapsed section hides it", tc.name)
		}
	}
	// And both must be in the page column, ahead of the sections they report on.
	dock := between(t, index, `class="dock"`, `id="modal-forward"`)
	for _, banner := range []string{`id="conn-load-banner"`, `id="session-load-banner"`} {
		if !strings.Contains(dock, banner) {
			t.Errorf("%s belongs to the page column, not a section body", banner)
		}
	}
}

// The z-index ladder: a surface that can open a dialog must sit below it.
//
// The edit-connection dialog is reachable from the entries drawer and from the
// full-screen terminal's hamburger, so a modal below either one opens *behind*
// it. Nothing errors when that happens — the class is removed and the dialog is
// briefly in the DOM and invisible — so it is pinned here instead.
func TestModalsOutrankEverySurfaceThatOpensThem(t *testing.T) {
	css := readAppCSS(t)
	z := func(sel string) int {
		t.Helper()
		// A selector can appear in several rules (e.g. .drawer-scrim is first
		// declared as display:none), so take the declaration that sets z-index.
		re := regexp.MustCompile(regexp.QuoteMeta(sel) + `\s*\{[^}]*z-index:\s*(\d+)`)
		m := re.FindStringSubmatch(css)
		if m == nil {
			t.Fatalf("%s declares no z-index", sel)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s has an unparsable z-index: %v", sel, err)
		}
		return n
	}

	modal := z(".modal-backdrop")
	for _, below := range []string{
		"#sec-entries-body",            // the drawer
		".drawer-scrim",                // its scrim
		".shell-window.win-fullscreen", // the terminal that hosts the hamburger
		".session-tabbar",              // the tab bar
	} {
		if got := z(below); got >= modal {
			t.Errorf("%s is z-index %d, at or above the modal's %d: a dialog opened from it renders behind it", below, got, modal)
		}
	}
	// Toasts stay on top, and so does the notify stack: a server-pushed
	// notify_user message must not be swallowed by a dialog. The modal was
	// already below both at 20000, so this pins the existing precedence.
	for _, above := range []string{"#ui-copy-toast", "#ui-notify-stack"} {
		if got := z(above); got <= modal {
			t.Errorf("%s is z-index %d, below the modal's %d", above, got, modal)
		}
	}
}

// NetHub is the resource column, not a desktop drawer. The desktop frame has
// one flexible workspace track and one explicit sidebar length; the only drawer
// selectors are in the narrow media query and the JS keeps that breakpoint in
// step with its own layout decision.
func TestNetHubIsAResponsiveResourceSidebar(t *testing.T) {
	css := readAppCSS(t)
	index := readAssetLF(t, "index.html")
	forms := readAssetLF(t, "static/js/conn-form.js")
	dialogs := readAssetLF(t, "static/js/dialogs.js")

	if !strings.Contains(index, `<aside class="nethub" id="nethub"`) {
		t.Fatal("index.html should place NetHub in a persistent aside")
	}
	if !strings.Contains(index, `id="nethub-rail"`) {
		t.Fatal("the collapsed state needs a visible rail mount")
	}
	if !strings.Contains(index, `id="conn-grid"`) || !strings.Contains(index, `id="conn-add"`) {
		t.Fatal("the sidebar must retain its node list and add action")
	}
	frame := between(t, css, ".workspace-frame {", "}")
	for _, want := range []string{
		"display: grid",
		"grid-template-columns: var(--nethub-w) minmax(0, 1fr)",
		"transition: grid-template-columns",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("desktop NetHub layout must contain %q; got %q", want, frame)
		}
	}
	if strings.Contains(frame, "position: fixed") || strings.Contains(frame, "50vw") {
		t.Errorf("the desktop frame must stay in layout, not escape into a drawer: %q", frame)
	}
	for _, want := range []string{
		".workspace-frame.nethub-collapsed { --nethub-w: 56px; }",
		"@media (max-width: 799px)",
		"#sec-entries-body.drawer-open { transform: translateX(0); }",
		".drawer-scrim { display: block; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("responsive NetHub layout must contain %q", want)
		}
	}
	if !strings.Contains(dialogs, "localStorage.getItem(NETHUB_STATE_KEY)") ||
		!strings.Contains(dialogs, "localStorage.setItem(NETHUB_STATE_KEY") {
		t.Error("manual NetHub state should persist through the existing localStorage mechanism")
	}
	if !strings.Contains(forms, "toggleNetHub()") || !strings.Contains(forms, "window.termcpToggleEntriesDrawer") {
		t.Error("the trigger and terminal header should use the shared NetHub state")
	}
	if strings.Contains(forms, "transform: translateX") || strings.Contains(forms, "document.body.style.overflow = open") {
		t.Error("conn-form.js must not own a desktop drawer implementation")
	}
}

// The sidebar is an access resource readout: cards carry identity, protocol,
// runtime state, address metadata and session count, while the rail keeps state
// dots and the active-node mark when the body is collapsed.
func TestNetHubCardsKeepNodeAndSessionHierarchy(t *testing.T) {
	css := readAppCSS(t)
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	for _, want := range []string{
		"node-lamp", "node-proto", "node-status", "node-addr", "node-sess",
		"nethub-rail-status", "nethub-rail-active", "nethub-rail-expand",
	} {
		if !strings.Contains(css, want) || !strings.Contains(dialogs, want) {
			t.Errorf("NetHub should paint %q in both the resource card and rail", want)
		}
	}
	for _, want := range []string{
		"runningSessionsForNode", "nodeSessionInfo", "nethub.sessions.one",
		"nethub.sessions.other", "nodeProtocol(c)", "nodeStatusLabel(state)",
	} {
		if !strings.Contains(dialogs, want) {
			t.Errorf("NetHub renderer misses %q", want)
		}
	}
	if !strings.Contains(dialogs, "status !== 'running'") {
		t.Error("session counts must distinguish active sessions from archived records")
	}
}

// The three visual regressions that are easiest to miss in source review are
// pinned here at the selector/wiring level: the rail owns no second cluster icon,
// the narrow drawer's ancestor cannot create the low stacking context that put it
// under the scrim, and node cards share the host-card rail and corner instead of
// being excluded from them.
func TestNetHubRegressionFixes(t *testing.T) {
	css := readAppCSS(t)
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	ui := readAssetLF(t, "static/js/ui-socket.js")

	if strings.Contains(dialogs, "rail.appendChild(glyph)") || strings.Contains(dialogs, "glyph.innerHTML = '<img class=\"nethub-icon\"") {
		t.Error("the rail must not append a second NetHub cluster glyph; the trigger already owns it")
	}
	if !strings.Contains(dialogs, "item.className = 'nethub-rail-item'") ||
		!strings.Contains(dialogs, "class=\"nethub-rail-status is-") {
		t.Error("the collapsed rail must still render node items and their status lamps")
	}
	if !strings.Contains(css, ".nethub { position: relative; top: auto; z-index: auto; margin-bottom: 10px; }") {
		t.Error("the narrow breakpoint must remove .nethub's stacking context so the drawer can outrank the scrim")
	}
	if !strings.Contains(css, "#sec-entries-body {\n    position: fixed") ||
		!strings.Contains(css, "z-index: 24000") || !strings.Contains(css, "#sec-entries-body.drawer-open { transform: translateX(0); }") {
		t.Error("the narrow drawer must be fixed, high in the root stack, and open through its transform")
	}
	// A node card is a host card with a status readout on it, not a different plate:
	// it takes the state rail, the lit left gradient and the notched corner from the
	// shared rules. Excluding it — with a :not(.node-card) variant, or by restating
	// padding/background on a node-card rule — is exactly what shaved its left edge
	// and erased the rail, so both spellings of that mistake are pinned here.
	if strings.Contains(css, "not(.node-card)") {
		t.Error("node cards must share the host-card rules; a :not(.node-card) exclusion drops their left rail and corner")
	}
	for _, rule := range []string{
		".conn-tile.entry-card {",
		".workspace-sessions, .modal, .conn-tile.entry-card, .drawer-add,",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("the shared host-card/notch rule %q must name the whole entry card, node cards included", rule)
		}
	}
	if strings.Contains(css, ".conn-tile.node-card {") {
		t.Error(".conn-tile.node-card must not be a rule of its own: restating padding/background " +
			"overrides the host-card rail inset and the lit gradient it inherits")
	}
	if !strings.Contains(css, ".conn-tile.node-card .") {
		t.Error("the node card's own treatment belongs in descendant rules, which leave the plate alone")
	}
	if !strings.Contains(ui, "window._lastConnections = j.connections") {
		t.Error("a session WebSocket frame carrying connections must refresh the stored NetHub list")
	}
}

// TestNetHubSidebarKeepsThePageFromScrolling guards the bug where the WHOLE body
// scrolled: the resource panel capped itself at `calc(100dvh - 134px)`, a hand-count
// of the chrome above and below it (header 57 + dock padding 16 + gap 8 + trigger 50
// + dock bottom padding 16). Those sum to 147, not 134, so the panel was permanently
// 13px too tall and the document grew a scrollbar of its own — however few nodes the
// list held. That it was a CONSTANT error and not a content one is what made it hard
// to see: 30 nodes and 60 nodes overflowed by exactly the same 13px (measured in
// headless Chrome), and the page scrolled with an EMPTY sidebar too.
//
// The number cannot be re-derived from the stylesheet alone, so what is pinned here is
// its shape: the ceiling is a viewport minus a single chrome constant, and the same
// constant serves both the panel and the rail that replaces it collapsed. Two
// different numbers is exactly how the two drifted apart.
func TestNetHubSidebarKeepsThePageFromScrolling(t *testing.T) {
	css := readAppCSS(t)

	// The chrome the panel must reserve: everything between the viewport top and the
	// panel, plus what sits below it. Each term is pinned to the rule that owns it, so
	// changing one of them fails here instead of silently reintroducing the scrollbar.
	header := between(t, css, ".app-header {", "}")
	if !strings.Contains(header, "padding: 12px 16px") {
		t.Error("the header's padding changed; the chrome above the dock is 57px (48 + two 1px borders), " +
			"so the sidebar ceiling in workspace.css must be recounted")
	}
	if !strings.Contains(header, "border-bottom: 1px solid") {
		t.Error("the header's bottom border is part of its height; the chrome constant counts it")
	}
	dock := between(t, css, ".dock { padding: 16px 0;", "}")
	if dock == "" {
		t.Error("the dock no longer declares `padding: 16px 0`; the chrome constant counts that padding")
	}
	btn := between(t, css, ".nethub-btn {", "}")
	if !strings.Contains(btn, "flex: 0 0 50px") {
		t.Errorf("the trigger's height changed; the chrome constant counts it: %q", btn)
	}
	body := between(t, css, ".nethub-body {", "}")
	if !strings.Contains(body, "margin-top: 8px") {
		t.Errorf("the gap above the panel changed; the chrome constant counts it: %q", body)
	}

	// Chrome = header 57 + dock top padding 16 + (panel gap 8 + trigger 50) + dock
	// bottom padding 16. The panel itself sits below the trigger, so it reserves the
	// header, both dock paddings and the trigger's own row.
	const chrome = 147
	for _, m := range []struct{ name, rule string }{
		{"the panel", body},
		{"the collapsed rail", between(t, css, ".workspace-frame.nethub-collapsed .nethub-rail {", "}")},
	} {
		want := fmt.Sprintf("calc(100dvh - %dpx)", chrome)
		if !strings.Contains(m.rule, want) {
			t.Errorf("%s must cap itself at %q so it cannot push the page past the viewport; got %q",
				m.name, want, m.rule)
		}
	}

	// One constant, two consumers. A second literal is how 134 and 147 coexisted.
	if n := strings.Count(css, fmt.Sprintf("calc(100dvh - %dpx)", chrome)); n != 2 {
		t.Errorf("expected the panel and the rail to share ONE chrome constant; found %d uses", n)
	}
	if strings.Contains(css, "100dvh - 134px") {
		t.Error("134px is the old hand-count; the chrome is 147px (header 57 + dock padding 32 + gap 8 + trigger 50)")
	}

	// The dock's min-height used to subtract a guessed 52px header from a percentage
	// that resolved to auto anyway, so it never applied — dead arithmetic that looked
	// like it held the dock to the viewport while the panel's ceiling did the work.
	if strings.Contains(css, "min-height: calc(100% - 52px)") {
		t.Error("the dock's min-height subtracted a guessed header height and never applied; " +
			"the sidebar's viewport ceiling is what keeps the page from scrolling")
	}
}

// Sessions and the archive are translucent, and the plate is the ONE element that
// blurs. Every card composites over that already-blurred backdrop, so a blur per
// card would only add work: a 22-session list would pay 22 filters for the same
// pixels.
func TestWorkspaceSurfacesAreTranslucent(t *testing.T) {
	css := readAppCSS(t)

	plate := between(t, css, ".workspace-sessions {", "}")
	if !strings.Contains(plate, "background-color: var(--glass-plate)") {
		t.Errorf("the sessions plate should be translucent; got %q", plate)
	}
	// background-color, never the shorthand: the CP2077 corner is painted with
	// background-image layers declared at the end of the file, and `background:`
	// resets them, so the plate would silently lose its drawn edges.
	if strings.Contains(plate, "background:") {
		t.Errorf("the plate must not use the background shorthand, which resets the corner strokes: %q", plate)
	}
	if !strings.Contains(plate, "backdrop-filter: blur(var(--glass-blur))") {
		t.Errorf("the plate should be the element that blurs the backdrop; got %q", plate)
	}

	// Every glass value carries an alpha. An opaque one is the bug this test
	// exists for: it looks like the others in the stylesheet and paints a slab.
	for _, token := range []string{"--glass-plate:", "--glass-plate-strong:", "--glass-tile:", "--glass-archived:"} {
		i := strings.Index(css, token)
		if i < 0 {
			t.Fatalf("app.css declares no %s", token)
		}
		line := css[i : i+strings.Index(css[i:], ";")]
		if !strings.Contains(line, "rgba(") {
			t.Errorf("%s is not translucent (%q); a glass surface needs an alpha", token, line)
		}
	}

	// Cards read from tokens too, so retuning the glass moves the plate and the
	// cards on it together instead of leaving half the surface opaque.
	tile := between(t, css, ".conn-tile.sess-tile {\n  padding:", "}")
	if !strings.Contains(tile, "background-color: var(--glass-tile)") {
		t.Errorf("the session tile should paint the translucent card token; got %q", tile)
	}
	if strings.Contains(tile, "var(--bg-card)") {
		t.Errorf("the session tile still paints an opaque surface: %q", tile)
	}
	if !strings.Contains(css, ".conn-tile.sess-tile.archived { background-color: var(--glass-archived)") {
		t.Error("an archived card should stay translucent while reading as the dimmer one")
	}
}

// The backdrop is the drop-in slot for the Blackwall artwork, so it has to be a
// layer the page never lays out and never hit-tests — and it has to be reachable
// by replacing one file, with the stylesheet pointing at it through a variable
// rather than growing an effect of its own.
func TestBackdropIsAFixedDropInLayer(t *testing.T) {
	css := readAppCSS(t)
	index := readAssetLF(t, "index.html")

	if !strings.Contains(index, `class="page-backdrop"`) {
		t.Fatal("index.html should carry the backdrop layer")
	}
	rule := between(t, css, ".page-backdrop {", "}")
	for _, want := range []string{"position: fixed", "inset: 0", "pointer-events: none", "background: var(--blackwall-image)"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the backdrop should be %q; got %q", want, rule)
		}
	}
	// z-index: -1 is what puts it BEHIND the page. A positioned element at auto
	// paints with the z-index:0 layer, above static content, so dropping this one
	// declaration puts the artwork on top of the app.
	if !strings.Contains(rule, "z-index: -1") {
		t.Errorf("the backdrop needs z-index: -1 to stay behind the page; got %q", rule)
	}

	// The artwork lives on body's layer, not the root's: body must not paint its
	// own box background over the negative layer, so the canvas colour moves to html.
	if !strings.Contains(css, "html { background: var(--bg-canvas); }") {
		t.Error("the canvas colour belongs to html, or body's background paints over the backdrop")
	}

	if !strings.Contains(css, "--blackwall-image: url(") {
		t.Fatal("app.css declares no --blackwall-image, so the artwork has no slot")
	}
	// The file the variable names must exist and be served, or the layer is empty
	// and the failure is invisible over an already-dark canvas.
	m := regexp.MustCompile(`--blackwall-image: url\(\.\./\.\./([^)]+)\)`).FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("--blackwall-image is not pointed at an app asset: %q", css[strings.Index(css, "--blackwall-image"):])
	}
	rr := httptest.NewRecorder()
	embeddedStaticServer().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/"+m[1], nil))
	if rr.Code != http.StatusOK {
		t.Errorf("the backdrop artwork %s is not served (GET /%s = %d)", m[1], m[1], rr.Code)
	}

	// The breathing backdrop is deliberately the CHEAPEST possible animation: one
	// opacity animation on this single fixed layer. A full animated glitch layer
	// (SVG turbulence, clip-path tearing, blend modes) was measured against a
	// streaming terminal and cost frames the terminal itself needs, so this pins
	// the layer to opacity only — anything that filters, blends, clips or repaints
	// the artwork behind every terminal frame has to fail here instead of shipping.
	if !strings.Contains(rule, "animation: blackwallBreathe") {
		t.Errorf("the backdrop should breathe through its own animation; got %q", rule)
	}
	frames := between(t, css, "@keyframes blackwallBreathe {", "\n  }\n")
	if !strings.Contains(frames, "opacity:") {
		t.Errorf("blackwallBreathe should animate opacity; got %q", frames)
	}
	// The swing has to be wide enough to SEE. A keyframe that only nudges opacity
	// (e.g. .82 -> 1 over this dark artwork) is present in the cascade and still
	// reads as a static background, which is exactly the bug this guards: the
	// effect has to move a meaningful share of the layer's luminance.
	low := regexp.MustCompile(`opacity:\s*\.(\d+)`).FindStringSubmatch(frames)
	if low == nil {
		t.Fatalf("blackwallBreathe has no low opacity value: %q", frames)
	}
	lowVal, err := strconv.ParseFloat("0."+low[1], 64)
	if err != nil {
		t.Fatalf("cannot read the low opacity %q: %v", low[0], err)
	}
	if lowVal > 0.6 {
		t.Errorf("blackwallBreathe only dips to %.2f; over this dark artwork that is invisible — use a wider swing", lowVal)
	}
	if !strings.Contains(frames, "opacity: 1") {
		t.Errorf("blackwallBreathe should reach full opacity at its peak; got %q", frames)
	}
	for _, heavy := range []string{"filter:", "mix-blend-mode", "clip-path", "transform:"} {
		if strings.Contains(frames, heavy) {
			t.Errorf("blackwallBreathe animates %s, which forces extra work per frame; only opacity stays composited", heavy)
		}
	}
	// A reduced-motion user gets the artwork still, at its brighter value: an
	// opacity animation is exactly what that preference asks to be spared.
	reduced := between(t, css, "@media (prefers-reduced-motion: reduce) {", "\n}")
	if !strings.Contains(reduced, ".page-backdrop { animation: none") {
		t.Errorf("the backdrop's breathing is not disabled under prefers-reduced-motion; got %q", reduced)
	}
}

// The add control is a plate of the drawer's own language, so it carries the
// shared CP2077 corner treatment (the 45° cut with its drawn edges) rather than
// a plain rectangle. The file keeps exactly ONE clip-path declaration, so the
// only correct way to give it the corner is to name it in the shared rules — a
// clip-path of its own would be a second shape waiting to drift.
func TestAddControlCarriesThePlateCorner(t *testing.T) {
	css := readAppCSS(t)
	notchRule := ruleContaining(t, css, "--notch-run:")
	strokeRule := ruleContaining(t, css, "background-position:")
	for _, rule := range []struct{ name, body string }{{"corner", notchRule}, {"edge strokes", strokeRule}} {
		if !strings.Contains(rule.body, ".drawer-add") {
			t.Errorf("the add control is missing from the shared plate %s rule, so it renders as a bare rectangle", rule.name)
		}
	}
	if n := strings.Count(css, "clip-path:"); n != 1 {
		t.Errorf("app.css declares %d clip-paths; the corner shape must stay declared once", n)
	}
	if strings.Contains(between(t, css, ".drawer-add {", "}"), "clip-path") {
		t.Error(".drawer-add must not declare its own clip-path")
	}
}

// The drawer's bottom row holds the two ways a profile enters the list: the
// add control, which takes the row's width, and the import key, a fixed square
// icon beside it. The row is pinned to the drawer's bottom edge only while the
// list overflows — below that the list owns its content height and the row
// follows the last entry, which is why the list's flex is `0 1 auto` and not
// `1 1 auto`. Both keys keep the dashed "empty slot" look and stay their own
// component — a host card's rail and hover lift are the list's language, not
// an action bar's.
func TestAddControlIsTheDrawersBottomAction(t *testing.T) {
	css := readAppCSS(t)
	row := between(t, css, ".drawer-add-row {", "}")
	if !strings.Contains(row, "flex: 0 0 auto") {
		t.Errorf("the bottom row must be pinned outside the list's flex; got %q", row)
	}
	add := between(t, css, ".drawer-add-grow {", "}")
	for _, want := range []string{"flex: 1 1 0", "min-width: 0", "margin: 0"} {
		if !strings.Contains(add, want) {
			t.Errorf("the add control should take the row's width; got %q", add)
		}
	}
	importKey := between(t, css, ".drawer-add-row .drawer-import {", "}")
	if !strings.Contains(importKey, "flex: 0 0 44px") {
		t.Errorf("the import key is a square icon beside the add control; got %q", importKey)
	}
	// The shared plate rule keeps both keys centred and self-sized; the row
	// override only changes how they share the width.
	plate := between(t, css, ".drawer-add {", "}")
	for _, want := range []string{"align-items: center", "justify-content: center"} {
		if !strings.Contains(plate, want) {
			t.Errorf("the add plate should be %q; got %q", want, plate)
		}
	}
	if !strings.Contains(between(t, css, "#sec-entries-body > #conn-grid {", "}"), "flex: 0 1 auto") {
		t.Error("the control only reaches the bottom edge while the list overflows; that is the list's flex, checked here as its partner")
	}

	// It has to live outside the scrolling list: the list scrolls and would
	// carry it away. Its home is the entries drawer, which is what makes the two
	// assertions below a pair rather than one.
	entries := readAssetLF(t, "index.html")
	if strings.Contains(between(t, entries, `id="sec-entries-body"`, `id="conn-grid"`), `id="conn-add"`) {
		t.Error("the add action must not sit between the drawer's header and its list")
	}
	if !strings.Contains(between(t, entries, `id="conn-grid"`, `id="sec-sessions"`), `id="conn-add"`) {
		t.Error("the add action belongs to the entries drawer, after its scrolling host list")
	}

	grid := between(t, css, "#sec-entries-body > #conn-grid {", "}")
	if !strings.Contains(grid, "overflow-y: auto") {
		t.Errorf("the host list must scroll; got %q", grid)
	}
	if drawer := between(t, css, "#sec-entries-body {", "}"); strings.Contains(drawer, "overflow-y: auto") {
		t.Error("the drawer itself must not scroll: a scrolling drawer takes the toolbar and the add action with it")
	}

	// The action is created once in the markup and wired in conn-form.js; a
	// re-render of the list must not rebuild it or it would come back as a card.
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	if strings.Contains(dialogs, "entry-card-add") {
		t.Error("dialogs.js still rebuilds the add card on every list render")
	}
	if !strings.Contains(readAssetLF(t, "static/js/conn-form.js"), "getElementById('conn-add')") {
		t.Error("conn-form.js should wire the drawer's add action")
	}
}

// A session tile's name is its rename control, and the selection checkbox sits
// on that same row.
//
// Both were somewhere else: the pencil button sat next to the sid, so it read as
// "edit the id" rather than "rename the session", and the checkbox was pinned to
// the icon's top-left corner, where it looked like it belonged to the icon. The
// name has to stay the click target for opening nothing — it renames — so the
// tile's open handler must ignore clicks that land on it.
func TestSessionTileNameRenamesAndCheckboxSitsOnItsRow(t *testing.T) {
	sessions := readAssetLF(t, "static/js/sessions.js")
	css := readAppCSS(t)

	// The checkbox is inside the name row, not the icon stack.
	if !strings.Contains(sessions, `'<div class="sess-name-row">' +`) {
		t.Fatal("the session tile should have a sess-name-row holding the checkbox and the name")
	}
	row := between(t, sessions, `class="sess-name-row">`, `</div>`)
	if !strings.Contains(row, `class="sess-checkbox"`) {
		t.Error("the checkbox belongs on the name row, on the same line as the name it selects")
	}
	if !strings.Contains(row, "sess-entry-line") {
		t.Error("the name row should also hold the name")
	}
	// And out of the icon stack, where it used to sit.
	stack := between(t, sessions, `'<div class="conn-tile-stack">'`, `'</div>' +`)
	if strings.Contains(stack, "sess-checkbox") {
		t.Error("the checkbox should have left the icon stack")
	}

	// The two elements must stay adjacent for the gap to mean anything: a name
	// that grows to fill the row strands the checkbox at the card's edge, and the
	// CSS gap then has nothing to do with the visual spacing.
	if !strings.Contains(css, ".sess-entry-line ") && !strings.Contains(css, ".sess-entry-line {") {
		t.Fatal("the name rule should exist in the stylesheet")
	}
	nameRule := between(t, css, ".sess-entry-line {", "}")
	if !strings.Contains(nameRule, "flex: 0 1 auto") {
		t.Errorf("the name must size to its text (flex: 0 1 auto), or the checkbox is pushed away from it; got %q", nameRule)
	}
	rowRule := between(t, css, ".conn-tile.sess-tile .sess-name-row {", "}")
	for _, want := range []string{"justify-content: center", "padding: 0 4px"} {
		if !strings.Contains(rowRule, want) {
			t.Errorf("the name row should have %s so the pair is centred with breathing room", want)
		}
	}

	// The pencil is gone from both the markup and the stylesheet.
	if strings.Contains(sessions, "sess-rename-btn") || strings.Contains(css, "sess-rename-btn") {
		t.Error("the rename button next to the sid should be gone: the name is the control now")
	}
	if strings.Contains(readAssetLF(t, "static/js/util.js"), "SVG_PENCIL_12") {
		t.Error("SVG_PENCIL_12 is dead once the rename button is gone")
	}

	// Renaming hangs off the name element, and the name is keyboard reachable.
	name := between(t, sessions, "var nameEl = tile.querySelector('.sess-entry-line');", "\n\n")
	for _, want := range []string{"nameEl.addEventListener('click', doRename)", "nameEl.addEventListener('keydown'"} {
		if !strings.Contains(name, want) {
			t.Errorf("the name element should carry %s", want)
		}
	}
	if !strings.Contains(sessions, `class="sess-entry-line" role="button" tabindex="0"`) {
		t.Error("the name is now an actionable control and needs button semantics and a tab stop")
	}

	// A click on the name must not also open the session: the tile's own handler
	// has to skip it, or a rename would open a terminal behind the prompt.
	open := between(t, sessions, "tile.onclick = function (e) {", "\n    };")
	for _, want := range []string{".sess-entry-line", ".sess-sid-line", ".sess-status-ic"} {
		if !strings.Contains(open, want) {
			t.Errorf("the tile's open handler must ignore clicks on %s: each is its own control", want)
		}
	}

	// The sid copies, and it is keyboard reachable for the same reason the name is.
	sid := between(t, sessions, "var sidEl = tile.querySelector('.sess-sid-line');", "\n\n")
	for _, want := range []string{"sidEl.addEventListener('click', doCopy)", "sidEl.addEventListener('keydown'"} {
		if !strings.Contains(sid, want) {
			t.Errorf("the sid is the copy control and should carry %s", want)
		}
	}
	if !strings.Contains(sessions, `class="sess-sid-line" role="button" tabindex="0"`) {
		t.Error("the sid is now actionable and needs button semantics and a tab stop")
	}
	// The separate copy button is gone from the session tile. The entry card keeps
	// its own (dialogs.js), so this checks the tile's markup, not the class.
	tileHtml := between(t, sessions, "tile.innerHTML =", "tile.title =")
	if strings.Contains(tileHtml, "sess-copy-btn") {
		t.Error("the session tile should not carry a separate copy button: the sid copies")
	}

	// Status is a lamp at the left of the id, not a word on the name.
	if !strings.Contains(sessions, `class="sess-status-ic `) {
		t.Error("the tile should carry a status lamp")
	}
	if strings.Contains(css, ".sess-dead {") {
		t.Error("the DEAD word badge should be gone: the lamp says it now")
	}
	meta := between(t, sessions, `'<div class="sess-meta-row">'`, `'</div>' +`)
	if !strings.Contains(meta, "statusIc") || !strings.Contains(meta, "sess-sid-line") {
		t.Error("the lamp belongs immediately before the sid")
	}
	if i, j := strings.Index(meta, "statusIc"), strings.Index(meta, "sess-sid-line"); i > j {
		t.Error("the lamp must precede the sid, not follow it")
	}
	// The lamp's colours come from tokens. A hardcoded pair is a second palette
	// the theme cannot reach — and because the skin layer re-declares the lamp
	// further down, a hex in the base layer is a value that never renders at all.
	// Both layers are asserted: the base one is the fallback the page keeps if the
	// skin block is dropped, the skin one is what a browser actually paints.
	base := between(t, css, ".conn-tile.sess-tile .sess-status-ic.is-live {", "}")
	skin := css[strings.LastIndex(css, ".conn-tile.sess-tile .sess-status-ic.is-live {"):]
	skin = skin[:strings.Index(skin, "}")+1]
	for name, rule := range map[string]string{"base": base, "skin": skin} {
		if !strings.Contains(rule, "background: var(--") {
			t.Errorf("the %s lamp declares no token background: %q", name, rule)
		}
	}
	if !strings.Contains(skin, "background: var(--accent)") {
		t.Errorf("the painted lamp is the accent hue; got %q", skin)
	}
	for _, want := range []string{
		"animation: sessCyanPulse",
		".conn-tile.sess-tile .sess-status-ic.is-dead { background: var(--danger); }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the status lamp should style %s", want)
		}
	}
	for _, hardcoded := range []string{"#1a7f37", "#cf222e"} {
		if strings.Contains(css, hardcoded) {
			t.Errorf("the lamp hardcodes %s; its colour belongs to a token", hardcoded)
		}
	}
}
