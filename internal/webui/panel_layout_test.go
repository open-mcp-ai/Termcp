package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPanelLayoutPersistsAcrossReload exercises the sessionStorage layout logic
// that keeps session panels from collapsing back into a single window after reload.
//
// When a session is split across multiple peer panels, the server does not know
// which window owns which shell; it only knows the session's shell set. The
// arrangement belongs to this tab alone and lives in sessionStorage.
func TestPanelLayoutPersistsAcrossReload(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise panel layout rules")
	}

	shellWindows := readAssetLF(t, "static/js/shell-windows.js")
	sessionsJS := readAssetLF(t, "static/js/sessions.js")

	if !strings.Contains(shellWindows, "var PANEL_LAYOUT_KEY = 'termcp_panel_layout';") {
		t.Fatal("shell-windows.js must define PANEL_LAYOUT_KEY")
	}
	if !strings.Contains(shellWindows, "function savePanelLayout()") {
		t.Fatal("shell-windows.js must export savePanelLayout")
	}
	if !strings.Contains(shellWindows, "function restorePanelLayout(sessionById)") {
		t.Fatal("shell-windows.js must export restorePanelLayout")
	}
	if !strings.Contains(sessionsJS, "restorePanelLayout(sessionById)") {
		t.Fatal("applySessionsSnapshot must invoke restorePanelLayout with session map")
	}

	dir := t.TempDir()
	extracted := between(t, shellWindows, "/* --- the workspace this tab had on screen", "\n/** The channels a panel shows")
	if err := os.WriteFile(filepath.Join(dir, "layout.js"), []byte(extracted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `
const fs = require('fs');
const vm = require('vm');
const path = require('path');
const src = fs.readFileSync(path.join(path.dirname(process.argv[1]), 'layout.js'), 'utf8');

const store = {};
const calls = { opened: [], tabs: [], switched: [], minimized: [], rects: [] };

const ctx = {
  console,
  setTimeout: (fn) => { fn(); return 1; },
  clearTimeout: () => {},
  sessionStorage: {
    getItem(k) { return store[k] || null; },
    setItem(k, v) { store[k] = String(v); }
  },
  allShellWins: () => ctx._windows || [],
  sessionPanelWindows: (sid) => (ctx._windows || []).filter(w => w._parentSid === sid),
  _windows: [],
  isTiledWin: () => false,
  isMobileViewport: () => false,
  clampShellWindowIntoContainer: () => {},
  sessionAPI: (sid, path) => '/api/sessions/' + sid + path,
  fetch: (url) => {
    // Return two shells for s1
    if (url.includes('/shells')) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          shells: [
            { shell_id: 'sh-1', index: 1, status: 'running' },
            // sh-2 was closed on the server after the layout was saved.
          ]
        })
      });
    }
    return Promise.resolve({ ok: false });
  },
  openShellWindow: (conn, sid, pos, opts) => {
    calls.opened.push({ conn, sid, opts });
    const win = {
      _sid: sid, _parentSid: sid, _connName: conn, _channels: {}, style: {},
      classList: { contains: () => false }
    };
    win._channels[opts.shell] = { tabEl: {} };
    ctx._windows.push(win);
    return win;
  },
  createChannelTab: (win, sid, opt) => {
    calls.tabs.push({ win, sid, opt });
    win._channels[sid] = { tabEl: {} };
  },
  switchChannelTab: (win, sid) => { calls.switched.push({ win, sid }); },
  minimizeShellWindow: (win) => { calls.minimized.push(win); },
  savePanelLayoutSoon: () => {}
};

vm.createContext(ctx);
ctx.window = ctx;
vm.runInContext(src, ctx);

// Setup two peer panels for session 's1':
// Panel A: has sh-1
// Panel B: has sh-2
const winA = {
  _parentSid: 's1', _connName: 'hostA',
  _channels: { 'sh-1': {} }, _activeChannelSid: 'sh-1',
  style: { left: '10px', top: '20px', width: '400px', height: '300px' },
  classList: { contains: () => false }
};
const winB = {
  _parentSid: 's1', _connName: 'hostA',
  _channels: { 'sh-2': {} }, _activeChannelSid: 'sh-2',
  style: { left: '420px', top: '20px', width: '400px', height: '300px' },
  classList: { contains: () => false }
};
ctx._windows = [winA, winB];

// Before restore, saving must be gated off
ctx.savePanelLayout();
if (store['termcp_panel_layout']) {
  console.error('FAIL: savePanelLayout ran before restore');
  process.exit(1);
}

// Mark restored, now saving should persist the two panels
ctx._panelLayoutRestored = true;
ctx.savePanelLayout();
const raw = store['termcp_panel_layout'];
if (!raw) {
  console.error('FAIL: expected layout in storage');
  process.exit(1);
}
const doc = JSON.parse(raw);
if (!doc.panels || doc.panels.length !== 2) {
  console.error('FAIL: expected 2 panels in doc, got: ' + raw);
  process.exit(1);
}
if (doc.panels[0].shells[0] !== 'sh-1' || doc.panels[1].shells[0] !== 'sh-2') {
  console.error('FAIL: panels did not record their shells: ' + raw);
  process.exit(1);
}

// Now simulate a fresh page reload:
ctx._panelLayoutRestored = false;
ctx._panelLayoutRestoring = false;
ctx._windows = [];

// Session map has s1 running
const sessionById = {
  s1: { id: 's1', name: 'hostA', status: 'running' }
};

ctx.restorePanelLayout(sessionById);

// Wait for async fetch
setTimeout(() => {
  if (calls.opened.length !== 1) {
    console.error('FAIL: restorePanelLayout should skip a panel whose shell no longer exists; opened: ' + calls.opened.length);
    process.exit(1);
  }
  if (calls.opened[0].opts.shell !== 'sh-1') {
    console.error('FAIL: restored panel did not request its surviving shell: ' + JSON.stringify(calls.opened));
    process.exit(1);
  }
  process.exit(0);
}, 20);
`
	if err := os.WriteFile(filepath.Join(dir, "test.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, filepath.Join(dir, "test.js"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node test failed: %v\n%s", err, string(out))
	}
}
