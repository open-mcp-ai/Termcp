package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAChannelIsShownByOnePanelOnly pins the ownership rule behind splitting a
// session across panels.
//
// The gesture is "drag a channel tab to another panel, or split it out when the
// source has another channel left behind". Any channel can start a drag so a
// singleton remains mergeable, but its drop into empty space is a no-op. A channel
// must remain ONE view: the server keeps one watch per channel id and addWatch
// REPLACES the previous reader, so a channel restored into two windows makes each
// of them miss part of the stream — the two panels take turns blanking out, which
// reads like a broken terminal rather than like two views.
//
// The rules the probe exercises are the shipped ones: `channelFitsWindow` decides
// whether a window may adopt a channel, and `moveChannelToWindow` /
// `detachChannelToPanel` are the two drops. The DOM is the only thing stubbed.
func TestAChannelIsShownByOnePanelOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the channel ownership rules")
	}
	js := readAssetLF(t, "static/js/terminal-view.js")
	uiSocket := readAssetLF(t, "static/js/ui-socket.js")
	terminalWindow := readAssetLF(t, "static/js/terminal-window.js")
	shellWindows := readAssetLF(t, "static/js/shell-windows.js")
	sessionsJS := readAssetLF(t, "static/js/sessions.js")
	for _, legacy := range []string{"_detachedPanel", "opts.detach"} {
		if strings.Contains(js+terminalWindow+shellWindows, legacy) {
			t.Fatalf("panel identity must not depend on %q; every window of a session is a peer", legacy)
		}
	}

	if !strings.Contains(js, "ch.closeFromOwner = function () { closeChannelTab(win, sessionId); }") {
		t.Fatal("each shell channel must register its own teardown on its owning window")
	}
	closeWindow := between(t, uiSocket, "function closeShellWindow(win)", "\nfunction setupShellWindowDrag(")
	if !strings.Contains(closeWindow, "disposeWinResources(win)") || strings.Contains(closeWindow, "sendTerminalWatch(win._sid") {
		t.Fatal("closing a window must drain its owned resource list without hardcoding session terminal watch drops")
	}

	// functions, so a rewrite that re-parents the tab instead of rebuilding the
	// view has to keep the same contract or fail here.
	if !strings.Contains(js, "var owner = _channelOwner[sessionId];") {
		t.Fatal("createChannelTab no longer refuses a channel another window already shows")
	}
	if strings.Contains(sessionsJS, "win._primaryShellId") {
		t.Fatal("session panels must not privilege one shell as a permanent primary channel")
	}
	if strings.Contains(terminalWindow, "_primaryShellId") {
		t.Fatal("shell windows must not retain primary-channel identity")
	}
	if !strings.Contains(terminalWindow, "win._readOnly = false;") {
		t.Fatal("a newly established live session must initialise its window read-only flag as false")
	}
	dir := t.TempDir()
	parts := []string{
		between(t, js, "var _channelOwner = Object.create(null);", "\nfunction channelFitsWindow(") + "\n",
		between(t, js, "function claimChannel(", "\n}\n") + "\n}\n",
		between(t, js, "function releaseChannel(", "\n}\n") + "\n}\n",
		between(t, js, "function channelFitsWindow(", "\n}\n") + "\n}\n",
		"var _channelDrag = null;\n",
		between(t, js, "function canStartChannelDrag(", "\nfunction _channelDropTargetFor(") + "\n",
		between(t, js, "function syncChannelTabDraggable(", "\n}\n") + "\n}\n",
		between(t, js, "function _channelDropTargetFor(", "\n}\n") + "\n}\n",
		between(t, js, "function _channelNeighbor(", "\n}\n") + "\n}\n",
		between(t, js, "function _dropChannelView(", "\n}\n") + "\n}\n",
		between(t, js, "function moveChannelToWindow(", "\n}\n") + "\n}\n",
		between(t, js, "function detachChannelToPanel(", "\n}\n") + "\n}\n",
	}
	if err := os.WriteFile(filepath.Join(dir, "rules.js"), []byte(strings.Join(parts, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	// The dock's panel list is composed in shell-windows.js; the same sandbox holds
	// both, because "which panels this session has" is the other half of the rule.
	dockJS := readAssetLF(t, "static/js/shell-windows.js")
	dockParts := between(t, dockJS, "function sessionPanelWindows(", "\nfunction openSessionPanelsMenu(")
	if err := os.WriteFile(filepath.Join(dir, "panels.js"), []byte(dockParts), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `
const fs = require('fs');
const vm = require('vm');
const path = require('path');
const src = fs.readFileSync(path.join(path.dirname(process.argv[1]), 'rules.js'), 'utf8') +
  fs.readFileSync(path.join(path.dirname(process.argv[1]), 'panels.js'), 'utf8');

const calls = { watches: [], disposed: [], created: [], opened: [], empty: [], archived: [] };
const ctx = {
  console,
  window: null,
  setTimeout() { return 1; },
  clearTimeout() {},
  savePanelLayoutSoon() {},
  t: key => key,
  _channelAddMenu: null,
  sendTerminalWatch(sid, on) { calls.watches.push({ sid, on }); },
  switchChannelTab(win, sid) { win._activeChannelSid = sid; },
  _showChannelEmpty(win, show) { calls.empty.push({ win: win.name, show: show }); },
  updateTermScrollButton() {},
  refreshSessionTabbar() {},
  openShellWindow(label, sid, pos, opts) { calls.opened.push({ label, sid, pos, opts }); },
  closeShellWindow(win) { calls.closed = (calls.closed || []).concat([win.name]); },
  /* The window's teardown list is the parent side of the ownership rule: a child
     registers its own closer here, and destroying the window runs the list. The
     probe implements exactly that pair, so a channel that moves must visibly
     travel between the two lists. */
  addWinDisposable(win, fn) { if (!win._disposables) win._disposables = []; win._disposables.push(fn); },
  removeWinDisposable(win, fn) {
    if (!win._disposables) return;
    var i = win._disposables.indexOf(fn);
    if (i >= 0) win._disposables.splice(i, 1);
  },
  disposeWinResources(win) {
    if (!win._disposables || !win._disposables.length) return;
    var list = win._disposables;
    win._disposables = [];
    for (var i = list.length - 1; i >= 0; i--) list[i]();
  },
  allShellWins() { return ctx.windows || []; },
  windows: [],
  createChannelTab(win, sid, opt) {
    if (win._channels[sid]) return;
    if (ctx._channelOwner[sid] && ctx._channelOwner[sid] !== win) { calls.blocked = sid; return; }
    calls.created.push({ win: win.name, sid: sid, index: opt.index, readOnly: !!opt.readOnlyHistory });
    addView(win, sid, opt.index);
  }
};
vm.createContext(ctx);
ctx.window = ctx;
vm.runInContext(src, ctx);
ctx._channelOwner = Object.create(null);

function fakeWin(name, sid) {
  const w = {
    name: name, _sid: sid, _channels: {}, _channelCount: 0, _activeChannelSid: null,
    _parentSid: sid, _connName: 'box', _readOnly: false,
    querySelector(sel) {
      if (sel === '.shell-channel-tabs') {
        return { querySelectorAll: () => Object.keys(w._channels).map(k => w._channels[k].tabEl) };
      }
      return null;
    }
  };
  return w;
}
function addView(w, sid, index) {
  const tab = { parentNode: null, getAttribute: () => sid, draggable: false };
  const ch = {
    term: { dispose() { calls.disposed.push(sid); } },
    instEl: { parentNode: null },
    tabEl: tab,
    readOnlyHistory: false,
    streamDone: false,
    fitRo: null,
    urlIndex: index
  };
  /* The production shape: the channel's teardown is registered on the window that
     owns it, so the window's destruction is what closes the shell. */
  ch.closeFromOwner = function () { calls.archived.push(sid); };
  ctx.addWinDisposable(w, ch.closeFromOwner);
  w._channels[sid] = ch;
  w._channelCount += 1;
  w._activeChannelSid = sid;
  ctx.claimChannel(w, sid);
  ctx.syncChannelTabDraggable(w);
}

let failures = 0;
function eq(label, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    console.log('FAIL ' + label + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
    failures++;
  }
}

/* Every channel tab can start a drag: a one-channel panel must be mergeable into
   another panel. Its outside drop is separately rejected so the source cannot be
   left as an empty window. */
{
  const first = fakeWin('first', 's1');
  addView(first, 'a', 1);
  eq('a one-channel first window can start a drag', ctx.canStartChannelDrag(first), true);
  eq('its single tab is draggable', first._channels.a.tabEl.draggable, true);
  addView(first, 'b', 2);
  eq('a multi-channel window can drag too', ctx.canStartChannelDrag(first), true);
  eq('both tabs are draggable', [first._channels.a.tabEl.draggable, first._channels.b.tabEl.draggable], [true, true]);
  const panel = fakeWin('panel', 's1');
  addView(panel, 'c', 3);
  eq('a one-channel split panel can start a merge drag', ctx.canStartChannelDrag(panel), true);
  eq('its single tab is draggable too', panel._channels.c.tabEl.draggable, true);
}

/* One view per channel: a channel another window shows is not adoptable, and the
   rule is OWNERSHIP — not which window was there first. */
{
  const first = fakeWin('first', 's1');
  addView(first, 'a', 1);
  ctx.windows = [first];
  eq('the window that owns it may keep it', ctx.channelFitsWindow(first, 'a'), true);
  const panel = fakeWin('panel', 's1');
  eq('a peer panel may not take a channel another panel shows', ctx.channelFitsWindow(panel, 'a'), false);
  eq('nor may a third window', ctx.channelFitsWindow(fakeWin('other', 's1'), 'a'), false);
}

/* Adoption is symmetric: ANY panel of the session may take an unowned channel, and
   the one that claims it is the owner. There is no home/guest distinction left. */
{
  const first = fakeWin('first', 's1');
  const peer = fakeWin('peer', 's1');
  ctx.windows = [first, peer];
  eq('either panel may adopt an unowned channel', ctx.channelFitsWindow(first, 'new'), true);
  eq('its peer may adopt it too', ctx.channelFitsWindow(peer, 'new'), true);
  ctx.claimChannel(peer, 'new');
  eq('the peer that claimed it keeps it', ctx.channelFitsWindow(peer, 'new'), true);
  eq('the first panel cannot open a second view', ctx.channelFitsWindow(first, 'new'), false);
}

/* Merging: the source loses the view (watch included), the target builds it; if
   that was the source's only channel, its now-empty window closes. */
{
  const first = fakeWin('first', 's1');
  const peer = fakeWin('peer', 's1');
  addView(first, 'a', 3);
  calls.watches.length = 0; calls.disposed.length = 0; calls.created.length = 0; calls.closed = [];
  ctx.moveChannelToWindow(first, 'a', peer);
  eq('the source stops showing it', Object.keys(first._channels), []);
  eq('the source stops its watch', calls.watches, [{ sid: 'a', on: false }]);
  eq('the xterm is disposed', calls.disposed, ['a']);
  eq('the target builds the same channel, same index', calls.created,
    [{ win: 'peer', sid: 'a', index: 3, readOnly: false }]);
  eq('the emptied source closes, even if it was first-created', calls.closed, ['first']);
  ctx.windows = [peer];
  eq('ownership moved with it', ctx.channelFitsWindow(peer, 'a'), true);
  eq('the old window cannot take it back', ctx.channelFitsWindow(first, 'a'), false);
}

/* Round-trip symmetry: split a channel out into peer, then drag it back into
   the original panel. Both directions must work without role asymmetry — and the
   read-only flag must be compared as a BOOLEAN, because a window established
   through the dial path and one opened by openShellWindow are not the only shapes
   a panel can have: a flag that was never assigned reads as undefined, while a
   normal live window carries false; a strict comparison silently refused to merge
   them. */
{
  const first = fakeWin('first', 's1');
  const peer = fakeWin('peer', 's1');
  addView(first, 'sh-stay', 1);
  addView(first, 'sh-move', 2);
  calls.created.length = 0; calls.closed = [];

  ctx.moveChannelToWindow(first, 'sh-move', peer);
  eq('channel moved to peer panel', Object.keys(peer._channels), ['sh-move']);
  eq('source retained its other channel', Object.keys(first._channels), ['sh-stay']);
  eq('source did not close', calls.closed || [], []);

  /* THE DRAG BACK. The target is a live panel whose flag was never assigned — the
     shape a window has when it was established by the dial path. */
  first._readOnly = undefined;
  ctx._channelDrag = { win: peer, sid: 'sh-move' };
  eq('merge direction: an unset read-only flag is still a live target',
    ctx._channelDropTargetFor({ target: { closest: () => first } }), first);

  calls.closed = [];
  ctx.moveChannelToWindow(peer, 'sh-move', first);
  eq('channel merged back into first panel', Object.keys(first._channels).sort(), ['sh-move', 'sh-stay']);
  eq('peer panel emptied and closed', calls.closed, ['peer']);
  eq('channel ownership is now on the merged panel', ctx.channelFitsWindow(first, 'sh-move'), true);
  eq('closed peer panel no longer owns it', ctx.channelFitsWindow(peer, 'sh-move'), false);

  /* A LIVE panel and a READ-ONLY one are not merge partners: handing a live shell
     to a locked view would make the shell unwritable. That drop is not a merge. */
  const locked = fakeWin('locked', 's1');
  locked._readOnly = true;
  addView(locked, 'sh-dead', 9);
  ctx._channelDrag = { win: first, sid: 'sh-stay' };
  eq('live drag onto a read-only panel is not a merge',
    ctx._channelDropTargetFor({ target: { closest: () => locked } }), { empty: true });
  ctx._channelDrag = null;
}

/* A singleton cannot split off into empty space: that drop is a no-op and the
   source remains intact. */
{
  const first = fakeWin('first', 's1');
  addView(first, 'b', 2);
  calls.closed = [];
  ctx._channelDrag = { win: first, sid: 'b' };
  const drop = ctx._channelDropTargetFor({ target: { closest: () => null } });
  eq('outside drop from a singleton is rejected', drop, null);
  eq('the source retains its only channel', Object.keys(first._channels), ['b']);
  eq('the source remains open', calls.closed || [], []);
}

/* Multi-channel panels may still split one channel into its own panel. */
{
  const home = fakeWin('home', 's1');
  addView(home, 'b', 2);
  addView(home, 'c', 3);
  calls.opened.length = 0;
  ctx.detachChannelToPanel(home, 'b', { x: 40, y: 60 });
  eq('a panel is opened for the dropped channel', calls.opened, [{
    label: 'box', sid: 's1', pos: { x: 40, y: 60 },
    opts: { shell: 'b', index: 2, readOnly: false, channelReadOnly: false }
  }]);
  eq('the source keeps its other channel', Object.keys(home._channels), ['c']);
}

/* A panel that lost its last channel closes itself: dragging that tab back to the
   session's window has to leave ONE panel behind, not an empty husk. */
{
  const home = fakeWin('home', 's1');
  const panel = fakeWin('panel', 's1');
  addView(panel, 'd', 4);
  calls.closed = [];
  ctx.moveChannelToWindow(panel, 'd', home);
  eq('the emptied panel closes', calls.closed, ['panel']);
  eq('and its channel lands in the session window', calls.created[calls.created.length - 1],
    { win: 'home', sid: 'd', index: 4, readOnly: false });
}

/* Which panels one session has, and what a row in the pull-up menu says. */
{
  const home = fakeWin('home', 's1');
  const panel = fakeWin('panel', 's1');
  const other = fakeWin('other', 's2');
  addView(panel, 'a', 3);
  addView(home, 'b', 1);
  // The container order is DOM order; the list must not depend on it.
  ctx.windows = [panel, other, home];
  eq('panels of one session, in workspace container order',
    ctx.sessionPanelWindows('s1').map(w => w.name), ['panel', 'home']);
  eq('a panel is named by the channels it holds', ctx.panelChannelLabel(home), 'shell-1');
  eq('and by the index the tab shows', ctx.panelChannelLabel(panel), 'shell-3');
  eq('an empty panel says so', ctx.panelChannelLabel(fakeWin('empty', 's1')), 'channel.empty');
}

/* Closing the last channel shows the empty state, which is the only way back to
   the add control when every tab is gone. */
{
  const home = fakeWin('home', 's1');
  addView(home, 'c', 1);
  calls.empty.length = 0;
  ctx._dropChannelView(home, 'c');
  eq('the empty state appears', calls.empty, [{ win: 'home', show: true }]);
}

/* Moving a channel re-parents its teardown: closing the source window must NOT
   archive the channel that moved, and closing the destination window MUST. */
{
  const src = fakeWin('src', 's1');
  const dst = fakeWin('dst', 's1');
  addView(src, 'migrated', 1);
  eq('registered on source', (src._disposables || []).length, 1);
  eq('not on destination yet', (dst._disposables || []).length, 0);

  ctx.moveChannelToWindow(src, 'migrated', dst);
  eq('unregistered from source on drop', (src._disposables || []).length, 0);
  eq('registered on destination after move', (dst._disposables || []).length, 1);

  calls.archived.length = 0;
  ctx.disposeWinResources(src);
  eq('closing source window does not touch the migrated shell', calls.archived, []);

  ctx.disposeWinResources(dst);
  eq('closing destination window reclaims the shell via parent teardown', calls.archived, ['migrated']);
}

if (failures) process.exit(1);
console.log('PASS one channel, one panel');
`
	probe := filepath.Join(dir, "probe.js")
	if err := os.WriteFile(probe, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, probe).CombinedOutput(); err != nil {
		t.Fatalf("channel ownership: %v\n%s", err, out)
	}
}

// TestTheAddMenuFollowsItsWindow pins the fix for a menu that stayed behind.
//
// The menu is portaled to <body> with position:fixed — it has to be, because
// #pane-workspace carries a backdrop-filter and would become the containing block
// of a fixed descendant, positioning the menu against the pane grid instead of the
// viewport. The cost of that portal is that the menu does not travel with the
// window in the DOM: when the window was dragged, the menu stayed where it opened
// and pointed at nothing.
//
// So the anchor's rect is re-read. What is pinned here is only that: the same
// anchor, moved, moves the menu with it, and the flip above/below follows too.
func TestTheAddMenuFollowsItsWindow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the menu placement")
	}
	js := readAssetLF(t, "static/js/terminal-view.js")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "menu.js"),
		[]byte(between(t, js, "function repositionChannelAddMenu(", "\n}\n")+"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `
const fs = require('fs');
const vm = require('vm');
const path = require('path');
const src = fs.readFileSync(path.join(path.dirname(process.argv[1]), 'menu.js'), 'utf8');

const ctx = { console, window: { innerWidth: 800, innerHeight: 600 } };
vm.createContext(ctx);
vm.runInContext(src, ctx);

// An anchor the test moves, and a menu whose size the stylesheet would decide.
const anchor = { isConnected: true, rect: { left: 100, top: 520, bottom: 540 }, getBoundingClientRect() { return this.rect; } };
const el = { offsetWidth: 120, offsetHeight: 60, style: {}, _anchor: anchor };
ctx._channelAddMenu = el;
let failures = 0;
function eq(label, got, want) {
  if (got !== want) { console.log('FAIL ' + label + ': got ' + got + ', want ' + want); failures++; }
}

ctx.repositionChannelAddMenu();
eq('opens under the button it points at', el.style.left, '100px');
// The footer button sits on the bottom edge with no room below, so it flips up.
eq('no room below: it opens above', el.style.top, '456px');

// The window is dragged 200 to the right and 40 up: the menu must follow.
anchor.rect = { left: 300, top: 480, bottom: 500 };
ctx.repositionChannelAddMenu();
eq('follows the window horizontally', el.style.left, '300px');
eq('and vertically', el.style.top, '504px');

// Near the right edge the menu is pulled inside the viewport instead of hanging
// off it; the window may be dragged anywhere, a portal cannot.
anchor.rect = { left: 780, top: 480, bottom: 500 };
ctx.repositionChannelAddMenu();
eq('clamped to the viewport', el.style.left, '672px');

// An anchor that left the document (its window was closed) is not something to
// point at: the caller closes the menu, and this must not throw on the way.
anchor.isConnected = false;
ctx.repositionChannelAddMenu();
eq('a detached anchor leaves the last position alone', el.style.left, '672px');

if (failures) process.exit(1);
console.log('PASS the add menu follows its window');
`
	probe := filepath.Join(dir, "probe.js")
	if err := os.WriteFile(probe, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, probe).CombinedOutput(); err != nil {
		t.Fatalf("menu placement: %v\n%s", err, out)
	}
}
