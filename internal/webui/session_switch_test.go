package webui

import (
	"io/fs"
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
	// Session-based entry points resolve the session first; the tab bar already
	// owns its window (including a pending dial without a session id), and a tab for
	// a session with no window opens one through the resolver.
	for _, tc := range []struct {
		name, body, want string
	}{
		{"session tile", sessions, "focusSessionWindow("},
		{"switcher item", termView, "focusSessionWindow("},
	} {
		if !strings.Contains(tc.body, tc.want) {
			t.Errorf("%s should call %s", tc.name, tc.want)
		}
	}
	// …and a tab that OWNS its window must reach the normaliser without a session
	// lookup, because a status card's _sid is the extension's NAME (resolving it as a
	// session would dial a terminal named after the extension) and a pending dial has
	// no id to resolve at all. A tab with no window is the opposite case and goes
	// through the resolver — see TestAWindowlessTabOpensTheSessionItStandsFor.
	tabBody := between(t, shellWins, "var order = [];", "order.forEach(function (t) { scroll.appendChild(t); });")
	if !strings.Contains(tabBody, "focusShellWindow(cur.win);") {
		t.Error("clicking a tab must raise the window it owns")
	}
	/* The resolver belongs to the ONE branch that has no window to raise. A lookup
	   on the window path is the bug this pins: a status card's `_sid` is a name, and
	   a dial has no id yet. */
	if n := strings.Count(tabBody, "focusSessionWindow("); n != 1 {
		t.Errorf("the tab click resolves a session %d time(s); exactly the window-less branch may", n)
	}
	if !strings.Contains(between(t, tabBody, "if (!cur || !cur.win) {", "focusShellWindow(cur.win);"), "focusSessionWindow(") {
		t.Error("the resolver must sit inside the window-less branch")
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
// TestTheHeaderGlyphTicksTheWindow pins what the monitor glyph does now.
//
// It used to be two things at once: decoration on desktop (a drawing that did
// nothing when pressed) and, on touch, a menu of the server's sessions. Both were
// wrong for the same reason — a monitor glyph reads as "this screen", so the one
// action it suggests is picking it. The dock's tick already existed for that, so
// the glyph drives the same component: two views of one control rather than a
// second control with its own meaning.
//
// The mobile menu is gone with it. It listed sessions that had no window here, but
// raising a window is a click on its dock tab, so the menu cost a hidden meaning on
// a picture of a screen and bought a list the dock already shows.
func TestTheHeaderGlyphTicksTheWindow(t *testing.T) {
	termView := readTerminalJS(t)
	setup := between(t, termView, "function setupMobileSessionSwitcher", "\n}\n")

	if !strings.Contains(setup, ".shell-header-icon") {
		t.Error("the tick binds to the existing header monitor glyph")
	}
	// It drives the dock's selection, which is what makes the glyph and the dock's
	// own tick the same control.
	if !strings.Contains(setup, "_dockBatch.toggleSelection") {
		t.Error("the glyph must tick through the shared batch component, not its own set")
	}
	// No viewport gate: the glyph means one thing everywhere. A gate here would be
	// the old bug — decoration on desktop, action on touch.
	if strings.Contains(setup, "isMobileViewport()") {
		t.Error("the glyph must not change meaning by viewport; it is a tick everywhere")
	}
	// And no menu: the function that drew it is gone, so a leftover call would be a
	// runtime error rather than a feature.
	if strings.Contains(termView, "function openSessionSwitchMenu(") {
		t.Error("the session-switch menu is gone; the dock lists the windows")
	}
	if strings.Contains(termView, "openSessionSwitchMenu(") {
		t.Error("nothing may call openSessionSwitchMenu: the function no longer exists")
	}
	if !strings.Contains(termView, "win.aria.select") {
		t.Error("the glyph needs an accessible name describing the tick")
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

	// Picking a session has to undo whatever state the window was left in, so it
	// routes through the shared normaliser — the same steps the tab bar needs for a
	// window it already owns (a pending dial has no session id yet, and a status
	// card's _sid is the extension's NAME, so neither can go through a session
	// lookup).
	if !strings.Contains(body, "focusShellWindow(ex, opts)") {
		t.Error("focusSessionWindow should normalise and raise through focusShellWindow")
	}
	// The normaliser itself owns the steps. Asserting them here rather than in
	// focusSessionWindow's own body keeps this test about the BEHAVIOUR (a window in
	// any of those states ends up raised) instead of about which function spells it
	// out — the split that made this test fail when the steps moved.
	normalise := between(t, shellWins, "function focusShellWindow", "\n}\n")
	for _, want := range []string{"restoreShellWindow(ex)", "expandShellWindow(ex)", "bringShellWindowToFront(ex)"} {
		if !strings.Contains(normalise, want) {
			t.Errorf("focusShellWindow should call %s", want)
		}
	}
	// A window raised while the tab bar's global "hide all" is on must come back
	// into view, or the window the user just picked stays invisible.
	if !strings.Contains(normalise, "showAllWindows()") {
		t.Error("focusShellWindow should clear the tab bar's hide-all state")
	}
	// opts travel with the window: the extension panel asks for one extension's
	// channel, and dropping them would land on whichever tab was last active.
	if !strings.Contains(normalise, "opts.shell") || !strings.Contains(normalise, "opts.readOnly") {
		t.Error("focusShellWindow must honour opts.shell and opts.readOnly")
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
// climbing past the drawer's rung would put terminals over the entries drawer.
//
// Both numbers are now NAMED rungs from z-layers.css, and the JS reads them
// through zRung() instead of restating them — a copy in JS cannot disagree with
// the stylesheet out loud, so the two drift the first time either moves.
func TestFullscreenZIndexIsHandedOverNotIncremented(t *testing.T) {
	util := readAssetLF(t, "static/js/util.js")
	body := between(t, util, "function bringShellWindowToFront", "\n}\n")

	for _, want := range []string{"fullscreenZ()", "win-fullscreen"} {
		if !strings.Contains(body, want) {
			t.Errorf("bringShellWindowToFront should reference %s", want)
		}
	}
	// The rung comes from the ladder, not from a literal in this file.
	if !strings.Contains(util, "function fullscreenZ() { return zRung('--z-fullscreen'") {
		t.Error("the full-screen z-index must be read from the --z-fullscreen rung")
	}
	if strings.Contains(util, "var FULLSCREEN_Z =") {
		t.Error("a JS copy of the full-screen rung is a second source for a stylesheet decision")
	}
	// No increment-and-store counter for the full-screen path.
	if strings.Contains(body, "_shellFullZSeq") {
		t.Error("the full-screen z-index must not be an incrementing counter")
	}

	// The rung has to sit between the dock and the drawer, or the terminal hides
	// the drawer (or sinks behind the dock). The dock is what the floating tab bar
	// became: it is pinned to the viewport bottom, so the full-screen window must
	// still paint over it. The VALUES live in z-layers.css; the rules that use
	// them just name the rung.
	css := readAppCSS(t)
	full := between(t, css, ".shell-window.win-fullscreen {", "}")
	if !strings.Contains(full, "z-index: var(--z-fullscreen)") {
		t.Errorf("the full-screen rule should use the full-screen rung; got %q", strings.TrimSpace(full))
	}
	// The dock's stacking is declared in workspace.css beside the rest of its
	// geometry. Read that sheet rather than the bundle: terminal.css also carries
	// a `.sessdock` rule (the mobile hide), and the bundle concatenates in
	// manifest order, so a bare lookup would land on the wrong one.
	workspaceCSS := readAssetLF(t, "static/css/workspace.css")
	dock := between(t, workspaceCSS, ".sessdock {", "}")
	if !strings.Contains(dock, "z-index: var(--z-dock)") {
		t.Error("the dock should use the dock rung, below the full-screen window")
	}
	// And the order those rungs encode is what the paragraph above claims.
	ladder := readAssetLF(t, "static/css/z-layers.css")
	if rungValue(t, ladder, "--z-dock") >= rungValue(t, ladder, "--z-fullscreen") {
		t.Error("the full-screen rung must be above the dock's")
	}
	if rungValue(t, ladder, "--z-fullscreen") >= rungValue(t, ladder, "--z-drawer") {
		t.Error("the full-screen rung must stay below the drawer's, or a terminal hides the drawer")
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
	// sessionBelongsToNode and runningSessionsForNode are contiguous: the slice runs
	// from the first to the comment that opens the repaint helper.
	state := between(t, body, "function sessionBelongsToNode(", "\n/** Repaint the node cards")
	// The count checks below depend on skipping a subsystem's own session, and that
	// rule lives with the plates (sessions.js reads it too): the page's own
	// predicate comes along rather than a restatement of it.
	sessions := readAssetLF(t, "static/js/sessions.js")
	hidden := between(t, sessions, "function isHiddenSession(", "\n")

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

function runningFor(sessions, wins) {
  sandbox.sessions = sessions;
  sandbox.wins = wins || [];
  vm.runInContext('window._lastSessionsSnapshot = sessions; window.wins = wins;', sandbox);
  // A string, because check() compares with ===.
  return vm.runInContext('runningSessionsForNode("prod").map(function (s) { return s.id; }).join(",")', sandbox);
}

// What the card counts: live sessions on THIS profile, and nothing else. A dead
// session is history (the archive keeps it, the count does not), a session on
// another host is another host's, and a subsystem's own connection — an
// extension's session, which carries this profile's name — is not this node's
// session to report.
check('a running session is counted', runningFor([{ id: 'y', name: 'x', ssh_config: 'prod', status: 'running' }], []), 'y');
check('an archived session is not', runningFor([{ id: 'z', name: 'x', ssh_config: 'prod', status: 'exited' }], []), '');
check('another host is not counted', runningFor([{ id: 'w', name: 'prod', ssh_config: 'staging', status: 'running' }], []), '');
check('a hidden session is not counted',
  runningFor([{ id: 'h', name: 'ext:prod', ssh_config: 'prod', status: 'running', namespace: 'ext:prod' }], []), '');
check('a legacy record falls back to its name',
  runningFor([{ id: 'l', name: 'prod', status: 'running' }], []), 'l');

console.log(bad === 0 ? 'ATTRIBUTION OK' : bad + ' attribution failure(s)');
process.exit(bad === 0 ? 0 : 1);
`
	tmp := t.TempDir()
	predicatePath := filepath.Join(tmp, "predicate.js")
	if err := os.WriteFile(predicatePath, []byte(hidden+"\n"+state), 0o600); err != nil {
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
//
// Failure messages carry CONTEXT, because "marker not found" is the least useful
// sentence a sliced-source test can print: it names a string the author already
// knows, and nothing about the file it was looked for in. When the terminator is
// missing the remainder is printed — the commonest cause is a function whose body
// closes at a different indent than "\n}\n", and seeing the real shape is what
// turns a hunt into a one-line fix.
func between(t *testing.T, src, marker, end string) string {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("marker %q not found; %s", marker, nearestInSource(src, marker))
	}
	rest := src[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("terminator %q not found after marker %q; the slice runs to the end of %s. First 40 lines after the marker:\n%s",
			end, marker, linesOf(src), headLines(rest, 40))
	}
	return rest[:j]
}

// nearestInSource describes why a marker was not found, in terms that shorten the
// hunt: the lines that look like the one asked for.
//
// The first version reported the longest surviving common prefix — which for
// `function gamma(` is "function ", a string every function in the file shares. That
// is a fact about the language, not about the failure. The useful answer is the
// candidates: when a function was renamed, seeing the declarations that DID survive
// names the rename in one line, and when the whole block is gone the empty list
// says so.
func nearestInSource(src, want string) string {
	keyword := leadingWord(want)
	var candidates []string
	if keyword != "" {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || !containsWord(trimmed, keyword) {
				continue
			}
			candidates = append(candidates, trimmed)
			if len(candidates) == 5 {
				break
			}
		}
	}
	if len(candidates) == 0 {
		return "nothing in the file starts the way it does (" + linesOf(src) + ")"
	}
	return "lines that do: " + strings.Join(candidates, " | ") +
		" (" + linesOf(src) + "; the marker was renamed or removed)"
}

// leadingWord returns the first identifier-ish run of s (`function ` -> `function`,
// `var order = [` -> `var`), which is what "the same kind of line" means here.
func leadingWord(s string) string {
	s = strings.TrimLeft(s, " \t\n")
	for i, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' {
			return s[:i]
		}
	}
	return s
}

// containsWord reports whether line contains word as a whole identifier, so a
// marker starting with `var` does not list every line holding `variable`.
func containsWord(line, word string) bool {
	for from := 0; ; {
		i := strings.Index(line[from:], word)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(word)
		leftOK := start == 0 || !isIdentByte(line[start-1])
		rightOK := end == len(line) || !isIdentByte(line[end])
		if leftOK && rightOK {
			return true
		}
		from = start + 1
		if from >= len(line) {
			return false
		}
	}
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// linesOf reports how much source was searched, so a slice failure cannot be
// confused with an empty file.
func linesOf(src string) string {
	return strconv.Itoa(strings.Count(src, "\n")+1) + " lines"
}

// headLines returns the first n lines of s, or all of it.
func headLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n… (" + strconv.Itoa(len(lines)-n) + " more lines)"
}

// rungValue returns the integer value assigned to a named rung in z-layers.css.
func rungValue(t *testing.T, ladder, name string) int {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `:\s*(-?\d+)`)
	m := re.FindStringSubmatch(ladder)
	if m == nil {
		t.Fatalf("ladder declares no %s rung", name)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%s has an unparsable value: %v", name, err)
	}
	return n
}

// rungValueOfRule looks up the rung named by sel's `z-index: var(--z-…)`
// declaration in css, then resolves that rung against the ladder.
//
// Asserting a numeric z-index at the call site instead would re-create the second
// copy the ladder exists to delete: the test would keep passing while the real
// order lived somewhere else.
func rungValueOfRule(t *testing.T, css, ladder, sel string) int {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(sel) + `\s*\{[^}]*z-index:\s*var\((--z-[a-z-]+)\)`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		// A dialog may inherit the base dialog rung through its class; fall back
		// to .modal-backdrop when querying an undeclared modal id.
		if strings.HasPrefix(sel, "#modal-") {
			return rungValueOfRule(t, css, ladder, ".modal-backdrop")
		}
		t.Fatalf("%s declares no z-index rung via var(--z-…)", sel)
	}
	return rungValue(t, ladder, m[1])
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
		{"connections", "conn-load-banner", `id="sec-entries-body"`, `id="ext-plate"`},
		{"sessions", "session-load-banner", `id="sec-exts-body"`, `id="modal-forward"`},
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
//
// Since every stacking declaration now names a rung (z-layers.css), this reads
// the RUNG a rule uses and then looks its value up on the ladder. Asserting the
// numbers here instead would re-create the second copy the ladder exists to
// delete: the test would keep passing while the real order lived somewhere else.
func TestModalsOutrankEverySurfaceThatOpensThem(t *testing.T) {
	css := readAppCSS(t)
	ladder := readAssetLF(t, "static/css/z-layers.css")
	z := func(sel string) int {
		t.Helper()
		return rungValueOfRule(t, css, ladder, sel)
	}

	modal := z(".modal-backdrop")
	for _, below := range []string{
		"#sec-entries-body",            // the drawer
		".drawer-scrim",                // its scrim
		".shell-window.win-fullscreen", // the terminal that hosts the hamburger
		".sessdock",                    // the dock, which replaced the tab bar
	} {
		if got := z(below); got >= modal {
			t.Errorf("%s is at rung %d, at or above the modal's %d: a dialog opened from it renders behind it", below, got, modal)
		}
	}
	// Toasts stay on top, and so does the notify stack: a server-pushed
	// notify_user message must not be swallowed by a dialog.
	for _, above := range []string{"#ui-copy-toast", "#ui-notify-stack"} {
		if got := z(above); got <= modal {
			t.Errorf("%s is at rung %d, below the modal's %d", above, got, modal)
		}
	}
}

// A dialog raised FROM a dialog must outrank it.
//
// This is the "确认框在对话框下面" bug, and it is invisible in source review: the
// confirm element exists, `hidden` is off, the backdrop is full-screen — and the
// editor that raised it paints over it, so the user presses Delete and nothing
// happens. At one shared rung the winner is DOM order, and every editor is
// declared after the confirm dialogs.
//
// The second tier is not a bigger number for its own sake: it exists because
// these dialogs are the ones reachable from INSIDE another dialog.
func TestDialogsRaisedFromDialogsOutrankTheirParent(t *testing.T) {
	css := readAppCSS(t)
	ladder := readAssetLF(t, "static/css/z-layers.css")
	base := rungValueOfRule(t, css, ladder, ".modal-backdrop")

	// The editors: each of these can raise confirmDialog from one of its keys.
	parents := []string{"#modal-conn", "#modal-ext", "#modal-start", "#modal-forward"}
	// The second tier: confirm (ext/conn delete), cmd (channel prompt), and the
	// close-session dialog (raised from a dock batch action).
	children := []string{"#modal-confirm", "#modal-cmd", "#modal-close-session"}

	for _, child := range children {
		got := rungValueOfRule(t, css, ladder, child)
		if got <= base {
			t.Errorf("%s is at rung %d, not above the dialog rung %d: a confirm raised from an open dialog renders behind it", child, got, base)
		}
		// Still a dialog: below the menus, toasts and the notify stack.
		if got >= rungValue(t, ladder, "--z-popover") {
			t.Errorf("%s is at rung %d, at or above the popover rung: a menu opened from it would be cut off", child, got)
		}
	}
	for _, parent := range parents {
		// The editors inherit the base rung from .modal-backdrop and must not
		// declare a rung of their own — a second declaration is how one dialog
		// silently ends up in the confirm tier.
		if m := regexp.MustCompile(regexp.QuoteMeta(parent) + `\s*\{[^}]*z-index:`).FindString(css); m != "" {
			t.Errorf("%s declares its own z-index; the editors must inherit the base dialog rung", parent)
		}
	}
}

// A menu anchored to the pointer is appended to <body> and positioned wherever
// the pointer was — which can be over the dock, a drawer, or a dialog. It has to
// outrank all of them, and it sat at 60, i.e. underneath every content surface:
// right-clicking a session tab opened a menu the dock painted over.
func TestPointerMenusOutrankEverythingTheyOpenOver(t *testing.T) {
	css := readAppCSS(t)
	ladder := readAssetLF(t, "static/css/z-layers.css")
	popover := rungValue(t, ladder, "--z-popover")

	for _, menu := range []string{".sess-tab-menu", ".shell-window-switch-menu", ".shell-file-ctx-menu"} {
		if got := rungValueOfRule(t, css, ladder, menu); got != popover {
			t.Errorf("%s should sit on the popover rung %d; got %d", menu, popover, got)
		}
	}
	// Above every surface a menu can be opened over, and below the stacks the
	// user did not ask for.
	for _, below := range []string{"--z-modal-top", "--z-drawer", "--z-dock"} {
		if popover <= rungValue(t, ladder, below) {
			t.Errorf("the popover rung must be above %s", below)
		}
	}
	for _, above := range []string{"--z-toast"} {
		if popover >= rungValue(t, ladder, above) {
			t.Errorf("the popover rung must stay below %s: a message the user did not ask for may not be hidden by a menu", above)
		}
	}
}

// A menu belongs to the surface that owns it: it must beat its siblings inside
// that surface, but it must NEVER reach for a page-level rung to do it. A header
// menu at the popover rung would float over the terminals on a page whose header
// is supposed to stay behind them.
func TestInSurfaceMenusStayOnTheDropdownRung(t *testing.T) {
	css := readAppCSS(t)
	ladder := readAssetLF(t, "static/css/z-layers.css")
	dropdown := rungValue(t, ladder, "--z-dropdown")

	for _, menu := range []string{".lang-menu", ".theme-menu", ".clients-panel", ".nethub-open-menu"} {
		if got := rungValueOfRule(t, css, ladder, menu); got != dropdown {
			t.Errorf("%s is an in-surface menu and should use the dropdown rung %d; got %d", menu, dropdown, got)
		}
	}
	// The dropdown rung ranks menus against SIBLINGS only, so it may not try to
	// climb over the content band; the header is what carries it up when needed.
	if dropdown >= rungValue(t, ladder, "--z-workspace") {
		t.Error("the dropdown rung must stay below the content band: the header carries its menu up, not the menu itself")
	}
}

// The ladder is the ONLY place a page layer may be written as a number.
//
// This is what keeps the next "one more rung" from arriving as a literal. It
// walks every shipped stylesheet (excluding the vendored xterm sheet, whose
// stacking is internal to that library) and fails on any numeric z-index outside
// the ladder file. `z-index: auto` is allowed: it removes a stacking context
// rather than ranking one, and one of those resets is load-bearing.
func TestNoStylesheetRanksALayerWithALiteral(t *testing.T) {
	// Reads the sheets directly instead of through the manifest: the point is to
	// cover every file a page can load, including the ones no manifest imports.
	files := []string{
		"static/css/base.css", "static/css/approval.css", "static/css/terminal.css",
		"static/css/timeline.css", "static/css/workspace.css", "static/css/theme.css",
		"static/css/theme-controls.css", "static/css/panels.css",
		"static/css/terminal-theme.css", "static/css/noglass.css", "static/css/docs.css",
		"static/css/tokens.css",
	}
	themes, err := fs.Glob(Assets(), "themes/*/assets/static/css/*.css")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, themes...)

	// A z-index declaration with a numeric value — including a negative one, and
	// including the shorthand `inset: 0; z-index: 3` forms on one line.
	literal := regexp.MustCompile(`z-index:\s*(-?\d+)`)
	for _, path := range files {
		body, err := readAsset(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Comments describe the ladder and may quote a number; rules may not. Rather
		// than pattern-match comment syntax per line, strip every /* … */ run
		// first — a prose line inside a block comment is otherwise indistinguishable
		// from a declaration.
		stripped := stripCSSComments(normalizeNL(body))
		for i, line := range strings.Split(stripped, "\n") {
			if m := literal.FindStringSubmatch(line); m != nil {
				t.Errorf("%s:%d ranks a layer with the literal %s; name it in static/css/z-layers.css and use var(--z-…)",
					path, i+1, m[1])
			}
		}
	}
}

// Every rung the ladder declares is actually used, and every rung a stylesheet
// names is actually declared: a typo in `var(--z-modal-tp)` renders as no
// declaration at all, which silently drops the element to the flow layer, i.e.
// under everything. That failure looks like "the dialog is behind the page".
func TestEveryZIndexRungIsDeclaredAndUsed(t *testing.T) {
	ladder := readAssetLF(t, "static/css/z-layers.css")
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(-\-z-[a-z-]+):`).FindAllStringSubmatch(ladder, -1) {
		declared[m[1]] = true
	}
	if len(declared) < 20 {
		t.Fatalf("the ladder declares only %d rungs; it looks truncated", len(declared))
	}

	bundle := readAppCSS(t)
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`var\((--z-[a-z-]+)\)`).FindAllStringSubmatch(bundle, -1) {
		used[m[1]] = true
		if !declared[m[1]] {
			t.Errorf("a stylesheet reads var(%s), which the ladder does not declare: the declaration is dropped and the element falls to the flow layer", m[1])
		}
	}
	// The JS reads two rungs by name through zRung(); a rename there would be
	// silently ignored too (zRung falls back to the literal).
	util := readAssetLF(t, "static/js/util.js")
	for _, m := range regexp.MustCompile(`zRung\('(--z-[a-z-]+)'`).FindAllStringSubmatch(util, -1) {
		used[m[1]] = true
		if !declared[m[1]] {
			t.Errorf("util.js reads zRung(%s), which the ladder does not declare", m[1])
		}
	}

	for rung := range declared {
		if !used[rung] {
			t.Errorf("the ladder declares %s but no rule uses it; a rung nothing reads is a number that will drift", rung)
		}
	}
}

// A JS fallback for a rung is a copy of that rung's value, and a copy is only
// safe while it agrees. util.js states both numbers it may need before the
// stylesheet has applied; this pins them to the ladder.
func TestJSZIndexFallbacksMatchTheirRungs(t *testing.T) {
	util := readAssetLF(t, "static/js/util.js")
	ladder := readAssetLF(t, "static/css/z-layers.css")

	pairs := map[string]string{
		"--z-fullscreen": "fullscreenZ",
	}
	_ = pairs
	for _, tc := range []struct{ fn, rung string }{
		{"function fullscreenZ() { return zRung('--z-fullscreen'", "--z-fullscreen"},
		{"var low = zRung('--z-window'", "--z-window"},
		{"var dock = zRung('--z-dock'", "--z-dock"},
	} {
		i := strings.Index(util, tc.fn)
		if i < 0 {
			t.Fatalf("util.js no longer reads %s", tc.rung)
		}
		seg := util[i:]
		if end := strings.Index(seg, "\n"); end >= 0 {
			seg = seg[:end]
		}
		m := regexp.MustCompile(`,\s*(-?\d+)\)`).FindStringSubmatch(seg)
		if m == nil {
			t.Fatalf("%s has no fallback literal: %q", tc.rung, seg)
		}
		want := strconv.Itoa(rungValue(t, ladder, tc.rung))
		if m[1] != want {
			t.Errorf("%s falls back to %s but the ladder says %s; the copy has drifted", tc.rung, m[1], want)
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
		// The flexible track is the CENTRE, which the extension column occupies;
		// the session rail takes the third. The order is the point of the layout
		// rework (hosts · extensions · sessions), so it is pinned here.
		"grid-template-columns: var(--nethub-w) minmax(0, 1fr) var(--ext-w)",
		"--ext-w: 260px",
		"transition: grid-template-columns",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("desktop frame layout must contain %q; got %q", want, frame)
		}
	}
	if strings.Contains(frame, "position: fixed") || strings.Contains(frame, "50vw") {
		t.Errorf("the desktop frame must stay in layout, not escape into a drawer: %q", frame)
	}
	for _, want := range []string{
		".workspace-frame.nethub-collapsed { --nethub-w: 56px; }",
		"@media (max-width: 799px)",
		`#sec-entries-body.drawer-open {
    transform: translateX(0); visibility: visible;`,
		".drawer-scrim { display: block; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("responsive NetHub layout must contain %q", want)
		}
	}
	// The closed drawer is taken out of the page by visibility, not by trusting
	// its transform to land: a transition whose end state is off-screen can be
	// left at its start, which would park the drawer over the page it left.
	// visibility must therefore ride on the closed rule, and must not itself be
	// transitioned — a delayed flip stalls the same way and holds the pane
	// visible for the slide it was meant to end.
	if !strings.Contains(css, "transform: translateX(-100%); visibility: hidden;") ||
		strings.Contains(css, "transition: transform .22s ease, visibility") {
		t.Error("the closed drawer must be visibility:hidden with no transition on visibility, or a stalled slide leaves it over the page")
	}
	if !strings.Contains(dialogs, "localStorage.getItem(NETHUB_STATE_KEY)") ||
		!strings.Contains(dialogs, "localStorage.setItem(NETHUB_STATE_KEY") {
		t.Error("manual NetHub state should persist through the existing localStorage mechanism")
	}
	// The drawer scope must not write that preference: below the breakpoint the
	// panel is an overlay, and closing an overlay is not a decision about how the
	// desktop sidebar sits. Writing it there rails the column a wide screen later
	// arrives at — the sidebar collapses for a choice made about a different
	// shape of the same page.
	if !strings.Contains(dialogs, "if (persist && !drawer)") {
		t.Error("the NetHub preference must only be written above the drawer breakpoint")
	}
	// Crossing back out of the drawer scope keeps a drawer the user had open as
	// the expanded column it becomes, so a rotation into a width whose default
	// is the rail does not put away a panel the user was reading.
	if !strings.Contains(dialogs, "var wasOpen = wasDrawer && body && body.classList.contains('drawer-open')") {
		t.Error("crossing out of the drawer scope must keep an open drawer open as a column")
	}
	if !strings.Contains(forms, "toggleNetHub()") || !strings.Contains(forms, "window.termcpToggleEntriesDrawer") {
		t.Error("the trigger and terminal header should use the shared NetHub state")
	}
	if strings.Contains(forms, "transform: translateX") || strings.Contains(forms, "document.body.style.overflow = open") {
		t.Error("conn-form.js must not own a desktop drawer implementation")
	}
}

// The folding column answers the same collapse question NetHub does, and the
// drawer scope must not reach into the desktop preference from this side either:
// closing the overlay on a narrow screen would otherwise rail the column a wide
// screen later arrives at.
//
// The owner is archive-column.js: the right-hand column that folds holds the
// archive, and extensions.js no longer owns a fold at all (the extension plate is
// the page's main region). Pointing this at the old module was exactly the kind
// of test that keeps passing while the behaviour has moved somewhere else.
func TestFoldingColumnKeepsTheDrawerScopeOutOfThePreference(t *testing.T) {
	arch := readAssetLF(t, "static/js/archive-column.js")
	if !strings.Contains(arch, "if (persist && !drawer)") {
		t.Error("the column's preference must only be written above the drawer breakpoint")
	}
	if !strings.Contains(arch, "var wasOpen = wasDrawer && panel && panel.classList.contains('drawer-open')") {
		t.Error("crossing out of the drawer scope must keep an open drawer open as a column")
	}
	/* dialogs.js puts one drawer away when the other opens, through a typeof
	   guard because it loads first. That guard is what made the swap's renames
	   dangerous: pointing it at a function nobody defines any more fails
	   silently, and two drawers then sit open over one another. So the names it
	   calls are pinned to their definitions here. */
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	if !strings.Contains(dialogs, "typeof archiveCollapsed === 'function'") {
		t.Error("dialogs.js should guard its cross-drawer call: the other module loads after it")
	}
	for _, fn := range []string{"archiveCollapsed", "setArchiveCollapsed"} {
		if !strings.Contains(dialogs, fn+"(") {
			t.Errorf("opening the host drawer should put the archive drawer away: dialogs.js never calls %s", fn)
		}
		if !strings.Contains(arch, "function "+fn+"(") {
			t.Errorf("dialogs.js calls %s, which archive-column.js does not define: the typeof guard hides it", fn)
		}
	}
	// One owner only: a second module writing the same key would let the two
	// disagree about which one the page is in.
	for _, other := range []string{"static/js/extensions.js", "static/js/shell-windows.js", "static/js/dialogs.js"} {
		if strings.Contains(readAssetLF(t, other), "ARCHIVE_STATE_KEY") {
			t.Errorf("%s also writes the column's preference; archive-column.js is its one owner", other)
		}
	}
}

// The sidebar is an access resource readout: cards carry identity, protocol,
// runtime state, address metadata and session count, while the rail keeps state
// dots and the active-node mark when the body is collapsed.
func TestNetHubCardsKeepNodeAndSessionHierarchy(t *testing.T) {
	css := readAppCSS(t)
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	for _, want := range []string{
		"node-proto", "node-addr", "node-sess",
		"nethub-rail-item", "nethub-rail-active", "nethub-rail-expand",
	} {
		if !strings.Contains(css, want) || !strings.Contains(dialogs, want) {
			t.Errorf("NetHub should paint %q in both the resource card and rail", want)
		}
	}
	// The reachability verdict is out of this column: it was derived from the
	// session list, which keeps archives and holds sessions no card shows, so it
	// called a host dead for a history someone chose to keep. What a card reports
	// is what Termcp is configured to do and what is running on it.
	for _, dead := range []string{"nodeState", "nodeStatusLabel", "nodeStatusHint", "nethub-rail-status", "data-node-state"} {
		if strings.Contains(dialogs, dead) || strings.Contains(css, dead) {
			t.Errorf("NetHub still paints %q, the retired reachability readout", dead)
		}
	}
	for _, want := range []string{
		"runningSessionsForNode", "nodeSessionInfo", "nethub.sessions.one",
		"nethub.sessions.other", "nodeProtocol(c)",
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
	css := readThemeCSS(t, "cyberpunk")
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	ui := readAssetLF(t, "static/js/ui-socket.js")

	if strings.Contains(dialogs, "rail.appendChild(glyph)") || strings.Contains(dialogs, "glyph.innerHTML = '<img class=\"nethub-icon\"") {
		t.Error("the rail must not append a second NetHub cluster glyph; the trigger already owns it")
	}
	if !strings.Contains(dialogs, "item.className = 'nethub-rail-item'") ||
		strings.Contains(dialogs, "nethub-rail-status") {
		t.Error("the collapsed rail renders node items by name; it carries no reachability lamp")
	}
	// The margin the base rule gives .nethub on desktop is deliberately not part
	// of what is pinned: on the narrow layout the two keys share one row, so a
	// bottom margin on only one of them would push the workspace down alone. What
	// has to survive is the stacking-context reset.
	if !strings.Contains(css, ".nethub { position: relative; top: auto; z-index: auto;") {
		t.Error("the narrow breakpoint must remove .nethub's stacking context so the drawer can outrank the scrim")
	}
	if !strings.Contains(css, "#sec-entries-body {\n    position: fixed") ||
		!strings.Contains(css, "z-index: var(--z-drawer)") || !strings.Contains(css, `#sec-entries-body.drawer-open {
    transform: translateX(0); visibility: visible;`) {
		t.Error("the narrow drawer must be fixed, high in the root stack, and open through its transform")
	}
	// Same guard as the base theme: the closed rack is gone by visibility, and
	// visibility is not transitioned, so a slide that never advances cannot leave
	// the rack sitting over the terminal.
	if !strings.Contains(css, "transform: translateX(-100%); visibility: hidden;") {
		t.Error("the closed rack must be visibility:hidden, or a stalled transition leaves it over the page")
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
// scrolled, through the two shapes it has taken.
//
// FIRST it was `calc(100dvh - 134px)`, a hand-count of the chrome above and below the
// panel. Those terms sum to 147, not 134, so the panel was permanently 13px too tall
// and the document grew a scrollbar of its own — however few nodes the list held. That
// it was a CONSTANT error and not a content one is what made it hard to see: 30 nodes
// and 60 nodes overflowed by exactly the same 13px.
//
// THEN it was `calc(100dvh - --app-header-h - --dock-h - 90px)`: the measured form of
// the same mistake, and worse in one way — `--dock-h` is published by an element in
// THIS layout, so the panel's height depended on a measurement whose own height could
// depend on the page the panel had just changed. The constant was wrong too (measured
// at 1400x900: 40px of overflow with a full archive, 112px with a status banner up).
//
// NOW there is no arithmetic at all: body is one viewport tall, `.dock` is its
// flexible row, the frame takes that height, the grid row is `minmax(0, 1fr)` — a
// DEFINITE row — and every column says `max-height: 100%` against it. Flex does the
// subtraction, so it cannot be out of date and cannot feed back into its own inputs.
// What is pinned here is that chain, because a single missing link restores the
// scrollbar with no other symptom.
//
// The observable half — a page that does not scroll with 80 extensions in it, and
// column heights that settle rather than oscillate — is measured in a real browser by
// TestNoColumnOutgrowsTheViewport.
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

	// THE CHAIN, LINK BY LINK. Each one is load-bearing: without the definite body
	// there is no free space to distribute, without the flexible dock and frame there
	// is nothing to distribute it through, without the definite row `max-height: 100%`
	// resolves against an auto height and is ignored, and without the caps the
	// columns grow the row they were supposed to fit in.
	for _, m := range []struct{ name, rule, want string }{
		{"the page is exactly one viewport tall", between(t, css, "body { letter-spacing", "}"), "height: 100dvh"},
		{"the dock is its flexible row", between(t, css, ".dock { flex: 1 1 auto", "}"), "display: flex"},
		{"the dock does not extend the document", between(t, css, ".dock { flex: 1 1 auto", "}"), "overflow: clip"},
		{"the frame takes that height", between(t, css, ".workspace-frame {", "}"), "flex: 1 1 auto"},
		{"the columns' row is definite", between(t, css, ".workspace-frame {", "}"), "grid-template-rows: minmax(0, 1fr)"},
		{"the hosts column is capped by it", between(t, css, ".nethub {", "}"), "max-height: 100%"},
		{"the extension column is capped by it", between(t, css, ".workspace-exts {\n  display: flex", "}"), "max-height: 100%"},
		{"the panel takes the leftover height", body, "flex: 1 1 auto"},
		{"the collapsed rail takes it too", between(t, css, ".workspace-frame.nethub-collapsed #nethub .nethub-rail {", "}"), "flex: 1 1 auto"},
		{"the archive body takes it too", between(t, css, ".extpanel-body {", "}"), "flex: 1 1 auto"},
	} {
		if !strings.Contains(m.rule, m.want) {
			t.Errorf("%s: %q must appear in %q", m.name, m.want, m.rule)
		}
	}

	// NO ARITHMETIC OVER MEASURED VALUES IN A CEILING, and that is the actual fix:
	// `--dock-h` is published by an element inside this layout, so a ceiling built
	// from it depends on a measurement whose height can depend on the page the ceiling
	// just changed. The hand-counted forms are gone for the same reason they always
	// drift, and both must stay gone.
	for _, stale := range []string{
		"100dvh - 134px", "100dvh - 147px",
		"calc(100dvh - var(--app-header-h, 60px) - var(--dock-h, 44px) - 90px)",
	} {
		if strings.Contains(css, stale) {
			t.Errorf("%q is a chrome sum in a column's ceiling. The panel's height must come from the "+
				"layout (the row it shares), not from arithmetic over measured values — one of them "+
				"(--dock-h) is published by an element in this same layout, which makes the ceiling "+
				"depend on its own result", stale)
		}
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
	css := readThemeCSS(t, "cyberpunk")

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
	css := readThemeCSS(t, "cyberpunk")
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
	// A NEGATIVE rung is what puts it BEHIND the page. A positioned element at auto
	// paints with the z-index:0 layer, above static content, so dropping this one
	// declaration puts the artwork on top of the app. The number lives in
	// z-layers.css; what is pinned here is that the rule reads that rung and that
	// the rung is negative.
	if !strings.Contains(rule, "z-index: var(--z-behind)") {
		t.Errorf("the backdrop needs the behind rung to stay behind the page; got %q", rule)
	}
	ladder := readAssetLF(t, "static/css/z-layers.css")
	if rungValue(t, ladder, "--z-behind") >= 0 {
		t.Error("--z-behind must be negative, or the artwork paints over the page's static content")
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
	css := readThemeCSS(t, "cyberpunk")
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
	if !strings.Contains(between(t, entries, `id="conn-grid"`, `id="ext-plate"`), `id="conn-add"`) {
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
