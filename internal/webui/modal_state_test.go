package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every dialog in the page is ONE singleton element reused by every open
// (index.html declares each .modal-backdrop once; showModal/hideModal only
// toggle a class). Anything a dialog writes that the next open does not write
// again is therefore residue: the user reads it as belonging to whatever is on
// screen now. The connection editor was the worst case, because the residue was
// a *verdict* — "✓ Connected in 12 ms" left standing under a host that had never
// been dialled, and a Test button still disabled on "Testing…".
//
// These tests run the REAL openConnModal under node, with the same slice
// boundary the dialog's other test uses (declaration to the next top-level
// statement), so they exercise the shipped code rather than a copy of its rules.
func TestConnectionDialogResetsItsStateOnOpen(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the connection dialog")
	}
	body := readAssetLF(t, "static/js/conn-form.js")
	modal := between(t, body, "function openConnModal(", "\ndocument.getElementById('conn-f-auth').onchange")
	// The reset helpers are declared after the dialog they serve; splice them in
	// so the probe runs them as the page does (same script scope, hoisted).
	for _, fn := range []string{
		"function bumpConnModalSeq()",
		"function connModalSeq()",
		"function resetConnTestResult()",
	} {
		if !strings.Contains(body, fn) {
			t.Fatalf("conn-form.js no longer defines %s(); a dialog reopened after a Test keeps the previous verdict on screen", fn)
		}
		modal += "\n" + between(t, body, fn, "\n}\n") + "\n}\n"
	}
	if !strings.Contains(modal, "resetConnTestResult()") {
		t.Fatal("openConnModal defines the Test reset but never calls it; the previous open's verdict stands")
	}

	script := `
const fs = require('fs');
const vm = require('vm');

function makeEl(id) {
  return { id: id, style: {}, classList: { add() {}, remove() {} },
           addEventListener() {}, value: '', textContent: '', checked: false,
           readOnly: false, disabled: false, querySelector() { return null; } };
}

const modal = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = {
  document: {
    _els: {},
    getElementById(id) {
      if (!this._els[id]) this._els[id] = makeEl(id);
      return this._els[id];
    },
    querySelectorAll() { return []; },
    addEventListener() {},
  },
  t: k => k,
  window: {},
  /* A dial that never settles, so the probe controls exactly when a response
     lands relative to the next open. */
  fetch: () => new Promise(() => {}),
  URLSearchParams: function () {},
  console: console,
};
vm.createContext(sandbox);
vm.runInContext('var editingConnName = \'\'; var connEntries = null;\n' +
  'var _connTemplateRemote = \'\';\n' +
  'function _connTOMLToForm() {} function _connShowView() {}\n' +
  'function loadConnections() {} function resetConnPasswordVisibility() {}\n' +
  'function showModal() {}\n' +
  modal, sandbox);

let bad = 0;
function check(name, got, want) {
  const same = JSON.stringify(got) === JSON.stringify(want);
  if (!same) { console.log('FAIL ' + name + ': got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want)); bad++; }
  else console.log('PASS ' + name);
}
const d = sandbox.document;
const el = id => d.getElementById(id);

// What a previous session leaves behind: a verdict, a busy button, and the TOML
// glyph over a form view.
function leaveResidue() {
  el('conn-test-result').style.display = 'block';
  el('conn-test-result').textContent = '\u2713 Connected in 12 ms';
  el('conn-test-result').style.color = '#1a7f37';
  el('conn-test').disabled = true;
  el('conn-test').textContent = 'test.testing';
  el('modal-conn-err').style.display = 'block';
  el('modal-conn-err').textContent = 'stale failure';
  el('conn-form-view').style.display = 'none';
  el('conn-config-view').style.display = '';
  el('conn-icon-form').style.display = 'none';
  el('conn-icon-toml').style.display = '';
}

leaveResidue();
vm.runInContext("openConnModal(false, '')", sandbox);

check('verdict is cleared', el('conn-test-result').textContent, '');
check('verdict is hidden', el('conn-test-result').style.display, 'none');
check('verdict colour is cleared', el('conn-test-result').style.color, '');
check('Test button is enabled again', el('conn-test').disabled, false);
check('Test button is relabelled', el('conn-test').textContent, 'common.test');
check('previous error is cleared', el('modal-conn-err').textContent, '');
check('previous error is hidden', el('modal-conn-err').style.display, 'none');
check('a new profile reopens in form view', el('conn-form-view').style.display, '');
check('the TOML view is hidden', el('conn-config-view').style.display, 'none');
check('the form glyph is showing', el('conn-icon-form').style.display, '');
check('the TOML glyph is hidden', el('conn-icon-toml').style.display, 'none');

// A verdict is about the profile it was run against, so a dial that resolves
// after the dialog has moved on to another profile must not paint. Same rule for
// the profile read itself: the dialog shows the newer open's host.
leaveResidue();
check('the dialog has an open generation', typeof el('modal-conn')._termcpOpenSeq, 'number');
const first = el('modal-conn')._termcpOpenSeq;
vm.runInContext("openConnModal(true, 'second', 'remote')", sandbox);
check('a later open bumps the generation', el('modal-conn')._termcpOpenSeq, first + 1);
check('a stale verdict cannot survive the second open either', el('conn-test-result').textContent, '');

console.log(bad === 0 ? 'MODAL STATE OK' : bad + ' state failure(s)');
process.exit(bad === 0 ? 0 : 1);
`
	tmp := t.TempDir()
	modalPath := filepath.Join(tmp, "modal.js")
	if err := os.WriteFile(modalPath, []byte(modal), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmp, "modal_state_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, modalPath).CombinedOutput()
	if err != nil {
		t.Fatalf("modal state probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "MODAL STATE OK") {
		t.Fatalf("modal state probe did not pass:\n%s", out)
	}
}

// The dialogs also have to survive a request that outlives them. Both the
// profile read and the Test verdict are dials with no upper bound on latency, so
// a response can arrive after the user has reopened the dialog — or opened a
// different profile in it. Applying it then paints one profile's answer under
// another profile's name, which is the same class of lie as the residue above,
// only harder to see because the field is written while the user is looking.
func TestConnectionDialogDropsStaleResponses(t *testing.T) {
	body := readAssetLF(t, "static/js/conn-form.js")

	if !strings.Contains(body, "function bumpConnModalSeq()") {
		t.Fatal("the connection dialog lost its open-generation counter; a slow dial can paint over a later open")
	}
	// Each asynchronous writer records the generation it started under and drops
	// its result once the dialog has moved on. The count is the number of writers
	// that must guard: the profile read (both the success and the failure arm)
	// and the three arms of a Test.
	if n := strings.Count(body, "connModalSeq() !== seq"); n < 5 {
		t.Errorf("only %d response(s) are guarded against a stale open; expected every arm of the profile read and the Test", n)
	}
	for _, want := range []string{
		"var seq = bumpConnModalSeq();",
		"var seq = connModalSeq();",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("conn-form.js no longer records the open generation (%q)", want)
		}
	}
	// Closing ends the open: a verdict that lands after the user has left the
	// dialog belongs to nothing on screen.
	close := between(t, body, "document.getElementById('modal-conn-close').onclick", "\n};")
	if !strings.Contains(close, "bumpConnModalSeq()") {
		t.Error("closing the connection dialog must invalidate in-flight work for that open")
	}
}

// Write-only state is residue too — in the code rather than on screen. Each of
// these three was read by nothing, so each one could only ever be a stale value
// waiting for a reader to be added back:
//
//   - _connDirty was the leave-page guard's "unsaved profile edits" reason. The
//     reason was removed when the guard was narrowed to open terminal windows
//     (nothing about this tab staying open depends on an unsaved form; the
//     editor is a dialog over a page that keeps its state server-side), and the
//     twelve writers stayed behind.
//   - _fwdSshCfg plus its hidden field #fw-ssh-config-modal existed to label a
//     forward with the profile the modal was opened from. The server derives
//     that label from the session it creates the forward on, so the value was
//     collected, echoed into an input nothing read, and never sent.
//   - startConnName duplicated #start-ssh-config, which the launch dialog
//     actually reads; the two were written in different places, so the loser
//     could only ever disagree with the winner.
//
// They are pinned to absence rather than to a comment, because dead state that
// looks like live state is what the next reader will trust.
func TestNoWriteOnlyDialogState(t *testing.T) {
	files := map[string]string{}
	for _, f := range jsModules(t) {
		files[f] = readAssetLF(t, f)
	}
	index := readAssetLF(t, "index.html")
	all := index
	for _, b := range files {
		all += "\n" + b
	}
	for _, gone := range []string{
		"_connDirty",
		"_fwdSshCfg",
		"startConnName",
		"fw-ssh-config-modal",
	} {
		for name, b := range files {
			if strings.Contains(b, gone) {
				t.Errorf("%s still carries write-only dialog state %q", name, gone)
			}
		}
		if strings.Contains(index, gone) {
			t.Errorf("index.html still carries write-only dialog state %q", gone)
		}
	}
}

// The dialogs' own singleton-element resets are asserted where the markup is, so
// a field added to a dialog has one place to be listed: every input the next open
// must not inherit is named here.
func TestSingletonDialogsResetTheirFields(t *testing.T) {
	forms := readAssetLF(t, "static/js/conn-form.js")
	importRun := between(t, forms, "document.getElementById('conn-import-open').onclick", "\n};")
	for _, want := range []string{
		"getElementById('conn-import-file').value = ''",
		"getElementById('conn-import-err').style.display = 'none'",
		"getElementById('conn-import-result').style.display = 'none'",
	} {
		if !strings.Contains(importRun, want) {
			t.Errorf("the import dialog does not reset %q on open", want)
		}
	}
	// A run that is still going leaves the button disabled on the previous
	// open's request; reopening must not inherit that.
	if !strings.Contains(importRun, "conn-import-run") {
		t.Error("the import dialog must re-enable its run button on open; a previous import leaves it disabled")
	}
	start := between(t, forms, "function openStartModal(", "\n}")
	// The launch dialog's target is a hidden field, so it is the one piece of its
	// state a user cannot see or correct: it must be set from the argument.
	if !strings.Contains(start, "getElementById('start-ssh-config')") || !strings.Contains(start, "= connName") {
		t.Error("the launch dialog must write its target from the profile it was opened for, not inherit the previous one")
	}
}

// The forward dialog remembers exactly one thing — the session it will create the
// forward on — and it must rebind it on every open, including the open whose only
// job is to say there is no session. The old code returned before the assignment
// on that path, so the dialog reported "no session" while still holding the last
// one: "Create" would then have posted a listener to a session the user had not
// aimed at. Same rule as the reset above, on the field that decides where a
// request lands rather than what it says.
func TestForwardDialogRebindsItsSessionOnEveryOpen(t *testing.T) {
	fm := readAssetLF(t, "static/js/forward-modal.js")
	open := between(t, fm, "function openForwardModal(", "\n}")
	// The binding must come before the no-session early return, so both paths set it.
	bind := strings.Index(open, "_fwdSessionId = sessionId")
	bail := strings.Index(open, "if (!sessionId) {")
	if bind < 0 {
		t.Fatal("openForwardModal no longer binds the session it was opened for")
	}
	if bail < 0 {
		t.Fatal("openForwardModal lost its no-session path")
	}
	if bind > bail {
		t.Error("openForwardModal binds the session after the no-session early return; that path keeps the previous session bound")
	}
	if strings.Contains(open, "_fwdSshCfg") {
		t.Error("the forward dialog is collecting an ssh profile again; the server derives that label from the session")
	}
}
