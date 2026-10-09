package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClosingARunningSessionAsksBeforeItDestroys runs the real askCloseSession
// under node.
//
// The window's "x" used to delete outright: one misclick on a live terminal
// erased its on-disk transcript for good, and the archive had no way to grow
// except through the batch bar. The key now asks, because the two outcomes are
// genuinely different and the icon cannot express the difference — archiving
// disconnects the session and keeps its output readable, deleting erases it.
//
// The assertions are about the DECISION, not the markup, and each one is a way
// the prompt could be wrong while still looking right:
//
//   - a running session must ask, and the answer must be able to be "archive";
//   - cancelling must issue NO request at all (a prompt that acts on cancel is
//     worse than no prompt);
//   - archive must call POST /terminate — the endpoint the batch bar uses —
//     and must NOT call DELETE, which is what makes it recoverable;
//   - an already-archived session must not be offered archiving again, and must
//     still be deletable.
func TestClosingARunningSessionAsksBeforeItDestroys(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the close dialog")
	}
	body := readAssetLF(t, "static/js/shell-windows.js")

	// The real functions, spliced so the probe runs shipped code. `between` cuts
	// at the next top-level close, so each is taken on its own.
	var code strings.Builder
	for _, fn := range []string{"askCloseSession", "isSessionDeadInWindows", "archiveOrDeleteDialog", "archiveSessionById", "deleteSessionById"} {
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
	// bindShellWindowCloseButton is the entry point: it must route through the
	// prompt rather than issuing the DELETE itself.
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
  window: { _lastSessionsSnapshot: [] },
  _keydown: [],
  _els: {},
  _clones: {},
  document: {
    getElementById(id) {
      if (!sandbox._els[id]) {
        sandbox._els[id] = {
          id: id, textContent: '', style: {}, onclick: null, hidden: false,
          classList: { add() {}, remove() {}, contains() { return false; } },
          addEventListener(type, fn) { sandbox._els[id]['on' + type] = fn; },
          removeEventListener() {},
          // A clone is what the dialog re-wires each open, and the clone is the
          // live node afterwards (the original is replaced). It is recorded, so
          // the probe can press the key that is actually on screen rather than
          // the detached original.
          cloneNode() {
            const cid = id;
            const clone = { focus() { sandbox._focused = cid; }, parentNode: { replaceChild() {} },
              addEventListener(type, fn) { if (type === 'click') clone._click = fn; } };
            sandbox._clones[cid] = clone;
            return clone;
          },
          parentNode: { replaceChild() {} },
          focus() { sandbox._focused = id; },
        };
      }
      return sandbox._els[id];
    },
    addEventListener(type, fn) { if (type === 'keydown') sandbox._keydown.push(fn); },
    removeEventListener(type, fn) {
      if (type !== 'keydown') return;
      const i = sandbox._keydown.indexOf(fn); if (i >= 0) sandbox._keydown.splice(i, 1);
    },
  },
  hideModal() { sandbox._hidden = true; }, showModal() { sandbox._hidden = false; },
  setLoadBanner() {}, showCopyToast(m) { sandbox._toasts.push(m); },
  stripSessionPrefix: s => s,
  closeShellWindow() { sandbox._closed++; },
  _toasts: [], _closed: 0, _focused: '', _hidden: true,
  // confirmDialog's answer is driven per case; the archive dialog is NOT stubbed
  // by default, because its own key handling is part of what this test verifies.
  confirmDialog: () => Promise.resolve(sandbox._confirmAnswer),
  applyI18n() {},
  _confirmAnswer: true,
  _stubArchiveDialog: null,
};
vm.createContext(sandbox);
vm.runInContext(src, sandbox);

// askCloseSession consults archiveOrDeleteDialog; a case can substitute an
// answer, but the default runs the REAL one so its Escape handling is exercised.
// The real function is captured first, or the override would call itself.
const realArchiveDialog = vm.runInContext('archiveOrDeleteDialog', sandbox);
sandbox.archiveOrDeleteDialog = (name) => {
  if (sandbox._stubArchiveDialog) return Promise.resolve(sandbox._stubArchiveDialog);
  return realArchiveDialog(name);
};
// Let the probe press the archive dialog's own keys. The dialog replaces each
// button with a fresh clone per open, so the pressed node must be the CLONE.
sandbox.pressEscape = () => {
  sandbox._keydown.forEach((fn) => fn({ key: 'Escape', stopPropagation() {} }));
};
sandbox.press = (id) => {
  const el = sandbox._clones[id] || sandbox._els[id];
  if (!el || !el._click) throw new Error('no click handler on ' + id);
  el._click({ preventDefault() {} });
};

let bad = 0;
function check(name, cond, extra) {
  if (!cond) { console.log('FAIL ' + name + (extra ? ': ' + extra : '')); bad++; }
  else console.log('PASS ' + name);
}
function reset(snapshot, answer) {
  sandbox.window._lastSessionsSnapshot = snapshot;
  sandbox._confirmAnswer = answer === undefined ? true : answer;
  sandbox._toasts = []; sandbox._closed = 0; sandbox._focused = ''; sandbox._hidden = true;
  sandbox._stubArchiveDialog = null;
  sandbox.pressArchive = () => sandbox.press('modal-close-archive');
  sandbox.pressCancel = () => sandbox.press('modal-close-cancel');
  sandbox._calls = [];
  sandbox._keydown = [];
  sandbox._els = {};
  sandbox._clones = {};
  sandbox.fetch = (url, opt) => { sandbox._calls.push(((opt && opt.method) || 'GET') + ' ' + url); return Promise.resolve({ ok: true, status: 204 }); };
}
const live = [{ id: 's1', name: 'box', status: 'running' }];
const dead = [{ id: 's1', name: 'box', status: 'exited' }];

// A running session must ASK: with the archive dialog declining to answer, no
// request may be issued. (The dialog is driven through its markup, which the
// sandbox stubs, so this case asserts the routing decision.)
reset(live);
(async () => {
  // 1) Running session -> the archive dialog is consulted, and choosing archive
  //    calls terminate, never DELETE.
  reset(live);
  sandbox._stubArchiveDialog = 'archive';
  let outcome = await vm.runInContext("askCloseSession('s1', {})", sandbox);
  check('running + archive -> outcome archived', outcome === 'archived', JSON.stringify(outcome));
  check('running + archive -> POST /terminate', sandbox._calls.some(c => c === 'POST /api/sessions/s1/terminate'), JSON.stringify(sandbox._calls));
  check('running + archive -> never DELETE', !sandbox._calls.some(c => c.indexOf('DELETE') === 0), JSON.stringify(sandbox._calls));

  // 2) Choosing delete on a running session does erase it.
  reset(live);
  sandbox._stubArchiveDialog = 'delete';
  outcome = await vm.runInContext("askCloseSession('s1', {})", sandbox);
  check('running + delete -> outcome deleted', outcome === 'deleted', JSON.stringify(outcome));
  check('running + delete -> DELETE', sandbox._calls.some(c => c === 'DELETE /api/sessions/s1'), JSON.stringify(sandbox._calls));

  // 3) Cancel must issue NOTHING. This is the assertion that catches a prompt
  //    that acts on the safe answer.
  reset(live);
  sandbox._stubArchiveDialog = 'cancelled';
  outcome = await vm.runInContext("askCloseSession('s1', {})", sandbox);
  check('running + cancel -> outcome cancelled', outcome === 'cancelled', JSON.stringify(outcome));
  check('running + cancel -> no request at all', sandbox._calls.length === 0, JSON.stringify(sandbox._calls));
  check('running + cancel -> window not closed', sandbox._closed === 0);

  // 3b) The dialog's OWN keys, run for real (not stubbed). Escape must cancel,
  //     and it must issue no request: a keyboard answer that deletes is the
  //     worst possible failure of a prompt that exists to prevent exactly that.
  reset(live);
  let p = vm.runInContext("askCloseSession('s1', {})", sandbox);
  check('running -> dialog opened', sandbox._hidden === false, 'hidden=' + sandbox._hidden);
  check('running -> archive key focused', sandbox._focused === 'modal-close-archive', 'focused=' + sandbox._focused);
  sandbox.pressEscape();
  outcome = await p;
  check('real Escape -> cancelled', outcome === 'cancelled', JSON.stringify(outcome));
  check('real Escape -> no request', sandbox._calls.length === 0, JSON.stringify(sandbox._calls));

  // 3c) The dialog's own archive key must archive.
  reset(live);
  p = vm.runInContext("askCloseSession('s1', {})", sandbox);
  sandbox.pressArchive();
  outcome = await p;
  check('real archive key -> archived', outcome === 'archived', JSON.stringify(outcome));
  check('real archive key -> terminate only', sandbox._calls.length === 1 && sandbox._calls[0] === 'POST /api/sessions/s1/terminate', JSON.stringify(sandbox._calls));

  // 3d) The dialog's own cancel key must cancel.
  reset(live);
  p = vm.runInContext("askCloseSession('s1', {})", sandbox);
  sandbox.pressCancel();
  outcome = await p;
  check('real cancel key -> cancelled', outcome === 'cancelled', JSON.stringify(outcome));
  check('real cancel key -> no request', sandbox._calls.length === 0, JSON.stringify(sandbox._calls));

  // 4) An archived session is not offered archiving: it goes straight to the
  //    delete confirmation (the pre-existing behaviour for a DEAD tile).
  reset(dead);
  outcome = await vm.runInContext("askCloseSession('s1', {})", sandbox);
  check('archived + confirm -> deleted', outcome === 'deleted', JSON.stringify(outcome));
  check('archived + confirm -> DELETE', sandbox._calls.some(c => c === 'DELETE /api/sessions/s1'), JSON.stringify(sandbox._calls));
  check('archived + confirm -> never terminate', !sandbox._calls.some(c => c.indexOf('/terminate') > 0), JSON.stringify(sandbox._calls));

  // 5) Declining the archived delete confirmation must also issue nothing.
  reset(dead, false);
  outcome = await vm.runInContext("askCloseSession('s1', {})", sandbox);
  check('archived + decline -> cancelled', outcome === 'cancelled', JSON.stringify(outcome));
  check('archived + decline -> no request', sandbox._calls.length === 0, JSON.stringify(sandbox._calls));

  // 6) An id absent from the snapshot is treated as LIVE, so it is asked about
  //    rather than deleted silently: unknown must fail toward the recoverable
  //    answer, never toward erasure.
  reset([]);
  sandbox._stubArchiveDialog = 'cancelled';
  outcome = await vm.runInContext("askCloseSession('ghost', {})", sandbox);
  check('unknown id -> asked (treated as live)', outcome === 'cancelled', JSON.stringify(outcome));
  check('unknown id -> nothing deleted', sandbox._calls.length === 0, JSON.stringify(sandbox._calls));

  process.exit(bad ? 1 : 0);
})();
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "probe.js")
	srcPath := filepath.Join(dir, "close.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcPath, []byte(code.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, scriptPath, srcPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("close-session probe failed:\n%s", out)
	}
	if !strings.Contains(string(out), "PASS unknown id -> nothing deleted") {
		t.Fatalf("probe did not run to completion:\n%s", out)
	}
}

// TestTheCloseButtonDoesNotDeleteByItself pins the wiring a helper test cannot
// see: the button handler must route through askCloseSession. If it kept its own
// fetch, the prompt above would be dead code and the x would still erase.
func TestTheCloseButtonDoesNotDeleteByItself(t *testing.T) {
	body := readAssetLF(t, "static/js/shell-windows.js")
	bind := between(t, body, "function bindShellWindowCloseButton(", "\n}\n")

	if !strings.Contains(bind, "askCloseSession(") {
		t.Error("the close button must route through askCloseSession: a running session has to be asked about " +
			"before its transcript is destroyed")
	}
	if strings.Contains(bind, "method: 'DELETE'") {
		t.Error("the close button must not issue the DELETE itself: that is the silent-erase path the prompt replaced")
	}
	// A placeholder window has no session to ask about; it must still close.
	if !strings.Contains(bind, "win._placeholder") {
		t.Error("a placeholder (dial in progress) has no session, so its close must stay immediate")
	}
}

// TestTheArchivePromptMakesArchiveTheDefault pins the dialog's shape, because the
// order and the default focus are what keep a destructive answer from being the
// one a hurried Enter picks.
func TestTheArchivePromptMakesArchiveTheDefault(t *testing.T) {
	index := readAssetLF(t, "index.html")
	if !strings.Contains(index, `id="modal-close-session"`) {
		t.Fatal("index.html no longer declares the close-session dialog")
	}
	dialog := between(t, index, `id="modal-close-session"`, "</div>\n  </div>")
	for _, id := range []string{"modal-close-archive", "modal-close-delete", "modal-close-cancel", "modal-close-msg"} {
		if !strings.Contains(dialog, id) {
			t.Errorf("the close dialog must offer %s; got %q", id, dialog)
		}
	}
	// The destructive key must be the danger-styled one, and the recommended one
	// must not be: the styling is the only cue distinguishing them at a glance.
	if !strings.Contains(dialog, `class="btn btn-danger" id="modal-close-delete"`) {
		t.Error("delete is the destructive answer and must carry the danger styling")
	}
	if strings.Contains(dialog, `btn-danger" id="modal-close-archive"`) {
		t.Error("archive is the recoverable answer and must not be styled as destructive")
	}
	// The action row ends with the recommended key. This is a visual-order claim
	// and it is the one the dialog relies on: the rightmost button is where a
	// habitual click lands, and it must be the recoverable answer, not delete.
	// The dialog also focuses archive explicitly (asserted below), so the layout
	// and the focus agree instead of pointing at different keys.
	ai := strings.Index(dialog, `id="modal-close-archive"`)
	di := strings.Index(dialog, `id="modal-close-delete"`)
	if ai < 0 || di < 0 {
		t.Fatalf("the dialog must offer both archive and delete; archive at %d, delete at %d", ai, di)
	}
	if ai < di {
		t.Errorf("archive is the recommended answer and must be the LAST key in the row (rightmost), so a "+
			"habitual click reaches the recoverable action; archive at %d, delete at %d", ai, di)
	}

	// And the dialog's own logic must focus archive, so Enter archives.
	body := readAssetLF(t, "static/js/shell-windows.js")
	fn := between(t, body, "function archiveOrDeleteDialog(", "\n}\n")
	if !strings.Contains(fn, "a.focus()") {
		t.Error("the archive key must take focus: Enter on a session the user meant to put away must not delete it")
	}
	if !strings.Contains(fn, "'archive'") || !strings.Contains(fn, "'delete'") || !strings.Contains(fn, "'cancelled'") {
		t.Error("the dialog must be able to answer archive, delete or cancelled")
	}
	// Escape is the only keyboard answer, and it must cancel.
	if !strings.Contains(fn, "Escape") || !strings.Contains(fn, "done('cancelled')") {
		t.Error("Escape must cancel the dialog: the safe answer on an accidental key is to do nothing")
	}
}

// TestClosePromptStringsExistInEveryCatalog guards the prompt's copy. A missing
// key renders as the key itself, so a user would read "session.close.archive" on
// the button that decides whether their transcript survives.
func TestClosePromptStringsExistInEveryCatalog(t *testing.T) {
	cat := readAssetLF(t, "static/js/i18n-catalog.js")
	for _, key := range []string{"session.close.title", "session.close.message", "session.close.hint", "session.close.archive", "session.close.archived"} {
		if n := strings.Count(cat, "'"+key+"':"); n != 3 {
			t.Errorf("%s must be defined in all three catalogs; found %d", key, n)
		}
	}
}
