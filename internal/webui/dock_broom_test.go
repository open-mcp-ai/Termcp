package webui

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestTheBroomClosesArchivedWindowsAndNothingElse runs the real sweep under node.
//
// The dock's broom exists because the only way to get an ended session's window
// off the workspace was to close them one at a time. What it must NOT be is a
// second delete key: the archive column already owns erasing records, and a key
// that looks like tidying while it destroys transcripts is the failure this pins
// against.
//
// Each assertion is a way the sweep could be wrong while still looking right:
//
//   - a window whose session is still running must survive (it is not archived);
//   - a status card (extension) and a dial in flight have no session, so they
//     must survive too — closing them would kill something mid-connection;
//   - every window of an ended session must go, including the tiled ones;
//   - the key must be disabled while there is nothing to sweep, or it invites a
//     press that does nothing.
func TestTheBroomClosesArchivedWindowsAndNothingElse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the dock broom")
	}
	var code strings.Builder
	for _, src := range []struct{ file, fn string }{
		{"static/js/shell-windows.js", "closeArchivedWindows"},
		{"static/js/shell-windows.js", "archivedSessionWindows"},
		{"static/js/shell-windows.js", "paintCloseArchivedKey"},
		{"static/js/shell-windows.js", "isSessionDeadInWindows"},
		{"static/js/sessions.js", "isDeadSession"},
	} {
		body := readAssetLF(t, src.file)
		start := strings.Index(body, "function "+src.fn+"(")
		if start < 0 {
			t.Fatalf("%s no longer defines %s()", src.file, src.fn)
		}
		end := strings.Index(body[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %s", src.fn)
		}
		code.WriteString(body[start : start+end+3])
		code.WriteString("\n")
	}

	script := `
const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = {
  console: console,
  t: k => k,
  tCount: (one, other, v) => (v.count === 1 ? one : other),
  window: {},
  _closed: [], _toasts: [],
  showCopyToast: m => sandbox._toasts.push(m),
  refreshSessionTabbar() {},
  closeShellWindow: w => sandbox._closed.push(w.id)
};
vm.createContext(sandbox);
vm.runInContext(src, sandbox);

function win(id, sid, extra) {
  return Object.assign({ id: id, _parentSid: sid || null, _placeholder: false }, extra || {});
}

const cases = [
  { name: 'nothing open', snapshot: [], wins: [], want: [] },
  { name: 'live session stays', snapshot: [{ id: 's1', status: 'running' }],
    wins: [win('w1', 's1')], want: [] },
  { name: 'ended session goes', snapshot: [{ id: 's1', status: 'exited' }],
    wins: [win('w1', 's1'), win('w2', 's1')], want: ['w1', 'w2'] },
  { name: 'mixed set', snapshot: [{ id: 's1', status: 'exited' }, { id: 's2', status: 'running' }],
    wins: [win('w1', 's1'), win('w2', 's2'), win('w3', 's1')], want: ['w1', 'w3'] },
  { name: 'status card and dial survive', snapshot: [{ id: 's1', status: 'exited' }],
    wins: [win('w1', 's1'), win('card', 'ext-name', { _extName: 'foo' }), win('dial', null, { _placeholder: true })],
    want: ['w1'] },
  { name: 'unknown session is not archived', snapshot: [], wins: [win('w1', 'ghost')], want: [] }
];

let bad = 0;
function eq(label, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    bad++;
    console.log('FAIL ' + label + ': got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want));
  }
}

cases.forEach(c => {
  sandbox.window._lastSessionsSnapshot = c.snapshot;
  sandbox.allShellWins = () => c.wins;
  sandbox._closed = [];
  sandbox.closeArchivedWindows();
  eq(c.name, sandbox._closed, c.want);
  const btn = { disabled: null };
  sandbox.document = { getElementById: () => btn };
  sandbox.paintCloseArchivedKey();
  eq(c.name + ': key disabled', btn.disabled, c.want.length === 0);
});

if (bad) process.exit(1);
console.log('broom ok');
`
	scriptPath := t.TempDir() + "/broom.js"
	codePath := t.TempDir() + "/real.js"
	if err := os.WriteFile(codePath, []byte(code.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, codePath).CombinedOutput()
	if err != nil {
		t.Fatalf("broom probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "broom ok") {
		t.Fatalf("broom probe did not report success:\n%s", out)
	}
}

// TestTheArchiveKeepsThreeKeys pins the sweep key's HOME.
//
// The broom began life as a fourth icon injected into the archive column's
// toolbar, next to export / delete / invert. Two things were wrong with that: the
// column's keys all act on the ticked ROWS, so a key acting on windows sat in a
// row whose meaning it quietly contradicted; and it landed as the count's adjacent
// sibling, which broke the `#archive-toggle-count[hidden] + #archive-batch-export`
// fallback that pushes the run to the right edge — the whole toolbar slid left.
// It now belongs to the dock, where the verbs are about windows.
func TestTheArchiveKeepsThreeKeys(t *testing.T) {
	sessions := readAssetLF(t, "static/js/sessions.js")
	shell := readAssetLF(t, "static/js/shell-windows.js")
	index := readAssetLF(t, "index.html")
	for _, gone := range []string{"archive-clear-all", "clearAllArchivedSessions", "paintClearAllKey"} {
		if strings.Contains(sessions, gone) || strings.Contains(shell, gone) || strings.Contains(index, gone) {
			t.Errorf("the archive toolbar still carries %q; the sweep belongs to the dock, and a fourth key "+
				"in that row broke the count's adjacent-sibling push to the right edge", gone)
		}
	}
	if !strings.Contains(shell, "id = 'dock-close-archived'") {
		t.Error("shell-windows.js no longer injects the dock's broom key")
	}
	if strings.Contains(index, "dock-close-archived") {
		t.Error("the broom is injected by shell-windows.js; it must not spend the index.html byte budget")
	}
	cluster := between(t, shell, "// Fixed right-side window controls", "function updateTileToggleButton")
	if !strings.Contains(cluster, "className = 'ext-key ext-key-icon'") {
		t.Error("the broom must use .ext-key.ext-key-icon")
	}
	if !strings.Contains(cluster, "dock.insertBefore(broom, actions)") {
		t.Error("the broom must sit before actions: left of the vertical divider border")
	}
}
