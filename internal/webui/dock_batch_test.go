package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheDockWearsTheSharedBatchRow pins the dock to the SHARED component.
//
// The dock's batch keys were once written by hand: a private mode flag, a private
// ticked map and a private painter, all in shell-windows.js. They worked, and that
// is what made the mistake hard to see — the dock looked right while being the
// fourth copy of a component that exists precisely so a column's batch row cannot
// drift from its neighbours'. The drift this time was the shape of the thing: the
// dock grew a key set (dock-key, dock-toggle-count, dock-invert) that no other
// column had, so a change to the real row would reach three columns and miss it.
//
// So: the dock builds its row through createBatchBar, its keys wear the shared
// chip, and it owns no state of its own.
func TestTheDockWearsTheSharedBatchRow(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	index := readAssetLF(t, "index.html")

	// It is a caller of the component, not a reimplementation.
	if !strings.Contains(shell, "createBatchBar({") {
		t.Fatal("the dock must build its batch row through createBatchBar")
	}
	for _, dead := range []string{"_dockSelectOn", "_dockTicked", "dockPaintBar", "dockToggleTick", "dockInvertTicks", "dockEnterSelect", "dockExitSelect"} {
		if strings.Contains(shell, dead) {
			t.Errorf("the dock still owns %s; the shared component owns the mode and the ticked set", dead)
		}
	}

	// Its keys are flat shared chips, in the markup, with the ids the config names.
	for _, want := range []string{
		`class="ext-key ext-key-icon" id="dock-sel-invert"`,
		/* Icon-only: it carried a "Stop" label, which made this key wider than the
		   two beside it for a word that repeats what the X already is. The tooltip
		   and aria-label still carry the full sentence. */
		`class="ext-key ext-key-icon ext-key-danger" id="dock-close-ticked"`,
		/* The count shares the archive's chip class and starts IN THE FLOW at zero:
		   a hidden readout cannot be told apart from a broken one, which is how the
		   dock's counter was reported ("always 0" — the number was right, the element
		   carrying it was `hidden` because the component's paint path returned early
		   for every alwaysOn caller, and the dock's onUpdate hid it at zero anyway). */
		`id="dock-toggle-count"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("the dock's markup must carry %s", want)
		}
	}
	// No selection-mode switch, and no private key vocabulary of its own.
	for _, bespoke := range []string{
		`id="dock-select-toggle"`, `class="dock-key`, `id="dock-invert"`, `id="dock-exit-select"`,
	} {
		if strings.Contains(index, bespoke) {
			t.Errorf("the dock has grown %s; every tab carries its own tick, so there is no mode to switch", bespoke)
		}
	}
	if !strings.Contains(shell, "alwaysOn: true") {
		t.Error("the dock runs alwaysOn: the tick is the tab's own control, so there is no mode")
	}
}

// TestTheDockIsOneRowWideAndTwoRowsNarrow pins where the second row is allowed.
//
// One row could not hold a readable tab strip plus the keys that answer a
// selection plus the window controls on a phone: measured at 390px the tabs were
// down to 106px between them, narrower than the labels they carry, while the two
// key groups held 190px. So the narrow layout wraps: the strip takes a full line
// and the keys sit under it.
//
// The mistake this pins against is making that unconditional. Wrapping on a
// desktop costs 24px of terminal height at every width to hold a second row whose
// only content is a batch group that is EMPTY until something is ticked — the dock
// doubled in height, permanently, for keys nobody could see. The second row is a
// width problem, so it is scoped to the width block; the wide layout is one row
// with the strip absorbing the slack.
func TestTheDockIsOneRowWideAndTwoRowsNarrow(t *testing.T) {
	index := readAssetLF(t, "index.html")
	css := readAssetLF(t, "static/css/workspace.css")

	// Markup order is the fallback layout and the wrap order: strip, then the
	// batch group, then the window controls.
	dock := between(t, index, `class="sessdock" id="sessdock"`, `<div id="modal-forward"`)
	tabs := strings.Index(dock, `id="session-tabs-scroll"`)
	batch := strings.Index(dock, `id="dock-batch"`)
	actions := strings.Index(dock, `id="session-tabbar-actions"`)
	if tabs < 0 || batch < 0 || actions < 0 {
		t.Fatalf("the dock is missing a zone: tabs=%d batch=%d actions=%d", tabs, batch, actions)
	}
	if !(tabs < batch && batch < actions) {
		t.Errorf("the dock's zones must be ordered tabs < batch < actions (got %d, %d, %d)", tabs, batch, actions)
	}

	// The WIDE layout: one row, no wrap, and the strip is what flexes. The batch
	// group is ordered to the left end by CSS, so it can sit after the strip in
	// the markup and still lead the row.
	bar := between(t, css, ".sessdock-bar {", "}")
	if strings.Contains(bar, "flex-wrap: wrap") {
		t.Error("the base bar must not wrap: the second row belongs to the narrow block")
	}
	strip := between(t, css, ".sessdock-tabs {", "}")
	if !strings.Contains(strip, "flex: 1 1 auto") {
		t.Error("on a wide screen the strip is the flexible middle: `flex: 1 1 auto` is what absorbs the slack")
	}
	if strings.Contains(strip, "100%") {
		t.Error("`flex-basis: 100%` in the base rule would force the wrap at every width")
	}
	batchRule := between(t, css, ".sessdock-batch {", "}")
	if !strings.Contains(batchRule, "order: -1") {
		t.Error("the batch group leads the row by CSS order: it must not be the first child in the markup")
	}
	// The cap follows the shape: one row wide.
	dockRule := between(t, css, ".sessdock {", "}")
	if !strings.Contains(dockRule, "max-height: 42px") {
		t.Error("the wide dock is ONE row: its cap must be the one-row height (26px tab + padding)")
	}

	// The NARROW layout: the wrap, the two-row cap, and the right-pinned controls.
	// Anchored on the block's own comment rather than on the media query: there are
	// two narrow blocks on this page and only one of them is about the dock.
	narrow := between(t, css, "/* The dock goes to TWO rows here, and only here.", "\n}")
	if narrow == "" {
		t.Fatal("the dock's narrow block is gone")
	}
	if !strings.Contains(narrow, ".sessdock-bar { flex-wrap: wrap") {
		t.Error("the narrow block is what turns the dock into two rows")
	}
	if !strings.Contains(narrow, ".sessdock { max-height: 70px; }") {
		t.Error("two rows need the taller cap, and only the narrow block may raise it")
	}
	if !strings.Contains(narrow, ".sessdock-actions { margin-left: auto; }") {
		t.Error("with the strip on its own line, the window controls need the auto margin to reach the right edge")
	}
	if !strings.Contains(narrow, ".sessdock-empty { left: 10px") {
		t.Error("the empty note must clear the batch group on a wide screen and not on a narrow one")
	}
}

// TestTheDockTickIsATabControl pins the monitor glyph as the tick target.
//
// The tab does two things and needs two targets: the glyph ticks (for a batch
// close) and the rest of the tab opens its window. That split is what lets this
// bar have no selection mode at all — the same arrangement the archive's rows use.
// The glyph must therefore be a real button (keyboard and screen readers), and a
// click on the tab body must never tick.
func TestTheDockTickIsATabControl(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	start := strings.Index(shell, "function refreshSessionTabbar(")
	if start < 0 {
		t.Fatal("refreshSessionTabbar is gone")
	}
	fn := shell[start : start+strings.Index(shell[start:], "\n}\n")]
	if !strings.Contains(fn, `'<button type="button" class="session-tab-tick"`) {
		t.Error("the tab's tick must be a real <button>: it is the only tick target on the row")
	}
	if !strings.Contains(fn, "e.target.closest('.session-tab-tick')") {
		t.Error("a click on the glyph must tick, and only that click")
	}
	// The tab body opens what it stands for — with no mode to consult.
	if !strings.Contains(fn, "focusShellWindow(cur.win);") {
		t.Error("a tab click must still raise its window")
	}
	if strings.Contains(fn, "_dockBatch.isOn()") {
		t.Error("the tab must not branch on a selection mode: this bar has none")
	}
}

// TestTheDockListsEveryRunningSession pins what the bar is a list OF.
//
// It used to be a list of open WINDOWS, built from allShellWins() alone. Every
// other way a session can start — an agent over MCP (`pty`, `session_start`), a
// script through POST /api/sessions, a connect whose window was closed here —
// produced a live session the page could not see, address or stop. A session you
// cannot see is a session you cannot take back, so the list is now the union:
// open windows, then running sessions with no window.
//
// The three boundaries matter as much as the union:
//   - a DEAD session belongs to the archive column, not to this bar;
//   - a subsystem's own connection (an extension's `ext:` session) belongs to the
//     extension column, and is the same rule the plates apply;
//   - the snapshot comes from a Go map, so its order is random — an unsorted list
//     would reshuffle the tabs on every frame.
func TestTheDockListsEveryRunningSession(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	fn := between(t, shell, "function dockEntries() {", "\n}\n")
	if fn == "" {
		t.Fatal("dockEntries is gone: the dock has no data source of its own")
	}
	for _, want := range []string{
		"allShellWins()",               // the windows the page already holds
		"window._lastSessionsSnapshot", // …plus the sessions it does not
		"s.status !== 'running'",       // over sessions are the archive's
		"isHiddenSession(",             // a subsystem's own connection is not a user session
		"covered[",                     // a window and its session are one tab, never two
		".sort(",                       // ListAll order is random; the bar must not jump
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("dockEntries must use %s", want)
		}
	}
	// The predicate is asked for by name: sessions.js loads after this file, and the
	// page's first paint runs before it.
	if !strings.Contains(fn, "typeof isHiddenSession === 'function'") {
		t.Error("isHiddenSession must be feature-detected: sessions.js is not loaded yet on the first paint")
	}
	// And the refresh must actually use it, or the union is dead code.
	refresh := between(t, shell, "function refreshSessionTabbar() {", "\n}\n")
	if !strings.Contains(refresh, "dockEntries()") {
		t.Error("refreshSessionTabbar must render dockEntries(), not allShellWins()")
	}
	if strings.Contains(refresh, "paintSessdockEmpty(wins.length === 0)") {
		t.Error("the empty note must follow the ENTRY count: a page with no windows but a live session is not empty")
	}
}

// TestAWindowlessTabOpensTheSessionItStandsFor pins the click of a tab that has
// no window behind it.
//
// The old rule was "the tab owns its window, so raise it directly" — correct for
// a window, and impossible for a session this page never opened. Resolving a
// session is right only on that path, so the branch is what the test pins: a tab
// WITH a window must not go through the lookup (a status card's `_sid` is the
// extension's NAME, and a dial has no id at all), and a tab WITHOUT one must.
func TestAWindowlessTabOpensTheSessionItStandsFor(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	fn := between(t, shell, "function refreshSessionTabbar() {", "\n}\n")
	if !strings.Contains(fn, "if (!cur || !cur.win) {") {
		t.Fatal("the tab click must branch on whether it has a window")
	}
	windowless := between(t, fn, "if (!cur || !cur.win) {", "focusShellWindow(cur.win);")
	if !strings.Contains(windowless, "focusSessionWindow(") {
		t.Error("a tab for a session with no window must open one through focusSessionWindow")
	}
	if !strings.Contains(windowless, "cur.session.id") {
		t.Error("the window it opens must be for THAT session")
	}
}

// TestTheDockBatchCloseArchivesAndDoesNotDelete pins what the batch's verb does.
//
// The dock is the only list of sessions, so its close key is the page's main way
// to end several at once. It must STOP AND ARCHIVE: POST /terminate keeps the
// transcript, DELETE erases it, and the page no longer offers to erase a session
// from anywhere but the archive column.
func TestTheDockBatchCloseArchivesAndDoesNotDelete(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")
	start := strings.Index(shell, "function dockCloseEntries(")
	if start < 0 {
		t.Fatal("shell-windows.js no longer defines dockCloseEntries")
	}
	end := strings.Index(shell[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit dockCloseEntries")
	}
	body := shell[start : start+end]
	if !strings.Contains(body, "archiveSessionById(") {
		t.Error("the batch close must archive through archiveSessionById")
	}
	if strings.Contains(body, "deleteSessionById(") {
		t.Error("the batch close must not delete: the archive column is where erasing lives")
	}
	// An entry with no session behind it (a connecting window, a status card) must
	// not reach the archive endpoint at all.
	if !strings.Contains(body, "entry.sid") || !strings.Contains(body, "_extName") {
		t.Error("a placeholder or a status card must be closed, not archived: they carry no session id")
	}
	// A window-less entry has no DOM node to drop, and the close path must not try.
	if !strings.Contains(body, "if (w) closeShellWindow(w)") {
		t.Error("the close must tolerate an entry with no window: stopping the session is the whole effect")
	}
}

// TestTheCloseDialogCountsEveryEntryItWillClose pins the number.
//
// The count and the action have to use the SAME predicate, and they did not. The
// action stopped every window carrying a session id; the count additionally excluded
// `_placeholder`, i.e. every window still dialling. With four windows mid-connect and
// two up, ticking all six and pressing close announced "stop and archive 2 running
// sessions" and then stopped six — and a confirmation whose number is wrong is worse
// than no number, because that number is read as the scope of what is being confirmed.
//
// `_placeholder` is the wrong test on its own anyway: a placeholder starts with
// `_sid = null` and gains the id when the dial succeeds, so a window can be a
// placeholder AND own a session. The id is the only thing that says whether there is
// something to stop.
func TestTheCloseDialogCountsEveryEntryItWillClose(t *testing.T) {
	js := readAssetLF(t, "static/js/shell-windows.js")
	fn := between(t, js, "function dockCloseEntries(keys) {", "\n}\n")
	if fn == "" {
		t.Fatal("dockCloseEntries is gone")
	}
	// The count: a session id and not an extension card.
	countPred := between(t, fn, "var stopping = entries.filter(function (e) {", "});")
	if countPred == "" {
		t.Fatal("the dialog's count predicate is gone")
	}
	if strings.Contains(countPred, "_placeholder") {
		t.Error("the count excludes placeholder windows while the action stops their sessions: the number in the dialog would be too small")
	}
	if !strings.Contains(countPred, "e.sid") {
		t.Error("the count must key on the session id — it is the only proof there is something to stop")
	}
	// The action: the same test, so the two can never drift again.
	act := between(t, fn, "entries.forEach(function (entry) {", "/* No session behind it")
	if act == "" {
		t.Fatal("the close action is gone")
	}
	/* The slice includes the comment above the branch, which mentions
	   `_placeholder` by name while explaining why it is gone. Only the CODE matters,
	   so the condition line is what gets checked. */
	cond := between(t, act, "if (entry.sid", "{")
	if cond == "" {
		t.Fatal("the close action's condition is gone")
	}
	if strings.Contains(cond, "_placeholder") {
		t.Error("the close action skips placeholder windows that already own a session, leaving an orphan running on the server")
	}
	// And the dialog counts ENTRIES: the question it asks is "how many of these am I
	// closing", which is the size of the selection the user made.
	if !strings.Contains(fn, "count: entries.length") {
		t.Error("the message must report the number of tabs being closed, not a subset of them")
	}
}

// TestDockCloseEntriesActsOnWhatItCounted runs the real dockCloseEntries under
// node against a real confirmDialog stub.
//
// The count in the confirmation is the scope of the action, so the two must be the
// same predicate — and the previous version of this file pinned that with
// `strings.Contains(fn, "count: entries.length")`, which fails on any rename and
// passes on any lie. This drives the function instead: what it announces, what it
// posts, and what it closes are read back from the run.
//
// The four entry kinds are covered because they are the four ways the count can be
// wrong while the code looks right:
//
//	window + live session   stopped, window dropped
//	window-less live session stopped, nothing dropped
//	extension status card   dropped only, never POSTed (its _sid is a NAME)
//	connecting window       POSTed if it already owns a session, dropped otherwise
func TestDockCloseEntriesActsOnWhatItCounted(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the batch close")
	}
	js := readAssetLF(t, "static/js/shell-windows.js")
	var code strings.Builder
	// The whole close path, taken from the shipped file: the count, the confirm, the
	// per-entry action and the toast are one unit under test.
	for _, fn := range []string{"dockEntryByKey", "dockCloseEntries", "archiveSessionById"} {
		start := strings.Index(js, "function "+fn+"(")
		if start < 0 {
			t.Fatalf("shell-windows.js no longer defines %s", fn)
		}
		end := strings.Index(js[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %s", fn)
		}
		code.WriteString(js[start : start+end+3])
		code.WriteString("\n")
	}
	// isDeadSession is sessions.js's predicate and decides whether an entry counts
	// as "stopping" anything. Spliced, not restated.
	sessions := readAssetLF(t, "static/js/sessions.js")
	ds := strings.Index(sessions, "function isDeadSession(")
	if ds < 0 {
		t.Fatal("sessions.js no longer defines isDeadSession")
	}
	code.WriteString(sessions[ds : ds+strings.Index(sessions[ds:], "\n}\n")+3])
	code.WriteString("\n")
	// isSessionDeadInWindows lives beside the close path and is what the count reads.
	start := strings.Index(js, "function isSessionDeadInWindows(")
	if start < 0 {
		t.Fatal("isSessionDeadInWindows is gone")
	}
	code.WriteString(js[start : start+strings.Index(js[start:], "\n}\n")+3])

	script := `
const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = {
  console,
  __tabs: [],
  window: { _lastSessionsSnapshot: [] },
  document: {
    getElementById(id) {
      if (id !== 'session-tabs-scroll') return null;
      return { children: sandbox.__tabs };
    },
  },
  t: k => k,
  tCount: (one, other, p) => (p.count === 1 ? one : other) + '#' + p.count,
  apiPath: p => p,
  stripSessionPrefix: s => s,
  __confirm: null,      // { count, danger } captured from the dialog
  __archived: [],       // session ids POSTed to /terminate
  __closed: [],         // windows dropped
  __toasts: [],
  showCopyToast(m) { sandbox.__toasts.push(m); },
  setLoadBanner() {},
  confirmDialog(opts) {
    sandbox.__confirm = { message: opts.message, danger: opts.danger, title: opts.title };
    return Promise.resolve(true);
  },
  closeShellWindow(w) { sandbox.__closed.push(w.id || w._sid || 'anon'); },
  _dockBatch: { setMode() {} },
  fetch: (url) => {
    sandbox.__archived.push(String(url));
    return Promise.resolve({ ok: true, status: 204, text: () => Promise.resolve('') });
  },
};
vm.createContext(sandbox);
vm.runInContext(src, sandbox);

// A tab whose _entry is the entry under test, keyed the way the dock keys it.
function tab(key, entry) {
  return { _entry: entry, getAttribute: a => (a === 'data-win-id' ? key : null) };
}
function windowEntry(key, sid, extra) {
  const w = Object.assign({ id: 'w-' + key, _sid: sid, _extName: null }, extra || {});
  return { key, win: w, sid: sid || '', session: null };
}
function liveSnap(ids) {
  sandbox.window._lastSessionsSnapshot = ids.map(id => ({ id, status: 'running' }));
}
function deadSnap(ids) {
  sandbox.window._lastSessionsSnapshot = ids.map(id => ({ id, status: 'exited' }));
}

let bad = 0;
function check(name, got, want) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) { console.log('FAIL ' + name + ': got ' + g + ' want ' + w); bad++; }
  else console.log('PASS ' + name);
}

async function run(name, tabs, snap) {
  sandbox.__tabs = tabs;
  sandbox.__confirm = null;
  sandbox.__archived = [];
  sandbox.__closed = [];
  sandbox.__toasts = [];
  snap();
  const keys = tabs.map(t => t.getAttribute('data-win-id'));
  vm.runInContext('dockCloseEntries', sandbox)(keys);
  // The action runs on a promise chain; let it settle.
  await new Promise(r => setTimeout(r, 50));
  return {
    announced: sandbox.__confirm && sandbox.__confirm.message,
    danger: sandbox.__confirm && sandbox.__confirm.danger,
    archived: sandbox.__archived.map(u => u.replace('/api/sessions/', '').replace('/terminate', '')),
    closed: sandbox.__closed,
    toasts: sandbox.__toasts,
  };
}

(async () => {
  // Two live sessions: both stopped, both windows dropped, count is 2.
  let r = await run('live', [
    tab('sess:a', windowEntry('sess:a', 'a')),
    tab('sess:b', windowEntry('sess:b', 'b')),
  ], () => liveSnap(['a', 'b']));
  check('two live windows: stops both', r.archived, ['a', 'b']);
  check('two live windows: drops both', r.closed, ['w-sess:a', 'w-sess:b']);
  check('two live windows: announces the window count', r.announced, 'dock.close.msg.live.other#2');
  check('two live windows: the dialog is marked destructive', r.danger, true);

  // A window-less session: stopped, nothing closed. This is the case the dock could
  // not express at all before it started listing sessions.
  r = await run('windowless', [
    tab('sess:c', { key: 'sess:c', win: null, sid: 'c', session: { id: 'c', name: 'agent', status: 'running' } }),
  ], () => liveSnap(['c']));
  check('window-less session: is stopped', r.archived, ['c']);
  check('window-less session: drops no window', r.closed, []);
  check('window-less session: still warned about', r.danger, true);

  // An extension status card: its _sid is the extension NAME, so it must never be
  // posted to /terminate.
  r = await run('ext', [
    tab('win:1', { key: 'win:1', win: { id: 'w-ext', _sid: 'hub', _extName: 'hub' }, sid: '', session: null }),
  ], () => liveSnap([]));
  check('status card: never posted', r.archived, []);
  check('status card: window dropped', r.closed, ['w-ext']);
  check('status card: not counted as stopping a session', r.announced, 'dock.close.msg.window.one#1');
  check('status card: dialog is not destructive', r.danger, false);

  // A connecting window that already owns a session: the session is stopped (that
  // was the orphan bug) and the window goes with it.
  r = await run('pending', [
    tab('sess:d', windowEntry('sess:d', 'd', { _placeholder: true })),
  ], () => liveSnap(['d']));
  check('connecting window with an id: session stopped', r.archived, ['d']);
  check('connecting window with an id: window dropped', r.closed, ['w-sess:d']);

  /* A BATCH of connecting windows that all own sessions. The count is the tab
     count either way, so the only thing that can go wrong here is 'danger':
     excluding '_placeholder' from the STOPPING predicate (the historical bug) makes
     this dialog read as "just closing windows" while it is about to stop N running
     sessions. A mixed batch cannot show it — one non-placeholder entry masks it —
     which is why the all-placeholder case is the one worth asserting. */
  r = await run('pending-batch', [
    tab('sess:p1', windowEntry('sess:p1', 'p1', { _placeholder: true })),
    tab('sess:p2', windowEntry('sess:p2', 'p2', { _placeholder: true })),
  ], () => liveSnap(['p1', 'p2']));
  check('connecting batch: both stopped', r.archived, ['p1', 'p2']);
  check('connecting batch: the dialog warns about stopping sessions', r.danger, true);
  check('connecting batch: announces the tab count', r.announced, 'dock.close.msg.live.other#2');

  // A connecting window with no id yet: nothing to stop, window dropped.
  r = await run('pending-noid', [
    tab('win:2', { key: 'win:2', win: { id: 'w-pending', _sid: null, _placeholder: true }, sid: '', session: null }),
  ], () => liveSnap([]));
  check('connecting window with no id: nothing posted', r.archived, []);
  check('connecting window with no id: window dropped', r.closed, ['w-pending']);

  // Already over: the count must not claim it is "stopping" anything.
  r = await run('dead', [
    tab('sess:e', windowEntry('sess:e', 'e')),
  ], () => deadSnap(['e']));
  check('over session: not counted as stopping', r.announced, 'dock.close.msg.window.one#1');

  console.log(bad === 0 ? 'DOCK CLOSE OK' : bad + ' dock-close failure(s)');
  process.exit(bad === 0 ? 0 : 1);
})();
`
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "spliced.js")
	if err := os.WriteFile(srcPath, []byte(code.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe.js")
	if err := os.WriteFile(probe, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, probe, srcPath).CombinedOutput()
	if err != nil {
		t.Fatalf("dock close probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "DOCK CLOSE OK") {
		t.Fatalf("dock close probe did not pass:\n%s", out)
	}
}
