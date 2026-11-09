package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheDockListsSessionsNotJustWindows runs the real dock composition under node.
//
// The dock is the page's one list of sessions, and it was built from the open
// windows (`allShellWins()`) and nothing else. That made every session started
// elsewhere invisible here: an agent's `pty` / `session_start` over MCP, a script
// posting to /api/sessions, a connect whose window this page then closed. Those are
// live sessions with live shells, and a session you cannot see is a session you
// cannot raise, address or stop.
//
// The boundaries are as much of the contract as the union, and each of them is a
// way the list could be wrong while still looking plausible:
//
//   - a DEAD session belongs to the archive column, not the dock;
//   - a subsystem's own connection (an extension's `ext:` session) belongs to the
//     extension column, and is the same rule the plates apply;
//   - a window and its session are ONE tab, never two;
//   - a session keeps its tab when its window closes here;
//   - the order is stable, because the snapshot comes from a Go map.
func TestTheDockListsSessionsNotJustWindows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the dock composition")
	}
	dir := t.TempDir()
	for _, name := range []string{"util.js", "shell-windows.js"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(readAssetLF(t, "static/js/"+name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// isHiddenSession is sessions.js's rule, which loads AFTER shell-windows.js in
	// the page — and the dock has to feature-detect it for exactly that reason. It is
	// spliced from the real module rather than restated, so a change to the shipped
	// rule fails here instead of passing against a copy.
	sessions := readAssetLF(t, "static/js/sessions.js")
	hidden := between(t, sessions, "function isHiddenSession(", "\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "hidden.js"), []byte(hidden+"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `
const fs = require('fs');
const vm = require('vm');
const path = require('path');
const dir = path.dirname(process.argv[1]);
const sources = ['util.js', 'shell-windows.js', 'hidden.js'].map(name => fs.readFileSync(path.join(dir, name), 'utf8'));

// Only the browser's DOM boundary is simulated. Every list decision — which
// sessions count, what order they take, what a click does — is the shipped code.
function element(id = '', className = '') {
  const node = { id, className, children: [], parentNode: null, attrs: {}, listeners: {},
    style: { setProperty(k, v) { this[k] = v; } },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    removeAttribute(k) { delete this.attrs[k]; },
    getAttribute(k) { return this.attrs[k] == null ? null : this.attrs[k]; },
    addEventListener(k, fn) { this.listeners[k] = fn; },
    appendChild(child) {
      if (child.parentNode) child.parentNode.removeChild(child);
      this.children.push(child); child.parentNode = this;
      return child;
    },
    removeChild(child) {
      this.children = this.children.filter(x => x !== child); child.parentNode = null;
    },
    remove() { if (this.parentNode) this.parentNode.removeChild(this); },
    insertBefore(child, ref) {
      if (child.parentNode) child.parentNode.removeChild(child);
      const i = ref ? this.children.indexOf(ref) : -1;
      if (i < 0) this.children.push(child); else this.children.splice(i, 0, child);
      child.parentNode = this;
      return child;
    },
    get firstChild() { return this.children[0] || null; },
    querySelectorAll(selector) {
      const out = [];
      const matches = child => selector.startsWith('.') && selector.slice(1).split('.').every(c => child.classList.contains(c));
      function visit(parent) {
        parent.children.forEach(child => { if (matches(child)) out.push(child); visit(child); });
      }
      visit(this); return out;
    },
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
    getBoundingClientRect() { return { left: 100, top: 100, width: 640, height: 480, bottom: 580 }; },
    scrollIntoView() {}
  };
  const classes = () => new Set(node.className.split(/\s+/).filter(Boolean));
  node.classList = {
    contains(c) { return classes().has(c); },
    add(...cs) { const all = classes(); cs.forEach(c => all.add(c)); node.className = [...all].join(' '); },
    remove(...cs) { const all = classes(); cs.forEach(c => all.delete(c)); node.className = [...all].join(' '); },
    toggle(c, force) {
      const on = force === undefined ? !this.contains(c) : force;
      on ? this.add(c) : this.remove(c); return on;
    }
  };
  Object.defineProperty(node, 'innerHTML', { set(html) {
    node.children.slice().forEach(child => node.removeChild(child));
    for (const tag of html.matchAll(/<[a-z]+\b([^>]*)>/g)) {
      const cls = /class="([^"]+)"/.exec(tag[1]);
      const id = /id="([^"]+)"/.exec(tag[1]);
      node.appendChild(element(id ? id[1] : '', cls ? cls[1] : ''));
    }
  } });
  return node;
}

function fixture() {
  const body = element(), ids = {};
  for (const id of ['shell-windows-container', 'pane-workspace', 'pane-grid', 'sessdock',
    'session-tabs-scroll', 'session-tabbar-actions']) ids[id] = element(id);
  body.appendChild(ids['shell-windows-container']);
  body.appendChild(ids['pane-workspace']);
  ids['pane-workspace'].appendChild(ids['pane-grid']);
  body.appendChild(ids['sessdock']);
  const dockBar = element('sessdock-bar', 'sessdock-bar');
  ids['sessdock-bar'] = dockBar;
  ids['sessdock'].appendChild(dockBar);
  ['session-tabs-scroll', 'session-tabbar-actions'].forEach(id => dockBar.appendChild(ids[id]));
  const document = {
    body, documentElement: element(),
    getElementById(id) {
      if (ids[id]) return ids[id];
      function find(node) { if (node.id === id) return node; for (const child of node.children) { const found = find(child); if (found) return found; } }
      return find(body) || null;
    },
    createElement() { return element(); },
    querySelector(s) { return body.querySelector(s); },
    querySelectorAll(s) { return body.querySelectorAll(s); },
    addEventListener() {}
  };
  const openRequests = [];
  const panelRequests = [];
  const ctx = { document, console, innerWidth: 1280, innerHeight: 800,
    localStorage: { getItem() { return null; }, setItem() {} },
    matchMedia() { return { matches: false }; }, addEventListener() {},
    requestAnimationFrame() { return 1; }, applyI18n() {}, onLangChange() {}, t: key => key,
    tCount: (one, other, p) => (p.count === 1 ? one : other) + ':' + p.count,
    openShellWindow(label, sid, pos, opts) { openRequests.push({ label, sid, pos, opts }); },
    openSessionPanelsMenu(entry, tab) { panelRequests.push({ key: entry.key, panels: entry.panels }); },
    closeShellWindow(w) { w.remove(); ctx.refreshSessionTabbar(); },
    lockWindowReadonly(w) { w.readonlyApplied = true; }
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  sources.forEach(src => vm.runInContext(src, ctx));
  // The menu builder itself is DOM-heavy and has its own probe; here the question is
  // WHEN the press asks instead of raising. Overridden after the sources load
  // because shell-windows.js defines the real one as a global.
  ctx.openSessionPanelsMenu = (entry, tab) => { panelRequests.push({ key: entry.key, panels: entry.panels }); };
  function snapshot(list) { ctx.window._lastSessionsSnapshot = list; ctx.refreshSessionTabbar(); }
  function win(n, sid) {
    const w = element('shell-window-' + n, 'shell-window');
    w._sid = sid; w._placeholder = sid === null; w._pendingConnName = 'fixture-' + n;
    w._connName = sid === null ? '' : 'fixture-' + n;
    w._channels = {}; w._term = { focus() { w.focused = true; } };
    ids['shell-windows-container'].appendChild(w);
    ctx.bringShellWindowToFront(w);
    return w;
  }
  const tabs = () => ids['session-tabs-scroll'].children;
  const keys = () => tabs().map(t => t.getAttribute('data-win-id'));
  const labels = () => tabs().map(t => t.querySelector('.session-tab-label').textContent);
  return { ctx, ids, document, win, snapshot, tabs, keys, labels, openRequests, panelRequests };
}

let failures = 0;
function eq(label, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) { console.log('FAIL ' + label + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want)); failures++; }
}

// A session started by an agent: running, no window on this page.
{
  const f = fixture();
  f.snapshot([{ id: 'agent1', name: 'agent-work', status: 'running', ssh_config: 'box', created_at: 2 }]);
  eq('agent session: one tab', f.keys(), ['sess:agent1']);
  eq('agent session: labelled from the record', f.labels(), ['agent-work · agent1']);
  eq('agent session: no window of its own', f.tabs()[0]._win, null);
  // Its click OPENS the session, which is the only thing a session with no window
  // here can offer. A guarded call (not a missing function) is what makes that safe
  // before sessions.js has loaded.
  f.tabs()[0].listeners.click({ target: { closest() { return null; } }, clientX: 10, clientY: 20 });
  eq('agent session: click opens its terminal', f.openRequests.map(r => r.sid), ['agent1']);
}

// A session with no window here does NOT pulse on its own.
//
// It used to: "nobody has opened this session yet" was its own state, and the tab
// stayed lit until a human clicked it once. That reads wrong on a bright accent
// theme — an unclicked tab sat highlighted forever, which looks like a permanently
// selected tab rather than like something asking to be looked at. The rule is about
// the WINDOW now (see dockNoteOutput): a tab pulses when its window changed while
// nobody was watching, and a session with no window has no window to watch.
{
  const f = fixture();
  f.snapshot([{ id: 'agent2', name: 'agent-work', status: 'running', created_at: 2 }]);
  // NO WINDOW, NO MARK. The rule is about a window that changed while nobody was
  // looking; with no window nothing could have changed, so marking it "fresh" said
  // an event happened when none had. It was also a strobe: the mark was set on
  // every strip rebuild, so the tab pulsed once a second for as long as the session
  // ran, and a reload re-marked every window-less session at once — the report of
  // tabs flickering on a page with nothing new on it.
  eq('a session with no window here does not pulse', f.tabs()[0].classList.contains('is-fresh'), false);
  // Opening it still works — the press asks for its window; the mark question is
  // moot because there was no mark to clear.
  f.tabs()[0].listeners.click({ target: { closest() { return null; } }, clientX: 10, clientY: 20 });
  eq('pressing a window-less tab asks for its window', f.openRequests.map(r => r.sid), ['agent2']);
}

// A window and its session are ONE tab, and the tab outlives the window.
{
  const f = fixture();
  const w = f.win(1, 's1');
  f.snapshot([{ id: 's1', name: 'box', status: 'running', ssh_config: 'box', created_at: 1 }]);
  eq('window + session: one tab', f.keys(), ['sess:s1']);
  eq('window + session: the tab owns the window', f.tabs()[0]._win === w, true);
  // Closing the window here does not end the session, so its tab must stay — now
  // as the window-less form, not as a second tab.
  f.ctx.closeShellWindow(w);
  eq('window closed: the session keeps its tab', f.keys(), ['sess:s1']);
  eq('window closed: the tab is window-less', f.tabs()[0]._win, null);
}

// What is NOT a dock tab, and why.
{
  const f = fixture();
  f.snapshot([
    { id: 'dead1', name: 'over', status: 'exited', created_at: 1 },
    { id: 'ext1', name: 'ext:box', status: 'running', namespace: 'ext:box', created_at: 2 },
    { id: 'live1', name: 'now', status: 'running', created_at: 3 }
  ]);
  eq('dead and extension sessions are not dock tabs', f.keys(), ['sess:live1']);
}

// A window that has no session (a dial in flight, an extension's card) is still a
// tab of its own — it is the only place a connecting window is listed — and it is
// not confused with a session.
{
  const f = fixture();
  const pending = f.win(1, null);
  eq('connecting window: a tab with its own key', f.keys(), ['win:shell-window-1']);
  eq('connecting window: labelled as connecting', f.labels(), ['fixture-1…']);
  pending._placeholder = false;
  pending._extName = 'hub';
  pending._sid = 'hub';
  f.ctx.refreshSessionTabbar();
  eq('status card: keyed by its window, never by the extension name', f.keys(), ['win:shell-window-1']);
}

// The snapshot is a Go map, so its order is random: the bar must sort, or the tabs
// reshuffle on every frame.
{
  const f = fixture();
  f.snapshot([
    { id: 'c', name: 'c', status: 'running', created_at: 30 },
    { id: 'a', name: 'a', status: 'running', created_at: 10 },
    { id: 'b', name: 'b', status: 'running', created_at: 20 }
  ]);
  eq('window-less sessions are ordered by creation', f.keys(), ['sess:a', 'sess:b', 'sess:c']);
  f.snapshot([
    { id: 'b', name: 'b', status: 'running', created_at: 20 },
    { id: 'a', name: 'a', status: 'running', created_at: 10 },
    { id: 'c', name: 'c', status: 'running', created_at: 30 }
  ]);
  eq('a reordered frame renders the same bar', f.keys(), ['sess:a', 'sess:b', 'sess:c']);
}

// A tab does NOT MOVE when its window is opened.
//
// Grouping windows ahead of window-less sessions made pressing a tab shift the whole
// bar one place: the reader pressed the third tab and watched the FIRST one go quiet,
// because the first place now held the tab they had just pressed. Position has to
// follow the session's creation time and nothing else.
{
  const f = fixture();
  f.snapshot([
    { id: 'a', name: 'a', status: 'running', created_at: 10 },
    { id: 'b', name: 'b', status: 'running', created_at: 20 },
    { id: 'c', name: 'c', status: 'running', created_at: 30 }
  ]);
  eq('precondition: three window-less tabs in creation order', f.keys(), ['sess:a', 'sess:b', 'sess:c']);
  // Press the LAST one. It opens a window; every tab must keep its place.
  f.tabs()[2].listeners.click({ target: { closest() { return null; } }, clientX: 10, clientY: 20 });
  eq('pressing the third tab opens its window', f.openRequests.map(r => r.sid), ['c']);
  f.win(3, 'c');   // the window that press asked for
  f.snapshot([
    { id: 'a', name: 'a', status: 'running', created_at: 10 },
    { id: 'b', name: 'b', status: 'running', created_at: 20 },
    { id: 'c', name: 'c', status: 'running', created_at: 30 }
  ]);
  eq('opening a window does not move its tab', f.keys(), ['sess:a', 'sess:b', 'sess:c']);
  eq('the pressed tab keeps its own mark cleared', f.tabs()[2].classList.contains('is-fresh'), false);
  // The other two have no window either, so they carry no mark: a pulse here said
  // "output arrived" about sessions that had printed nothing.
  eq('the other two do not pulse either', [f.tabs()[0].classList.contains('is-fresh'), f.tabs()[1].classList.contains('is-fresh')], [false, false]);
}

// OUTPUT NOBODY LOOKED AT also marks the tab, and looking clears it.
//
// The first pulse answers "nobody has opened this session yet". A session that IS
// open here could still print for ten minutes in a minimized window, or behind
// another one, and the bar said nothing — the reader had to remember which of eight
// tabs was the busy one. Same pulse, second reason, cleared by the two things that
// mean "I have looked": pressing the tab, and output arriving while its window is
// the one on top.
{
  const f = fixture();
  const lower = f.win(1, 's1');
  const upper = f.win(2, 's2');
  f.snapshot([
    { id: 's1', name: 'a', status: 'running', created_at: 1 },
    { id: 's2', name: 'b', status: 'running', created_at: 2 }
  ]);
  const tabOf = sid => f.tabs().find(t => t.getAttribute('data-win-id') === 'sess:' + sid);
  eq('precondition: the raised window is the watched one', upper.classList.contains('win-active'), true);
  // The top window printing is the reader reading: no mark.
  f.ctx.dockNoteOutput(upper);
  eq('a watched window does not pulse on output', tabOf('s2').classList.contains('is-fresh'), false);
  // The window underneath printing has no witness.
  f.ctx.dockNoteOutput(lower);
  eq('output in a window nobody is watching pulses its tab', tabOf('s1').classList.contains('is-fresh'), true);
  // The mark survives a repaint of the bar: a sessions frame can arrive mid-burst.
  f.snapshot([
    { id: 's1', name: 'a', status: 'running', created_at: 1 },
    { id: 's2', name: 'b', status: 'running', created_at: 2 }
  ]);
  eq('a repaint keeps the mark', f.tabs().find(t => t.getAttribute('data-win-id') === 'sess:s1').classList.contains('is-fresh'), true);
  // Raising it is the other way a human says "show me", so the repaint that follows
  // clears the mark rather than leaving a tab pulsing at its own window.
  f.ctx.bringShellWindowToFront(lower);
  f.ctx.refreshSessionTabbar();
  eq('raising the window clears the mark', f.tabs().find(t => t.getAttribute('data-win-id') === 'sess:s1').classList.contains('is-fresh'), false);
  // A minimized window is not a witness, whatever its z-index says.
  lower.classList.add('win-minimized');
  f.ctx.dockNoteOutput(lower);
  eq('a minimized window is not a witness', f.tabs().find(t => t.getAttribute('data-win-id') === 'sess:s1').classList.contains('is-fresh'), true);
  // The press clears it, and is a look for the output already on screen.
  f.tabs().find(t => t.getAttribute('data-win-id') === 'sess:s1').listeners.click({ target: { closest() { return null; } }, clientX: 10, clientY: 20 });
  eq('the press clears the output mark too', f.tabs().find(t => t.getAttribute('data-win-id') === 'sess:s1').classList.contains('is-fresh'), false);
}

// The empty note follows the ENTRY count, not the window count: a page with no
// windows but a live agent session is not empty.
{
  const f = fixture();
  f.snapshot([{ id: 'agent1', name: 'agent', status: 'running', created_at: 1 }]);
  eq('no windows, one session: no empty note', f.document.getElementById('sessdock-empty'), null);
  f.snapshot([]);
  eq('nothing at all: the note is back', f.document.getElementById('sessdock-empty') != null, true);
}

// A SESSION SPLIT ACROSS PANELS IS STILL ONE TAB.
//
// Dragging a channel tab out of its window gives that channel a panel of its own;
// the session behind both windows has not changed, so the dock must not grow a
// second tab for it, and the tab that exists must raise the window holding the
// SESSION — raising a one-channel panel instead would hide every other channel.
{
  const f = fixture();
  const home = f.win(1, 's1');
  const panel = f.win(2, 's1');
  home._channels = { a: { urlIndex: 1 }, b: { urlIndex: 2 } };
  panel._channels = { a: { urlIndex: 1 } };
  f.snapshot([{ id: 's1', name: 'box', status: 'running', created_at: 1 }]);
  eq('a split session has one tab', f.keys(), ['sess:s1']);
  eq('the tab raises a panel that still shows a channel', f.tabs()[0]._win === home, true);
  /* The press must ASK which panel, not guess: the reader dragged a channel out to
     look at it separately, so the tab is the way back to either. */
  eq('the tab says how many panels it stands for', f.tabs()[0]._entry.panels, 2);
  eq('and shows the count', f.tabs()[0].querySelector('.session-tab-panels').textContent, '2');
  f.panelRequests.length = 0;
  f.tabs()[0].listeners.click({ target: { closest() { return null; } }, clientX: 10, clientY: 20 });
  eq('pressing it offers the panels', f.panelRequests, [{ key: 'sess:s1', panels: 2 }]);
  eq('and raises nothing by itself', home.focused, undefined);

  // Tiled mode moves a window into the grid, which is a different container and a
  // different position in allShellWins(). Panels are peers, so a window changing
  // container must not change the fact that the session has two of them.
  f.ids['pane-grid'].appendChild(home);
  f.snapshot([{ id: 's1', name: 'box', status: 'running', created_at: 1 }]);
  eq('a tiled panel is still one of two', f.tabs()[0]._entry.panels, 2);
  f.panelRequests.length = 0;
  f.tabs()[0].listeners.click({ target: { closest() { return null; } }, clientX: 10, clientY: 20 });
  eq('and the press still asks', f.panelRequests, [{ key: 'sess:s1', panels: 2 }]);

  // One panel closed: the survivor is the tab, and the press stops asking a
  // question there is one answer to. Either panel can be the survivor — nothing
  // ranks one above the other.
  home.remove();
  f.snapshot([{ id: 's1', name: 'box', status: 'running', created_at: 1 }]);
  eq('the remaining panel takes the tab over', f.tabs()[0]._win === panel, true);
  eq('one panel: no count', f.tabs()[0].querySelector('.session-tab-panels').hidden, true);
  f.panelRequests.length = 0;
  f.tabs()[0].listeners.click({ target: { closest() { return null; } }, clientX: 10, clientY: 20 });
  eq('one panel: the press raises it directly', f.panelRequests, []);
  eq('one panel: the window was raised', panel.focused, true);
}

if (failures) process.exit(1);
console.log('PASS dock lists sessions, not just windows');
`
	probe := filepath.Join(dir, "probe.js")
	if err := os.WriteFile(probe, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, probe).CombinedOutput(); err != nil {
		t.Fatalf("dock session composition: %v\n%s", err, out)
	}
}

// TestTheDockIsToldWhenOutputArrives pins the wiring side of the output pulse.
//
// The behaviour above lives in shell-windows.js and is exercised directly. What that
// could not catch is the frame path forgetting to call it at all: the set, the
// repaint, the clear — all correct, and nothing ever marking a session, which looks
// exactly like "the feature was never needed". The terminal frame in ui-socket.js is
// the only place output reaches a window, so the call belongs there and only there.
func TestTheDockIsToldWhenOutputArrives(t *testing.T) {
	sock := readAssetLF(t, "static/js/ui-socket.js")
	if !strings.Contains(sock, "dockNoteOutput(win)") {
		t.Fatal("the terminal frame path does not tell the dock about output: a session " +
			"printing in a window nobody is watching would never mark its tab")
	}
	/* INPUT TOO. An agent driving a session is a change in its window exactly like
	   output is, and it is the one an operator most needs to see: the window looks
	   idle while somebody types into it. */
	if !strings.Contains(sock, "dockNoteOutput(winA)") {
		t.Error("the shell_activity path does not tell the dock about input: a window " +
			"being driven from elsewhere would stay quiet in the bar")
	}
	// Guarded, because the frame handler is also reached on pages that load
	// ui-socket.js before shell-windows.js.
	if !strings.Contains(sock, "typeof dockNoteOutput === 'function'") {
		t.Error("the call must be feature-detected: this module also loads on pages " +
			"that never load the dock")
	}
	shell := readAssetLF(t, "static/js/shell-windows.js")
	if !strings.Contains(shell, "function dockWindowWatched(") {
		t.Error("the output pulse must have a definition of \"the reader is looking at " +
			"it\", or a window that is already on screen would pulse at its own output")
	}
	/* ONE rule, and it is about the window. "This page has never opened this
	   session" was a second state, and a tab wearing it stayed lit until someone
	   clicked it — on a bright accent theme that is a permanently highlighted tab,
	   not something asking to be looked at. It is gone; a leftover class, a
	   leftover set or a leftover keyframe would bring it back without the rule. */
	for _, dead := range []string{"is-unseen", "sessionTabUnseen", "dockUnseen", "_dockSeen", "dockMarkSeen"} {
		if strings.Contains(shell, dead) {
			t.Errorf("shell-windows.js still carries %q: the unread-session state was "+
				"replaced by \"its window changed while nobody was watching\"", dead)
		}
		if strings.Contains(readAppCSS(t), dead) || strings.Contains(readAssetLF(t, "static/css/theme.css"), dead) {
			t.Errorf("the stylesheets still carry %q", dead)
		}
	}
	css := readAppCSS(t)
	for _, want := range []string{".session-tab.is-fresh {", ".session-tab.is-fresh .session-tab-label {"} {
		if !strings.Contains(css, want) {
			t.Errorf("the stylesheet carries no %q: the mark would be invisible", want)
		}
	}
	theme := readAssetLF(t, "static/css/theme.css")
	/* The pulse must survive prefers-reduced-motion. The old rule held the lit state
	   instead, and on a bright accent theme that turned the tab into a permanent
	   block of the theme's own colour: the readers who asked for less motion got a
	   tab that looked styled rather than a tab asking to be looked at, so the signal
	   was gone on exactly those machines. A hard cut between two colours is not the
	   motion this query targets. */
	reduced := between(t, theme, "@media (prefers-reduced-motion: reduce) {", "\n}")
	if strings.Contains(reduced, "is-fresh {") || strings.Contains(reduced, "is-fresh .session-tab-label") {
		t.Errorf("prefers-reduced-motion still touches the fresh mark; the pulse is the "+
			"one signal that must reach every reader. Block was:\n%s", reduced)
	}
	/* FINITE, NOT INFINITE. The pulse marks an EVENT — output arrived while nobody
	   was looking — and an event ends. `infinite` turned any condition that stayed
	   true into a metronome: a tab blinking once a second with nothing new to say,
	   which is the flicker report this file exists to prevent. Six cycles is three
	   seconds of signal, then the tab settles; dockClearFresh still removes it on
	   the press, and the pulse never depends on a condition that persists. */
	if !strings.Contains(readAppCSS(t), "animation: sessionTabPulse 1s steps(1, end) 6") {
		t.Error("the fresh pulse must be finite (an event, not a state); infinite turned " +
			"any persistent condition into a once-a-second strobe")
	}
	if strings.Contains(readAppCSS(t), "sessionTabPulse 1s steps(1, end) infinite") {
		t.Error("the fresh pulse still runs forever")
	}
}
