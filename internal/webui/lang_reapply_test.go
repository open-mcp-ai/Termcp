package webui

import (
	"strings"
	"testing"
)

// TestReapplyWindowLanguageCoversTheTWrittenSurfaces pins the language switch's
// reach into one open session window.
//
// applyI18n only rewrites nodes that carry a data-i18n* hook. A session window
// is a mix: most of its chrome is marked, but several surfaces are written from
// t() at paint time — the header's drawer and tick controls, the DEAD badge, the
// transcript rail's accessible name — and the review queue is rendered HTML
// whose buttons come from t() at queue-load time. A language switch that only
// ran applyI18n left every one of those in the previous language until the
// window happened to be rebuilt, which is exactly the "the window ignores the
// language switch" report.
//
// reapplyWindowLanguage is the hook that runs for every open window on a
// switch. These assertions pin that it repaints each t()-written surface.
func TestReapplyWindowLanguageCoversTheTWrittenSurfaces(t *testing.T) {
	fn := between(t, readTerminalJS(t), "function reapplyWindowLanguage(", "\n}\n")
	if fn == "" {
		t.Fatal("reapplyWindowLanguage is gone; the language switch has no per-window hook")
	}

	// Header controls written once at creation: title and aria-label both.
	for _, want := range []string{
		"'.shell-window-drawer-btn'",
		"t('win.aria.connections')",
		"'.shell-header-icon-btn'",
		"t('win.aria.select')",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("reapplyWindowLanguage must repaint the header controls (missing %s)", want)
		}
	}

	// The DEAD badge says the session is over; its text and tooltip are
	// t()-written at badge creation and must be rewritten here.
	for _, want := range []string{
		"'.shell-dead-badge'",
		"t('session.dead.badgeTitle')",
		"t('session.dead.badge')",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("reapplyWindowLanguage must repaint the dead badge (missing %s)", want)
		}
	}

	// The transcript rail's accessible name, and the repaint that rewrites the
	// labels a visible pop or card is showing.
	for _, want := range []string{
		"'.term-rail'",
		"t('rail.label')",
		"railGeometryChanged(",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("reapplyWindowLanguage must repaint the transcript rail (missing %s)", want)
		}
	}

	// An OPEN review panel holds rendered rows; paintReviewLabels only covers the
	// lock and the switch, so the queue itself must be reloaded.
	if !strings.Contains(fn, "loadReviewQueue(") {
		t.Error("reapplyWindowLanguage must reload an open review queue; its rows are t()-rendered HTML")
	}

	// The stateful titles and the dynamic panels keep their repaints.
	for _, want := range []string{
		"paintReviewLabels(",
		"_refreshFwList",
		"_shellFileBrowse",
		"shellStatusPaint(",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("reapplyWindowLanguage lost an existing repaint path (missing %s)", want)
		}
	}
}

// TestTheClientsTriggerLivesInTheDock pins the clients menu's home and shape.
//
// It was a header dropdown whose panel was a fixed 340px slab — always that
// wide, whatever it held — and its trigger spent header width on the word
// "Clients"/“连接者”. The menu moved to the far-right end of the bottom dock
// bar, icon-only, opening UPWARD (a panel anchored below the viewport-bottom
// bar opens off-screen) with a width that hugs its content.
func TestTheClientsTriggerLivesInTheDock(t *testing.T) {
	index := readAssetLF(t, "index.html")

	// Home: inside the dock bar, and nowhere in the header.
	dock := between(t, index, `class="sessdock" id="sessdock"`, `<div id="modal-forward"`)
	if !strings.Contains(dock, `id="clients-menu"`) {
		t.Fatal("the clients menu is no longer in the dock bar")
	}
	header := between(t, index, `<header class="app-header">`, `</header>`)
	if strings.Contains(header, `id="clients-menu"`) {
		t.Error("the header still carries the clients menu; it moved to the dock")
	}

	// The trigger keeps its label span (the narrow-header rule and the i18n
	// engine both key on it) but the dock does not draw it: the dot and the
	// tooltip are the whole trigger.
	if !strings.Contains(index, `class="clients-label"`) {
		t.Fatal("the clients label span is gone; the narrow-screen rule and i18n key on it")
	}
	css := readAppCSS(t)
	if !strings.Contains(css, ".sessdock .clients-label { display: none; }") {
		t.Error("the dock must hide the clients label, not the markup lose the span")
	}
	// Dock key anatomy: a 26px square key beside the arrangement controls.
	if !strings.Contains(css, ".sessdock .clients-btn {") {
		t.Error("the dock's clients trigger should wear the dock's own key anatomy")
	}

	// The panel opens upward and hugs its content, with a cap.
	panel := between(t, css, ".clients-panel {", "}")
	for _, want := range []string{
		"bottom: calc(100% + 6px)",
		"top: auto",
		"width: max-content",
		"max-width:",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("the clients panel must %s (a downward, fixed-width slab is the shape that was removed)", want)
		}
	}
	if strings.Contains(panel, "width: min(340px") {
		t.Error("the clients panel is a fixed 340px slab again")
	}

	// The dock bar must not depend on its middle to pin the controls right: the
	// empty note is no longer drawn, so the auto margin is what holds the edge.
	actions := between(t, css, ".sessdock-actions {", "}")
	if !strings.Contains(actions, "margin-left: auto") {
		t.Error("the window controls must carry margin-left: auto; the strip and the empty note cannot be relied on")
	}
	if !strings.Contains(css, ".sessdock-empty {\n  display: none;\n}") {
		t.Error("the empty note must not be drawn, at any width")
	}
}

// TestNarrowAndWideViewportModeTransitions pins the window mode switch.
//
// In wide viewports (>768px) a session window is a draggable floating window
// with default dimensions (640x480, centred, resizable). In narrow viewports
// (<=768px, or touch phones in landscape) it is a full-screen terminal filling
// the viewport (100vw x 100dvh) without inline geometry fighting the stylesheet.
//
// The bug this guards against: mobileViewport was keyed strictly to
// (pointer: coarse) && (hover: none). On a desktop, narrowing the window never
// engaged full-screen (the window overflowed 640x480), and opening on mobile
// then expanding or rotating never dropped full-screen (mobile never flipped to
// false, so the window stayed 100vw even on a wide monitor).
func TestNarrowAndWideViewportModeTransitions(t *testing.T) {
	js := readAssetLF(t, "static/js/shell-windows.js")

	// isNarrowViewport must inspect width and height, not just pointer capability.
	narrow := between(t, js, "function isNarrowViewport()", "\n}\n")
	if !strings.Contains(narrow, "768") {
		t.Error("isNarrowViewport must use the 768px narrow breakpoint")
	}
	if !strings.Contains(narrow, "innerWidth") {
		t.Error("isNarrowViewport must inspect window.innerWidth")
	}

	// onViewportChange must transition both ways: entering narrow applies
	// full-screen; leaving narrow removes full-screen and re-seats floating geometry.
	vpHook := between(t, js, "function onViewportChange()", "\n  }\n")
	if !strings.Contains(vpHook, "applyWindowViewportMode(w)") {
		t.Error("onViewportChange must apply full-screen mode when entering narrow")
	}
	if !strings.Contains(vpHook, "w.classList.remove('win-fullscreen')") {
		t.Error("onViewportChange must remove full-screen mode when entering wide")
	}
	// Leaving narrow restores the remembered desktop rect (a dragged window
	// returns to its place), falling back to the cascade only for windows that
	// were opened while narrow.
	if !strings.Contains(vpHook, "w._desktopRect") {
		t.Error("leaving narrow must restore the remembered desktop rect, not re-cascade every window")
	}
	if !strings.Contains(vpHook, "positionShellWindowFromClick(w, null)") {
		t.Error("a window opened while narrow must still be placed as a desktop window")
	}
	// The rect is remembered when ENTERING full-screen, before the inline styles
	// are cleared — clearing first is the variant that loses the position.
	enter := between(t, js, "function applyWindowViewportMode(", "\n}\n")
	if !strings.Contains(enter, "_desktopRect") {
		t.Error("applyWindowViewportMode must remember the desktop rect before clearing inline styles")
	}
	if i, j := strings.Index(enter, "_desktopRect"), strings.Index(enter, "win.style.width = ''"); i < 0 || j < 0 || i > j {
		t.Error("the desktop rect must be saved before the inline styles are cleared")
	}
}

// TestSessionDeadBadgeCarriesI18nHooks pins the "Session ended" badge wiring.
//
// When a session exits, the title bar gains a .shell-dead-badge element. It was
// painted once from t() without data-i18n attributes, so a subsequent language
// switch never updated its text or tooltip — the user saw the previous language's
// copy ("会话已结束") frozen indefinitely as if hard-coded.
func TestSessionDeadBadgeCarriesI18nHooks(t *testing.T) {
	sessions := readAssetLF(t, "static/js/sessions.js")
	badgeFunc := between(t, sessions, "function setWindowDeadBadge(", "\n}\n")
	if badgeFunc == "" {
		t.Fatal("setWindowDeadBadge is gone")
	}
	if !strings.Contains(badgeFunc, `setAttribute('data-i18n', 'session.dead.badge')`) {
		t.Error("dead badge must carry data-i18n attribute so applyI18n can rewrite it")
	}
	if !strings.Contains(badgeFunc, `setAttribute('data-i18n-title', 'session.dead.badgeTitle')`) {
		t.Error("dead badge must carry data-i18n-title attribute")
	}

	// And the link state dot must also carry its title hook when dead.
	winJS := readAssetLF(t, "static/js/terminal-window.js")
	linkFunc := between(t, winJS, "function setWindowLinkState(", "\n}\n")
	if !strings.Contains(linkFunc, `setAttribute('data-i18n-title', 'session.dead.badgeTitle')`) {
		t.Error("link state dot must update data-i18n-title when marked is-dead")
	}
}

// TestEndedMarkerRewritesInPlace pins how the terminal-end marker follows language switches.
//
// The marker lives IN the terminal (written via term.writeln) so it stays with
// the session's own output, exactly where users expect it to be. When the language
// switches, reapplyEndedMarker addresses the marker row directly via ANSI cursor
// positioning and rewrites it in place — guarded so it touches only a line that
// still reads as the marker in some known language.
func TestEndedMarkerRewritesInPlace(t *testing.T) {
	sessions := readAssetLF(t, "static/js/sessions.js")

	// showTerminalEndedMarker must write the marker with writeln into the terminal
	marker := between(t, sessions, "function showTerminalEndedMarker(", "\n}\n")
	if !strings.Contains(marker, "ch.term.writeln") {
		t.Error("the ended marker must be written into the xterm buffer as a terminal line")
	}
	if !strings.Contains(marker, "t('term.ended')") {
		t.Error("the ended marker must use t('term.ended')")
	}

	// reapplyEndedMarker must exist and rewrite the line in place via ANSI
	reapply := between(t, sessions, "function reapplyEndedMarker(", "\n}\n")
	if reapply == "" {
		t.Fatal("reapplyEndedMarker is missing")
	}
	if !strings.Contains(reapply, `term.write('\x1b7\x1b[' + row + ';1H\x1b[2K\x1b[33m' + next + '\x1b[0m\x1b8')`) {
		t.Error("reapplyEndedMarker must rewrite the line in place via ANSI save/CUP/EL/restore")
	}
	if !strings.Contains(reapply, "isEndedMarkerText(current)") {
		t.Error("reapplyEndedMarker must guard against rewriting lines that are not markers")
	}

	// The legacy win._term bypass in ui-socket.js must stay gone.
	sock := readAssetLF(t, "static/js/ui-socket.js")
	if strings.Contains(sock, "_term.writeln") {
		t.Error("ui-socket must not writeln to win._term; that bypassed the one ended-marker renderer")
	}

	// terminal-switcher.js must invoke reapplyEndedMarker during language change
	switcher := readAssetLF(t, "static/js/terminal-switcher.js")
	if !strings.Contains(switcher, "reapplyEndedMarker(ch)") {
		t.Error("terminal-switcher must call reapplyEndedMarker on language change")
	}
}

// TestArchiveSearchAndInvertKeysCarryI18nHooks pins the i18n hooks on dynamic inputs.
//
//  1. The three list filter boxes (hosts, extensions, archive) come from one
//     builder, ensureGridSearch. It must carry data-i18n-placeholder and
//     data-i18n-aria so applyI18n rewrites the box on language switches.
//  2. The batch bar's invert key and the archive toggle rewrite their titles dynamically
//     based on selection/expanded state. They must stamp the matching data-i18n-* hook
//     so a language switch reproduces the CURRENT state's wording instead of falling back
//     to a static disagreeing markup key.
func TestArchiveSearchAndInvertKeysCarryI18nHooks(t *testing.T) {
	util := readAssetLF(t, "static/js/util.js")
	builder := between(t, util, "function ensureGridSearch(", "\n}\n")
	if !strings.Contains(builder, "setAttribute('data-i18n-placeholder', placeholderKey)") {
		t.Error("the shared filter box must carry data-i18n-placeholder so a language switch rewrites it")
	}
	if !strings.Contains(builder, "setAttribute('data-i18n-aria', placeholderKey)") {
		t.Error("the shared filter box must carry data-i18n-aria")
	}

	// All three columns must go through that one builder, not a private copy.
	for _, tc := range []struct{ file, key string }{
		{"static/js/sessions.js", "archive.search.placeholder"},
		{"static/js/dialogs.js", "hosts.search.placeholder"},
		{"static/js/extensions.js", "ext.search.placeholder"},
	} {
		src := readAssetLF(t, tc.file)
		if !strings.Contains(src, "ensureGridSearch(") {
			t.Errorf("%s does not use the shared filter builder", tc.file)
		}
		if !strings.Contains(src, "'"+tc.key+"'") {
			t.Errorf("%s does not name its catalog key %q", tc.file, tc.key)
		}
	}

	batch := readAssetLF(t, "static/js/batch-bar.js")
	paintInvert := between(t, batch, "function paintInvert(", "\n  }\n")
	if !strings.Contains(paintInvert, "setAttribute('data-i18n-title', key)") {
		t.Error("paintInvert must update data-i18n-title to match its dynamic key")
	}

	col := readAssetLF(t, "static/js/archive-column.js")
	trigger := between(t, col, "function paintArchiveTrigger(", "\n}\n")
	if !strings.Contains(trigger, "setAttribute('data-i18n-title', key)") {
		t.Error("paintArchiveTrigger must update data-i18n-title to match its dynamic key")
	}
}

// TestTabbarRefreshWritesNothingWhenNothingChanged pins the flicker fix.
//
// refreshSessionTabbar runs SIX times per arrangement click: directly from
// setLayoutMode, once per tiled window through setActivePane, once through
// syncShellWindowVisibility, and once each from the pane-grid and container
// MutationObservers. The strip used to be rebuilt on every pass — `appendChild`
// on a node already in the DOM is a remove-then-insert, so two tabs cost twelve
// DOM moves per click with zero new terminal output anywhere, which the eye
// reads as the tab strip flickering whenever the operator rearranges windows.
//
// Two guards make an unchanged refresh write nothing:
//   - the strip is reordered only when its children actually differ (identity,
//     not just count), and
//   - every per-tab write (label, title, aria-selected, data-sid, data-win-id,
//     hidden) reads before it writes.
//
// The guard must be an identity comparison: a count-only check would still
// re-insert every tab when two tabs swap order, and that swap is exactly the
// case a raise produces.
func TestTabbarRefreshWritesNothingWhenNothingChanged(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")

	// The reorder is guarded by an identity walk over the existing children.
	if !strings.Contains(shell, "var stripDiffers = kids.length !== order.length;") {
		t.Fatal("the tab strip must compare its current children against the new order before touching the DOM")
	}
	if !strings.Contains(shell, "if (kids[i] !== order[i]) { stripDiffers = true; break; }") {
		t.Error("the comparison must be by node identity: a count-only check re-inserts every tab when two swap")
	}
	if !strings.Contains(shell, "if (stripDiffers) order.forEach(function (t) { scroll.appendChild(t); });") {
		t.Error("appendChild must run only when the strip differs; an unconditional pass is a remove+insert per tab")
	}
	// Exactly one appendChild pass may exist in the function, and it is the guarded
	// one: a second, unconditional pass would reinstate the remove+insert churn.
	refreshFn := between(t, shell, "function refreshSessionTabbar() {", "\n}\n")
	if n := strings.Count(refreshFn, "scroll.appendChild("); n != 1 {
		t.Errorf("refreshSessionTabbar has %d appendChild call site(s); the strip must be written from one guarded pass", n)
	}

	// Per-tab writes read before they write. An unconditional textContent
	// assignment destroys and recreates the text node on every pass.
	for _, want := range []string{
		"if (labelEl && labelEl.textContent !== label) labelEl.textContent = label;",
		"if (tab.title !== label) tab.title = label;",
		"if (tab.getAttribute('aria-selected') !== selVal) tab.setAttribute('aria-selected', selVal);",
		"if (panelsEl.hidden !== panelsHidden) panelsEl.hidden = panelsHidden;",
		"if (tab.getAttribute('data-win-id') !== entry.key) tab.setAttribute('data-win-id', entry.key);",
		"if (tab.classList.contains('active') !== tabActive) tab.classList.toggle('active', tabActive);",
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("per-tab write is not guarded; a no-op refresh would still churn the strip: missing %q", want)
		}
	}

	// The approval badge is repainted after every refresh, so it needs the same
	// treatment or the strip flickers from that path instead.
	approval := readAssetLF(t, "static/js/approval.js")
	if !strings.Contains(approval, "if (badge.hidden !== hide) badge.hidden = hide;") {
		t.Error("the review badge's hidden flag must be guarded: applyApprovalCounts runs on every refresh")
	}
	if !strings.Contains(approval, "if (badge.textContent !== text) badge.textContent = text;") {
		t.Error("the review badge's count must be guarded: assigning textContent replaces the text node")
	}
}

// TestPaneArrangementInvariants pins four bugs a live arrangement session
// exposed in the tile/float machinery (shell-windows.js).
//
// The repro was: open termcp://blackbox, open a second window, rearrange — and
// the blackbox tab started pulsing though nothing new had been printed. Reading
// the arrangement code with that repro in hand surfaced four defects, each one
// pinned here by the code that prevents it.
func TestPaneArrangementInvariants(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")

	// 1. A maximized pane hides its neighbours, so their output is news again.
	//    Every OTHER tiled pane is on screen at once — that is the point of the
	//    grid — and pane-active marks the pane that owns the keyboard, not the
	//    one the eye can see. Conflating the two made a visible pane report its
	//    own output as unseen, forever (clearing the mark required taking the
	//    keyboard from the pane the operator was typing in).
	watched := between(t, shell, "function dockWindowWatched(", "\n}\n")
	if !strings.Contains(watched, "var maxed = document.querySelector('.shell-window.pane-maxed');") {
		t.Error("a tiled pane behind a maximized pane must count as unwatched; the maximized pane covers it")
	}
	if !strings.Contains(watched, "return winHasPixels(win);") {
		t.Error("a visible tiled pane must count as watched: pane-active marks the keyboard, not the eye")
	}
	if !strings.Contains(shell, "function winHasPixels(") {
		t.Fatal("winHasPixels is gone: on-screen is measured from the DOM, not from a class name")
	}

	// 2. A fullscreen window entering the grid sheds the class. The class says
	//    "the stylesheet decides my box" while the grid says "I decide your
	//    cell" — a fullscreen pane kept its 100vw width, its fullscreen z-index
	//    beat its neighbours', and refreshSessionTabbar counted it as fullscreen,
	//    which hid the page header while tiled.
	tile := between(t, shell, "function tileWindow(", "\n}\n")
	if !strings.Contains(tile, "win.classList.remove('win-fullscreen')") {
		t.Error("tileWindow must drop .win-fullscreen: a fullscreen pane inside the grid poisoned the layout")
	}
	if !strings.Contains(tile, "win._desktopRect = {") {
		t.Error("tileWindow must save the narrow-viewport rect before dropping the class, so untile can restore it")
	}

	// 3. Untiling on a narrow viewport re-runs the viewport-mode pass. The saved
	//    float rect is DESKTOP geometry; writing it inline on a narrow viewport
	//    would beat the fullscreen media query and leave a 640x480 box hanging
	//    off a phone screen.
	untile := between(t, shell, "function untileWindow(", "\n}\n")
	if !strings.Contains(untile, "applyWindowViewportMode(win);") {
		t.Error("untileWindow must re-evaluate the viewport mode: the restored inline rect would override the fullscreen rule")
	}
	if !strings.Contains(untile, "if (_paneActiveWin === win) _paneActiveWin = null;") {
		t.Error("untileWindow must clear _paneActiveWin when the demoted pane was the active one")
	}

	// 4. Tiling with ZERO windows refuses. The workspace overlay would otherwise
	//    open over the whole page with not one pane inside it, eating every click
	//    on the main page — and the close-the-last-pane observer cannot rescue
	//    this case, because an empty overlay never fires a childList mutation.
	mode := between(t, shell, "function setLayoutMode(", "\n}\n")
	if !strings.Contains(mode, "allShellWins().length === 0") {
		t.Error("setLayoutMode must refuse 'tile' with zero windows: the empty overlay would trap the page")
	}
}

// TestDialInFlightKeepsItsPlaceInTheBar pins the fix for a tab that teleported.
//
// A connecting window has no session id, so it cannot sort by created_at; it was
// prefixed onto the bar (bare.concat(entries)) — the LEFT end. The moment the
// connection landed the entry gained created_at and fell to the sorted tail:
// measured on a page with two established sessions, the dialling tab sat at index
// 0 while connecting and at index 2 the instant it connected, so the tab the
// reader was watching jumped the width of the bar.
//
// A dial in flight is the NEWEST thing on the bar, which is exactly where the
// session it becomes will live. Appending it makes connecting move nothing.
func TestDialInFlightKeepsItsPlaceInTheBar(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	entries := between(t, shell, "function dockEntries() {", "\n}\n")
	if strings.Contains(entries, "return bare.concat(entries);") {
		t.Error("dockEntries must not prefix session-less windows: a connecting tab would jump from the left end " +
			"to the sorted tail the moment its session id arrives")
	}
	if !strings.Contains(entries, "return entries.concat(bare);") {
		t.Error("session-less windows (a dial in flight, an extension card) belong at the END: the slot the session " +
			"they become will hold, so connecting moves nothing")
	}
	// The sort's own comment already described the right order; keep them agreeing.
	if !strings.Contains(entries, "Infinity") {
		t.Error("the created_at sort must still treat an unsnapshotted session as newest")
	}
}

// TestArrangementControlsAreDesktopOnly pins the narrow-viewport rule.
//
// On a phone every terminal is the full-screen layer, so the desktop
// arrangement keys (tile toggle, strategy select, hide/show eye) cannot do
// anything useful: splitting a 390px viewport into two panes leaves ~180px per
// terminal, which cannot render readable text. They used to be VISIBLE there
// anyway — the coarse-pointer rule in terminal.css hid .sessdock, but
// workspace.css declares `.sessdock { display: flex }` later in a sheet that
// loads after it, so the hide never won.
//
// Hiding the keys alone would strand anyone who was tiled when the viewport
// narrowed: the mode would stay tile with no visible way back. So three rules
// have to hold together, and this test pins all three.
func TestArrangementControlsAreDesktopOnly(t *testing.T) {
	css := readAssetLF(t, "static/css/workspace.css")

	// 1. The keys are hidden at the narrow breakpoint.
	// The keys live in the 799px block (it predates the 768px one and covers the
	// same phones plus small-tablet widths); the marker comment after it bounds
	// the slice.
	start := strings.Index(css, "@media (max-width: 799px) {")
	if start < 0 {
		t.Fatal("the 799px breakpoint block is gone: the arrangement keys would show on a phone")
	}
	narrow := css[start:]
	if end := strings.Index(narrow, "/* Extremely narrow"); end > 0 {
		narrow = narrow[:end]
	}
	for _, sel := range []string{".session-tabbar-tile,", ".session-tabbar-select,", ".session-tabbar-toggle {"} {
		if !strings.Contains(narrow, sel) {
			t.Errorf("the narrow block must hide the arrangement key %q: it cannot arrange anything on a phone", sel)
		}
	}
	if !strings.Contains(narrow, "display: none !important;") {
		t.Error("the arrangement keys need !important: .session-tabbar-tile is declared display:inline-flex elsewhere")
	}
	// The session tabs stay: switching sessions is the one thing still useful.
	if strings.Contains(narrow, ".sessdock-tabs {\n    display: none") {
		t.Error("the tab strip must stay visible on narrow viewports: it is how sessions are switched there")
	}

	shell := readAssetLF(t, "static/js/shell-windows.js")

	// 2. Tiling is refused while narrow, so a saved tile preference cannot
	//    restore an unreachable grid on a phone.
	mode := between(t, shell, "function setLayoutMode(", "\n}\n")
	if !strings.Contains(mode, "mode === 'tile' && isNarrowViewport()") {
		t.Error("setLayoutMode must refuse tile on a narrow viewport: the toggle is hidden there")
	}

	// 3. Crossing INTO narrow while tiled drops back to float, or the operator
	//    is stuck in a grid whose exit button just disappeared.
	viewport := between(t, shell, "var mobile = isMobileViewport();", "refreshSessionTabbar();")
	if !strings.Contains(viewport, "if (mobile && _layoutMode === 'tile') {") {
		t.Error("crossing into a narrow viewport must leave tile mode: the arrangement controls are hidden there")
	}
}
