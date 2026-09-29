package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestChannelStatusRule runs the tab chip's state machine under node.
//
// The rule it encodes is the point of the feature: a submit (enter, or a key a
// line editor treats as one) is what closes a line, the *echo* of typing is not
// output, and a channel whose content has not changed for three seconds is done.
//
// The middle one is what makes the machine necessary: every keystroke comes back
// as bytes, so a rule that paints on arrival strobes while a person types, and a
// rule that looks for line breaks in the bytes ends a line that is still being
// written (a redraw contains one). The fix is not a timing window around the
// echo — that guess fails in both directions — but the server's submit flag,
// which says whether the input ended the line. The test therefore drives the
// echo with *no* window at all: however late it arrives, it cannot close a line.
func TestChannelStatusRule(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the status machine")
	}
	body, err := readAsset("static/js/ui-socket.js")
	if err != nil {
		t.Fatal(err)
	}
	// The machine's functions plus the constants and t() they need.
	names := []string{
		"function shellStatusPaint(",
		"function shellStatusSet(",
		"function shellStatusOnInput(",
		"function shellStatusOnBytes(",
		"function shellStatusOnIdle(",
		"function shellStatusTouch(",
	}
	parts := make([]string, 0, len(names)+5)
	for _, n := range names {
		at := strings.Index(body, n)
		if at < 0 {
			t.Fatalf("ui-socket.js no longer defines %q; the channel chip is untested now", n)
		}
		rest := body[at:]
		end := strings.Index(rest, "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %q", n)
		}
		parts = append(parts, rest[:end+2])
	}
	// Constants the machine reads. The idle window is part of the rule — it is what
	// turns "nothing changed for a while" into "done" — so a machine extracted
	// without it would not run.
	for _, c := range []string{
		"var SHELL_KIND_OUTPUT = 0;", "var SHELL_KIND_API = 1;", "var SHELL_KIND_AI = 2;",
		"var SHELL_STATUS_IDLE_MS = 3000;",
	} {
		if !strings.Contains(body, c) {
			t.Fatalf("ui-socket.js no longer defines %s", c)
		}
		parts = append(parts, c)
	}

	driver := `
var fails = 0;
function eq(name, got, want) {
  if (got !== want) { console.log('FAIL ' + name + ': got ' + got + ', want ' + want); fails++; }
  else { console.log('PASS ' + name); }
}
function every(list, want) {
  for (var i = 0; i < list.length; i++) { if (list[i] !== want) return false; }
  return true;
}

// i18n + a fake tab element, so the machine can paint.
var KEYS = {};
global.t = function (k) { return KEYS[k] || k; };
function fakeEl() {
  return {
    textContent: '', className: '', title: '', style: {},
    setAttribute(k, v) { this[k] = v; },
    getAttribute(k) { return this[k] == null ? null : this[k]; },
    removeAttribute(k) { delete this[k]; }
  };
}
function makeWin() {
  var tab = fakeEl();
  var chip = fakeEl();
  tab.querySelector = function (sel) { return sel === '.shell-channel-tab-state' ? chip : null; };
  return { _channels: { s1: { tabEl: tab, statusState: '' } }, _chip: chip };
}
function state(w) { return w._channels.s1.statusState; }
function text(w) { return w._chip.textContent; }

// A clock and a timer queue, both under the test's control: the rule is about
// when bytes arrive, so the test has to say when.
var NOW = 0;
Date.now = function () { return NOW; };
var timers = [];
var nextTimerId = 1;
global.setTimeout = function (fn, ms) {
  var id = nextTimerId++;
  timers.push({ id: id, fn: fn, at: NOW + ms });
  return id;
};
global.clearTimeout = function (id) {
  timers = timers.filter(function (t) { return t.id !== id; });
};
// Advance the clock, firing whatever is due on the way — a timer that is cleared
// by a later byte must not fire, which is how "output keeps a channel running"
// is expressed.
function tick(ms) {
  var target = NOW + ms;
  for (;;) {
    var due = null;
    for (var i = 0; i < timers.length; i++) {
      if (timers[i].at <= target && (!due || timers[i].at < due.at)) due = timers[i];
    }
    if (!due) break;
    timers = timers.filter(function (t) { return t.id !== due.id; });
    NOW = due.at;
    due.fn();
  }
  NOW = target;
}

// --- plain output: no input anywhere near it ---------------------------
var w = makeWin();
shellStatusOnBytes(w, 's1', 'hello');
eq('output with no input paints running', state(w), 'running');
tick(3000);
eq('three seconds of silence after output paints done', state(w), 'done');

// --- a person types; the echo must not flip it back -------------------
var w2 = makeWin();
shellStatusOnInput(w2, 's1', 'api', false);
eq('typing shows the typing chip', state(w2), 'typing');
shellStatusOnBytes(w2, 's1', 'e');
eq('the echo of a keystroke is not output', state(w2), 'typing');
shellStatusOnBytes(w2, 's1', 'c');
shellStatusOnBytes(w2, 's1', 'h');
eq('a whole word echoed keeps the typing chip', state(w2), 'typing');
// A redraw is bytes too. Nothing here looks for a newline, so a redraw that
// contains one cannot close a line the person is still typing — the exact frame
// a real capture produced (a CR LF and a continuation prompt).
shellStatusOnBytes(w2, 's1', '\r\n> ');
eq('a redraw cannot turn typing into output', state(w2), 'typing');
shellStatusOnBytes(w2, 's1', 'MORE');
eq('typing continues after the redraw', state(w2), 'typing');

// The strobe, measured: ten keystrokes, each echoed, sampled after every event.
// A per-event rule fails this on the first echo. No timing is involved — the
// echo of the whole word arrives with the line still unsubmitted, and that is
// the only fact the machine needs.
var w3 = makeWin();
var seen = [];
for (var k = 0; k < 10; k++) {
  shellStatusOnInput(w3, 's1', 'api', false);
  seen.push(state(w3));
  shellStatusOnBytes(w3, 's1', 'x');
  seen.push(state(w3));
}
eq('ten keystrokes and their echoes never leave typing', every(seen, 'typing'), true);

// A late echo — seconds after the keystroke, the case a timing window gets wrong
// on a laggy link — still cannot close the line. This is why the rule has no
// window: the pending line, not the clock, decides.
var w4 = makeWin();
shellStatusOnInput(w4, 's1', 'api', false);
tick(10000);
shellStatusOnBytes(w4, 's1', 'laggy echo');
eq('a late echo cannot close a pending line', state(w4), 'typing');
// ...and once the submit does arrive, output follows normally.
shellStatusOnInput(w4, 's1', 'api', true);
shellStatusOnBytes(w4, 's1', 'result\r\n');
eq('after the submit the command output paints running', state(w4), 'running');

// --- the enter key ends the line, and the chip says so at once ---------
var w5 = makeWin();
shellStatusOnInput(w5, 's1', 'api', false);
shellStatusOnInput(w5, 's1', 'api', true);
eq('enter ends the typed line', state(w5), 'running');
shellStatusOnBytes(w5, 's1', '\r\n');
eq('the echo of the submitted line stays running', state(w5), 'running');
eq('a submitted line leaves no kind behind', w5._channels.s1.lineKind, SHELL_KIND_OUTPUT);

// --- an agent types: the agent's line reads as the agent's -------------
var w6 = makeWin();
shellStatusOnInput(w6, 's1', 'ai', false);
eq('agent input shows the ai chip', state(w6), 'ai');
shellStatusOnBytes(w6, 's1', 'ls');
eq('an agent echo keeps the ai chip', state(w6), 'ai');
shellStatusOnBytes(w6, 's1', '\r\n> ');
eq('an agent line survives a redraw containing a newline', state(w6), 'ai');
shellStatusOnInput(w6, 's1', 'api', false);
eq('a person typing on an agent line does not lower it', state(w6), 'ai');
shellStatusOnBytes(w6, 's1', 'x');
eq('and the echo of that keystroke does not lower it either', state(w6), 'ai');
shellStatusOnInput(w6, 's1', 'ai', true);
eq('the agent submit ends the line', state(w6), 'running');

// --- both on one line: agent beats the person -------------------------
var w7 = makeWin();
shellStatusOnInput(w7, 's1', 'api', false);
eq('a person alone reads typing', state(w7), 'typing');
shellStatusOnInput(w7, 's1', 'ai', false);
eq('agent beats a person on the same line', state(w7), 'ai');

// --- an agent sends a whole line at once (the MCP paste) ---------------
// One frame carrying several characters is just a keystroke that types several
// characters: nothing changes but the size of the echo, and there is no window to
// expire, so the chip holds until the submit. This is why MCP needs no special
// handling.
var w8 = makeWin();
shellStatusOnInput(w8, 's1', 'ai', false);
eq('a pasted agent line reads as ai', state(w8), 'ai');
shellStatusOnBytes(w8, 's1', 'echo agent\n');
eq('its echo keeps the ai chip', state(w8), 'ai');
shellStatusOnInput(w8, 's1', 'ai', true);
shellStatusOnBytes(w8, 's1', 'agent\n');
eq('the command output after the submit paints running', state(w8), 'running');

// --- three seconds without a change is finished ------------------------
// Even a line that was never submitted: the chip reports that nothing is
// changing. The line is still pending, so a further keystroke returns it.
var w9 = makeWin();
shellStatusOnInput(w9, 's1', 'api', false);
eq('typing', state(w9), 'typing');
tick(3000);
eq('a line that stopped changing for three seconds reads as done', state(w9), 'done');
eq('but the line was never submitted, so it is still pending', w9._channels.s1.lineKind, SHELL_KIND_API);
shellStatusOnInput(w9, 's1', 'api', false);
eq('typing again revives the chip from done', state(w9), 'typing');
// Another keystroke's echo is a change, so the chip leaves "done" even if the
// input event itself were missed.
var w9b = makeWin();
shellStatusOnInput(w9b, 's1', 'ai', false);
tick(3000);
eq('pending agent line reads done after silence', state(w9b), 'done');
shellStatusOnBytes(w9b, 's1', 'more');
eq('a byte after that silence revives the pending kind, not running', state(w9b), 'ai');

// A submitted command that prints nothing must still reach done: the submit
// arms the idle timer itself, which is the only signal a silent command gives.
var w10 = makeWin();
var before = timers.length;
shellStatusOnInput(w10, 's1', 'api', true);
eq('a submit arms the idle timer', timers.length > before, true);
tick(3000);
eq('a submitted command that prints nothing ends as done', state(w10), 'done');

// --- a command that keeps printing stays running -----------------------
// Each byte pushes the idle timer out, so a build or a stream never reads as
// finished while it is still producing.
var w11 = makeWin();
var whileStreaming = [];
for (var s = 0; s < 5; s++) {
  tick(1000);
  shellStatusOnBytes(w11, 's1', 'line\n');
  whileStreaming.push(state(w11));
}
eq('a command that keeps printing stays running', every(whileStreaming, 'running'), true);

// --- ended is terminal and cannot be overwritten -----------------------
var w12 = makeWin();
shellStatusSet(w12, 's1', 'ended');
eq('ended paints', state(w12), 'ended');
shellStatusOnBytes(w12, 's1', 'more output');
eq('output cannot revive an ended channel', state(w12), 'ended');
shellStatusOnInput(w12, 's1', 'ai', false);
eq('input cannot revive an ended channel', state(w12), 'ended');
tick(3000);
eq('the idle timer cannot revive an ended channel', state(w12), 'ended');

// --- done is not overwritten by a late idle timer ----------------------
var w13 = makeWin();
shellStatusOnBytes(w13, 's1', 'x');
tick(3000);
shellStatusOnIdle(w13, 's1');
eq('a repeated idle stays done', state(w13), 'done');

// --- every state paints a distinct class -------------------------------
var classes = {};
['typing', 'ai', 'running', 'done', 'ended'].forEach(function (s) {
  var ww = makeWin();
  shellStatusSet(ww, 's1', s);
  classes[s] = ww._chip.className;
  if (!ww._chip.className) { console.log('FAIL ' + s + ' painted no class'); fails++; }
});
var painted = {};
Object.keys(classes).forEach(function (s) {
  if (painted[classes[s]]) { console.log('FAIL ' + s + ' shares a class with ' + painted[classes[s]]); fails++; }
  painted[classes[s]] = s;
});
console.log('PASS all five states paint distinct classes');

if (fails) { console.log(fails + ' status check(s) failed'); process.exit(1); }
console.log('STATUS MACHINE OK');
`
	scriptPath := filepath.Join(t.TempDir(), "status_check.js")
	if err := os.WriteFile(scriptPath, []byte(strings.Join(parts, "\n")+driver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath).CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("channel status checks failed: %v\n%s", err, got)
	}
	if strings.Contains(got, "FAIL") {
		t.Errorf("channel status regressed:\n%s", got)
	}
	if !strings.Contains(got, "STATUS MACHINE OK") {
		t.Fatalf("harness did not run to completion:\n%s", got)
	}
	t.Logf("\n%s", got)
}

// TestShellActivityIsWired pins the wiring the chip depends on, at the level a
// Go test can see: the browser is told the *source* of an input event, and the
// bytes it gets back are held for the echo window rather than painted as output.
//
// The event itself (who sent input, on both the keystroke and named-key paths)
// is tested behaviourally in internal/session/activity_test.go. What is checked
// here is the Web UI end of the chain, where a missing subscription or a renamed
// frame field would leave an MCP-driven channel showing nothing at all — a
// failure no session-package test can observe.
func TestShellActivityIsWired(t *testing.T) {
	ui := readAssetLF(t, "static/js/ui-socket.js")
	// The frame must be handled, or the server's push is discarded.
	if !strings.Contains(ui, "j.type === 'shell_activity'") {
		t.Error("ui-socket.js ignores shell_activity frames; an agent-driven channel would never update")
	}
	// The state labels must be literal t() calls: a computed key reads as an
	// orphan to the catalog test, which would prune the translation and leave
	// the raw key on screen.
	for _, want := range []string{
		"t('channel.state.typing')", "t('channel.state.ai')",
		"t('channel.state.running')", "t('channel.state.done')",
		"t('channel.endedTab')", "t('session.ended')",
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("ui-socket.js does not call %s as a literal", want)
		}
	}
	// The tab must carry the chip node, and the language switch must repaint it.
	tv := readAssetLF(t, "static/js/terminal-view.js")
	if !strings.Contains(tv, "shell-channel-tab-state") {
		t.Error("terminal-view.js no longer renders the channel status chip")
	}
	if !strings.Contains(tv, "shellStatusPaint") {
		t.Error("terminal-view.js does not repaint the chip on a language switch")
	}
	// The rule must be the submit, not a timing window: a window around the echo
	// fails in both directions (a laggy link puts a real echo outside it and the
	// chip strobes; a fast command puts real output inside it and the chip reads
	// "typing" for a command that already finished). Two regressions are worth
	// naming because both were observed: closing a line on a newline in the output
	// (a redraw contains one, which ended an agent's line 13ms after it opened),
	// and painting every arriving byte as output.
	if !strings.Contains(ui, "j.submit") {
		t.Error("ui-socket.js ignores the submit flag; the chip would have to guess line ends from output bytes")
	}
	if !strings.Contains(ui, "lineKind") {
		t.Error("ui-socket.js no longer tracks the pending line's kind; the echo of typing would read as output")
	}
	if strings.Contains(ui, "echoUntil") || strings.Contains(ui, "SHELL_STATUS_ECHO_MS") {
		t.Error("ui-socket.js is back to guessing echo by arrival time; the submit flag already says whether a line ended")
	}
	if strings.Contains(ui, "indexOf('\\n')") {
		t.Error("ui-socket.js still treats a newline in the output as a line end; a redraw would close the line")
	}
}
