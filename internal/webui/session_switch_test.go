package webui

import (
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
	termView := readAssetLF(t, "static/js/terminal-view.js")
	sessions := readAssetLF(t, "static/js/sessions.js")
	css := readAssetLF(t, "static/css/app.css")

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
	termView := readAssetLF(t, "static/js/terminal-view.js")
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
	css := readAssetLF(t, "static/css/app.css")
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
	css := readAssetLF(t, "static/css/app.css")
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
	css := readAssetLF(t, "static/css/app.css")
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
	termView := readAssetLF(t, "static/js/terminal-view.js")
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
	css := readAssetLF(t, "static/css/app.css")

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
}

// The z-index ladder: a surface that can open a dialog must sit below it.
//
// The edit-connection dialog is reachable from the entries drawer and from the
// full-screen terminal's hamburger, so a modal below either one opens *behind*
// it. Nothing errors when that happens — the class is removed and the dialog is
// briefly in the DOM and invisible — so it is pinned here instead.
func TestModalsOutrankEverySurfaceThatOpensThem(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")
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

// The add card is a single glyph with no label, so it centres in whatever width
// the card has — which differs by breakpoint (glyph-sized on desktop, full row
// on a phone). Without this the glyph sits left, where a real entry's label
// would start, and on a phone that reads as a stray glyph in an empty row.
func TestAddCardCentresItsGlyph(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")
	rule := between(t, css, ".conn-tile.entry-card.entry-card-add .entry-card-inner {", "}")
	if !strings.Contains(rule, "justify-content: center") {
		t.Error("the add card's inner row should centre its glyph")
	}
	// The shared base rule is left-aligned because real entries have a label
	// beside the icon; the add card has to override it, not inherit it.
	base := between(t, css, ".entry-card-inner {", "}")
	if !strings.Contains(base, "flex-direction: row") {
		t.Errorf("the base entry-card row changed shape; re-check the add card override: %q", base)
	}
	if seen := strings.Index(css, ".conn-tile.entry-card.entry-card-add .entry-card-inner"); seen < strings.Index(css, ".entry-card-inner {") {
		t.Error("the centring rule must come after the base rule it overrides")
	}
}
