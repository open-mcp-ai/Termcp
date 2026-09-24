package webui

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// Every key name the UI can display must be one the server can actually send.
// The two lists live in different files and a drift is silent until a human has
// already approved a key that then fails at execution time.
func TestApprovalKeyLabelsMatchServer(t *testing.T) {
	js := readAssetLF(t, "static/js/approval.js")
	block := between(t, js, "var KEY_LABELS = {", "};")
	re := regexp.MustCompile(`'([a-z+]+)':\s*'`)
	var names []string
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		t.Fatal("no key names found in KEY_LABELS")
	}
	for _, k := range names {
		if _, err := session.KeyBytes(k, true, false); err != nil {
			t.Errorf("KEY_LABELS names key %q, which the server rejects: %v", k, err)
		}
	}
	// The keys a reviewer most needs to be able to name.
	has := map[string]bool{}
	for _, k := range names {
		has[k] = true
	}
	for _, k := range []string{"ctrl+c", "enter", "tab", "esc"} {
		if !has[k] {
			t.Errorf("KEY_LABELS must cover %q", k)
		}
	}
	// An unknown key must still render as itself rather than vanish: a decision
	// must never be made against a blank chip.
	if !strings.Contains(js, "KEY_LABELS[key] || key") {
		t.Error("keyLabel must fall back to the raw name for an unknown key")
	}
}

// Review mode is a switch plus a queue. The human's job is to accept or refuse
// what the AI wrote, so there is no composer: typing belongs in the terminal
// behind the panel, and an agent's input arrives on its own.
func TestApprovalUIIsWired(t *testing.T) {
	index := readAssetLF(t, "index.html")
	if !strings.Contains(index, `static/js/approval.js`) {
		t.Error("index.html must load approval.js")
	}
	// The page-level worklist is gone: a decision is about one command line in
	// one session, and the terminal already says which session that is. A header
	// button opened a modal that asked the question the terminal answers.
	for _, gone := range []string{
		`id="modal-approvals"`, `id="approvals-open"`, `class="approvals-open"`,
		`id="modal-approval-compose"`, `id="approval-compose-text"`, `id="approvals-approver"`,
	} {
		if strings.Contains(index, gone) {
			t.Errorf("index.html still carries removed approval UI: %s", gone)
		}
	}

	tv := readAssetLF(t, "static/js/terminal-view.js")
	// Review mode must NOT block the operator's keyboard. Blocking it locked the
	// human out of the session they were watching: they could not interrupt a
	// running command, which is not what "review the AI" means. The gate lives on
	// the MCP surface, which is the AI's.
	//
	// Asserted as an absence in the onData path, because that is where the bug
	// was: the handler dropped every keystroke while the session gated. Comments
	// that explain the rule are stripped first, so prose about `_approvalMode`
	// does not read as a use of it (the explanation is exactly what belongs
	// here, and a test that forbade mentioning the field would drive it out).
	onData := between(t, tv, "term.onData(function (data) {", "var ch = win._channels[sessionId];")
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(onData, "")
	if strings.Contains(code, "_approvalMode") {
		t.Error("onData must not consult review mode: the human's keyboard is not what review gates")
	}
	if strings.Contains(tv, "typing is disabled") {
		t.Error("the 'typing is disabled' notice must be gone: typing works, so the message would be a lie")
	}
	// The mode is still tracked: the title bar and the panel show it.
	if !strings.Contains(tv, "win._approvalMode") {
		t.Error("terminal-view.js must still track review mode for its own UI state")
	}

	// The push event has to be routed, or a request never reaches the browser
	// without a manual refresh.
	sock := readAssetLF(t, "static/js/ui-socket.js")
	if !strings.Contains(sock, "onApprovalEvent") {
		t.Error("ui-socket.js must route the approval push event")
	}

	// And the session snapshot must carry the flag to opened windows.
	sess := readAssetLF(t, "static/js/sessions.js")
	if !strings.Contains(sess, "applyApprovalMode(w, !!s.approval_mode)") {
		t.Error("sessions.js must apply review mode to open windows from the snapshot")
	}
}

// The terminal's review toggle calls the same write path as the session card's
// lock. Two entrances to one switch is exactly where a rule gets lost, so this
// pins the shared call rather than the click.
func TestReviewToggleUsesTheSharedApprovalWritePath(t *testing.T) {
	tv := readAssetLF(t, "static/js/terminal-view.js")
	if !strings.Contains(tv, "function applyReviewToggle(") {
		t.Fatal("terminal-view.js has no review switch; the terminal cannot enter review mode")
	}
	if !strings.Contains(tv, "return setSessionApproval(") && !strings.Contains(tv, "p = setSessionApproval(") {
		t.Error("the review switch must go through setSessionApproval, the same path the session card uses")
	}
	shared := readAssetLF(t, "static/js/sessions.js")
	if !strings.Contains(shared, "function setSessionApproval(") {
		t.Error("expected the shared setSessionApproval helper to exist in sessions.js")
	}
	// One reviewer, so neither entrance may send a threshold.
	if strings.Contains(tv, "setSessionApproval(sid, true, ") {
		t.Error("the review switch must not send an approver count")
	}
	if strings.Contains(shared, "setSessionApproval(s.id, true, ") {
		t.Error("the session card must not prompt for an approver count")
	}
	// A silent double submit would race two PATCHes for one switch.
	if !strings.Contains(tv, "tg.disabled = true") {
		t.Error("the review switch must disable itself while the write is in flight")
	}
}

// A gated session must not render as ungated just because the page's session
// snapshot predates the gate. That shipped once: the window showed "Review: off"
// while the server dropped every keystroke, so typing looked broken and the
// reason was invisible.
func TestApprovalStateIsReadFromTheServerNotOnlyTheSnapshot(t *testing.T) {
	tv := readAssetLF(t, "static/js/terminal-view.js")
	// The call site, not just the definition: a helper that is never invoked is
	// exactly the bug this test exists for.
	if !strings.Contains(tv, "  refreshApprovalState(win, sessionId);") {
		t.Fatal("applyApprovalModeFromSnapshot must call refreshApprovalState; the snapshot alone can be stale")
	}
	if !strings.Contains(tv, "function refreshApprovalState(win, sessionId) {") {
		t.Fatal("terminal-view.js must define refreshApprovalState")
	}
	if !strings.Contains(tv, "sessionAPI(sessionId, '/approval')") {
		t.Error("the authoritative refresh must query the session's approval endpoint")
	}
	// Both window builders must seed, or the create path (finalizePendingShellWindow)
	// leaves _approvalMode undefined and forwards typing the server will drop.
	// Count call sites only: the definition contains the same substring.
	seeds := strings.Count(tv, "  applyApprovalModeFromSnapshot(win, sessionId);")
	if seeds < 2 {
		t.Errorf("both window builders must seed the review UI, found %d call site(s)", seeds)
	}
}

// The review lock is an icon-only control at the head of the tab strip, and the
// queue panel outlives the terminal tab.
//
// Both facts are load-bearing. The header row is the only one visible on all four
// pane tabs, so the control has to live in it; and the panel must not sit inside
// .shell-terminal-wrap, because that wrapper is display:none while Files or
// Forwardings is open - exactly where a file write or a forward approval arrives.
// That bug shipped once: the control and the whole queue vanished on the two tabs
// whose operations are now gated.
//
// The lock sits IN the strip rather than beside the title because the strip is
// what scrolls: at a phone width the header cannot hold everything, and of the
// three ways out (shrink the name to nothing, drop a control, scroll a group) the
// scroll is the only one that keeps every control reachable. Beside the title the
// lock competed with the session name for the same fixed space instead.
//
// The control carries the mode as a glyph (open/closed lock, the same pair the
// session card uses) rather than as a labelled pill: words were the widest thing
// on the row for a one-bit state.
func TestReviewLockLeadsTheTabStripAndSurvivesEveryTab(t *testing.T) {
	tv := readAssetLF(t, "static/js/terminal-view.js")

	locks := strings.Count(tv, "SHELL_REVIEW_LOCK_HTML")
	// One definition plus one use per window template.
	if locks < 3 {
		t.Errorf("the review lock must be shared markup referenced by both templates, found %d reference(s)", locks)
	}
	lock := between(t, tv, "var SHELL_REVIEW_LOCK_HTML =", ";")
	if n := strings.Count(lock, "<button"); n != 1 {
		t.Errorf("the review lock must hold exactly one control, found %d", n)
	}
	// Icon-only: no label text, or the width that was the whole reason for the
	// change comes straight back.
	for _, gone := range []string{"shell-review-label", "Review: off", "Review: on"} {
		if strings.Contains(lock, gone) {
			t.Errorf("the lock must not carry a text label (%s): that is the pill it replaced", gone)
		}
	}
	if !strings.Contains(lock, "shell-review-lock-icon") {
		t.Error("the lock needs a dedicated icon element: applyApprovalMode swaps its glyph")
	}

	// Inside the tab strip, before the pane tab buttons: the gate leads the faces
	// it governs, and the strip is the row's scrollable part.
	if !strings.Contains(tv, "'<div class=\"shell-tab-bar\">' +") {
		t.Fatal("expected the tab strip markup")
	}
	strip := between(t, tv, "'<div class=\"shell-tab-bar\">' +", "'<button type=\"button\" class=\"shell-tab-btn active\"")
	if !strings.Contains(strip, "SHELL_REVIEW_LOCK_HTML") {
		t.Error("the review lock must be inside the tab strip, ahead of the pane tabs")
	}
	// And nowhere else, or one state would have two switches.
	if strings.Contains(tv, "SHELL_REVIEW_BAR_HTML") {
		t.Error("the old footer bar must be gone; the lock replaced it")
	}
	if strings.Contains(tv, "shell-switch-review") {
		t.Error("the session-menu review row is gone: with a scrollable strip the lock is reachable on touch")
	}
	// The title cluster must not still carry it, or it competes with the name again.
	cluster := between(t, tv, "'<div class=\"shell-window-title-cluster\">'", "'<div class=\"shell-tab-bar\">'")
	if strings.Contains(cluster, "SHELL_REVIEW_LOCK_HTML") {
		t.Error("the lock must not sit in the title cluster: there it competes with the session name for fixed space")
	}

	// The panel is a sibling of the pane panels, not a child of the terminal
	// wrapper that gets hidden on the file and forward tabs.
	content := between(t, tv, "'<div class=\"shell-window-content\"><div class=\"shell-terminal-wrap\">'", "SHELL_REVIEW_SHEET_HTML +")
	if strings.Contains(content, "SHELL_REVIEW_SHEET_HTML") {
		t.Error("the panel must not be inside .shell-terminal-wrap: that wrapper is hidden on the Files and Forwardings tabs")
	}
	if n := strings.Count(tv, "SHELL_REVIEW_SHEET_HTML +"); n < 2 {
		t.Errorf("both window templates need the review panel, found %d", n)
	}

	// The channel tabs must live inside the footer, or they will not sit on its left.
	if !regexp.MustCompile(`shell-window-footer">'\s*\+\s*\n?\s*'<div class="shell-channel-tabs">`).MatchString(tv) {
		t.Error("the channel tabs must be nested inside the footer, on its left")
	}
}

// The count lives on the lock, and only there.
//
// It was briefly badged onto the pane tab each request belonged to — a file write
// badged Files, a forward badged Forwardings — on the theory that the badge would
// say WHICH face was waiting. In use that was wrong: those tabs show the file
// listing and the forward list, NOT the queue, so a dot on them promises something
// the tab cannot deliver. You click Files, see no pending write, and the badge has
// taught you to ignore badges.
//
// The lock is the one control that opens the queue, so it is the one place a count
// means "there is a decision waiting, right here".
func TestApprovalCountLivesOnTheLockAndNotOnThePaneTabs(t *testing.T) {
	js := readAssetLF(t, "static/js/approval.js")
	tv := readAssetLF(t, "static/js/terminal-view.js")
	css := readAssetLF(t, "static/css/app.css")

	// The lock carries the count element, and the panel it opens is the queue.
	lock := between(t, tv, "var SHELL_REVIEW_LOCK_HTML =", ";")
	if !strings.Contains(lock, "shell-review-count") {
		t.Error("the lock must carry the count: it is the control that opens the queue")
	}
	if !strings.Contains(lock, `shell-review-count" hidden`) {
		t.Error("the count must start hidden: with nothing waiting there is no number to show")
	}

	// The per-face machinery is gone, and must not come back without solving the
	// problem it failed at (a tab that shows the queue it badges).
	for _, gone := range []string{"kindFace", "_approvalFaces", "shell-tab-review"} {
		if strings.Contains(js, gone) {
			t.Errorf("the pane-tab badge machinery (%s) must be gone: those tabs do not show the queue", gone)
		}
	}
	// has-review still marks the SESSION tab in the bottom tab bar. That is a
	// different surface with a different job: it is how a minimized window (whose
	// lock is display:none) says it has something waiting.
	if !strings.Contains(js, "tab.classList.toggle('has-review', !!n)") {
		t.Error("the session tab must still mark a session with a waiting decision")
	}
	if strings.Contains(css, "shell-tab-review") {
		t.Error("the pane-tab badge CSS must be gone with the code that created it")
	}

	// The paint writes BOTH the number and its visibility: showing a badge without
	// writing its text left a red "0" beside a queue that had one.
	paint := between(t, js, "function applyApprovalCounts() {", "\n}")
	if !strings.Contains(paint, "num.textContent = String(n)") {
		t.Error("applyApprovalCounts must write the lock count's number as well as its visibility")
	}
	// And it must paint the lock, not a tab.
	if !strings.Contains(paint, ".shell-review-count") {
		t.Error("applyApprovalCounts must paint the lock's count")
	}

	// Anchored to the corner so the lock keeps its 26px and the header row does not
	// reflow when a request arrives.
	count := between(t, css, ".shell-review-count {", "}")
	for _, want := range []string{"position: absolute", "right:"} {
		if !strings.Contains(count, want) {
			t.Errorf("the count must be %s so it does not change the lock's width", want)
		}
	}
	if !strings.Contains(css, ".shell-review-count[hidden] { display: none; }") {
		t.Error("the hidden count must actually be hidden")
	}
}

// The window resizes from all four corners with no drawn glyph. The glyph used
// to sit in the bottom-right over a 30px footer and swallowed clicks on the
// control underneath it; the whole corner is now the target, so nothing is
// covered and no space has to be reserved for an icon.
func TestWindowResizesFromFourCornersWithoutAGlyph(t *testing.T) {
	js := readAssetLF(t, "static/js/terminal-view.js")
	css := readAssetLF(t, "static/css/app.css")

	for _, corner := range []string{"nw", "ne", "sw", "se"} {
		if !strings.Contains(js, `data-corner="`+corner+`"`) {
			t.Errorf("missing the %s resize grip", corner)
		}
		if !strings.Contains(css, `data-corner="`+corner+`"`) {
			t.Errorf("no cursor/position rule for the %s grip", corner)
		}
	}
	// The drawn triangle is what created the collision; it must not come back.
	if strings.Contains(css, ".shell-window-resize-handle::after") {
		t.Error("the resize glyph must stay removed: it overlaps controls")
	}
	// The strip is thin on the horizontal axis and long along the edge: a square
	// block is what overlapped the header and footer rows.
	handle := regexp.MustCompile(`(?m)^\s*\.shell-window-resize-handle \{([^}]*)}`).FindStringSubmatch(css)
	if handle == nil {
		t.Fatal("the grips need a rule")
	}
	if !strings.Contains(handle[1], "width: var(--shell-resize-grip)") ||
		!strings.Contains(handle[1], "height: var(--shell-resize-len)") {
		t.Error("a grip must be a thin strip (6px wide, 44px along the edge), not a corner block")
	}
	// And those numbers must stay small enough to fit in the rows' edge padding,
	// which is what removes the need for any z-index contest with the controls.
	for _, v := range []struct{ name, want string }{
		{"--shell-resize-grip", "6px"},
		{"--shell-resize-len", "44px"},
	} {
		if !strings.Contains(css, v.name+": "+v.want) {
			t.Errorf("%s must be %s: a wider grip reaches the header buttons and the footer controls", v.name, v.want)
		}
	}
	// The old square-corner variable is gone with the block it sized.
	if strings.Contains(css, "--shell-resize-corner") || strings.Contains(css, "--shell-resize-bottom") {
		t.Error("the corner-block variables are dead now that the grips are edge strips")
	}
	// A west/north drag has to move the window, not just resize it. That logic
	// lives in the resize handler, not in the window markup.
	resize := readAssetLF(t, "static/js/ui-socket.js")
	if !strings.Contains(resize, "pendingLeft") || !strings.Contains(resize, "pendingTop") {
		t.Error("dragging a north/west grip must adjust left/top as well as the size")
	}
	if !strings.Contains(resize, "handles.forEach") {
		t.Error("every corner grip must get a pointerdown listener")
	}
}

// The corner grips must not need to fight the controls for the corner.
//
// This has been wrong in three ways, which is why the test pins the geometry
// rather than the intent:
//
//   - A 30x30 block anchored on the window corner lay inside the 34px header and
//     30px footer, so it swallowed the header's close button (which resized the
//     window instead of closing it).
//   - Moving the grips into the terminal area to dodge that put them ~35px below
//     the top edge, so grabbing the window's own corner dragged the window.
//   - Lifting the header/footer above the grips buried them instead: measured in
//     a real browser, 0% of the north grips were reachable and the south ones
//     were down to a 4-6px sliver, because those rows are full-width.
//
// What works is a strip that fits where nothing interactive lives: 6px thick,
// 44px along the edge. The header's close button is 9px in from the window edge
// and the footer's review button 13px, so a 6px strip never overlaps them and no
// stacking is needed at all.
func TestResizeGripsAreEdgeStripsNotCornerBlocks(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")
	js := readAssetLF(t, "static/js/terminal-view.js")

	// The grips belong to the window (siblings of its content), so the window's
	// own corner starts a resize. Inside .shell-terminal-wrap they miss it.
	wrap := between(t, js, `'<div class="shell-channel-empty">'`, "SHELL_WINDOW_PANELS_HTML")
	if strings.Contains(wrap, "SHELL_RESIZE_HANDLES_HTML") {
		t.Error("the grips must not be inside .shell-terminal-wrap: anchored there they miss the window's corner")
	}
	if !strings.Contains(js, "SHELL_WINDOW_PANELS_HTML +\n    SHELL_RESIZE_HANDLES_HTML;") {
		t.Error("the grips belong after the window panels, as window-level overlays")
	}

	// Each corner is pinned to its own two edges, so the strip includes the
	// corner point (a diagonal drag from there resizes both axes).
	for _, tc := range []struct{ corner, edges string }{
		{"nw", `left: 0`},
		{"ne", `right: 0`},
		{"sw", `left: 0`},
		{"se", `right: 0`},
		{"nw|ne", `top: 0`},
		{"sw|se", `bottom: 0`},
	} {
		if !strings.Contains(css, `data-corner="`+strings.Split(tc.corner, "|")[0]+`"`) {
			t.Errorf("no rule for the %s grip", tc.corner)
		}
		if !regexp.MustCompile(regexp.QuoteMeta(tc.edges)).MatchString(css) {
			t.Errorf("the %s grip must be pinned to its edge (%s)", tc.corner, tc.edges)
		}
	}
	// The thing that made the block collide: a square. Pin the thin axis, and
	// keep it under the smallest edge padding any of those controls has (9px).
	grip := regexp.MustCompile(`--shell-resize-grip: (\d+)px`).FindStringSubmatch(css)
	if grip == nil {
		t.Fatal("the grip's thickness must be stated as a variable so it can be checked")
	}
	thick, _ := strconv.Atoi(grip[1])
	if thick <= 0 || thick >= 9 {
		t.Errorf("grip thickness %dpx must stay under the 9px the header's close button leaves at the edge", thick)
	}
	// And long enough along the edge to be an easy target (the old block gave
	// 30px; the strip is longer on purpose).
	l := regexp.MustCompile(`--shell-resize-len: (\d+)px`).FindStringSubmatch(css)
	if l == nil {
		t.Fatal("the grip's length must be stated so it can be checked")
	}
	if ln, _ := strconv.Atoi(l[1]); ln < 30 {
		t.Errorf("grip length %dpx: shorter than the block it replaced, so it is a smaller target", ln)
	}
}

// The cursor at each corner must match the diagonal that corner resizes along,
// and the dragging cursor the JS sets must agree with the one the stylesheet
// shows when the pointer is merely over the grip.
//
// These were wrong in a way a screenshot cannot catch: the rules were grouped by
// axis ("the north pair gets nesw"), which gave the left two grips the same
// cursor as the right two — nw needs nwse-resize, not nesw-resize. Two files
// carry the same table (CSS for hover, ui-socket.js while dragging), so a drift
// between them makes the pointer change direction mid-drag.
func TestResizeCursorMatchesItsDiagonal(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")
	js := readAssetLF(t, "static/js/ui-socket.js")

	// nw/se run ↘↖ (nwse-resize); ne/sw run ↗↙ (nesw-resize).
	want := map[string]string{
		"nw": "nwse-resize",
		"se": "nwse-resize",
		"ne": "nesw-resize",
		"sw": "nesw-resize",
	}
	for corner, cursor := range want {
		re := regexp.MustCompile(`data-corner="` + corner + `"\] \{[^}]*cursor: ([a-z-]+)`)
		m := re.FindStringSubmatch(css)
		if m == nil {
			t.Errorf("no cursor rule for the %s grip", corner)
			continue
		}
		if m[1] != cursor {
			t.Errorf("the %s grip shows %s; it resizes along %s", corner, m[1], cursor)
		}
	}

	// The drag-time table in ui-socket.js must say the same thing, or the cursor
	// jumps the moment the drag starts.
	table := between(t, js, "var cursors = {", "};")
	for corner, cursor := range want {
		re := regexp.MustCompile(`\b` + corner + `: '([a-z-]+)'`)
		m := re.FindStringSubmatch(table)
		if m == nil {
			t.Errorf("the drag cursor table has no entry for %s", corner)
			continue
		}
		if m[1] != cursor {
			t.Errorf("the drag cursor table says %s for %s, but the grip shows %s", m[1], corner, cursor)
		}
	}
}

// The review panel floats over the terminal instead of taking height from it.
//
// The panel used to be a flex sibling of the footer (order 9, max-height 62%),
// so opening it shrank the terminal by its own height and the PTY was refitted
// on every open. The terminal scrolled under the cursor each time a human
// checked the queue — the worst possible place to move the screen.
func TestReviewPanelFloatsAndDoesNotReflowTheTerminal(t *testing.T) {
	js := readAssetLF(t, "static/js/terminal-view.js")
	css := readAssetLF(t, "static/css/app.css")

	if !strings.Contains(js, "SHELL_REVIEW_SHEET_HTML") {
		t.Fatal("expected a shared review panel markup constant")
	}
	// Both window templates get the panel, or it is missing on one path.
	if n := strings.Count(js, "SHELL_REVIEW_SHEET_HTML +"); n < 2 {
		t.Errorf("both window templates need the review panel, found %d", n)
	}
	if strings.Contains(js, "shell-review-inbox") {
		t.Error("the separate inbox button is gone; the queue lives in the panel")
	}
	// One reviewer, so no threshold field: termcp cannot prove who an approver
	// is until accounts exist, and a count over claimed names is not a control.
	if strings.Contains(js, "shell-review-need") {
		t.Error("the approver-count field is gone with the one-reviewer model")
	}

	// The panel is absolutely positioned, capped and scrollable: it takes no
	// layout height, so nothing above or below it moves.
	panel := between(t, css, ".shell-review-sheet {", "}")
	for _, want := range []string{"position: absolute", "max-height"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the panel must be %s so it cannot resize the terminal", want)
		}
	}
	// A refit on open is the tell that it takes height again: out of flow, the
	// terminal's geometry does not change.
	if !strings.Contains(js, "function setReviewSheetOpen(win, open) {") {
		t.Fatal("expected setReviewSheetOpen to own the open/close behaviour")
	}
	open := between(t, js, "function setReviewSheetOpen(win, open) {", "}")
	if strings.Contains(open, "_fitWinSoon(win)") {
		t.Error("opening the panel must not refit the terminal: it takes no height, so a refit means it does")
	}
	// A floating panel needs a positioned ancestor, and the wrapper it is a child
	// of is the one that must carry it.
	if !regexp.MustCompile(`\.shell-terminal-wrap \{[^}]*position: relative`).MatchString(css) {
		t.Error("the panel's containing block (.shell-terminal-wrap) must be positioned")
	}
}

// The mode is one bit, so its control in the panel is one small switch — not the
// full-width "Turn on review" / "Turn off review" button that used to sit there.
//
// The old button was the panel's only content besides the queue, so it read as
// the panel's primary action and took a row of its own for a boolean. The switch
// is the same control the rest of the app uses for a mode, and its state is
// legible without opening anything (the footer button carries it too).
func TestReviewModeIsASmallSwitchNotABigButton(t *testing.T) {
	js := readAssetLF(t, "static/js/terminal-view.js")
	css := readAssetLF(t, "static/css/app.css")

	sheet := between(t, js, "var SHELL_REVIEW_SHEET_HTML =", "function openShellWindow")
	// A checkbox with role=switch: keyboard and screen-reader behaviour come for
	// free, and the visual track is a span next to it.
	if !strings.Contains(sheet, `type="checkbox"`) || !strings.Contains(sheet, `role="switch"`) {
		t.Error("the panel's mode control must be a checkbox with role=switch")
	}
	if !strings.Contains(sheet, "shell-review-switch-track") {
		t.Error("the switch needs its track element to draw the toggle")
	}
	// The big button is gone, and so is its label form.
	for _, gone := range []string{"Turn on review", "Turn off review", "shell-review-gate"} {
		if strings.Contains(sheet, gone) {
			t.Errorf("the panel still carries the old full-size button: %s", gone)
		}
	}
	// Small: a 26x14 track, not a padded button row.
	track := between(t, css, ".shell-review-switch-track {", "}")
	if !strings.Contains(track, "width: 26px") || !strings.Contains(track, "height: 14px") {
		t.Error("the switch must be a small track, not a button-sized control")
	}
	// The checkbox is visually hidden but still focusable and hit-testable via its
	// label, so the whole row is the target.
	toggle := between(t, css, ".shell-review-toggle {", "}")
	if !strings.Contains(toggle, "opacity: 0") || !strings.Contains(toggle, "position: absolute") {
		t.Error("the checkbox must be visually hidden behind the track")
	}
	// Checked state is drawn from the input, so the switch cannot show a state the
	// model disagrees with.
	if !strings.Contains(css, ".shell-review-toggle:checked + .shell-review-switch-track") {
		t.Error("the switch's on-state must be driven by :checked")
	}
	// And the JS drives the checkbox, not a label string.
	if !strings.Contains(js, "tg.checked = !!on") {
		t.Error("applyApprovalMode must set the checkbox, not its text")
	}
}

// A queued command is what a human has to see: a read-only command box with the
// two buttons BESIDE it. There is no field to edit, because the command was
// written by the AI and the decision is to accept or refuse it.
//
// The buttons sit to the right, stacked Accept-over-Reject: two buttons stacked
// are about the height of the two-line command box beside them, so the decision
// column fills the row instead of leaving a gap next to a one-line command.
func TestApprovalRowIsReadOnlyCommandPlusTwoButtonsBesideIt(t *testing.T) {
	js := readAssetLF(t, "static/js/approval.js")
	render := between(t, js, "function renderApprovalItem(it) {", "function decideApproval(")

	if strings.Contains(render, "<textarea") || strings.Contains(render, "contenteditable") {
		t.Error("the command must not be editable: the human decides, the AI writes")
	}
	if n := strings.Count(render, "<button"); n != 2 {
		t.Errorf("a pending row must hold exactly two buttons (accept, reject), found %d", n)
	}
	// The two labels are translated, so the row is checked for the two catalog
	// keys (and the buttons are counted above), not for English literals.
	for _, want := range []string{"t('review.accept')", "t('review.reject')"} {
		if !strings.Contains(render, want) {
			t.Errorf("the row is missing the %s button", want)
		}
	}
	// The command and the actions are siblings inside one flex row, which is what
	// puts them side by side. A structure where the actions are a sibling of the
	// ROW instead would drop them below it.
	if !strings.Contains(render, "approval-row") {
		t.Fatal("the command and the actions need a shared flex row")
	}
	// The row is one expression: `main` (command + keys) and `actions` are	the
	// two operands inside it, which is what puts them on one line.
	row := between(t, render, "'<div class=\"approval-row\">'", "'</div>'")
	if !strings.Contains(row, "main") {
		t.Error("the row must carry the command column")
	}
	if !strings.Contains(row, "actions") {
		t.Error("the row must carry the actions, side by side with the command")
	}

	css := readAssetLF(t, "static/css/app.css")

	eq := regexp.MustCompile(`(?m)^\s*\.approval-row \{([^}]*)}`).FindStringSubmatch(css)
	if eq == nil {
		t.Fatal("the row needs a flex rule, or the buttons fall under the command")
	}
	if !strings.Contains(eq[1], "display: flex") {
		t.Error("the row must be a flex container so the buttons sit beside the command")
	}
	// The command column shrinks (long commands scroll inside it); the actions do
	// not, or the buttons would be squeezed to nothing instead of the command
	// giving way.
	main := regexp.MustCompile(`(?m)^\s*\.approval-main \{([^}]*)}`).FindStringSubmatch(css)
	if main == nil || !strings.Contains(main[1], "min-width: 0") {
		t.Error("the command column must be shrinkable (min-width: 0), or a long command squeezes the buttons")
	}
	acts := regexp.MustCompile(`(?m)^\s*\.approval-row \.approval-actions \{([^}]*)}`).FindStringSubmatch(css)
	if acts == nil || !strings.Contains(acts[1], "flex: 0 0 auto") {
		t.Error("the actions must not shrink: they are the thing the human came to click")
	}
	// The command box is exactly two lines tall and scrolls inside itself: long
	// enough to read a wrapped command, fixed enough that it pairs with the
	// stacked buttons and never pushes the panel open.
	code := between(t, css, ".approval-code {", "}")
	if !strings.Contains(code, "height: calc(3em + 14px)") {
		t.Error("the command box must be exactly two lines tall (border-box: 2 × 1.5em of text plus the 14px of padding+border)")
	}
	if !strings.Contains(code, "line-height: 1.5") {
		t.Error("the box height is stated in em of text, so the line-height must be stated too")
	}
	if !strings.Contains(code, "overflow: auto") {
		t.Error("a command longer than the box must scroll inside it")
	}
	if strings.Contains(code, "max-height") {
		t.Error("a max-height box grows with a short command; the two-line height is what keeps the row stable")
	}
	// The decision column is vertical, so Accept sits over Reject.
	if !strings.Contains(acts[1], "flex-direction: column") {
		t.Error("Accept and Reject must be stacked in the decision column")
	}
	// The stacked pair must not be pushed down away from the command's first line.
	if strings.Contains(acts[1], "margin-top") {
		t.Error("the decision column must start level with the command, not offset from it")
	}
	// On a phone the pair stacks under the command and goes back side by side: a
	// narrow screen has no room for a stacked pair beside the command.
	touch := between(t, css, "@media (pointer: coarse) and (hover: none) {\n    .approval-row", "}")
	if !strings.Contains(touch, "flex-direction: column") {
		t.Error("touch layout must stack the row: side-by-side does not fit a phone")
	}
}

// Enter is the key that ends a command line, so showing it as a chip is noise:
// it never varies, and the request already IS a command. Only keys a human could
// not infer are shown, and a request whose only key was Enter renders no chips.
func TestApprovalHidesTheEnterChip(t *testing.T) {
	js := readAssetLF(t, "static/js/approval.js")
	if !strings.Contains(js, "function approvalChipsHtml(keys)") {
		t.Fatal("the chip list needs one builder, so the Enter filter cannot be bypassed")
	}
	body := between(t, js, "function approvalChipsHtml(keys) {", "\n}")
	if !strings.Contains(body, "!== 'enter'") {
		t.Error("Enter must be filtered out of the chips")
	}
	// The render must go through the builder: an inline map over req.keys would
	// bring the Enter chip back while the builder sat unused.
	render := between(t, js, "function renderApprovalItem(it) {", "function decideApproval(")
	if !strings.Contains(render, "approvalChipsHtml(req.keys)") {
		t.Error("the row must build its chips through approvalChipsHtml")
	}
	// A key that does carry information must still be shown.
	if !strings.Contains(js, "keyLabel(k)") {
		t.Error("named keys other than Enter must still render")
	}
}

// A session card's pending-review badge must survive the grid rebuilding itself.
//
// The grid is re-rendered from scratch on every session frame, and submitting a
// request emits one of those frames (the manager's list-change broadcast). When
// the badge was only painted by a separate pass, the very frame that announced
// the request wiped it: measured in a browser, the tab bar read "1" while the
// card stayed empty. Rendering the badge as part of the tile is what fixes it,
// so the assertion is on the tile builder rather than on any paint call.
func TestSessionTileRendersItsOwnReviewBadge(t *testing.T) {
	sess := readAssetLF(t, "static/js/sessions.js")

	if !strings.Contains(sess, "function reviewBadgeHtml(sid)") {
		t.Fatal("the tile needs a badge builder; a separate paint pass is wiped by the next re-render")
	}
	// The tile must call it while building the card, not leave a static empty div.
	if !strings.Contains(sess, "reviewBadgeHtml(sid) +") {
		t.Error("the tile markup must include reviewBadgeHtml(sid), or the badge only exists until the next frame")
	}
	if strings.Contains(sess, `'<div class="sess-review" hidden></div>' +`) {
		t.Error("the tile still hardcodes an empty hidden badge; that is the shape that got wiped")
	}

	// It reads the counts already in hand, so a re-render is self-sufficient.
	// The body is taken up to the next function, not to the first "}": the early
	// return's closing brace comes first and would cut the slice short.
	body := between(t, sess, "reviewBadgeHtml(sid) {", "\n}")
	if !strings.Contains(body, "_approvalCounts") {
		t.Error("the badge must render from _approvalCounts, or a re-render has nothing to draw from")
	}
	if !strings.Contains(body, "hidden") {
		t.Error("with nothing waiting the badge must be hidden rather than showing a 0")
	}
	// The label is translated, so the assertion is on the catalog key rather than
	// on an English literal: what matters is that the badge renders a sentence
	// around the number, and that the sentence is not merely the number again.
	if !strings.Contains(body, "tCount('review.badge.one', 'review.badge.other'") {
		t.Error("the badge must render a translated label, not a bare number")
	}
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		v := loadCatalogs(t)[lang]["review.badge.other"]
		if !strings.Contains(v, "{count}") || strings.TrimSpace(strings.Replace(v, "{count}", "", 1)) == "" {
			t.Errorf("%s: review.badge.other must wrap {count} in words, got %q", lang, v)
		}
	}

	// The paint pass still has to update an already-built tile (a decision made in
	// another tab changes the number without a re-render).
	appr := readAssetLF(t, "static/js/approval.js")
	paint := between(t, appr, "function applyApprovalCounts() {", "\n}")
	if !strings.Contains(paint, "badge.textContent") {
		t.Error("applyApprovalCounts must write the tile badge's text, not only toggle it")
	}
	// And the same for the lock's count. It once showed a red 0 because only its
	// visibility was written and never its number.
	if !strings.Contains(paint, "num.textContent = String(n)") {
		t.Error("applyApprovalCounts must write the lock count's number as well as its visibility")
	}
}

// A notification that names a session is a way to get to it. The click has to go
// through focusSessionWindow, which is the one entry point that covers every
// state the session can be in: no window yet, minimized, collapsed, tiled.
func TestNotificationClickOpensItsSession(t *testing.T) {
	js := readAssetLF(t, "static/js/util.js")
	body := between(t, js, "function showUiNotify(n) {", "function displaySessionShort(")

	if !strings.Contains(body, "focusSessionWindow(") {
		t.Fatal("clicking a notification must open its session through focusSessionWindow")
	}
	if !strings.Contains(body, "ui-notify-clickable") {
		t.Error("the clickable affordance must be added for a notification that names a session")
	}
	// Only notifications with a session may look clickable: one without has
	// nowhere to go.
	if !strings.Contains(body, "if (sid)") {
		t.Error("the click handling must be conditional on having a session")
	}
	// Clicking the close button must dismiss rather than also navigating.
	if !strings.Contains(body, "closest('.ui-notify-close')") {
		t.Error("the close button must not trigger the open-session action")
	}
	// Keyboard users need the same action.
	if !strings.Contains(body, "keydown") {
		t.Error("the card is focusable, so Enter/Space must open the session too")
	}
}

// The mobile header's session switcher must list sessions from the server
// snapshot, not the windows this browser happens to have open.
//
// Listing windows made it a view of local client state: a session created from
// another terminal (or another device) was in the server list and in the tile
// grid, but had no window here, so it never showed up in the switcher. The
// snapshot is already pushed over the WebSocket, so reading it keeps the menu
// live without a second source of truth.
func TestMobileSwitcherListsSessionsNotOpenWindows(t *testing.T) {
	tv := readAssetLF(t, "static/js/terminal-view.js")
	menu := between(t, tv, "function openSessionSwitchMenu(anchorBtn) {", "function setupMobileSessionSwitcher(win)")

	if strings.Contains(menu, "liveSessionWindows()") {
		t.Error("the switcher must read sessions, not the windows open in this tab")
	}
	if !strings.Contains(menu, "window._lastSessionsSnapshot") {
		t.Fatal("the switcher must read the pushed session snapshot")
	}
	// The filter must keep every session, not only the ones with a window here.
	// Checking for the snapshot string alone is not enough: a filter that then
	// drops window-less sessions reintroduces the bug while the name is intact.
	if regexp.MustCompile(`filter\(function \(s\) \{\s*return s && s\.id &&`).MatchString(menu) {
		t.Error("the session filter must not require an open window; that hid sessions created elsewhere")
	}
	// Opening from the switcher has to work for a session with no window yet,
	// which is the case the old code silently dropped.
	if !strings.Contains(menu, "focusSessionWindow(") {
		t.Error("selecting a session must open or focus it through focusSessionWindow")
	}
	// Dead sessions stay listed so the switcher agrees with the tile grid.
	if !strings.Contains(menu, "readOnly") {
		t.Error("a dead session must open read-only rather than be omitted")
	}
	// The helper that filtered to windows is gone; a leftover copy would invite
	// the same bug back.
	if strings.Contains(tv, "function liveSessionWindows()") {
		t.Error("liveSessionWindows is obsolete and must not be reintroduced")
	}
}

// A shared markup constant must be a single valid expression. Concatenation is
// easy to lose when such a block is rewritten, and a broken one is not a syntax
// error: JS treats adjacent string literals as one expression and silently keeps
// the first, so the element renders empty. That is exactly how the review sheet
// shipped as an empty div while every test passed.
func TestSharedMarkupConstantsAreWellFormed(t *testing.T) {
	tv := readAssetLF(t, "static/js/terminal-view.js")
	for _, name := range []string{"SHELL_REVIEW_LOCK_HTML", "SHELL_REVIEW_SHEET_HTML", "SHELL_RESIZE_HANDLES_HTML", "SHELL_WINDOW_PANELS_HTML"} {
		body := between(t, tv, "var "+name+" =", ";")
		// Drop the declaration itself: only the expression lines are checked.
		if i := strings.Index(body, "\n"); i >= 0 {
			body = body[i+1:]
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		// Every line but the last must continue the expression, or the literal is
		// silently cut short at the line that does not.
		lines := strings.Split(strings.TrimSpace(body), "\n")
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if trimmed == "" {
				continue
			}
			last := i == len(lines)-1
			if !last && !strings.HasSuffix(trimmed, "+") && !strings.HasSuffix(trimmed, ",") && !strings.HasSuffix(trimmed, "(") {
				t.Errorf("%s line %d does not continue the expression, so the markup is cut short: %q", name, i+1, trimmed)
			}
		}
		// The sheet must actually contain its controls.
		if name == "SHELL_REVIEW_SHEET_HTML" {
			for _, want := range []string{"shell-review-toggle", "shell-review-list", "shell-review-sheet-close"} {
				if !strings.Contains(body, want) {
					t.Errorf("the sheet markup is missing %s", want)
				}
			}
		}
	}
}

// The operator's keyboard must keep working while review is on.
//
// Review gates what the AI sends, and the AI's surface is MCP. Dropping the
// WebSocket stream whenever the session gated locked the human out of the very
// terminal they were watching — they could not interrupt a running command or
// type a reply, and the session looked frozen rather than protected.
//
// The assertion is on the handler's code, not on prose: a comment explaining the
// rule is what belongs there, so comments are stripped first.
func TestReviewDoesNotGateTheOperatorTerminal(t *testing.T) {
	ws := readGoSource(t, "ws.go")
	body := between(t, ws, "func (c *uiWS) handleWSInput(msg *wsClientMsg) {", "\nfunc (c *uiWS)")
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(body, "")
	if strings.Contains(code, "RequiresApproval") {
		t.Error("handleWSInput must not consult review mode: the operator's terminal is not what review gates")
	}
	// It must still write the bytes through, or "not gated" would mean "silently
	// dropped" — the same bug wearing a different mask.
	if !strings.Contains(code, "SendTerminalBytes") {
		t.Error("handleWSInput must still forward the operator's keystrokes")
	}
	// And the REST mirror of the human's terminal: the Web UI's own terminal
	// uses the socket, but a script driving it by hand is the operator too.
	io := readGoSource(t, "shellio.go")
	inBody := between(t, io, "func (h *Handler) handleShellInput(w http.ResponseWriter, r *http.Request) {", "\nfunc (h *Handler) handleShellKey")
	inCode := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(inBody, "")
	if strings.Contains(inCode, "RequiresApproval") {
		t.Error("handleShellInput must not gate the REST terminal: it is the operator's surface too")
	}
}

// readGoSource reads a package source file for a structural assertion. The
// embedded FS only holds the browser assets, so a Go file has to come from disk.
func readGoSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return normalizeNL(string(b))
}

// On a phone the header cannot hold every control at its natural width, so the
// tab strip scrolls and the session name keeps a floor.
//
// This replaces an earlier decision that hid the lock on touch and routed review
// through the session menu. That was the right answer to "the lock eats the
// session name" while the lock sat beside the name, competing for the same fixed
// space. In the strip the trade is different: the strip is the part of the row
// that gives way, so the lock can stay and the strip scrolls.
//
// Collapse goes away instead, and that is not cosmetic: on a full-screen phone
// window collapsing leaves a header bar over the page with the terminal gone,
// which is a state with no way back that a phone user hits by accident.
func TestPhoneHeaderScrollsTheStripAndDropsCollapse(t *testing.T) {
	css := readAssetLF(t, "static/css/app.css")
	js := readAssetLF(t, "static/js/terminal-view.js")

	touch := between(t, css, "@media (pointer: coarse) and (hover: none) {", "  .shell-header-controls {")
	if !strings.Contains(touch, ".shell-window-header-btns button.shell-window-collapse-btn { display: none; }") {
		t.Error("collapse must be hidden on touch: a collapsed full-screen window is a dead end")
	}
	if !strings.Contains(touch, ".shell-window-title-cluster { min-width: 44px; }") {
		t.Error("the session name needs a floor on touch, or the strip has nothing to be wider than")
	}
	// The lock is NOT hidden any more: the strip is what gives way.
	if strings.Contains(touch, ".shell-review-lock { display: none; }") {
		t.Error("the lock must be shown on touch now that the strip scrolls")
	}

	// The strip is the flexible, scrollable one.
	strip := between(t, css, ".shell-tab-bar {", "}")
	for _, want := range []string{"flex: 0 1 auto", "min-width: 0", "overflow-x: auto"} {
		if !strings.Contains(strip, want) {
			t.Errorf("the tab strip must be %s so it gives way instead of the name", want)
		}
	}
	// A visible scrollbar on a 34px row would eat the glyphs it exists to reach.
	if !strings.Contains(css, ".shell-tab-bar::-webkit-scrollbar { display: none; }") {
		t.Error("the strip's scrollbar must be hidden: the row is too short for one")
	}
	// The lock is a fixed glyph: a shrunk one is a smeared glyph.
	lock := between(t, css, ".shell-review-lock {", "}")
	if !strings.Contains(lock, "flex: 0 0 auto") {
		t.Error("the lock must not shrink or grow on the strip")
	}
	// One entrance, not two: with the lock reachable, the menu row is redundant.
	if strings.Contains(js, "shell-switch-review") {
		t.Error("the session-menu review row must be gone: the lock is the single entrance")
	}
}

// A notification about a pending decision must land ON the decision.
//
// Landing on the session and leaving the user to find the queue is the step this
// click exists to remove — and the case it matters most for is the minimized
// window, where the notification is often the only signal that anything is
// waiting at all.
//
// The flag is what makes it work for both entrances: showUiNotify is shared with
// the ordinary ui_notify path, so approval.js opts in per notification rather
// than the click guessing from the title.
func TestApprovalNotificationClickOpensTheReviewPanel(t *testing.T) {
	util := readAssetLF(t, "static/js/util.js")
	appr := readAssetLF(t, "static/js/approval.js")

	if !strings.Contains(util, "if (n.open_review) {") {
		t.Fatal("the notification click must honour an open_review flag")
	}
	// It must open the panel on the window it just focused, and cope with a window
	// that had to be CREATED first (a session with no window yet is built by the
	// focus call, so the panel cannot be opened before that returns).
	open := between(t, util, "if (n.open_review) {", "\n  }\n")
	if !strings.Contains(open, "setReviewSheetOpen(") {
		t.Error("open_review must open the review panel")
	}
	if !strings.Contains(open, "_placeholder") {
		t.Error("open_review must wait for a freshly created window: a placeholder has no panel yet")
	}
	// approval.js is the entrance that sets the flag.
	if !strings.Contains(appr, "open_review: true") {
		t.Error("the approval event must ask for the panel to open")
	}
	ev := between(t, appr, "function onApprovalEvent(payload) {", "\n}")
	if !strings.Contains(ev, "open_review") {
		t.Error("the flag must be set on the approval notification itself")
	}
}

// A notification must describe the thing it is asking about, for every shape of
// request — and it must come from the request itself, not be rebuilt here.
//
// The toast used to rebuild its line from Text and Keys with a fallback to the
// literal "(empty)". A file transfer and a port forward have NEITHER (they carry
// Summary), so every operation notification said "(empty)": something is waiting,
// and the reviewer is not told what.
//
// The fix is not a better fallback but the removal of the rebuild: Summary is the
// field that describes a request to a person, Submit refuses a request without
// one, and it is the single constructor. So this test drives the real Submit path
// and checks that what comes out is what a toast shows — for both request shapes.
func TestApprovalToastShowsTheRequestsOwnSummary(t *testing.T) {
	// A command line: Submit derives the summary from text + keys.
	q := approval.NewQueue("sess-1", approval.Options{})
	defer q.Close("test done")
	cmdID, err := q.SubmitShellInput("shell-1", "mcp", "rm -rf /tmp/x", []string{"enter"})
	if err != nil {
		t.Fatalf("submit a command line: %v", err)
	}
	cmd, err := q.Get(cmdID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := approvalSummary(cmd); !strings.Contains(got, "rm -rf /tmp/x") {
		t.Errorf("a command line's toast = %q, want it to name the command", got)
	}

	// An operation: no text, no keys, just a summary — the shape that used to
	// toast "(empty)".
	opID, err := q.Submit(approval.Submission{
		Source:  "mcp",
		Kind:    approval.KindFileWrite,
		Summary: "write 2.0 KB to /etc/hosts",
		Payload: []byte(`{"tool":"file_write","args":{"remote_path":"/etc/hosts"}}`),
	})
	if err != nil {
		t.Fatalf("submit an operation: %v", err)
	}
	op, err := q.Get(opID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := approvalSummary(op)
	if got != "write 2.0 KB to /etc/hosts" {
		t.Errorf("an operation's toast = %q, want its summary", got)
	}
	if strings.Contains(got, "(empty)") {
		t.Errorf("the toast says %q — the reviewer is told something is waiting but not what", got)
	}

	// A request with no summary cannot exist: Submit is the only constructor and
	// it refuses one. That is what lets approvalSummary be a plain field read with
	// no fallback — and a blank toast would be worse than "(empty)", because it
	// looks like a rendering glitch instead of a missing description.
	if _, err := q.Submit(approval.Submission{Source: "mcp", Kind: approval.KindFileWrite, Payload: []byte("{}")}); err == nil {
		t.Error("Submit must refuse a request with no summary: the toast has nothing else to show")
	}

	// And the panel renders the same field, so the two cannot disagree.
	js := readAssetLF(t, "static/js/approval.js")
	render := between(t, js, "function renderApprovalItem(it) {", "\n}")
	if !strings.Contains(render, "req.summary") {
		t.Error("the panel must render the operation's summary")
	}
}
