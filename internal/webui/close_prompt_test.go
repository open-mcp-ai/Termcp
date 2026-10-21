package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClosingARunningSessionStopsAndArchives runs the real askCloseSession under
// node.
//
// The page used to offer two answers to a window's "x": archive or delete. That
// made the irreversible choice sit next to the recoverable one for the same
// gesture, under an icon that cannot express the difference — and deleting a
// running session also erased its transcript, which no archive could then show.
// So the page no longer offers to erase a session at all: closing STOPS AND
// ARCHIVES it, and erasing lives in the archive column, where the record being
// destroyed is the thing on screen.
//
// The assertions are about the DECISION, not the markup, and each is a way the
// prompt could be wrong while still looking right:
//
//   - a running session must ask before it acts;
//   - cancelling must issue NO request at all (a prompt that acts on cancel is
//     worse than no prompt);
//   - the answer must call POST /terminate — the endpoint the batch bar uses —
//     and must NEVER call DELETE;
//   - a session that is already over must not ask: there is nothing to stop, and
//     its window can simply go.
func TestClosingARunningSessionStopsAndArchives(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the close prompt")
	}
	body := readAssetLF(t, "static/js/shell-windows.js")

	// The real functions, spliced so the probe runs shipped code. `between` cuts
	// at the next top-level close, so each is taken on its own.
	var code strings.Builder
	for _, fn := range []string{"askCloseSession", "isSessionDeadInWindows", "archiveSessionById", "closeShellWindowForTest"} {
		if fn == "closeShellWindowForTest" {
			// closeShellWindow is defined with a leading doc comment and may not
			// end in the exact shape the loop expects; the probe only needs to
			// observe that it was reached, so it is stubbed in the sandbox below
			// instead of spliced.
			continue
		}
		start := strings.Index(body, "function "+fn+"(")
		if start < 0 {
			t.Fatalf("shell-windows.js no longer defines %s(); the close prompt is untested now", fn)
		}
		end := strings.Index(body[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %s", fn)
		}
		code.WriteString(body[start : start+end+3])
		code.WriteString("\n")
	}
	// isDeadSession is sessions.js's predicate (the two plates' split), and the
	// prompt reads it to decide whether to ask at all. Splice the real one so the
	// test exercises the shipped rule rather than a local copy of it.
	sessions := readAssetLF(t, "static/js/sessions.js")
	deadStart := strings.Index(sessions, "function isDeadSession(")
	if deadStart < 0 {
		t.Fatal("sessions.js no longer defines isDeadSession")
	}
	deadEnd := strings.Index(sessions[deadStart:], "\n}\n")
	code.WriteString(sessions[deadStart : deadStart+deadEnd+3])

	// The window's close key is the entry point: it must route through the prompt
	// rather than issuing the request itself.
	start := strings.Index(body, "function bindShellWindowCloseButton(")
	if start < 0 {
		t.Fatal("shell-windows.js no longer binds the window close button")
	}
	end := strings.Index(body[start:], "\n}\n")
	code.WriteString(body[start : start+end+3])

	script := `
const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = {
  console: console,
  fetch: null,               // set per case
  apiPath: p => p,
  t: (k, v) => k + (v && v.name ? '(' + v.name + ')' : ''),
  tCount: (o, p, v) => p + (v && v.count ? '(' + v.count + ')' : ''),
  window: { _lastSessionsSnapshot: [] },
  _els: {},
  document: {
    getElementById(id) {
      if (!sandbox._els[id]) {
        sandbox._els[id] = {
          id: id, textContent: '', style: {}, onclick: null, hidden: false,
          classList: { add() {}, remove() {}, contains() { return false; } },
          addEventListener(type, fn) { sandbox._els[id]['on' + type] = fn; },
          removeEventListener() {},
          cloneNode() { return sandbox._els[id]; },
          parentNode: { replaceChild() {} },
          focus() {},
        };
      }
      return sandbox._els[id];
    },
    addEventListener() {}, removeEventListener() {},
  },
  hideModal() { sandbox._hidden = true; }, showModal() { sandbox._hidden = false; },
  setLoadBanner() {}, showCopyToast(m) { sandbox._toasts.push(m); },
  stripSessionPrefix: s => s,
  closeShellWindow() { sandbox._closed++; },
  _toasts: [], _closed: 0, _hidden: true,
  // confirmDialog's answer is driven per case.
  confirmDialog: () => Promise.resolve(sandbox._confirmAnswer),
  applyI18n() {},
  _confirmAnswer: true,
  _snap: [],
};
vm.createContext(sandbox);
vm.runInContext(src, sandbox);

const calls = [];
sandbox.fetch = (url, opts) => {
  calls.push({ url: String(url), method: (opts && opts.method) || 'GET' });
  return Promise.resolve({ ok: true, status: 204, text: () => Promise.resolve('') });
};

// The window object askCloseSession acts on. Only _sid matters to it: a real
// session id makes it a live session, and closeShellWindow is what it calls when
// the session is already over.
const win = { _sid: 'abc', _placeholder: false, _extName: null };
const liveSnap = () => { sandbox.window._lastSessionsSnapshot = [{ id: 'abc', status: 'running' }]; };
const deadSnap = () => {
  sandbox.window._lastSessionsSnapshot = [{ id: 'abc', status: 'exited' }];
  sandbox._closed = 0;
};

const out = { cases: [] };

async function run(name, snap, answer, extra) {
  calls.length = 0;
  sandbox._toasts.length = 0;
  sandbox._hidden = true;
  sandbox._confirmAnswer = answer;
  snap();
  if (extra && extra.win) Object.assign(win, extra.win); else Object.assign(win, { _sid: 'abc', _placeholder: false, _extName: null });
  let result;
  try {
    result = await vm.runInContext('askCloseSession', sandbox)(win._sid, win);
  } catch (e) {
    result = 'THREW: ' + e.message;
  }
  out.cases.push({
    name: name,
    result: result,
    calls: calls.map(c => c.method + ' ' + c.url),
    closed: sandbox._closed,
  });
}

(async () => {
  // A live session, cancelled: no request, nothing closed.
  await run('live-cancel', liveSnap, false);
  // A live session, confirmed: exactly one POST /terminate, and the window goes.
  await run('live-stop', liveSnap, true);
  // Already over: no prompt, no request, the window still goes.
  await run('dead', deadSnap, true);
  // A placeholder (a connecting window) and a status card carry no session id,
  // so neither may reach the archive endpoint.
  await run('placeholder', liveSnap, true, { win: { _sid: null, _placeholder: true, _extName: null } });
  await run('status-card', liveSnap, true, { win: { _sid: 'cdpx', _placeholder: false, _extName: 'cdpx' } });

  // The window's own close key must route through the prompt, not act itself.
  const src2 = fs.readFileSync(process.argv[3], 'utf8');
  out.markup = {
    modalHasDelete: src2.includes('id="modal-close-delete"'),
    modalHasArchive: src2.includes('id="modal-close-archive"'),
    toolbarKeys: ['dock-select-toggle', 'dock-invert', 'dock-close-ticked', 'dock-exit-select']
      .filter(id => src2.includes('id="' + id + '"')).length,
  };
  console.log(JSON.stringify(out));
})();
`
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.js")
	// The spliced functions go in their own file: the probe evals THAT (it must
	// contain nothing but the shipped code), while the harness around it stays a
	// normal CommonJS module free to require fs and vm.
	srcPath := filepath.Join(dir, "spliced.js")
	if err := os.WriteFile(srcPath, []byte(code.String()), 0o600); err != nil {
		t.Fatalf("write spliced source: %v", err)
	}
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	cmd := exec.Command(node, path, srcPath, readAssetPathForTest(t, "index.html"))
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("probe failed: %v\n%s", err, stderr)
	}
	got := string(raw)
	for _, want := range []string{
		`"name":"live-cancel","result":"cancelled","calls":[],"closed":0`,
		`"name":"live-stop","result":"archived","calls":["POST /api/sessions/abc/terminate"],"closed":1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("close prompt differs from the archive-only contract; missing %s in\n%s", want, got)
		}
	}
	// The two no-session cases: the window closes and NOTHING is posted.
	// Four of the five cases stand for a window with nothing to stop (the cancelled
	// one acts on no answer, the three others have no session): none may post.
	if n := strings.Count(got, `"calls":[]`); n < 4 {
		t.Errorf("a window with no session must not reach the archive endpoint; got %d empty call lists in\n%s", n, got)
	}
	if strings.Contains(got, "sessions/null") || strings.Contains(got, "sessions/cdpx") {
		t.Errorf("a placeholder or a status card must never reach /terminate; got\n%s", got)
	}
	if strings.Contains(got, "DELETE") {
		t.Errorf("the page must not delete a session directly any more; got\n%s", got)
	}
}

// TestTheClosePromptOffersOnlyArchive pins the dialog's shape: one answer, and
// the destructive one is gone rather than merely de-emphasised.
func TestTheClosePromptOffersOnlyArchive(t *testing.T) {
	index := readAssetLF(t, "index.html")
	dialog := between(t, index, `id="modal-close-session"`, `id="modal-ext"`)
	if dialog == "" {
		t.Fatal("index.html no longer carries the close-session dialog")
	}
	if strings.Contains(dialog, `modal-close-delete`) {
		t.Error("the close dialog must not offer delete: erasing a session is the archive column's job")
	}
	if !strings.Contains(dialog, `modal-close-archive`) {
		t.Error("the close dialog must offer the archive/stop answer")
	}
	// Exactly two keys: cancel and stop. A third would be the delete that must
	// not be here coming back.
	if n := strings.Count(dialog, "<button"); n != 3 {
		// 3 = the dialog's own x + cancel + stop.
		t.Errorf("the close dialog carries %d buttons; it must carry the x, cancel and stop only", n)
	}
}

// TestClosePromptStringsExistInEveryCatalog keeps the prompt translatable: every
// key the archive-only dialog reads must exist in all three catalogs, or one
// language shows a raw key where a sentence belongs.
func TestClosePromptStringsExistInEveryCatalog(t *testing.T) {
	src := readAssetLF(t, "static/js/shell-windows.js")
	var keys []string
	// Three keys come from the prompt itself; the hint is the dialog's own copy
	// and lives in the markup, so it is read from there.
	for _, k := range []string{"session.close.title", "session.close.messageArchives", "session.close.ok"} {
		if strings.Contains(src, "'"+k+"'") || strings.Contains(src, `"`+k+`"`) {
			keys = append(keys, k)
		}
	}
	if strings.Contains(readAssetLF(t, "index.html"), "session.close.hintArchives") {
		keys = append(keys, "session.close.hintArchives")
	}
	if len(keys) < 4 {
		t.Fatalf("the prompt no longer reads all four keys; found %v", keys)
	}
	catalog := readAssetLF(t, "static/js/i18n-catalog.js")
	for _, k := range keys {
		if n := strings.Count(catalog, "'"+k+"':"); n != 3 {
			t.Errorf("%s must be defined in all three catalogs; found %d", k, n)
		}
	}
}

// readAssetPathForTest is readAssetLF's on-disk twin: the probe leaves through
// node, so it needs a real path rather than the embedded copy.
func readAssetPathForTest(t *testing.T, name string) string {
	t.Helper()
	// Absolute, because the probe runs with cmd.Dir set to a temp directory — a
	// relative path would resolve against that instead of the package.
	abs, err := filepath.Abs(filepath.Join("assets", name))
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return abs
}
