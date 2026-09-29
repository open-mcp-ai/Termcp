package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRailRenderDrawsOneCellPerRow runs the module's own render path against a
// stub DOM.
//
// railCells maps the server's row layout onto statuses, which is where an
// off-by-one between rows and bytes turns into cells on the wrong lines. Only
// executing render catches that class of mistake — a rename inside render once
// left a dangling reference, so any shell with an input mark threw and the whole
// strip disappeared, which a user sees as "I typed something and the timeline
// vanished". This test drives render end to end and reads the cells that reach
// the DOM: one per terminal row, each placed by its row index, blank rows left
// empty.
func TestRailRenderDrawsOneCellPerRow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the render path")
	}
	body, err := readAsset("static/js/timeline.js")
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "timeline.js")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	script := `
const fs = require('fs');
const vm = require('vm');

// --- a DOM stub just complete enough for renderShellTimeline -----------
function makeEl(tag) {
  const el = {
    tagName: tag, className: '', style: {}, attrs: {}, childNodes: [],
    type: '', _parent: null, _rect: null,
    _has(c) { return String(this.className || '').split(/\s+/).indexOf(c) >= 0; },
    get classList() {
      const self = this;
      return {
        add() { const s = String(self.className || '').split(/\s+/).filter(Boolean);
                for (const c of arguments) if (s.indexOf(c) < 0) s.push(c);
                self.className = s.join(' '); },
        contains(c) { return self._has(c); }
      };
    },
    remove() { const p = this._parent; if (p) p.removeChild(this); },
    get hidden() { return !!this._hidden; },
    set hidden(v) { this._hidden = !!v; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    removeAttribute(k) { delete this.attrs[k]; },
    appendChild(c) { c._parent = this; this.childNodes.push(c); return c; },
    removeChild(c) { const i = this.childNodes.indexOf(c); if (i >= 0) { this.childNodes.splice(i, 1); c._parent = null; } },
    get parentNode() { return this._parent; },
    contains(c) { return c === this || this.childNodes.some(x => x.contains && x.contains(c)); },
    addEventListener() {}, removeEventListener() {},
    closest(sel) {
      const want = String(sel).replace(/^\./, '');
      let n = this;
      while (n) { if (String(n.className || '').split(/\s+/).indexOf(want) >= 0) return n; n = n._parent; }
      return null;
    },
    querySelectorAll(sel) {
      const want = String(sel).replace(/^\./, '');
      const out = [];
      const walk = (n) => {
        n.childNodes.forEach(ch => {
          if (String(ch.className || '').split(/\s+/).indexOf(want) >= 0) out.push(ch);
          walk(ch);
        });
      };
      walk(this);
      return out;
    },
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
    getBoundingClientRect() { return this._rect; },
    get clientHeight() { return this._h || 0; },
    set clientHeight(v) { this._h = v; },
    get offsetHeight() { return this._oh || 0; },
    set offsetHeight(v) { this._oh = v; },
    get offsetTop() { return parseFloat(String(this.style.top)) || 0; }
  };
  for (const k of ['innerHTML', 'textContent']) {
    let v = '';
    Object.defineProperty(el, k, { get() { return v; }, set(x) { v = String(x); } });
  }
  return el;
}

global.document = {
  createElement: makeEl, addEventListener() {}, removeEventListener() {},
  querySelector() { return null; }, querySelectorAll() { return []; }
};
global.window = global;

// The helpers the module expects from its sibling modules.
global.t = (k, p) => p ? k + ' ' + JSON.stringify(p) : k;
global.fmtTime = (ms) => new Date(ms).toISOString();
global.escapeHtml = (s) => String(s).replace(/[&<>"']/g, c =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// The module declares plain top-level functions and vars and has no load-time
// side effects, so evaluating it here is how the browser loads it.
vm.runInThisContext(fs.readFileSync(process.argv[2], 'utf8'));

const RAIL_H = 400;
const ROWS = 24;

function freshRail() {
  const r = makeEl('div');
  r.clientHeight = RAIL_H;
  const host = makeEl('div');
  host.classList.add('shell-channel-body');
  host.appendChild(r);
  return r;
}

function freshSt(rail) {
  return {
    el: rail, pop: makeEl('div'), card: makeEl('div'), sid: 's1',
    cell: null, pinned: false, timer: 0, lastFetchAt: 0, observer: null,
    docHandlers: [], lastBody: null,
    layoutSeq: 0, fetchSeq: 0, view: null,
    scrollBound: null, unsubs: [], resizeRo: null
  };
}

const cells = (rail) => rail.querySelectorAll('.term-rail-cell');
const rowsOf = (rail) => cells(rail).map(c => Number(c.getAttribute('data-row'))).sort((a, b) => a - b);
const pct = (s) => parseFloat(String(s));

let bad = 0;
function fail(msg) { console.error('FAIL ' + msg); bad++; }
function check(cond, msg) { if (!cond) fail(msg); }

// A terminal the module can read a window off, and a view built from a layout so
// these checks do not depend on railViewFor (which is exercised below).
// spansOf builds the row layout the server would answer with for a log of n
// uniform lines sharing the same byte count, which is enough for a render check:
// the module reads nothing but the per-row byte ranges.
function termAt(viewportY, cursorY, cursorX, baseY) {
  return {
    rows: ROWS,
    buffer: { active: { type: 'normal', viewportY: viewportY, baseY: baseY || 0, cursorY: cursorY, cursorX: cursorX } }
  };
}
function viewAt(spans, top) {
  return {
    top: top == null ? 0 : top, rows: ROWS, cols: 80, spans: spans, box: null,
    layoutSeq: 1, window: null
  };
}
/* Even row shares, the way a log of uniform lines lays out. */
function spansOf(n, total) {
  const out = [];
  for (let i = 0; i < n; i++) {
    out.push({ start: (total * i) / n, end: (total * (i + 1)) / n });
  }
  return out;
}

/* Five rows of a hundred bytes each, which is what a log of five full lines lays
   out to. A mark tiling the log then decides each row's status: a submitted line
   on row 1, the shell's output on rows 0/2/4, an agent's input on row 3. */
const anchors = spansOf(5, 500);
const genBody = {
  total_bytes: 500,
  marks: [
    { status: 'o', time: 1000, start: 0, end: 100 },
    { status: 'i', time: 2000, start: 100, end: 100 },
    { status: 'o', time: 3000, start: 100, end: 300 },
    { status: 'a', time: 4000, start: 300, end: 300 },
    { status: 'o', time: 5000, start: 300, end: 500 }
  ]
};

const rail = freshRail();
const st = freshSt(rail);

try {
  renderShellTimeline(st, genBody, viewAt(anchors));
} catch (e) {
  fail('renderShellTimeline threw: ' + e.message);
  process.exit(1);
}

// --- one cell per row ---------------------------------------------------
if (rail.hidden) fail('the rail is hidden although the payload has marks and laid-out rows');
const got = cells(rail);
check(got.length === 5, 'drew ' + got.length + ' cells for the five rows with bytes, want 5');
const rowIdx = rowsOf(rail);
check(JSON.stringify(rowIdx) === JSON.stringify([0, 1, 2, 3, 4]),
  'the cells sit on rows ' + JSON.stringify(rowIdx) + ', want 0..4');

// Every cell covers exactly one row: one step of the window, at that row's top.
const step = 100 / ROWS;
got.forEach((c) => {
  const r = Number(c.getAttribute('data-row'));
  const top = pct(c.style.top);
  const h = pct(c.style.height);
  if (Math.abs(top - r * step) > 1e-3) fail('row ' + r + ' is drawn at ' + c.style.top + ', want ' + (r * step) + '%');
  if (Math.abs(h - step) > 1e-3) fail('row ' + r + ' is ' + c.style.height + ' tall, want ' + step + '%');
  if (c.tagName !== 'button') fail('row ' + r + ' is a ' + c.tagName + '; cells must be focusable');
});

// --- the row's status comes from the content on it ----------------------
const statusOf = (r) => {
  const c = cells(rail).filter(x => Number(x.getAttribute('data-row')) === r)[0];
  return c ? c.getAttribute('data-status') : null;
};
const clsOf = (r) => {
  const c = cells(rail).filter(x => Number(x.getAttribute('data-row')) === r)[0];
  return c ? String(c.className) : '';
};
check(statusOf(0) === 'o', 'row 0 is ' + statusOf(0) + ', want the shell\'s output');
check(statusOf(1) === 'i', 'row 1 is ' + statusOf(1) + ', want the submitted line');
check(clsOf(1).indexOf('term-rail-input') >= 0, 'the submitted line lacks its class: ' + clsOf(1));
check(statusOf(2) === 'o', 'row 2 is ' + statusOf(2) + ', want output after the input');
check(statusOf(3) === 'a', 'row 3 is ' + statusOf(3) + ', want the agent\'s input to outrank the output sharing it');
check(clsOf(3).indexOf('term-rail-agent') >= 0, 'the agent line lacks its class: ' + clsOf(3));
check(statusOf(4) === 'o', 'row 4 is ' + statusOf(4) + ', want output');
check(got.every(c => c.getAttribute('data-start') !== null && c.getAttribute('data-end') !== null),
  'a cell does not report the bytes it covers');
/* The card prints these two as positions in the log, so they must be whole: a
   row's bounds are interpolated between two chunks and come out fractional. */
check(got.every(c => Number.isInteger(Number(c.getAttribute('data-start'))) &&
  Number.isInteger(Number(c.getAttribute('data-end')))),
  'a cell reports a fractional byte bound');
check(got.every(c => c.getAttribute('aria-label')), 'a cell has no accessible label');

// --- an output row is framed in the colour of the input that produced it --
/* "Test reached the shell → this is its output" is unreadable from the rail when
   everything is a flat green band, so an output row carries the colour of the
   input that produced it as a frame. The row's own status must not change: the
   cause is an annotation, not a recolour.

   Five rows, one byte range each. A typed line and an agent line sit on their own
   rows, and the output that follows each one runs over two rows: the row the input
   mark is on keeps the input's colour (it outranks the echo), and the row after
   it is output framed in that input's colour. */
const causedBody = {
  total_bytes: 500,
  marks: [
    { status: 'o', time: 1, start: 0, end: 100 },
    { status: 'i', time: 2, start: 100, end: 100 },
    { status: 'o', time: 3, start: 100, end: 300 },
    { status: 'a', time: 4, start: 300, end: 300 },
    { status: 'o', time: 5, start: 300, end: 500 }
  ]
};
const railCause = freshRail();
const stCause = freshSt(railCause);
function cellAt(rail, r) {
  return rail.querySelectorAll('.term-rail-cell').filter(x => Number(x.getAttribute('data-row')) === r)[0];
}
try {
  renderShellTimeline(stCause, causedBody, viewAt(spansOf(5, 500)));
  const causeCls = (r) => String((cellAt(railCause, r) || {}).className || '');
  const causeOf = (r) => (cellAt(railCause, r) || { getAttribute: () => null }).getAttribute('data-cause');
  const statusAt = (r) => (cellAt(railCause, r) || { getAttribute: () => null }).getAttribute('data-status');

  check(statusAt(0) === 'o' && causeCls(0).indexOf('term-rail-cause') < 0,
    'output from before any input is framed as caused by one: ' + statusAt(0) + ' ' + causeCls(0));
  check(causeCls(1).indexOf('term-rail-cause') < 0,
    'the input row itself is framed as caused by an input: ' + causeCls(1));
  check(statusAt(2) === 'o' && causeCls(2).indexOf('term-rail-cause-input') >= 0,
    'the output the human asked for is not framed in the human colour: ' + statusAt(2) + ' ' + causeCls(2));
  check(causeOf(2) === 'i', 'the human-caused row reports cause ' + causeOf(2));
  check(causeCls(3).indexOf('term-rail-cause') < 0,
    'the agent input row is framed as caused by an input: ' + causeCls(3));
  check(statusAt(4) === 'o' && causeCls(4).indexOf('term-rail-cause-agent') >= 0,
    'the output an agent asked for is not framed in the agent colour: ' + statusAt(4) + ' ' + causeCls(4));
  check(causeOf(4) === 'a', 'the agent-caused row reports cause ' + causeOf(4));
  check(causeOf(2) !== causeOf(4), 'both causes report the same input');

  /* The frame has to close around the run. Vertical edges alone are two lines, not
     a frame, so the first row of a run takes the top edge and the last takes the
     bottom one — and a single-row run takes both. A cap between two rows of the
     same run would cut a command's output back into the beads the flush tiling
     exists to prevent. Rows 2 and 4 are one-row runs here, so each carries both. */
  const has = (cls, k) => cls.split(/\s+/).indexOf(k) >= 0;
  check(has(causeCls(2), 'term-rail-cause-top') && has(causeCls(2), 'term-rail-cause-bottom'),
    'a one-row run does not close its frame: ' + causeCls(2));
  check(has(causeCls(4), 'term-rail-cause-top') && has(causeCls(4), 'term-rail-cause-bottom'),
    'a one-row agent run does not close its frame: ' + causeCls(4));

  /* A run of three rows (1..3: row 0 is the input mark itself, which carries no
     frame): the first takes the top only, the last the bottom only, and the seam
     between them takes neither edge. */
  const railRun = freshRail();
  renderShellTimeline(freshSt(railRun), {
    total_bytes: 400,
    marks: [
      { status: 'i', time: 1, start: 0, end: 0 },
      { status: 'o', time: 2, start: 0, end: 400 }
    ]
  }, viewAt(spansOf(4, 400)));
  const runCls = (r) => String((cellAt(railRun, r) || {}).className || '');
  check(!has(runCls(0), 'term-rail-cause'),
    'the input row itself is framed: ' + runCls(0));
  check(has(runCls(1), 'term-rail-cause-top') && !has(runCls(1), 'term-rail-cause-bottom'),
    'the first row of a run does not open the frame: ' + runCls(1));
  check(!has(runCls(2), 'term-rail-cause-top') && !has(runCls(2), 'term-rail-cause-bottom'),
    'a cap between two rows of one run would cut the band into beads: ' + runCls(2));
  check(has(runCls(3), 'term-rail-cause-bottom') && !has(runCls(3), 'term-rail-cause-top'),
    'the last row of a run does not close the frame: ' + runCls(3));
} catch (e) {
  fail('the cause case threw: ' + e.message);
}

/* --- a mark on a row boundary still colours the row --------------------- */
/* The layout tiles the log, so an input mark sits on a byte the row it landed on
   owns. It can be that row's *first* byte: the mark is recorded at the offset the
   keystroke ended at, and for a submitted line that offset is the carriage return
   or newline that closed the row of text above — the start of the next row. A
   client that required a mark to be strictly inside the span would then drop the
   mark and the input bar would be missing from the strip, which is exactly the
   bug this pins.

   Two rows: row 0 holds bytes [0,100), row 1 holds [100,200) and the input mark
   sits at offset 100, the first byte of row 1. */
try {
  const railEdge = freshRail();
  renderShellTimeline(freshSt(railEdge), {
    total_bytes: 200,
    marks: [
      { status: 'o', time: 1, start: 0, end: 100 },
      { status: 'i', time: 2, start: 100, end: 100 },
      { status: 'o', time: 3, start: 100, end: 200 }
    ]
  }, viewAt([{ start: 0, end: 100 }, { start: 100, end: 200 }], 0));
  const edgeAt = (r) => (cellAt(railEdge, r) || { getAttribute: () => null }).getAttribute('data-status');
  check(edgeAt(0) === 'o', 'row 0 is ' + edgeAt(0) + ', want output');
  check(edgeAt(1) === 'i',
    'row 1 is ' + edgeAt(1) + ', want the input mark that sits on its first byte');
} catch (e) {
  fail('the boundary-mark case threw: ' + e.message);
}

// --- rows the layout gives no bytes draw nothing ------------------------
/* Scrolled one row down, the cells must be the rows the viewport shows: the
   transcript above it must be absent, not stacked into the first row of the
   window. */
const railLow = freshRail();
renderShellTimeline(freshSt(railLow), genBody, { top: 3, rows: 2, cols: 80, spans: anchors.slice(3, 5), box: null, layoutSeq: 1 });
const lowRows = rowsOf(railLow);
check(JSON.stringify(lowRows) === JSON.stringify([3, 4]),
  'scrolled to row 3 the cells sit on ' + JSON.stringify(lowRows) + ', want 3 and 4');

/* A window entirely past the layout has no bytes at all, so the rail hides
   rather than drawing an empty strip beside the text. */
const railFar = freshRail();
renderShellTimeline(freshSt(railFar), genBody, { top: ROWS * 4, rows: 2, cols: 80, spans: [null, null], box: null, layoutSeq: 1 });
check(railFar.hidden, 'a window far below the layout must hide the rail, not draw it empty');

// --- visibility --------------------------------------------------------
try {
  const r2 = freshRail();
  const s2 = freshSt(r2);
  renderShellTimeline(s2, genBody, viewAt([]));
  check(r2.hidden, 'no laid-out rows must hide the rail: an approximate strip would look like information');
  renderShellTimeline(s2, { total_bytes: 0, marks: [] }, viewAt(anchors));
  check(r2.hidden, 'a shell with no marks must hide the rail');
  renderShellTimeline(s2, null, viewAt(anchors));
  check(r2.hidden, 'no payload must hide the rail');
  renderShellTimeline(s2, genBody, null);
  check(r2.hidden, 'no window must hide the rail');
  renderShellTimeline(s2, genBody, { top: 0, rows: 0, cols: 80, spans: anchors, box: null });
  check(r2.hidden, 'a zero-row window must hide the rail');
} catch (e) {
  fail('a degenerate payload threw: ' + e.message);
}

// --- a full-screen program's screen draws nothing ----------------------
/* vim, less and the pagers paint the alternate buffer: rows of their own that
   the log does not contain and the terminal throws away on exit. The rail's rows
   are the transcript's, so a strip drawn over the program would place cells
   describing lines the reader cannot see beside lines that are not in it — and
   because the alternate buffer is always at viewportY 0, a stale layout for the
   rows behind the program looks perfectly current. The rail therefore draws
   nothing while it is up, and draws again when the terminal puts its own rows
   back. */
try {
  const railAlt = freshRail();
  const stAlt = freshSt(railAlt);
  stAlt.sid = 's1';
  // The rail's response is one body: the laid-out rows and the marks over them.
  stAlt.lastBody = { cols: 80, top: 0, spans: anchors, marks: genBody.marks };
  stAlt.layoutSeq = 1;
  const termAlt = termAt(0, 0, 0, 0);
  const winAlt = { _rail: stAlt, _channels: { s1: { term: termAlt } }, _activeChannelSid: 's1', _term: termAlt };

  railRepaintWindow(winAlt, 's1', true);
  check(!railAlt.hidden && cells(railAlt).length > 0, 'the rail does not draw for a normal screen');

  // vim starts: the buffer changes, and neither a resize nor a scroll fires with it.
  termAlt.buffer.active = { type: 'alternate', viewportY: 0, baseY: 0, cursorY: 0, cursorX: 0 };
  railRepaintWindow(winAlt, 's1', false);
  check(railAlt.hidden, 'the rail drew cells over a full-screen program: its rows are not the transcript');

  // And the layout in hand survives: vim's exit puts the same rows back, so the
  // repaint is from the layout already held rather than a refetch.
  check(!!stAlt.lastBody, 'leaving the alternate screen must not throw away the transcript layout');
  termAlt.buffer.active = { type: 'normal', viewportY: 0, baseY: 0, cursorY: 3, cursorX: 0 };
  railRepaintWindow(winAlt, 's1', false);
  check(!railAlt.hidden && cells(railAlt).length === 5,
    'the rail did not come back when the full-screen program exited: ' +
    (railAlt.hidden ? 'still hidden' : cells(railAlt).length + ' cells'));

  // The change is subscribed to, since a buffer switch moves neither the viewport
  // nor the terminal's size.
  const stSub = freshSt(freshRail());
  stSub.sid = 's1';
  const fired = [];
  const termSub = termAt(0, 0, 0, 0);
  termSub.buffer.onBufferChange = (fn) => { fired.push(fn); return { dispose() {} }; };
  termSub.element = { querySelector: () => null };
  const winSub = { _rail: stSub, _channels: { s1: { term: termSub } }, _activeChannelSid: 's1', _term: termSub };
  railBindScroll(winSub, 's1');
  check(fired.length === 1, 'the buffer switch is not subscribed to; the rail would stay hidden after vim exits');
  if (fired.length === 1) {
    let scheduled = 0;
    const realScheduleAlt = shellRailSchedule;
    shellRailSchedule = function () { scheduled++; };
    fired[0]();
    shellRailSchedule = realScheduleAlt;
    check(scheduled === 1, 'the buffer change did not ask for a repaint (asked ' + scheduled + ')');
  }

  /* The path a refresh takes while the program is up, which is where a hidden
     strip can become a permanently hidden one: the fetch is refused, and if it
     cleared the marks in hand the repaint after the exit would have nothing to
     draw from until new output arrived to fetch again. */
  const railNoFetch = freshRail();
  const stNoFetch = freshSt(railNoFetch);
  stNoFetch.sid = 's1';
  stNoFetch.lastBody = { cols: 80, top: 0, spans: anchors, marks: genBody.marks };
  stNoFetch.layoutSeq = 1;
  const termNoFetch = termAt(0, 0, 0, 0);
  termNoFetch.buffer.active.type = 'alternate';
  let fetches = 0;
  const realFetch = global.fetch;
  global.fetch = function () { fetches++; return Promise.resolve({ ok: true, json: () => Promise.resolve({}) }); };
  const winNoFetch = { _rail: stNoFetch, _channels: { s1: { term: termNoFetch } }, _activeChannelSid: 's1', _term: termNoFetch };
  try {
    railFetch(winNoFetch, 's1');
  } finally {
    global.fetch = realFetch;
  }
  check(fetches === 0, 'a refresh asked the server for rows while a full-screen program was up (asked ' + fetches + ')');
  check(railNoFetch.hidden, 'the strip is not hidden while the program is up');
  check(!!stNoFetch.lastBody,
    'the refresh threw away the marks in hand; the strip would stay hidden after the program exits');
  termNoFetch.buffer.active.type = 'normal';
  railRepaintWindow(winNoFetch, 's1', false);
  check(!railNoFetch.hidden && cells(railNoFetch).length === 5,
    'the strip did not come back after the exit without a refetch');
} catch (e) {
  fail('the alternate-screen case threw: ' + e.message);
}

// --- a repaint from the same layout draws the same rail -----------------
/* The reported bug this replaces was a rail that collapsed when the user typed
   without pressing Enter: the row<->byte mapping was recorded from the stream, and
   a second message on the same row re-extended the last recorded range, after
   which the whole banner mapped to one row. The mapping is now derived from the
   log and the width, so no keystroke can move it — and the invariant that matters
   is exactly that: the same layout draws the same cells, no matter how many times
   it is drawn or what the terminal's cursor is doing.
 */
const banner = { total_bytes: 306, marks: [{ status: 'o', time: 1, start: 0, end: 306 }] };
const bannerSpans = spansOf(3, 306);
const railType = freshRail();
const stType = freshSt(railType);
try {
  stType.sid = 's1';
  const termType = termAt(0, 3, 0, 0);
  const winType = { _rail: stType, _channels: { s1: { term: termType } }, _activeChannelSid: 's1', _term: termType };
  stType.lastBody = { cols: 80, top: 0, spans: bannerSpans, marks: banner.marks };
  stType.layoutSeq = 1;
  railRepaintWindow(winType, 's1', true);
  const before = rowsOf(railType);
  const visible = !railType.hidden;
  const firstTop = cells(railType).length ? pct(cells(railType)[0].style.top) : NaN;

  // The keystroke: the terminal's cursor moves, nothing else changes.
  termType.buffer.active.cursorX = 3;
  termType.buffer.active.cursorY = 3;
  railRepaintWindow(winType, 's1', true);
  const after = rowsOf(railType);

  check(visible, 'the rail was hidden before the keystroke');
  if (railType.hidden) fail('typing without an enter hid the rail');
  check(JSON.stringify(before) === JSON.stringify([0, 1, 2]),
    'the fresh banner covers rows ' + JSON.stringify(before) + ', want 0..2');
  check(JSON.stringify(after) === JSON.stringify(before),
    'typing without an enter redrew the rail as ' + JSON.stringify(after) + ', want ' + JSON.stringify(before));
  check(Math.abs(pct(cells(railType)[0].style.top) - firstTop) < 1e-3,
    'typing without an enter moved the first cell from ' + firstTop + '% to ' + pct(cells(railType)[0].style.top) + '%');
} catch (e) {
  fail('the typing case threw: ' + e.message);
}

// --- a review decision badges its row rather than colouring it ----------
/* Approving or rejecting a command decides about the line it was typed on, so it
   shares that row: the row keeps the status of its content and the card carries a
   badge saying it was reviewed. Giving it a colour of its own would take the row
   away from the thing it is about. */
const railRev = freshRail();
const stRev = freshSt(railRev);
try {
  renderShellTimeline(stRev, {
    total_bytes: 300,
    marks: [
      { status: 'o', time: 1, start: 0, end: 100 },
      { status: 'i', time: 2, start: 100, end: 100 },
      { status: 'q', time: 3, start: 100, end: 100 },
      { status: 'o', time: 4, start: 100, end: 300 }
    ]
  }, viewAt(spansOf(3, 300)));
  const revRow = cells(railRev).filter(c => Number(c.getAttribute('data-row')) === 1)[0];
  if (!revRow) fail('the reviewed row drew no cell at all');
  else {
    check(revRow.getAttribute('data-status') === 'i',
      'the reviewed row is ' + revRow.getAttribute('data-status') + ', want the content status i');
    check(revRow.getAttribute('data-review') === '1', 'the reviewed row has no review flag');
    const plain = cells(railRev).filter(c => Number(c.getAttribute('data-row')) === 2)[0];
    check(plain && !plain.getAttribute('data-review'), 'a row with no review decision is flagged as reviewed');
    printShellRailCard(stRev, revRow);
    check(!stRev.card.hidden, 'the detail card stayed hidden');
    check(String(stRev.card.innerHTML).indexOf('term-rail-card-badge') >= 0,
      'the card of a reviewed row carries no review badge');
    printShellRailCard(stRev, plain);
    check(String(stRev.card.innerHTML).indexOf('term-rail-card-badge') < 0,
      'the card of an unreviewed row carries a review badge');
  }
} catch (e) {
  fail('the review case threw: ' + e.message);
}

// --- the interaction paths ---------------------------------------------
/* Hover and click are only exercised by a human, so a dangling reference in
   either one is invisible to every test that only renders. Both are driven here
   on a real cell, because that is how the bug that hid the rail was found: the
   code ran, but only after a user did something. */
try {
  shellRailShowPop(st, got[got.length - 1]);
  if (st.pop.hidden) fail('the hover label stayed hidden after shellRailShowPop');
  if (!st.pop.textContent) fail('the hover label has no text');
  if (!String(st.pop.style.top).length) fail('the hover label was not positioned');
  shellRailHidePop(st);
  if (!st.pop.hidden) fail('the hover label was not hidden after shellRailHidePop');

  printShellRailCard(st, got[0]);
  if (st.card.hidden) fail('the detail card stayed hidden after printShellRailCard');
  if (!st.card.innerHTML) fail('the detail card has no content');
  const card = String(st.card.innerHTML);
  ['term-rail-swatch', 'term-rail-card-row'].forEach(want => {
    if (card.indexOf(want) < 0) fail('the detail card misses ' + want);
  });
  printShellRailCard(st, null);
  if (!st.card.hidden) fail('the detail card was not hidden when closed');
  // A card opened on the newest cell is the common case and the one that pushes
  // against the end of the rail.
  printShellRailCard(st, got[got.length - 1]);

  // A cell is found from its child, which is how the mouseover and click handlers
  // reach it: they only ever see the event target.
  const inner = makeEl('span');
  got[0].appendChild(inner);
  check(shellRailCellFrom(inner) === got[0], 'a click inside a cell does not find its cell');
  check(shellRailCellFrom(makeEl('div')) === null, 'a click outside the cells found one anyway');

  const age = railRelativeAge(Date.now() - 5000);
  if (typeof age !== 'string') fail('railRelativeAge did not return a string');
} catch (e) {
  fail('an interaction path threw: ' + e.message);
}

// --- the strip is measured to the terminal's own screen box -------------
/* The cells are percentages of the strip, so the strip has to be the same box as
   the text's rows. They are not the same element: the strip hangs off the channel
   body, the rows belong to xterm's screen inside it. The module measures the two
   rects and positions the strip on the difference — a few pixels of inset — which
   is what keeps a cell on its line. */
try {
  const railBox = freshRail();
  const host = railBox._parent;
  host._rect = { top: 30, height: 400 };
  const screen = makeEl('div');
  screen.classList.add('xterm-screen');
  screen._rect = { top: 40, height: 300 };
  const screenHost = makeEl('div');
  screenHost.appendChild(screen);
  const term = {
    rows: ROWS,
    buffer: { active: { viewportY: 4, baseY: 0, cursorY: 2, cursorX: 0 } },
    element: screenHost
  };
  const stBox = freshSt(railBox);
  const win = { _rail: stBox, _channels: { s1: { term: term } }, _activeChannelSid: 's1' };
  stBox.lastBody = { cols: 80, top: 0, spans: anchors };
  stBox.layoutSeq = 1;
  const view = railViewFor(win, 's1');
  if (!view) fail('railViewFor returned no window for a live channel');
  else {
    check(view.top === 4, 'the window starts at row ' + view.top + ', want the viewport row 4');
    check(view.rows === ROWS, 'the window is ' + view.rows + ' rows, want the screen\'s ' + ROWS);
    if (!view.box) fail('the screen box was not measured');
    else {
      check(Math.abs(view.box.top - 10) < 1e-6, 'the strip was placed at ' + view.box.top + 'px, want 10 (the screen\'s inset)');
      check(Math.abs(view.box.height - 300) < 1e-6, 'the strip is ' + view.box.height + 'px tall, want the screen\'s 300');
    }
    check(view.spans && view.spans.length === ROWS,
      'the view did not take the laid-out rows: ' + (view.spans ? view.spans.length : 'none'));
  }
  renderShellTimeline(stBox, genBody, view);
  check(railBox.style.top === '10.00px', 'the strip top is ' + railBox.style.top + ', want 10.00px');
  check(railBox.style.height === '300.00px', 'the strip height is ' + railBox.style.height + ', want 300.00px');
  check(railBox.style.bottom === 'auto', 'the strip kept its CSS bottom, which would fight the measured top');

  // Nothing measured (a hidden window, a browser without rects): the stylesheet's
  // box stands and the percentages still tile it.
  renderShellTimeline(stBox, genBody, viewAt(anchors));
  check(!railBox.style.top && !railBox.style.height, 'a view with no box left the measured position in place');
} catch (e) {
  fail('the measurement case threw: ' + e.message);
}

// --- a repaint reuses the layout, and a new one is noticed --------------
/* A scroll repaints from the layout already in hand: the rows on screen have
   moved, but the layout that gives them their bytes has not, and fetching per
   wheel tick would put a request behind every event. A fetch that lands replaces
   the layout, and the repaint has to notice that even when the viewport did not
   move — the comparison therefore carries a layout sequence number rather than
   the layout itself, which would compare equal to itself.

   The force flag covers the one change invisible in the window: a resize moves
   the pixels the cells are drawn in while top and rows can stay as they were. */
try {
  const railRe = freshRail();
  const stRe = freshSt(railRe);
  stRe.sid = 's1';
  // A response with marks but no laid-out rows: there is nothing to map the marks
  // onto, so nothing is drawn.
  stRe.lastBody = genBody;
  const termRe = termAt(0, 2, 3, 0);
  const winRe = { _rail: stRe, _channels: { s1: { term: termRe } }, _activeChannelSid: 's1', _term: termRe };
  railRepaintWindow(winRe, 's1', true);
  check(cells(railRe).length === 0, 'a repaint with no layout drew cells anyway');

  // The rail's response arrives — the rows and the marks in one body — and the
  // rail draws the rows it describes from the marks in hand.
  stRe.lastBody = { cols: 80, top: 0, spans: anchors, marks: genBody.marks };
  const bodyInHand = stRe.lastBody;
  stRe.layoutSeq = 1;
  railRepaintWindow(winRe, 's1', true);
  check(cells(railRe).length === 5, 'the forced repaint drew ' + cells(railRe).length + ' cells, want 5');
  check(stRe.lastBody === bodyInHand, 'the repaint refetched the marks instead of reusing them');

  /* The same window again, unforced: the strip is already right, so the repaint
     is skipped — that is what keeps a burst of scroll events from rebuilding the
     rail on every frame. */
  const drawn = cells(railRe).length;
  railRepaintWindow(winRe, 's1', false);
  check(cells(railRe).length === drawn, 'a repeated repaint changed the strip although nothing moved');

  // A different channel must not be repainted by the one that is visible.
  railRepaintWindow(winRe, 's9', true);
  check(cells(railRe).length === drawn, 'a repaint for another channel touched this one\'s rail');

  /* New output: the layout the server lays out again has one row more, and the
     viewport has not moved. The unforced repaint has to see it, which is what the
     sequence number is for. */
  stRe.lastBody = {
    cols: 80,
    top: 0,
    spans: anchors.concat([{ start: 500, end: 700 }]),
    total_bytes: 700,
    marks: genBody.marks.slice(0, 4).concat([{ status: 'o', time: 6000, start: 300, end: 700 }])
  };
  stRe.layoutSeq = 2;
  railRepaintWindow(winRe, 's1', false);
  check(cells(railRe).length === 6,
    'new output laid out a sixth row but the rail kept ' + cells(railRe).length + ' cells; the view missed the new layout');

  /* A reflow: the same log at a narrower width is more rows, so the layout in hand
     is replaced wholesale. A view that compared its own spans would see the array
     it still holds and skip; the sequence number is what catches it. */
  stRe.lastBody = { cols: 40, top: 0, spans: spansOf(9, 700), marks: stRe.lastBody.marks };
  stRe.layoutSeq = 3;
  railRepaintWindow(winRe, 's1', false);
  check(cells(railRe).length === 9,
    'a reflow to 9 rows left the rail at ' + cells(railRe).length + ' cells');

  /* A viewport that has scrolled past the layout must ask for a new one rather
     than drawing nothing: the margin only covers so far. */
  termRe.buffer.active.viewportY = 500;
  const stFar = freshSt(freshRail());
  stFar.sid = 's1';
  stFar.lastBody = { cols: 80, top: 0, spans: anchors };
  const termFar = termAt(500, 0, 0, 0);
  const winFar = { _rail: stFar, _channels: { s1: { term: termFar } }, _activeChannelSid: 's1' };
  let asked = 0;
  const realSchedule = shellRailSchedule;
  shellRailSchedule = function () { asked++; };
  railEnsureWindow(winFar, 's1');
  shellRailSchedule = realSchedule;
  check(asked === 1, 'a viewport far past the layout did not ask for a new one (asked ' + asked + ')');

  // And a viewport inside it must not: the layout covers the screen with margin to
  // spare, which is what makes a scroll a repaint rather than a fetch.
  stFar.lastBody = { cols: 80, top: 490, spans: spansOf(40, 700) };
  asked = 0;
  shellRailSchedule = function () { asked++; };
  railEnsureWindow(winFar, 's1');
  shellRailSchedule = realSchedule;
  check(asked === 0, 'a viewport inside the layout asked anyway');
} catch (e) {
  fail('the repaint case threw: ' + e.message);
}

if (bad) { console.error(bad + ' render check(s) failed'); process.exit(1); }
console.log('RENDER OK: ' + got.length + ' rows drawn, review + interactions + measurement + repaint OK');
`
	scriptPath := filepath.Join(t.TempDir(), "render_check.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, tmp).CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("render path check failed: %v\n%s", err, got)
	}
	if !strings.Contains(got, "RENDER OK") {
		t.Fatalf("harness did not run to completion:\n%s", got)
	}
	t.Logf("\n%s", got)
}
