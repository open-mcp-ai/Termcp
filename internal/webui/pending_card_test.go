package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConnectingDialsAppearInTheSessionList runs the real pending-card helpers
// from sessions.js under node.
//
// Issue #79 asked for a connect in progress to be visible in the list and
// interruptible from there, so that everything running — dials and sessions —
// can be selected and stopped in one gesture. A dial has no session yet (the
// server registers one only when the handshake succeeds), so it is absent from
// every snapshot and the plate cannot render it from the data. These helpers
// synthesise a card per placeholder window instead.
//
// The bug this guards is a silent one: if the synthesised records stopped
// matching the plate's filter (a dial reported as dead, or with no id), the card
// would simply not appear — no error, no failing request, just a list that
// quietly omits the thing the user is waiting on and cannot select it.
func TestConnectingDialsAppearInTheSessionList(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the pending-card helpers")
	}
	body := readAssetLF(t, "static/js/sessions.js")

	// The helpers the plate's composition depends on, taken from the file so a
	// rename fails the test rather than silently testing a stale copy.
	var code strings.Builder
	for _, fn := range []string{
		"isDeadSession", "pendingCardId", "isPendingCardId",
		"pendingSessionRecords", "sessionsWithPending",
		"cancelPendingConn", "cancelAllPendingConns",
	} {
		start := strings.Index(body, "function "+fn+"(")
		if start < 0 {
			t.Fatalf("sessions.js no longer defines %s; the connecting card is untested now", fn)
		}
		end := strings.Index(body[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %s", fn)
		}
		code.WriteString(body[start : start+end+3])
		code.WriteString("\n")
	}

	script := `
const fs = require('fs');
const vm = require('vm');
const helpers = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = {};
vm.createContext(sandbox);
// The module's own globals these helpers read: the list of open windows, the
// session snapshot, the translator, and closeShellWindow. The stub for the last
// one is deliberately faithful — the real one calls win.remove(), so the window
// stops being returned by allShellWins(); a stub that only flagged it would make
// cancelAll look like it stopped dials that are still open.
vm.runInContext(` + "`" + `
var PENDING_ID_PREFIX = 'pending:';
var __wins = [];
var closed = [];
window = { _lastSessionsSnapshot: [] };
var t = function (k) { return k; };
function allShellWins() { return __wins; }
function closeShellWindow(w) {
  closed.push(w);
  w._closed = true;
  var i = __wins.indexOf(w);
  if (i >= 0) __wins.splice(i, 1);
}
function getPendingShellWindowForConn(n) {
  for (var i = 0; i < __wins.length; i++) {
    if (__wins[i]._placeholder && __wins[i]._pendingConnName === n) return __wins[i];
  }
  return null;
}
` + "`" + ` + helpers, sandbox);

let bad = 0;
function check(name, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    console.log('FAIL ' + name + ': got=' + JSON.stringify(got) + ' want=' + JSON.stringify(want));
    bad++;
  }
}

// Two dials in progress and one established session.
sandbox.__wins = [
  { _placeholder: true, _pendingConnName: 'kali' },
  { _placeholder: true, _pendingConnName: 'prod-db' },
  { _placeholder: false, _pendingConnName: '' },
];
vm.runInContext("window._lastSessionsSnapshot = [{ id: 'abc123', name: 'live', status: 'running' }];", sandbox);

const recs = vm.runInContext('pendingSessionRecords()', sandbox);
check('each dial becomes a card', recs.length, 2);
check('card ids are namespaced away from real session ids', recs.map(r => r.id), ['pending:kali', 'pending:prod-db']);
// 'running' is what puts the card on the live plate, which is what makes
// select-all reach it. Reporting a dial as dead would file it under the archive.
check('a dial reads as running', recs.map(r => r.status), ['running', 'running']);
check('a card names the profile it dials', recs.map(r => r.name), ['kali', 'prod-db']);

const composed = vm.runInContext('sessionsWithPending()', sandbox);
check('the plate reads snapshot + dials', composed.map(s => s.id), ['abc123', 'pending:kali', 'pending:prod-db']);

// A dial that finished must leave the list, or the card would outlive the window
// it stands for and offer to cancel something that no longer exists.
sandbox.__wins[0]._placeholder = false;
check('a finished dial leaves the list', vm.runInContext('pendingSessionRecords()', sandbox).length, 1);
sandbox.__wins[0]._placeholder = true;

check('cancelling one dial reports success', vm.runInContext("cancelPendingConn('kali')", sandbox), true);
check('...and closes exactly that window', sandbox.closed.map(w => w._pendingConnName), ['kali']);
check('cancelling an unknown profile does nothing', vm.runInContext("cancelPendingConn('nope')", sandbox), false);

sandbox.closed = [];
check('cancelAll stops every remaining dial', vm.runInContext('cancelAllPendingConns()', sandbox), 1);
check('...which is the one still open', sandbox.closed.map(w => w._pendingConnName), ['prod-db']);
// And once nothing is dialling, select-all has nothing stale to offer.
check('no dials left to cancel', vm.runInContext('cancelAllPendingConns()', sandbox), 0);

console.log(bad === 0 ? 'PENDING-CARD OK' : bad + ' failure(s)');
process.exit(bad === 0 ? 0 : 1);
`

	scriptPath := filepath.Join(t.TempDir(), "pending_card.js")
	helpersPath := filepath.Join(t.TempDir(), "helpers.js")
	if err := os.WriteFile(helpersPath, []byte(code.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, helpersPath).CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("pending-card check failed: %v\n%s", err, got)
	}
	if !strings.Contains(got, "PENDING-CARD OK") {
		t.Fatalf("harness did not run to completion:\n%s", got)
	}
}

// TestBatchActionsReachTheRightEndpointForEachSelectionKind pins the wiring the
// helper test cannot see: which endpoint each batch action calls, and that the
// selections feed it the composed list rather than the raw snapshot.
//
// Three separate ways to get this wrong, all invisible without a browser:
//
//   - select-all built from the snapshot alone never includes a dial, so the
//     "select everything running and stop it" gesture silently skips exactly the
//     thing the user opened the list to stop;
//   - the stop action calling DELETE would destroy the sessions it was asked to
//     disconnect, and the archive would stay empty;
//   - the delete action sending a synthetic pending id to the server would 404
//     on it instead of cancelling the dial.
func TestBatchActionsReachTheRightEndpointForEachSelectionKind(t *testing.T) {
	sessions := readAssetLF(t, "static/js/sessions.js")
	connForm := readAssetLF(t, "static/js/conn-form.js")
	index := readAssetLF(t, "index.html")

	// The stop action posts to /terminate, on a comma-joined list of real ids —
	// the shape the server's batch handler reads.
	stop := between(t, connForm, "function disconnectSelectedInRegion(region) {", "\n}\n")
	if !strings.Contains(stop, "'/terminate'") {
		t.Errorf("the batch stop action must POST to /terminate; disconnecting is not deleting:\n%s", stop)
	}
	if !strings.Contains(stop, "cancelAllPendingConns()") {
		t.Errorf("the batch stop action must also cancel the dials in the selection; otherwise a still-connecting card is skipped:\n%s", stop)
	}
	if !strings.Contains(stop, "method: 'POST'") {
		t.Errorf("the subscribe/terminate endpoint is a POST:\n%s", stop)
	}
	// The URL must be built from the real ids, never from the raw targets. A dial
	// carries a synthetic `pending:` id that no endpoint can resolve, so a stop
	// that joined `targets` would hand the server an id it can only report as
	// missing — and because the client treats session_not_found as success, the
	// stray id would leave no visible trace. This is the same partition the delete
	// branch asserts below, for the same reason; the two actions differ only in the
	// endpoint they reach.
	if !strings.Contains(stop, `liveIds.map(encodeURIComponent).join(',')`) {
		t.Errorf("the batch stop URL must be built from liveIds; a synthetic pending id cannot be resolved by the server:\n%s", stop)
	}
	if !strings.Contains(stop, "!s._pending") {
		t.Errorf("stop must partition the selection into dials and real sessions:\n%s", stop)
	}

	// Delete cancels dials instead of sending their synthetic ids to the server,
	// because a dial has no session for DELETE to remove.
	del := between(t, connForm, "function deleteSelectedInRegion(region) {", "\n}\n")
	if !strings.Contains(del, "sessionsWithPending()") {
		t.Errorf("delete must resolve its targets from the composed list, or it cannot tell a dial from a session:\n%s", del)
	}
	if !strings.Contains(del, "cancelAllPendingConns()") {
		t.Errorf("delete must cancel a selected dial rather than DELETE a synthetic id:\n%s", del)
	}
	// The call itself must be handed the real ids. Asserting only that `liveIds`
	// is declared somewhere in the function would pass while the delete call sent
	// the raw targets — including the synthetic ids — to the server.
	if !strings.Contains(del, "deleteResourcesBatch('sessions', liveIds)") {
		t.Errorf("delete must send only real session ids to the server; sending a pending id would 404 on it:\n%s", del)
	}
	if !strings.Contains(del, "!s._pending") {
		t.Errorf("delete must partition the selection into dials and real sessions:\n%s", del)
	}

	// Both select-all implementations walk the composed list.
	for _, want := range []string{
		"sessionsForRegion(region, sessionsWithPending())",
	} {
		if !strings.Contains(connForm, want) {
			t.Errorf("select-all must use %q so a dial is included in the selection", want)
		}
	}
	if strings.Contains(connForm, "sessionsForRegion(region, window._lastSessionsSnapshot || [])") {
		t.Error("select-all still reads the raw snapshot; a dial in progress would never be selected")
	}

	// The pending-aware membership has to reach the two other places that decide
	// what the plate shows and keeps: the render pass and the selection prune.
	render := between(t, sessions, "function renderSessionGrid(bannerMsg) {", "\n}\n")
	if !strings.Contains(render, "sessionsWithPending()") {
		t.Error("the render pass must compose the dials in, or no connecting card is ever drawn")
	}
	prune := between(t, sessions, "function pruneSessionSelections(present) {", "\n}\n")
	if !strings.Contains(prune, "region.ids.delete(id)") {
		t.Error("pruneSessionSelections no longer prunes")
	}
	// The prune is only correct if it was handed the composed list; that is the
	// caller's job (renderSessionGrid), asserted above.

	// The stop key exists in the markup and is wired for the live plate.
	if !strings.Contains(index, `id="session-stop-btn"`) {
		t.Error("index.html should carry the batch stop key in the sessions header")
	}
	if !strings.Contains(sessions, "stopId: 'session-stop-btn'") {
		t.Error("the sessions region should name its stop button so the batch bar can show it")
	}
	// The archive deliberately has no stop key: everything there has already
	// stopped, so the control would be a dead button.
	if strings.Contains(index, `id="archive-stop-btn"`) {
		t.Error("the archive plate should not carry a stop key")
	}
	if !strings.Contains(sessions, "var stopBtn = region.stopId ? document.getElementById(region.stopId) : null;") {
		t.Error("the batch bar must look the stop key up per region; the archive has none")
	}
}

// TestAPendingWindowRepaintsTheSessionPlate pins the trigger that makes a
// connecting card appear at all.
//
// This is the bug the first version of this feature shipped: the card was
// computed correctly, rendered correctly, and never drawn, because the Sessions
// plate is only repainted by events that a dial in progress does not produce. A
// dial exists solely as a placeholder window, so the window's insertion into the
// container is the one moment its card can appear — and nothing was listening to
// it. The plate kept showing the previous session frame, so the connecting work
// was invisible in the list and therefore unselectable, which is exactly what
// issue #79 asked for.
//
// A helper-level test cannot catch this: it calls the renderer itself, so it
// passes whether or not any real event ever does. What is asserted here is the
// wiring — that the container's own observer, the only signal a pending window
// emits, repaints the plate.
func TestAPendingWindowRepaintsTheSessionPlate(t *testing.T) {
	shell := readAssetLF(t, "static/js/shell-windows.js")

	// The container observer is the signal. Its callback must repaint the plate,
	// not only the tab bar.
	start := strings.Index(shell, "Keep the tab bar in sync as shell windows are opened/closed anywhere.")
	if start < 0 {
		t.Fatal("shell-windows.js no longer wires the shell-windows container observer")
	}
	block := shell[start:]
	if end := strings.Index(block, "})();"); end >= 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "MutationObserver") || !strings.Contains(block, "shellWindowsEl()") {
		t.Fatalf("this is no longer the container observer:\n%s", block)
	}
	if !strings.Contains(block, "refreshSessionTabbar()") {
		t.Error("the container observer no longer refreshes the tab bar")
	}
	if !strings.Contains(block, "renderSessionGrid(") {
		t.Error("the container observer must repaint the session plate: a dial in progress exists only as a " +
			"placeholder window, so its insertion is the only event that can draw its connecting card")
	}
	// The call is guarded because sessions.js loads after shell-windows.js; an
	// unguarded call would throw on the first window of the page's life.
	if !strings.Contains(block, "typeof renderSessionGrid === 'function'") {
		t.Error("the renderSessionGrid call must be guarded: sessions.js loads after this file")
	}

	// And a placeholder window must actually be a child of that container, or the
	// observer never fires for it.
	tw := readAssetLF(t, "static/js/terminal-window.js")
	open := between(t, tw, "function openPendingShellWindow(", "\n}\n")
	if !strings.Contains(open, "container.appendChild(win)") {
		t.Error("the pending window must be appended to the windows container, which is what the observer watches")
	}
	if !strings.Contains(open, "win._placeholder = true") {
		t.Error("the placeholder flag is what pendingSessionRecords keys on; without it no card is produced")
	}
	if !strings.Contains(open, "win._pendingConnName = connName") {
		t.Error("the card is identified by the profile it dials; the window must record it")
	}
}

// TestBatchStopKeyIsHiddenWithoutASelection pins the CSS that makes the stop key
// behave like the trash key.
//
// This shipped broken once: the button carried `hidden`, and it was still on
// screen. A `hidden` attribute is only the UA's `[hidden] { display: none }`,
// which any `display` declaration on a matching class outranks — and `.icon-btn`
// sets `display: inline-flex`. The trash key had been rescued by a rule naming
// `.batch-trash` alone, so a second key with the same attribute needed the same
// rescue and did not get it.
//
// The assertion is on the RULE, not on the markup: the markup already had
// `hidden`, and that is precisely what did not work.
func TestBatchStopKeyIsHiddenWithoutASelection(t *testing.T) {
	css := readAppCSS(t)
	index := readAssetLF(t, "index.html")

	// Both keys carry the attribute, so both need a rule that wins over .icon-btn.
	for _, key := range []string{"batch-trash", "batch-stop"} {
		if !strings.Contains(index, key) {
			continue // a key that does not exist needs no rule
		}
		if !strings.Contains(css, "."+key+"[hidden]") {
			t.Errorf("the %s key carries `hidden` but no CSS rule hides it; `.icon-btn` sets "+
				"`display: inline-flex`, which outranks the UA's [hidden] rule, so the key "+
				"would stay on screen with nothing selected", key)
		}
	}

	// The selector list has to sit after .icon-btn in the cascade or equal
	// specificity would let the base rule win; theme.css loads after base.css.
	rule := between(t, css, ".batch-trash[hidden]", "\n")
	if !strings.Contains(rule, "display: none") {
		t.Errorf("the hidden rule must set display: none; got %q", rule)
	}

	// And the batch bar must actually toggle it, on the same terms as the trash.
	sessions := readAssetLF(t, "static/js/sessions.js")
	bar := between(t, sessions, "function updateSessionBatchBar() {", "\n  });\n}")
	if !strings.Contains(bar, "stopBtn.hidden = count === 0") {
		t.Errorf("the stop key must be hidden exactly when nothing is selected, like the trash key:\n%s", bar)
	}
	if !strings.Contains(bar, "delBtn.hidden = count === 0") {
		t.Error("the trash key's own rule changed; the two keys are meant to appear together")
	}
}

// TestConnectingCardWritesItsStatusWordAsPlainText pins the two classes the
// connecting card adds, and the reason each exists.
//
// A class used in markup but declared nowhere is dead weight that silently
// renders as whatever it inherited — the failure is invisible in review and in a
// screenshot. The sid slot is the case that matters: it inherits the id's hover
// underline and `cursor: pointer`, which promise a copy the connecting card
// cannot offer (it has no id yet), so the plain-text branch has to override both.
func TestConnectingCardWritesItsStatusWordAsPlainText(t *testing.T) {
	sessions := readAssetLF(t, "static/js/sessions.js")
	css := readAppCSS(t)

	// Both classes the pending card introduces are used…
	for _, cls := range []string{"sess-status-ic is-connecting", "sess-sid-line is-pending"} {
		if !strings.Contains(sessions, `class="`+cls+`"`) {
			t.Errorf("the connecting card no longer emits %q; a rename would leave its styles orphaned", cls)
		}
	}
	// …and declared.
	conn := between(t, css, ".conn-tile.sess-tile .sess-status-ic.is-connecting {", "}")
	if conn == "" {
		t.Fatal("the connecting lamp has no rule; it would render as the 7px live dot")
	}
	// It must override the dot's shape, or the spinner draws inside a 7px circle.
	for _, want := range []string{"width:", "height:", "animation:"} {
		if !strings.Contains(conn, want) {
			t.Errorf("the connecting lamp should override %q from the base lamp rule; got %q", want, conn)
		}
	}
	pending := between(t, css, ".sess-sid-line.is-pending {", "}")
	if !strings.Contains(pending, "cursor: default") {
		t.Errorf("the status word is not clickable and must not keep the id's pointer cursor: %q", pending)
	}
	if !strings.Contains(css, ".sess-sid-line.is-pending:hover { color: var(--text-muted); text-decoration: none; }") {
		t.Error("the status word must drop the id's hover underline; that affordance promises a copy that cannot happen")
	}

	// The reduced-motion escape hatch lives with the other ones in theme.css. If
	// it were opened in base.css, `between()` would find it first and
	// TestBackdropIsAFixedDropInLayer would read the wrong block — that is a real
	// coupling, not a style preference.
	if !strings.Contains(css, ".conn-tile.sess-tile .sess-status-ic.is-connecting { animation: none; opacity: 1; transform: none; }") {
		t.Error("the connecting lamp's animation must be disabled under prefers-reduced-motion, in theme.css's existing block")
	}
}
