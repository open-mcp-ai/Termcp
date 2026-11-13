package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A Test verdict has to be REACHABLE, not merely written.
//
// The connection editor is taller than its dialog, so the user arrives at the
// Test button at the bottom edge of a scrolling container. The verdict used to
// be the last line of that container, appended *below* the row the button lives
// in: measured on the real page, a one-line "✓ Connected in 15 ms" had 15 of its
// 16 pixels under the fold and a real failure — the API appends a multi-line
// hint — 95 of 96. The dialog does not follow a growing child on its own, so the
// user saw a button that stopped saying "Testing…" and no verdict at all.
//
// Two halves fix that, and each can regress on its own: the verdict shares the
// action row (index.html + base.css, pinned by the layout probe in
// conn_verdict_layout_test.go) and the code that shows it scrolls the dialog to
// it (this test). This one runs the REAL click handler under node with a stubbed
// network, so it exercises the shipped code rather than a copy of its rules.
func TestConnectionTestVerdictIsBroughtIntoView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the connection dialog")
	}
	body := readAssetLF(t, "static/js/conn-form.js")
	handler := between(t, body, "document.getElementById('conn-test').onclick", "\n};")
	// between() drops the terminator, which for this statement is the closing brace
	// of the function expression; the probe has to run it, so put it back.
	handler += "\n};"
	// The helpers live after the dialog they serve; splice them in so the probe
	// runs them in the same scope the page does (hoisted function declarations).
	for _, fn := range []string{
		"function bumpConnModalSeq()",
		"function connModalSeq()",
		"function showConnTestResult(text, color)",
	} {
		if !strings.Contains(body, fn) {
			t.Fatalf("conn-form.js no longer defines %s(); a verdict that lands is not brought into view", fn)
		}
		handler += "\n" + between(t, body, fn, "\n}\n") + "\n}\n"
	}

	script := `
const fs = require('fs');
const vm = require('vm');

/* scrollIntoView is recorded rather than performed: this probe has no layout,
   which is exactly why the placement half is measured in a browser. What it can
   prove is that the code CALLS it — a verdict shown without a scroll is the bug
   this test exists for. */
function makeEl(id) {
  return { id: id, style: {}, classList: { add() {}, remove() {} },
           addEventListener() {}, value: '', textContent: '', checked: false,
           disabled: false, scrolls: 0, lastArg: null,
           scrollIntoView(arg) { this.scrolls++; this.lastArg = arg; },
           querySelector() { return null; } };
}

const handler = fs.readFileSync(process.argv[2], 'utf8');
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
  /* The i18n catalogue is its own asset; keep the key visible in the text so a
     check can tell a success verdict from a failure one. */
  t: (k, p) => k + (p ? JSON.stringify(p) : ''),
  apiPath: p => p,
  _connGetBody: () => 'host = "example"',
  fetch: null,
  Promise: Promise, JSON: JSON, String: String, console: console,
};
vm.createContext(sandbox);
vm.runInContext(handler, sandbox);

const el = id => sandbox.document.getElementById(id);
const btn = el('conn-test');
const out = el('conn-test-result');
const tick = () => new Promise(r => setTimeout(r, 0));
function clickTest() { btn.onclick.call(btn); }

let bad = 0;
function check(name, got, want) {
  const same = JSON.stringify(got) === JSON.stringify(want);
  if (!same) { console.log('FAIL ' + name + ': got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want)); bad++; }
  else console.log('PASS ' + name);
}

(async () => {
  // A success verdict is one short line: it has to be shown, coloured, and the
  // dialog scrolled to where it is.
  sandbox.fetch = () => Promise.resolve({ ok: true, json: () => Promise.resolve({ ok: true, duration_ms: 15 }) });
  clickTest();
  await tick();
  check('a success verdict is shown', out.style.display, 'block');
  check('a success verdict carries the timing', out.textContent.indexOf('test.ok') === 0, true);
  check('a success verdict is coloured', out.style.color, 'var(--success)');
  check('showing the verdict scrolls the dialog to it', out.scrolls, 1);
  check('the scroll asks for the nearest position', out.lastArg && out.lastArg.block, 'nearest');

  // A failure is the tall case: the API appends a multi-line hint.
  sandbox.fetch = () => Promise.resolve({ ok: true, json: () => Promise.resolve({ ok: false, error: 'i/o timeout' }) });
  clickTest();
  await tick();
  check('a failure verdict is shown', out.style.display, 'block');
  check('a failure verdict carries the reason', out.textContent.indexOf('test.failed') === 0, true);
  check('a failure verdict is coloured', out.style.color, 'var(--danger)');
  check('a failure verdict is scrolled to as well', out.scrolls, 2);

  // A verdict about a profile the user has left must not paint — and must not
  // yank the dialog either, which would be a scroll with nothing to show for it.
  let release;
  const textBefore = out.textContent;
  const scrollsBefore = out.scrolls;
  sandbox.fetch = () => new Promise(r => { release = r; });
  clickTest();
  vm.runInContext('bumpConnModalSeq()', sandbox);
  release({ ok: true, json: () => Promise.resolve({ ok: false, error: 'late answer' }) });
  await tick();
  check('a stale verdict does not paint', out.textContent, textBefore);
  check('a stale verdict does not scroll the dialog', out.scrolls, scrollsBefore);

  console.log(bad === 0 ? 'VERDICT OK' : bad + ' verdict failure(s)');
  process.exit(bad === 0 ? 0 : 1);
})();
`
	tmp := t.TempDir()
	handlerPath := filepath.Join(tmp, "handler.js")
	if err := os.WriteFile(handlerPath, []byte(handler), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmp, "verdict_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, handlerPath).CombinedOutput()
	if err != nil {
		t.Fatalf("verdict probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "VERDICT OK") {
		t.Fatalf("verdict probe did not pass:\n%s", out)
	}
}
