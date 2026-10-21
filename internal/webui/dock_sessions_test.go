package webui

import (
	"os"
	"os/exec"
	"path/filepath"
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
  const ctx = { document, console, innerWidth: 1280, innerHeight: 800,
    localStorage: { getItem() { return null; }, setItem() {} },
    matchMedia() { return { matches: false }; }, addEventListener() {},
    requestAnimationFrame() { return 1; }, applyI18n() {}, onLangChange() {}, t: key => key,
    tCount: (one, other, p) => (p.count === 1 ? one : other) + ':' + p.count,
    openShellWindow(label, sid, pos, opts) { openRequests.push({ label, sid, pos, opts }); },
    closeShellWindow(w) { w.remove(); ctx.refreshSessionTabbar(); },
    lockWindowReadonly(w) { w.readonlyApplied = true; }
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  sources.forEach(src => vm.runInContext(src, ctx));
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
  return { ctx, ids, document, win, snapshot, tabs, keys, labels, openRequests };
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

// The empty note follows the ENTRY count, not the window count: a page with no
// windows but a live agent session is not empty.
{
  const f = fixture();
  f.snapshot([{ id: 'agent1', name: 'agent', status: 'running', created_at: 1 }]);
  eq('no windows, one session: no empty note', f.document.getElementById('sessdock-empty'), null);
  f.snapshot([]);
  eq('nothing at all: the note is back', f.document.getElementById('sessdock-empty') != null, true);
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
