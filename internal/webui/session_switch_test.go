package webui

import (
	"net/http"
	"net/http/httptest"
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

// The host drawer is a fixed-height panel with one scrolling region, so its
// toolbar cannot scroll away: the batch import/export keys sit above a list that
// may be 22 hosts tall, and a toolbar that leaves with the list means the keys
// are only reachable when the list happens to be short.
func TestDrawerToolbarStaysAboveTheScrollingList(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")

	// The drawer is the flex column; the list is the item that gives up space.
	drawer := between(t, css, "#sec-entries-body {", "}")
	for _, want := range []string{"display: flex", "flex-direction: column", "overflow: hidden"} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer should be a fixed flex column (%q missing); got %q", want, drawer)
		}
	}
	if !strings.Contains(between(t, css, ".drawer-toolbar {", "}"), "flex: 0 0 auto") {
		t.Error("the toolbar must not shrink or grow: it is a fixed strip above the list")
	}
	// The list grows only when it overflows: `0 1 auto` keeps it content-height so
	// a short list leaves the add control right under the last entry, and lets it
	// shrink (with `min-height: 0`) so a long list scrolls instead of pushing the
	// drawer's bottom off screen. `1 1 auto` would stretch the list to the drawer's
	// full height and strand the add bar at the bottom of a nearly empty drawer.
	grid := between(t, css, "#sec-entries-body > #conn-grid {", "}")
	for _, want := range []string{"flex: 0 1 auto", "min-height: 0", "overflow-y: auto"} {
		if !strings.Contains(grid, want) {
			t.Errorf("the host list should stay content-height until it overflows, then scroll (%q missing); got %q", want, grid)
		}
	}
	if strings.Contains(grid, "flex: 1 1 auto") {
		t.Error("the host list must not grow when it does not overflow: the add control would be stranded at the drawer's bottom")
	}

	// The list has to be height-capped for the scroll to happen at all. It is: the
	// drawer is pinned top:0/bottom:0 and the list gives up the leftover height
	// (`flex: 0 1 auto` on the column's main axis + `min-height: 0`, without which
	// the item refuses to shrink below its content and the drawer overflows).
	drawerBounds := between(t, css, "#sec-entries-body {", "}")
	if !strings.Contains(drawerBounds, "top: 0") || !strings.Contains(drawerBounds, "bottom: 0") {
		t.Errorf("the drawer must stay a fixed slab (top:0/bottom:0) so its column has a height to distribute: %q", drawerBounds)
	}
	if strings.Contains(drawer, "overflow-y: auto") {
		t.Error("the drawer itself must not scroll: a scrolling drawer carries the toolbar with it")
	}

	// The panel is a flex column, so the shared collapse rule must not reset its
	// display back to block — that would unpick the whole strip.
	if !strings.Contains(css, "#sec-entries-body.collapsed { display: flex; }") {
		t.Error("the entries collapse rule must keep the drawer's flex column")
	}
}

// NetHub is a grid track of its own, not a card floating inside the sessions
// column: the frame's leading track is reserved for it, the trailing track
// mirrors that reservation, and the plates sit between the two — which is what
// centres them on the dock rather than a track to the right of its edge. The
// control itself cancels the dock's horizontal padding so it stays glued to the
// container's left edge without the tracks moving to meet it.
//
// The reservation is a length on purpose. In the flex row this replaced, NetHub
// and the sessions column traded space, so the widest translation of the label
// decided which of the two gave way — a CJK label is ~20px wider than the English
// one, which is exactly the margin these layouts run out of.
func TestNetHubTriggerHugsTheDockEdge(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")
	index := readAssetLF(t, "index.html")

	if !strings.Contains(index, `class="nethub-btn" id="open-host-drawer"`) {
		t.Error("the drawer trigger should be the NetHub layout control")
	}
	button := between(t, css, ".nethub-btn {", "}")
	for _, want := range []string{
		"border-left: 0", "border-radius: 0 10px 10px 0",
		// Laid out in the track it reserves, at that track's exact width: as a
		// percentage or an auto width the control and the reservation could
		// disagree, and the wider of the two sets the plates' offset.
		"width: var(--nethub-w)",
		// The edge attachment is the dock's own padding, cancelled here and
		// nowhere else. A hardcoded value would drift the moment the dock's
		// padding changes at a breakpoint; a margin on the frame instead would
		// move the plates off centre.
		"margin-left: calc(-1 * var(--dock-pad-x))",
	} {
		if !strings.Contains(button, want) {
			t.Errorf("NetHub must be %q; got %q", want, button)
		}
	}

	// Every .dock rule that sets padding has to name the same variable the
	// control cancels, or the trigger stops being attached at that breakpoint
	// while the CSS still looks right in the other two.
	dockRules := 0
	for rest := css; ; {
		i := strings.Index(rest, ".dock {")
		if i < 0 {
			break
		}
		rest = rest[i:]
		j := strings.Index(rest, "}")
		if j < 0 {
			t.Fatal("an unclosed .dock rule")
		}
		rule := rest[:j]
		rest = rest[j:]
		if !strings.Contains(rule, "padding") {
			continue
		}
		dockRules++
		if !strings.Contains(rule, "--dock-pad-x") {
			t.Errorf("a .dock padding rule must read --dock-pad-x, the same value the trigger cancels: %q", rule)
		}
		if !strings.Contains(rule, "padding: 16px var(--dock-pad-x)") &&
			!strings.Contains(rule, "padding: 12px var(--dock-pad-x)") {
			t.Errorf("the .dock padding should be expressed with --dock-pad-x, not wrapped around it: %q", rule)
		}
	}
	if dockRules < 3 {
		t.Errorf("expected the three dock paddings (base, wide, touch) to declare --dock-pad-x; found %d", dockRules)
	}

	frame := between(t, css, ".workspace-frame {", "}")
	for _, want := range []string{
		"display: grid",
		"grid-template-columns: var(--nethub-w) minmax(0, 1fr) var(--nethub-w)",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("the layout frame should have %q; got %q", want, frame)
		}
	}
	// The frame must NOT escape the dock. `margin-left: calc(50% - 50vw)` landed
	// NetHub on the window's left edge, which also slid the plates left of the
	// dock's centre; both ends of the reservation are tracks inside the dock now.
	if strings.Contains(frame, "50vw") {
		t.Errorf("the frame must stay inside the dock's content box, or the plates leave its centre: %q", frame)
	}
	// The escape belongs to the trigger, not to the frame: a negative margin here
	// would drag the tracks — and with them the plates' centre — off the dock.
	if strings.Contains(frame, "margin-left: calc(-1") {
		t.Errorf("the frame must not cancel the dock padding; only the trigger does: %q", frame)
	}
	// The reservation and the control must read the SAME value, or the button
	// paints wider than the track under it and overlaps the plates. The leading
	// track is sized by the frame's template (checked above) and the control by
	// --nethub-w (checked above); the trailing placeholder states the width
	// again, so it has to name the variable too.
	if !strings.Contains(between(t, css, ".workspace-slot-trail {", "}"), "width: var(--nethub-w)") {
		t.Error("the trailing placeholder must reserve NetHub's width from --nethub-w, or the two ends of the reservation drift apart")
	}
}

// Centring is a consequence of the two reservations being equal, so it is
// pinned here rather than left to a reading of the template: equal end tracks
// and one flexible middle track is what puts the plates on the dock's centre.
func TestWorkspaceMainIsCentredByTheMirroredTracks(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")

	frame := between(t, css, ".workspace-frame {", "}")
	tracks := strings.TrimSpace(between(t, frame, "grid-template-columns:", ";"))
	tracks = strings.TrimPrefix(tracks, "grid-template-columns:")
	tracks = strings.TrimSpace(tracks)
	// The two ends name the same length — that equality IS the centring, so it is
	// asserted as one string instead of by index: a template whose ends differ by
	// a variable or a magic number would put the plates off the dock's centre.
	if tracks != "var(--nethub-w) minmax(0, 1fr) var(--nethub-w)" {
		t.Errorf("the frame should be reservation/content/reservation with one shared length; got %q", tracks)
	}
}

// The three tracks have to stay three tracks: a flex row let NetHub share a line
// with the plates, and that is the arrangement this shape exists to prevent. The
// narrow fallback is where the sharing is allowed — as two ROWS, never one — and
// it drops the trailing reservation with the template, which would otherwise add
// an empty row under the plates on every phone.
func TestNarrowLayoutStacksNetHubAboveThePlates(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")
	index := readAssetLF(t, "index.html")

	// Markup order is what makes the stacked mode push the plates DOWN: NetHub is
	// the grid's first child, so it becomes the first row and the trailing
	// placeholder becomes the last one.
	lead := strings.Index(index, `class="nethub-btn" id="open-host-drawer"`)
	main := strings.Index(index, `class="workspace-main"`)
	trail := strings.Index(index, `class="workspace-slot-trail"`)
	if lead < 0 || main < 0 || trail < 0 {
		t.Fatal("index.html must carry NetHub and the trailing placeholder around .workspace-main")
	}
	if !(lead < main && main < trail) {
		t.Errorf("NetHub and the placeholder must bracket the content column (lead %d, main %d, trail %d), or the stacked mode pushes nothing down", lead, main, trail)
	}

	// One column when the three tracks no longer fit, with the trailing
	// reservation gone rather than stacked under the plates.
	i := strings.Index(css, "@media (max-width: 799px) {")
	if i < 0 {
		t.Fatal("app.css has no narrow fallback for the workspace frame")
	}
	narrow := css[i:]
	if j := strings.Index(narrow, "\n}"); j >= 0 {
		narrow = narrow[:j+2]
	}
	for _, want := range []string{
		".workspace-frame { grid-template-columns: minmax(0, 1fr); }",
		// display:none on the leftover child, because with the template dropped
		// the grid would give it an implicit row of its own.
		".workspace-slot-trail { display: none; }",
	} {
		if !strings.Contains(narrow, want) {
			t.Errorf("the narrow workspace fallback should declare %q; got %q", want, narrow)
		}
	}
}

// Sessions and the archive are translucent, and the plate is the ONE element that
// blurs. Every card composites over that already-blurred backdrop, so a blur per
// card would only add work: a 22-session list would pay 22 filters for the same
// pixels.
func TestWorkspaceSurfacesAreTranslucent(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")

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
	css := readAssetLF(t, "static/css/app.css")
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
	css := readAssetLF(t, "static/css/app.css")
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

// The add control is the drawer's bottom action, not a host card in the list: a
// trailing card inside the scrolling list would roll the affordance off screen
// exactly when the list got long enough to need it. It stays its own component —
// a host card's rail and hover lift are the list's language, not an action bar's
// — and every glyph-only control centres its glyph.
//
// It is pinned to the drawer's bottom edge only while the list overflows. Below
// that the list owns its content height and the control follows the last entry,
// which is why the list's flex is `0 1 auto` and not `1 1 auto`.
func TestAddControlIsTheDrawersBottomAction(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")
	rule := between(t, css, ".drawer-add {", "}")
	for _, want := range []string{"flex: 0 0 auto", "align-items: center", "justify-content: center"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the add action should be %q; got %q", want, rule)
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
	css := readAssetLF(t, "static/css/app.css")

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
