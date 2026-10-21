package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the shipped modules together: a layout change moves windows between
// containers, while hide/show and tab clicks must keep their visibility and
// selection consistent. Testing those functions individually misses the bug.
func TestSessionTabbarStateTransitions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise session tab bar transitions")
	}
	dir := t.TempDir()
	for _, name := range []string{"util.js", "shell-windows.js"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(readAssetLF(t, "static/js/"+name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	/* The dock counts the sessions a plate would show, and that filter is
	   isHiddenSession — sessions.js's rule, which loads AFTER shell-windows.js in
	   the page. Spliced from the real module rather than restated here: a local
	   copy would keep passing if the shipped rule changed, and the whole point of
	   the count is that an extension's own connection is not a user session. */
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

// Only the browser's DOM and rendering boundaries are simulated. All state
// transitions, tab listeners, window lookup and z-order use the real modules.
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
    // Both attributes, because generated markup carries ids as well as classes
    // (the dock's own count badge is created this way).
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
  // The dock replaced the floating tab bar: same ids for the tab strip and the
  // action cluster, but they now live in #sessdock, and the grip is gone (the
  // dock is pinned, so there is nothing to drag).
  for (const id of ['shell-windows-container', 'pane-workspace', 'pane-grid', 'sessdock',
    'session-tabs-scroll', 'session-tabbar-actions']) ids[id] = element(id);
  body.appendChild(ids['shell-windows-container']);
  body.appendChild(ids['pane-workspace']);
  ids['pane-workspace'].appendChild(ids['pane-grid']);
  body.appendChild(ids['sessdock']);
  // The dock is a band in the page: the workspace above it is what flexes, so
  // the bar's note shares the row with the tabs rather than covering them.
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
  const ctx = { document, console, innerWidth: 1280, innerHeight: 800,
    localStorage: { getItem() { return null; }, setItem() {} },
    matchMedia() { return { matches: false }; }, addEventListener() {},
    requestAnimationFrame() { return 1; }, applyI18n() {}, onLangChange() {}, t: key => key,
    /* tCount is i18n.js's; the note under test is built with it, so a stub that
       keeps the key and the number makes the assertion readable without pulling
       the whole catalog into the probe. */
    tCount: (one, other, p) => (p.count === 1 ? one : other) + ':' + p.count,
    openShellWindow(label, sid, pos, opts) { openRequests.push({ label, sid, pos, opts }); },
    lockWindowReadonly(w) { w.readonlyApplied = true; }
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  sources.forEach(src => vm.runInContext(src, ctx));
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
  const active = () => tabs().filter(tab => tab.getAttribute('aria-selected') === 'true').map(tab => tab._win.id);
  const visible = w => !w.classList.contains('win-hidden') && !w.classList.contains('win-minimized') &&
    (w.parentNode !== ids['pane-grid'] || ids['pane-workspace'].classList.contains('open'));
  function click(w) { tabs().find(tab => tab._win === w).listeners.click({ target: { closest() { return null; } } }); }
  return { ctx, ids, document, win, active, visible, click, openRequests };
}

let failures = 0;
function eq(label, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) { console.log('FAIL ' + label + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want)); failures++; }
}
function checkHidden(f, windows, label) {
  eq(label + ': windows hidden', windows.map(f.visible), windows.map(() => false));
  eq(label + ': workspace closed', f.ids['pane-workspace'].classList.contains('open'), false);
  eq(label + ': aria hidden', f.ids['pane-workspace'].getAttribute('aria-hidden'), 'true');
  eq(label + ': no active tab', f.active(), []);
  eq(label + ': show action', f.document.getElementById('session-tabbar-toggle').title, 'win.showAll');
}

// Run both directions and repeated switches: stale .win-hidden can survive
// moving to the grid, while a hidden grid's children have no such class yet.
for (const startMode of ['float', 'tile']) {
  const f = fixture(), a = f.win(1, 'session-one'), b = f.win(2, 'session-two');
  f.ctx.setLayoutMode(startMode);
  f.ctx.hideAllWindows();
  checkHidden(f, [a, b], startMode + ' hidden');
  for (const mode of ['tile', 'float', 'tile']) {
    f.ctx.setLayoutMode(mode);
    checkHidden(f, [a, b], startMode + ' hidden -> ' + mode);
  }
  f.ctx.showAllWindows();
  eq(startMode + ': show reveals both panes', [a, b].map(f.visible), [true, true]);
  eq(startMode + ': show restores active tab', f.active().length, 1);
  f.ctx.setLayoutMode('float');
  eq(startMode + ': shown panes remain visible as floats', [a, b].map(f.visible), [true, true]);
}

// A tab owns a window before the server assigns an id. Two concurrent dials
// both have a null session id; neither may accidentally select the other.
for (const mode of ['float', 'tile']) {
  const f = fixture(), a = f.win(1, null), b = f.win(2, null);
  eq(mode + ': a null session id is not a window identity', f.ctx.getShellWindowBySid(null), null);
  f.ctx.setLayoutMode(mode);
  f.click(a); f.click(b);
  eq(mode + ': second connecting tab targets its own window', f.active(), [b.id]);
  f.ctx.minimizeShellWindow(b);
  b.classList.add('shell-window-collapsed'); b._savedH = '480px';
  f.ctx.hideAllWindows();
  f.click(b);
  eq(mode + ': clicking hidden pending tab reveals it', f.visible(b), true);
  eq(mode + ': pending tab restores minimize', b._minimized, false);
  eq(mode + ': pending tab expands collapse', b.classList.contains('shell-window-collapsed'), false);
  eq(mode + ': pending tab retains correct selection', f.active(), [b.id]);
  eq(mode + ': pending tab focuses terminal', b.focused, true);
  eq(mode + ': pending tabs do not open duplicate windows', f.openRequests, []);
}

// Minimize is per window, unlike hide-all: showing all must not unminimize a
// different window. Session-based entry points must retain their old behavior.
for (const mode of ['float', 'tile']) {
  const f = fixture(), a = f.win(1, 'session-one'), b = f.win(2, 'session-two');
  f.ctx.setLayoutMode(mode); f.ctx.minimizeShellWindow(a); f.ctx.hideAllWindows(); f.ctx.showAllWindows();
  eq(mode + ': show-all preserves individual minimize', f.visible(a), false);
  eq(mode + ': show-all restores the other window', f.visible(b), true);
  f.ctx.focusSessionWindow('fixture-1', 'session-one', null, { readOnly: true });
  eq(mode + ': session lookup restores and selects window', f.active(), [a.id]);
  eq(mode + ': session lookup applies read-only', a.readonlyApplied, true);
  f.ctx.focusSessionWindow('fixture-new', 'session-new', null, { readOnly: true });
  eq(mode + ': unknown session opens with original options', f.openRequests,
    [{ label: 'fixture-new', sid: 'session-new', pos: null, opts: { readOnly: true } }]);
}

/* The empty row is a LABEL, never a key.
 *
 * The dock used to fold a session plate up from underneath, so the note could be
 * the way back to it. That layer is gone, and the note stays a plain label: there
 * is nothing behind the row to open, so dressing it as a button would promise a
 * list the page does not have.
 *
 * What "empty" means changed with the dock's data source. It used to mean "no open
 * windows"; now it means no ENTRY — no running session on the server and no window
 * in this page. The two cases below pin the new meaning from both sides: a page
 * whose only session belongs to a subsystem is empty (that session is the extension
 * column's), and a page with a live session and no window is not. */
{
  const f = fixture();
  f.ctx.window._lastSessionsSnapshot = [
    { id: 's1', status: 'running', namespace: 'ext:hub' }
  ];
  f.ctx.refreshSessionTabbar();
  const note = f.document.getElementById('sessdock-empty');
  eq('a subsystem-only session: the note says so', note.getAttribute('data-i18n'), 'dock.empty');
  eq('the note is not a key', note.getAttribute('role'), null);
  eq('no badge is created for a layer that does not exist', f.document.getElementById('sessdock-count'), null);
  // It sits in the bar, before the strip: the strip is a tablist and may hold
  // nothing but tabs.
  eq('the note is not inside the tablist', note.parentNode === f.ids['sessdock-bar'], true);
  eq('the note precedes the strip', f.ids['sessdock-bar'].children.indexOf(note),
    f.ids['sessdock-bar'].children.indexOf(f.ids['session-tabs-scroll']) - 1);

  // A live session with no window here is NOT an empty dock: it is the whole point
  // of listing sessions rather than windows.
  f.ctx.window._lastSessionsSnapshot = [
    { id: 's2', name: 'agent', status: 'running', created_at: 1 }
  ];
  f.ctx.refreshSessionTabbar();
  eq('a live session with no window removes the note', f.document.getElementById('sessdock-empty'), null);
  eq('a live session with no window is a tab', f.ids['session-tabs-scroll'].children.length, 1);

  // One window: the note goes, and the tab is the row's whole content.
  const f2 = fixture();
  f2.win(1, 'session-one');
  eq('a window removes the note', f2.document.getElementById('sessdock-empty'), null);
}

if (failures) process.exit(1);
console.log('PASS session tab bar state transitions');
`
	probe := filepath.Join(dir, "probe.js")
	if err := os.WriteFile(probe, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, probe).CombinedOutput(); err != nil {
		t.Fatalf("session tab bar state transitions: %v\n%s", err, out)
	}
}
