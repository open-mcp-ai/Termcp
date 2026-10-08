/* The terminal timeline rail.
 *
 * A shell's transcript is a byte log plus a status index: log.jsonl records one
 * line per status change, with the byte offset its span starts at and the Unix
 * millisecond it started. GET /api/shells/{id}/marks serves that index, and this
 * module draws it as a thin strip over the right edge of the terminal: one cell
 * per terminal row, coloured by what that row holds. It answers the question the
 * terminal itself cannot — when was this written, and did a human or an agent
 * write it.
 *
 * The unit is the row, because a row is what the reader sees. The strip is the
 * same height as the terminal and has exactly one cell per line, so a cell can
 * only be about the line beside it: a progress bar that repaints a megabyte into
 * one row is one cell, and a span covering five rows is five. That is why a
 * row's status is decided by content rather than by an offset — which marks
 * reach the bytes that landed on this row — and why the answer has an order: an
 * AI agent's input beats the echo of a human's input, which beats plain output.
 *
 * Divining a row from a byte needs something that knows both, and xterm holds
 * rows while the log holds bytes. So the terminal is not asked: the server
 * replays the log through a small terminal model at the width the terminal
 * currently has (GET /api/shells/{id}/rail) and answers with each row's byte
 * range. The index is then read as a tiling of those ranges, which makes a row's
 * status a question about content rather than about position.
 *
 * Nothing is recorded as output streams in, and that is the point. A mapping
 * recorded from the stream is only right for the terminal that produced it: a
 * reloaded channel delivers its whole transcript in one write, so a recording
 * would be a single span for the entire scrollback, and a window resize reflows
 * every line, so every recorded row number goes stale. The log and the width are
 * the only inputs the mapping has, so it is recomputed from them — on load, on
 * resize, on scroll — and one code path answers all three.
 *
 * Read-only by design. Hover (desktop) or tap (touch) shows the row's status and
 * time; clicking a cell does not scroll the terminal. A row is derivable from a
 * mark, so this is a choice rather than a limitation: the rail is a map of what
 * is on screen, and a control that jerks the viewport under a cursor aimed at a
 * cell costs more than it gives.
 *
 * A full-screen program takes the screen over: it paints the alternate buffer,
 * and nothing it paints is in the log. The rail draws nothing there — the rows it
 * would name are the ones the program is covering, so a strip over it is a map of
 * somewhere else — and comes back when the program exits and the terminal puts its
 * own rows back.
 *
 * Refresh is self-contained: a MutationObserver on the channel body notices new
 * output or a tab switch, and the terminal's own resize event notices a reflow.
 * Both schedule a fetch under a minimum gap, so a streaming terminal costs at
 * most one request per gap instead of one per chunk. Each response covers the
 * screen plus a screen of margin on either side, so a scroll within that margin
 * repaints from what is already in hand.
 */

/* Floor between two rail fetches for one window. Long enough that continuous
   output cannot turn into a request storm, short enough that the rail catches up
   while the user is still looking at the output that moved it.

   This used to be 800ms, from when a refresh meant the whole index of the shell
   (hundreds of milliseconds and megabytes of JSON on a long-lived shell) and the
   gap was what kept that off the hot path. A refresh now asks for one screen's
   rows, which the server lays out incrementally — it stops reading the log as
   soon as the window is settled — so the gap is not protecting a full index read.
   What it is for now is bounds: a shell flooding output still refreshes a few
   times a second instead of once per terminal frame. */
var SHELL_RAIL_MIN_GAP_MS = 150;

/* How many rows of margin a fetch asks for beyond the screen, on each side. The
   rail follows the scroll, and a response that covers only the screen it was
   asked for would put every scroll one request behind the text. The margin is
   what makes a scroll inside the fetched range a repaint rather than a fetch, and
   it costs the server one extra screen of layout work. */
var SHELL_RAIL_MARGIN_ROWS = 48;

/* Fetch the row layout of one channel's log at the terminal's current width and
   draw it. One request answers the whole rail — which bytes are on which row, and
   what the marks over those bytes are — because the row layout only exists on the
   server: the browser has the text, but a terminal model over the whole log is
   not something it can run per repaint.

   The width is read from the terminal rather than from a stored value, so a
   resize needs no invalidation: the next fetch simply lays the log out again at
   the new width, and the reflow that moved every row is answered by the same code
   path as the first load. */
function railFetch(win, sid) {
  var st = win && win._rail;
  if (!st) return;
  var term = railTermFor(win, sid);
  /* A full-screen program is on screen: its rows are not the log's, so there is
     nothing to ask for and nothing to draw. The strip is hidden from the layout
     already in hand rather than cleared, because the terminal puts the same rows
     back when the program exits — the repaint that follows then needs no refetch,
     and clearing `lastBody` here would leave the strip hidden until new output
     arrived to fetch it again. */
  if (railOnAltScreen(term)) {
    renderShellTimeline(st, st.lastBody, null);
    return;
  }
  var vis = railVisibleRange(term);
  if (!vis) {
    st.lastBody = null;
    renderShellTimeline(st, null, null);
    return;
  }
  var seq = ++st.fetchSeq;
  var want = railFetchWindow(vis);
  var url = apiPath('/api/shells/') + encodeURIComponent(sid) + '/rail?cols=' + Math.max(2, Number(term.cols) || 80) +
    '&height=' + Math.max(1, vis.rows) +
    '&top=' + want.top + '&count=' + want.count;
  fetch(url).then(function (r) {
    if (!r.ok) throw new Error('rail ' + r.status);
    return r.json();
  }).then(function (body) {
    /* The user may have switched shell, resized again, or scrolled on while this
       was in flight; a response for a state that is no longer current must not
       paint. The width is compared too, since a reflow during the request makes
       the row numbers in it describe the old layout. */
    if (!win._rail || win._rail !== st || seq !== st.fetchSeq) return;
    if (win._activeChannelSid !== sid) return;
    var nowTerm = railTermFor(win, sid);
    if (!nowTerm || Math.max(2, Number(nowTerm.cols) || 80) !== Number(body.cols)) {
      shellRailSchedule(win);
      return;
    }
    st.sid = sid;
    st.lastFetchAt = Date.now();
    /* One response carries both halves: the server lays the log out and reads the
       mark index over the bytes it laid out. It is kept as one — the marks the rail
       is drawn from and the rows they are read against — so a repaint after a
       scroll reuses it instead of asking again. */
    st.lastBody = body;
    st.layoutSeq += 1;
    railBindScroll(win, sid);
    railRepaintWindow(win, sid, true);
  }).catch(function () {});
}

/* The row window a fetch asks for: the visible screen, widened by the margin and
   clamped at the top of the log. */
function railFetchWindow(vis) {
  var count = vis.rows + SHELL_RAIL_MARGIN_ROWS * 2;
  var top = vis.top - SHELL_RAIL_MARGIN_ROWS;
  if (top < 0) {
    count += top;
    top = 0;
  }
  if (count < vis.rows) count = vis.rows;
  return { top: top, count: count };
}

/* Whether the layout in hand already describes the rows on screen, so a repaint
   can use it instead of asking again. Also requires the width to match: after a
   reflow the rows in the response name lines that no longer exist.

   The window a response covers is bounded by two different things, and only one of
   them is its array length. `spans` is answered at the requested `count` — it is
   padded with nulls for rows past the end of the log — so its length says how many
   rows were *asked for*, while `total_rows` says how many the layout actually has,
   in the terminal's own numbering (so it is an absolute row number, not a length
   counted from `top`). A fetch made while the log was shorter therefore answers a
   full-looking array whose tail is nulls, and testing the screen against the
   array's length alone claims coverage of rows that hold no span: the cells for the
   newest output are not drawn, and because this is the test that decides whether to
   fetch, nothing asks again until the screen has scrolled past the stale tail.

   That is the read as "the bar is shorter than the output": the strip stops a few
   rows above the tail and refills only once enough new output has arrived to push
   the screen out of the range the client believed it had. */
function railCoversWindow(st, vis, cols) {
  var b = st && st.lastBody;
  if (!b || !b.spans || !vis) return false;
  if (Number(b.cols) !== cols) return false;
  var top = Number(b.top) || 0;
  var end = top + b.spans.length;
  /* The layout's own extent, when the server reported one: rows at or past it hold
     no span, so they are not covered however long the array is. The number is an
     absolute row, which is why it is compared against `end` and not added to `top`
     — reading it as a count would extend the window past every row the terminal
     has. */
  var total = Number(b.total_rows);
  if (isFinite(total) && total >= 0 && total < end) end = total;
  return vis.top >= top && vis.top + vis.rows <= end;
}

/* The row spans of the visible screen, taken out of the layout in hand. A span is
   the byte range the server laid out for that row, or null for a row holding no
   bytes — which draws no cell rather than borrowing a neighbour's status. */
function railSpansFor(st, vis) {
  var b = st && st.lastBody;
  if (!b || !b.spans || !vis) return null;
  var top = Number(b.top) || 0;
  var out = new Array(vis.rows);
  var any = false;
  for (var i = 0; i < vis.rows; i++) {
    var at = vis.top + i - top;
    if (at < 0 || at >= b.spans.length) continue;
    var sp = b.spans[at];
    if (!sp) continue;
    var s = Number(sp.start);
    var e = Number(sp.end);
    if (!isFinite(s) || !isFinite(e) || e <= s) continue;
    out[i] = { start: s, end: e };
    any = true;
  }
  return any ? out : null;
}

/* The byte window the visible rows hold: the union of their ranges, which says
   which part of the mark index the cells are about. */
function railSpanWindow(spans) {
  if (!spans) return null;
  var start = Infinity;
  var end = -Infinity;
  for (var i = 0; i < spans.length; i++) {
    var s = spans[i];
    if (!s) continue;
    if (s.start < start) start = s.start;
    if (s.end > end) end = s.end;
  }
  if (start === Infinity || end <= start) return null;
  return { start: Math.floor(start), end: Math.ceil(end) };
}

/* How assertive a status is, for picking the status of a row several marks
   reach. The order is the whole point of the rail: an agent's input outranks a
   human's, which outranks the shell's own bytes — a row that carries a typed
   command and its echo is the user's line, not the terminal's. A review decision
   ranks zero on purpose: it is a verdict *about* a line, not content of its own,
   so it never colours a row (the card shows it as a badge instead). */
function railRank(status) {
  var s = String(status || '');
  if (s === 'a') return 3;
  if (s === 'i') return 2;
  if (s === 'o' || s === '') return 1;
  return 0;
}

/* The status of the input that produced each mark, or '' when none precedes it.
 *
 * An output belongs to the input that produced it: the log records an input as a
 * zero-length mark and the output that follows starts at the same offset, so an
 * output's cause is the nearest preceding i/a mark. Walking the marks in file
 * order is enough to know it, which is why this needs no server field: the
 * windowed response carries the marks in file order, including the input that
 * shares the offset the window starts inside.
 *
 * This is a display-layer reading of the index, not a change to it. A
 * long-running command whose output interleaves with the next input is attributed
 * to that later input, which is the approximation the log's ordering forces: it
 * holds offsets, not causality. */
function railCauses(marks) {
  var causes = new Array(marks.length);
  var last = '';
  for (var i = 0; i < marks.length; i++) {
    causes[i] = last;
    var s = String((marks[i] || {}).status || '');
    if (s === 'i' || s === 'a') last = s;
  }
  return causes;
}

/* One status cell per visible row, or null when there is nothing to read.
 *
 * Each row's status comes from the marks whose bytes reach it, at the priority
 * of railRank: AI input over interface input over output. Rows no mark reaches
 * come back null and are not drawn — the strip stays aligned with the text
 * instead of spreading cells over blank rows.
 *
 * The marks tile the log (each one's end is the next one's start), so a single
 * cursor serves every row: a span still running when the row ends is carried to
 * the next row, and a mark that ended above the row is never looked at again.
 * `spans` is the server's row layout: one byte range per row, or null for a row
 * that holds none. A row with no span is skipped without touching the cursor.
 * bounds is skipped without touching the cursor, because it holds no bytes for a
 * mark to land on. */
function railCells(marks, spans) {
  if (!spans || !marks || !marks.length) return null;
  var rows = spans.length;
  var n = marks.length;
  var causes = railCauses(marks);
  var cells = new Array(rows);
  /* The marks are ordered by offset and tile the log, so only those from the first
     visible byte on can reach a row; the ones before it are found by bisection
     instead of by walking the index from its start. That is what makes a repaint
     O(log n + rows) in the index rather than O(n) — the difference between a
     scroll costing microseconds and costing a full scan of a day-long shell's
     marks, on every scroll event. */
  var floor = Infinity;
  for (var f = 0; f < rows; f++) {
    var sp0 = spans[f];
    if (!sp0 || !(sp0.end > sp0.start) || sp0.start >= floor) continue;
    floor = Number(sp0.start);
  }
  var k = 0;        // next mark to consider
  if (floor !== Infinity) {
    var lo = 0;
    var hi = n;
    while (lo < hi) {
      var mid = (lo + hi) >> 1;
      var m0 = marks[mid] || {};
      var e0 = Number(m0.end);
      if (!isFinite(e0)) e0 = Number(m0.start);
      /* A mark whose end is below the first visible byte cannot reach it; a
         zero-length mark on the byte itself still can, and `e0 < floor` keeps
         it. */
      if (isFinite(e0) && e0 < floor) lo = mid + 1;
      else hi = mid;
    }
    k = lo;
  }
  /* No carry is carried in: the mark before k ends below the first visible byte,
     so it cannot reach any row, and the loop below picks up a span still running
     from the row it starts on. */
  var carry = null;   // a span that began on an earlier row and is still running
  var carryIdx = -1;  // its index, so an inherited row still knows its cause
  for (var r = 0; r < rows; r++) {
    var sp = spans[r];
    if (!sp) continue;
    var s = Number(sp.start);
    var e = Number(sp.end);
    if (!isFinite(s) || !isFinite(e) || e <= s) continue;
    var hit = null;
    var hitIdx = -1;
    var review = false;
    if (carry) {
      if (carry.end <= s) carry = null;
      else if (railRank(carry.status) > 0) { hit = carry; hitIdx = carryIdx; }
      else review = true;
    }
    while (k < n) {
      var m = marks[k];
      var ms = Number(m.start);
      if (!isFinite(ms)) { k++; continue; }
      var me = Number(m.end);
      if (!isFinite(me) || me < ms) me = ms;
      /* Marks are ordered, so once one starts at or past the row's end, so does
         every later one. */
      if (ms >= e) break;
      var rank = railRank(m.status);
      var point = me <= ms;
      /* A row's span covers every byte that reached it, and the rows tile the log,
         so a mark is reached by the row whose span contains it. A zero-length mark
         (an input, recorded at the offset the keystroke ended at) sits on the byte
         it followed: inside the row when that row holds later bytes, and on its
         start when the mark is the row's first byte — which is why a mark at
         exactly `s` still counts. */
      if (!(me > s || (point && ms >= s))) { k++; continue; }
      if (rank > 0) {
        if (!hit || rank > railRank(hit.status)) {
          hit = m;
          hitIdx = k;
        }
      } else {
        review = true;
      }
      if (me > e) {
        /* The span is still running: it colours the rows below this one too. */
        carry = m;
        carryIdx = k;
        break;
      }
      k++;
    }
    if (!hit) continue;
    var time = Number(hit.time);
    if (!isFinite(time)) time = 0;
    var meta = railMarkMeta(hit.status);
    var status = String(hit.status || '');
    cells[r] = {
      status: status,
      cls: meta.cls,
      label: meta.label,
      time: time,
      start: s,
      end: e,
      review: review,
      /* Only an output row is framed: the cause says what produced *these*
         bytes, and an input row is not produced by anything — it already has
         the colour of the input it is. The cause is read off the mark that
         actually decided the row (hitIdx), not off the last mark walked: a
         lower-ranked span that outlives the row becomes `carry`, and a later row
         inheriting it would otherwise be framed in the colour of a mark that
         never produced it. */
      cause: (status === 'o' || status === '') ? String(causes[hitIdx] || '') : ''
    };
  }
  return cells;
}

/* Whether the terminal is showing a full-screen program's screen rather than the
   transcript. The alternate buffer is a screen of its own that the log does not
   contain: the terminal throws it away when the program exits and puts the rows
   that were there before back. Its `viewportY` is always 0, which is what makes a
   stale layout for the rows behind the program look current — so this is asked
   before the window is read, not after. */
function railOnAltScreen(term) {
  return !!(term && term.buffer && term.buffer.active &&
    term.buffer.active.type === 'alternate');
}

/* The rows on screen right now, or null when there is no terminal to ask.
 *
 * The window is exactly the screen — `viewportY` rows down from the top of the
 * scrollback, `term.rows` of them — and nothing more: no margin, no clamp to
 * where the content stops. The strip is a second rendering of the same rows, so
 * its cells must be the rows the user is looking at and nothing else; a margin
 * would scale the text rows into a fraction of the strip and break the one thing
 * this layout is for. Rows the layout gives no bytes simply have no cell. */
function railVisibleRange(term) {
  if (!term || !term.buffer || !term.buffer.active) return null;
  /* No rows at all for the alternate screen: the honest answer, since the rail's
     rows are the transcript's and the program's are not in it. */
  if (railOnAltScreen(term)) return null;
  var rows = Number(term.rows);
  if (!isFinite(rows) || rows <= 0) return null;
  var top = Number(term.buffer.active.viewportY);
  if (!isFinite(top) || top < 0) top = 0;
  return { top: top, rows: rows, bottom: top + rows };
}

/* One cell's CSS class and human label for a log status. Literal t() calls per
   status rather than a computed key: the catalog tests only count keys a literal
   call site names, and a computed key would fail as an orphan. */
function railMarkMeta(status) {
  if (status === 'a') return { cls: 'term-rail-agent', label: t('rail.status.agent') };
  if (status === 'i') return { cls: 'term-rail-input', label: t('rail.status.input') };
  if (status === 'q' || status === 'A') return { cls: 'term-rail-review', label: t('rail.status.review') };
  return { cls: 'term-rail-output', label: t('rail.status.output') };
}

/** The cell under a node, or null. */
function shellRailCellFrom(node) {
  return node && node.closest ? node.closest('.term-rail-cell') : null;
}

/** Mount the rail in one terminal window. Idempotent: a window re-initialised in
    place keeps the rail it already has (a second rail would double every cell). */
function initShellTimeline(win) {
  if (!win || win._rail) return;
  /* Host is the channel body, not the terminal wrap: the wrap also holds the
     footer tab strip and the empty state, so a strip stretched across the wrap
     would run down over the footer's "+" and its cells would be measured against
     the wrong box. The body is the terminal's own box. */
  var host = win.querySelector('.shell-channel-body') || win.querySelector('.shell-terminal-wrap');
  if (!host) return;

  var el = document.createElement('div');
  el.className = 'term-rail';
  el.hidden = true;
  el.setAttribute('role', 'list');
  el.setAttribute('aria-label', t('rail.label'));
  /* The label lives in the rail so it can be positioned beside the cell it
     describes; it is not a native title, which no touch device can show. */
  var pop = document.createElement('div');
  pop.className = 'term-rail-pop';
  pop.hidden = true;
  el.appendChild(pop);
  /* The detail card is a sibling of the cells rather than a child of one: the
     cells are rebuilt on every refresh, so a card parented to one would vanish
     while the user was reading it. */
  var card = document.createElement('div');
  card.className = 'term-rail-card';
  card.hidden = true;
  el.appendChild(card);
  host.appendChild(el);

  var st = {
    el: el,
    pop: pop,
    card: card,
    sid: '',
    cell: null,
    pinned: false, // a tapped label stays until the next tap outside the rail
    timer: 0,
    lastFetchAt: 0,
    repaintRaf: 0,    // pending per-frame repaint, so a stream is one repaint per frame
    observer: null,
    docHandlers: [],
    lastBody: null,   // the server's response in hand: the rows and the marks over them
    layoutSeq: 0,     // bumped on every layout, so a repaint cannot compare equal
    fetchSeq: 0,      // orders responses so a slow one cannot overwrite a newer rail
    view: null,       // the row window last drawn, to skip identical repaints
    scrollBound: null, // the channel whose viewport 'scroll' we are listening to
    unsubs: [],       // xterm subscriptions for that channel (onResize, onBufferChange)
    resizeRo: null    // layout watcher on the strip's host, for a drag-resize
  };
  win._rail = st;

  /* A dragged window changes the host's box without changing the terminal's
     rows, so nothing else would tell the rail to re-measure. Observing the host
     also covers the fonts loading and the layout settling after a tab switch. */
  if (typeof ResizeObserver !== 'undefined') {
    try {
      st.resizeRo = new ResizeObserver(function () { railGeometryChanged(win, win._activeChannelSid); });
      st.resizeRo.observe(host);
    } catch (e) { st.resizeRo = null; }
  }

  el.addEventListener('mouseover', function (e) {
    var cell = shellRailCellFrom(e.target);
    if (cell) shellRailShowPop(st, cell);
  });
  el.addEventListener('mouseleave', function () {
    if (!st.pinned) shellRailHidePop(st);
  });
  /* Touch has no hover, so a tap opens the same thing a click does. Tapping the
     shown cell again dismisses it. */
  el.addEventListener('click', function (e) {
    var cell = shellRailCellFrom(e.target);
    if (!cell || cell === st.cell) {
      st.pinned = false;
      shellRailHidePop(st);
      printShellRailCard(st, null);
      return;
    }
    st.pinned = true;
    shellRailShowPop(st, cell);
    /* A click (or tap) opens the detail card: the hover label carries the status
       and the clock time, and the card adds what a hover cannot reach on a phone
       — the byte offset, the relative age and the row it sits on. */
    printShellRailCard(st, cell);
  });
  /* The rail sits over the terminal, so a press on it must not start a window
     drag or focus (and thus reflow) the PTY. */
  el.addEventListener('mousedown', function (e) { e.stopPropagation(); });

  var onDocDown = function (e) {
    if (el.contains(e.target)) return;
    st.pinned = false;
    shellRailHidePop(st);
    printShellRailCard(st, null);
  };
  document.addEventListener('mousedown', onDocDown, true);
  document.addEventListener('touchstart', onDocDown, true);
  st.docHandlers.push([document, 'mousedown', onDocDown, true], [document, 'touchstart', onDocDown, true]);

  var body = win.querySelector('.shell-channel-body');
  if (body && typeof MutationObserver !== 'undefined') {
    /* The rail lives inside the observed subtree, so its own redraw would count
       as new output and each render would schedule the fetch that renders again:
       a self-sustaining request every gap, forever. Records whose target is
       inside the rail are this module's own work and are skipped. */
    st.observer = new MutationObserver(function (records) {
      for (var i = 0; i < records.length; i++) {
        if (!st.el.contains(records[i].target)) {
          shellRailSchedule(win);
          return;
        }
      }
    });
    /* Class toggles cover a tab switch; childList/characterData cover output
       arriving in the visible terminal. */
    st.observer.observe(body, {
      childList: true,
      subtree: true,
      characterData: true,
      attributes: true,
      attributeFilter: ['class']
    });
  }

  addWinDisposable(win, function () { disposeShellTimeline(win); });
  shellRailSchedule(win);
}

/** Tear the rail down with its window: disconnect the observer, drop the
    document listeners (they live outside the window's DOM, so removing the
    window does not remove them) and the debounce timer. */
function disposeShellTimeline(win) {
  var st = win && win._rail;
  if (!st) return;
  if (st.timer) {
    clearTimeout(st.timer);
    st.timer = 0;
  }
  if (st.repaintRaf) {
    /* A frame that never ran would keep the guard set and no later repaint would
       be scheduled; the window is going away either way, so cancel it. */
    try { if (typeof cancelAnimationFrame === 'function') cancelAnimationFrame(st.repaintRaf); } catch (e0) {}
    st.repaintRaf = 0;
  }
  for (var i = 0; i < st.docHandlers.length; i++) {
    var h = st.docHandlers[i];
    try { h[0].removeEventListener(h[1], h[2], h[3]); } catch (e) {}
  }
  st.docHandlers = [];
  if (st.observer) {
    try { st.observer.disconnect(); } catch (e2) {}
    st.observer = null;
  }
  railUnbindScroll(st);
  if (st.resizeRo) {
    try { st.resizeRo.disconnect(); } catch (e3) {}
    st.resizeRo = null;
  }
  if (st.el && st.el.parentNode) st.el.parentNode.removeChild(st.el);
  win._rail = null;
}

/** Queue a debounced refresh. One pending timer per window: the first trigger
    after a fetch waits out the rest of the gap, and the last one always runs, so
    output that never stops still refreshes exactly once per gap. */
function shellRailSchedule(win) {
  var st = win && win._rail;
  if (!st || st.timer) return;
  var wait = SHELL_RAIL_MIN_GAP_MS - (Date.now() - st.lastFetchAt);
  if (wait < 0) wait = 0;
  st.timer = setTimeout(function () {
    st.timer = 0;
    st.lastFetchAt = Date.now();
    refreshShellTimeline(win);
  }, wait);
}

/* The terminal showing a channel, or null. The rail is a decoration of the
   terminal's own scrollback, so it needs that channel's term object: the byte
   window is meaningless without the rows it maps onto. */
function railTermFor(win, sid) {
  if (!win || !sid) return null;
  var ch = win._channels && win._channels[sid];
  return (ch && ch.term) || win._term || null;
}

/* Everything a paint needs about the channel right now, or null when there is no
   terminal to read it from: the rows on screen, the spans the server laid out for
   them, and the measured screen box to align the strip to. */
function railViewFor(win, sid) {
  var st = win && win._rail;
  if (!st) return null;
  var term = railTermFor(win, sid);
  var vis = railVisibleRange(term);
  if (!vis) return null;
  var cols = Math.max(2, Number(term.cols) || 80);
  var spans = railSpansFor(st, vis);
  return {
    top: vis.top,
    rows: vis.rows,
    cols: cols,
    spans: spans,
    /* The comparison fields are copied out rather than read back later: the
       layout is replaced on every fetch, and a view that kept the array would
       compare equal to itself and skip the repaint that new output needs. */
    layoutSeq: st.layoutSeq,
    window: railSpanWindow(spans),
    box: railScreenBox(st, term)
  };
}

/* The terminal's text box, in pixels relative to the strip's own host.
 *
 * The strip is a child of the channel body while the rows belong to
 * `.xterm-screen`, and those are not the same box: the terminal wrap and the
 * xterm element contribute insets of their own, and xterm keeps `.xterm-screen`
 * at exactly rows x cell height. Measuring rather than guessing is what puts a
 * cell on the row its text is on instead of a few pixels above it. */
function railScreenBox(st, term) {
  var host = st && st.el && st.el.parentNode;
  var screenEl = term && term.element && term.element.querySelector
    ? term.element.querySelector('.xterm-screen') : null;
  if (!host || !screenEl ||
      typeof screenEl.getBoundingClientRect !== 'function' ||
      typeof host.getBoundingClientRect !== 'function') return null;
  var sr = screenEl.getBoundingClientRect();
  var hr = host.getBoundingClientRect();
  if (!sr || !hr || !isFinite(sr.height) || sr.height <= 0) return null;
  return { top: sr.top - hr.top, height: sr.height };
}

/* Whether a repaint would change anything. A scroll moves `top`, a resize changes
   `rows` or the measured box, a fetch replaces the layout — so those are the
   things worth comparing. Scroll events fire in bursts (momentum, a reflow during
   a drag), and this keeps a burst to one repaint per actual movement. */
function railViewMoved(prev, next) {
  if (!prev || !next) return prev !== next;
  if (prev.top !== next.top || prev.rows !== next.rows || prev.cols !== next.cols) return true;
  if (prev.layoutSeq !== next.layoutSeq) return true;
  var a = prev.box;
  var b = next.box;
  if (!a || !b) return a !== b;
  return a.top !== b.top || a.height !== b.height;
}

/* Draw the rail from the layout in hand, using the current window. Called on
   scroll and on resize: the rows on screen have moved, but the layout that gives
   them their bytes may still cover them, and re-fetching per scroll event would
   put a request behind every wheel tick. Only a viewport that has left the
   fetched rows asks again (railEnsureWindow).

   `force` is for a repaint whose reason is invisible in the window itself: a
   resize changes the pixel box the cells are drawn in while `top` and `rows` can
   stay exactly as they were. */
function railRepaintWindow(win, sid, force) {
  var st = win && win._rail;
  if (!st || !st.lastBody || st.sid !== sid) return;
  var view = railViewFor(win, sid);
  if (!force && !railViewMoved(st.view, view)) return;
  renderShellTimeline(st, st.lastBody, view);
}

/* The terminal's geometry moved: the strip has to be measured again, and if the
   width changed the log has to be laid out again too.

   A resize reflows the text, so every row number in hand names a line that no
   longer exists at that position — this is the whole answer to a rail that is
   wrong after a resize, because there is no stored mapping to invalidate. A
   height-only change leaves the layout valid and costs a repaint. */
function railGeometryChanged(win, sid) {
  railRepaintWindow(win, sid, true);
  railEnsureWindow(win, sid);
}

/* Ask for a fresh layout when the rows on screen are not the ones in hand: a
   scroll past the margin, a resize, or the first paint. The fetch keeps its
   debounce, so a drag through a long scrollback asks once per gap rather than
   once per event. */
function railEnsureWindow(win, sid) {
  var st = win && win._rail;
  if (!st || st.sid !== sid) return;
  var term = railTermFor(win, sid);
  var vis = railVisibleRange(term);
  if (!vis) return;
  if (railCoversWindow(st, vis, Math.max(2, Number(term.cols) || 80))) return;
  shellRailSchedule(win);
}

/* Follow the scrollback and the row count of the channel being drawn. Two
   subscriptions, both on that channel: one 'scroll' listener on its viewport
   (xterm's own onScroll fires for programmatic scrolling too, which is why the
   viewport element is used — the rail tracks what the user sees, including a
   scroll caused by new output) and the terminal's own resize event, which is the
   only signal that `rows` changed without the pixel box changing with it. Both
   are moved when the active channel changes. */
function railBindScroll(win, sid) {
  var st = win && win._rail;
  if (!st || st.scrollBound === sid) return;
  railUnbindScroll(st);
  st.scrollBound = sid;
  var term = railTermFor(win, sid);
  var vp = term && term.element && term.element.querySelector('.xterm-viewport');
  if (vp) {
    var onScroll = function () {
      railRepaintWindow(win, sid);
      railEnsureWindow(win, sid);
    };
    vp.addEventListener('scroll', onScroll, { passive: true });
    st.scrollUnbind = function () { vp.removeEventListener('scroll', onScroll); };
  }
  if (term && typeof term.onResize === 'function') {
    try {
      st.unsubs.push(term.onResize(function () { railGeometryChanged(win, sid); }));
    } catch (e) {}
  }
  /* Entering or leaving the alternate screen changes which rows are on screen
     without moving `viewportY` (the alternate buffer is always at 0) and without
     necessarily resizing, so neither of the two subscriptions above fires: a
     repaint has to be asked for by name, or the rail would keep describing the
     rows the full-screen program covered. */
  var buf = term && term.buffer;
  if (buf && typeof buf.onBufferChange === 'function') {
    try {
      st.unsubs.push(buf.onBufferChange(function () { railGeometryChanged(win, sid); }));
    } catch (e) {}
  }
}

/** Release the scroll listener and the terminal subscriptions. */
function railUnbindScroll(st) {
  if (st.scrollUnbind) {
    try { st.scrollUnbind(); } catch (e) {}
    st.scrollUnbind = null;
  }
  for (var i = 0; i < st.unsubs.length; i++) {
    try { st.unsubs[i].dispose(); } catch (e2) {}
  }
  st.unsubs = [];
  st.scrollBound = null;
}

/** Fetch the mark index for the rows on screen and redraw the rail. A failure
    keeps the last good rail: this is a decoration, and a toast per blip would
    shout louder than the thing it describes. */
function refreshShellTimeline(win) {
  var st = win && win._rail;
  if (!st) return;
  var sid = win._activeChannelSid || '';
  if (!sid) {
    st.sid = '';
    st.lastBody = null;
    st.view = null;
    renderShellTimeline(st, null, null);
    return;
  }
  st.sid = sid;
  railFetch(win, sid);
}

/** Draw one marks response. Cells are rebuilt rather than reconciled: the strip
    holds one cell per visible row and a redraw is cheap, so nothing can be left
    behind by a scroll, a resize or a switch of channel.

    The window is exactly the screen — `view.top` rows into the scrollback,
    `view.rows` of them — which makes the strip a second rendering of the rows
    beside it: one cell per terminal row, aligned by row index. Each row's status
    comes from the marks reaching that row's bytes (see railCells), and a row no
    mark reaches draws no cell at all. An empty slot is the honest answer for a
    row the log knows nothing about, and it costs no alignment: every cell is
    placed by its row index, not stacked under its neighbour.

    A cell is a percentage of the strip, and the strip is measured to the
    terminal's screen box, so the cells stay on their rows through a resize
    without a repaint — while the measurement keeps them on the text's rows even
    though the strip's CSS box (the channel body) and the text's box (xterm's
    screen) are not the same. */
function renderShellTimeline(st, body, view) {
  var el = st.el;
  el.querySelectorAll('.term-rail-cell').forEach(function (c) { c.remove(); });
  st.pinned = false;
  shellRailHidePop(st);
  printShellRailCard(st, null);
  st.view = view || null;

  var marks = (body && body.marks) || [];
  /* No laid-out rows means no row has bytes, and without bytes the marks have
     nothing to be read against. An approximate strip would look like information
     while being wrong, so the rail stays hidden until a layout says where the
     bytes went. The response in hand is kept either way: it is what a later
     repaint draws from, and a hidden strip is not a reason to ask for it again. */
  if (!marks.length || !view || !view.spans || !view.spans.length) {
    el.hidden = true;
    return;
  }

  var cells = railCells(marks, view.spans);

  var box = view.box;
  if (box && isFinite(box.height) && box.height > 0) {
    el.style.top = Number(box.top).toFixed(2) + 'px';
    el.style.height = Number(box.height).toFixed(2) + 'px';
    el.style.bottom = 'auto';
  } else {
    /* Nothing measured (a hidden window, a browser without rects): fall back to
       the stylesheet's box. The cells are percentages, so they tile correctly in
       whatever box the strip ends up with. */
    el.style.top = '';
    el.style.height = '';
    el.style.bottom = '';
  }

  var step = 100 / view.rows;
  var drawn = 0;
  for (var r = 0; r < view.rows; r++) {
    var c = cells && cells[r];
    if (!c) continue;
    var cell = document.createElement('button');
    cell.type = 'button';
    cell.className = 'term-rail-cell ' + c.cls;
    cell.style.top = (r * step).toFixed(4) + '%';
    cell.style.height = step.toFixed(4) + '%';
    cell.setAttribute('data-row', String(view.top + r));
    cell.setAttribute('data-status', c.status);
    cell.setAttribute('data-time', String(c.time));
    /* Which input produced this row's bytes, for the frame around it. A class
       rather than an inline colour: the value lives in the stylesheet so
       `--assets` can restyle it, and the module only says *which* input caused
       the row. */
    if (c.cause) {
      cell.setAttribute('data-cause', c.cause);
      cell.classList.add(c.cause === 'a' ? 'term-rail-cause-agent' : 'term-rail-cause-input');
      /* The frame closes around the run, not around every row: a cap between two
         rows of one command's output would cut the band into beads, which is
         what the flush tiling exists to prevent. Only the ends of the run get a
         horizontal edge, so a command's output reads as one framed block. A row
         with no cell (no bytes) also ends the run, because the band visibly
         stops there. */
      var prev = r > 0 ? cells[r - 1] : null;
      var next = r + 1 < view.rows ? cells[r + 1] : null;
      if (!prev || prev.cause !== c.cause) cell.classList.add('term-rail-cause-top');
      if (!next || next.cause !== c.cause) cell.classList.add('term-rail-cause-bottom');
    }
    /* A row's byte bounds are interpolated, so they are fractional; the card must
       not print 1155.1538461538462 as if it were a position in the log. They are
       rounded for display only — nothing else reads them. */
    var cellStart = Math.round(c.start);
    cell.setAttribute('data-start', String(cellStart));
    cell.setAttribute('data-end', String(Math.max(cellStart, Math.round(c.end))));
    if (c.review) cell.setAttribute('data-review', '1');
    cell.setAttribute('data-label', c.label);
    cell.setAttribute('aria-label', t('rail.mark.title', { status: c.label, time: fmtTime(c.time) }));
    el.appendChild(cell);
    drawn++;
  }
  st.lastBody = body;
  el.hidden = !drawn;
}

/** Show the label for one cell. */
function shellRailShowPop(st, cell) {
  if (!cell) return;
  var meta = railMarkMeta(cell.getAttribute('data-status') || '');
  var timeMs = Number(cell.getAttribute('data-time'));
  if (!isFinite(timeMs)) timeMs = 0;
  st.cell = cell;
  st.pop.textContent = t('rail.mark.title', { status: meta.label, time: fmtTime(timeMs) });
  st.pop.hidden = false;
  // Centre the label on its cell, clamped to the strip: a cell at either end
  // would otherwise push the label out of the terminal's box.
  var railH = st.el.clientHeight || 0;
  var popH = st.pop.offsetHeight || 0;
  var centre = cell.offsetTop + (cell.offsetHeight / 2);
  if (railH - popH < 0) {
    centre = railH / 2;
  } else {
    centre = Math.min(railH - (popH / 2), Math.max(popH / 2, centre));
  }
  st.pop.style.top = Math.round(centre) + 'px';
}

function shellRailHidePop(st) {
  if (!st) return;
  st.cell = null;
  st.pop.hidden = true;
}

/** Relative age of a mark, in the largest unit that still reads as a number:
    "just now", "12s ago", "4m ago", "2h ago", "3d ago". A card that only shows
    an absolute clock time makes the reader do the subtraction, which is exactly
    the question the rail exists to answer (how long ago was this written). */
function railRelativeAge(timeMs) {
  if (!isFinite(timeMs) || timeMs <= 0) return '';
  var ms = Date.now() - timeMs;
  if (ms < 0) return t('rail.card.now');
  var sec = Math.floor(ms / 1000);
  if (sec < 5) return t('rail.card.now');
  if (sec < 60) return t('rail.card.secAgo', { n: sec });
  var min = Math.floor(sec / 60);
  if (min < 60) return t('rail.card.minAgo', { n: min });
  var hr = Math.floor(min / 60);
  if (hr < 24) return t('rail.card.hourAgo', { n: hr });
  return t('rail.card.dayAgo', { n: Math.floor(hr / 24) });
}

/** Open (or close, with a null cell) the detail card for one row. */
function printShellRailCard(st, cell) {
  if (!st || !st.card) return;
  if (!cell) {
    st.card.hidden = true;
    return;
  }
  var meta = railMarkMeta(cell.getAttribute('data-status') || '');
  var timeMs = Number(cell.getAttribute('data-time'));
  if (!isFinite(timeMs)) timeMs = 0;
  var start = Number(cell.getAttribute('data-start'));
  if (!isFinite(start)) start = 0;
  var end = Number(cell.getAttribute('data-end'));
  if (!isFinite(end)) end = start;

  var rows = [
    [t('rail.card.when'), fmtTime(timeMs)],
    [t('rail.card.age'), railRelativeAge(timeMs)],
    [t('rail.card.offset'), start + ' \u2192 ' + end],
    [t('rail.card.size'), (end - start) + ' B']
  ];
  var html = '<div class="term-rail-card-head">' +
    '<span class="term-rail-swatch ' + meta.cls + '"></span>' +
    escapeHtml(meta.label);
  /* A review decision shares the row it decided about, so it is a badge on that
     row's card rather than a colour of its own: the verdict is visible without
     stealing the row's status. */
  if (cell.getAttribute('data-review')) {
    html += '<span class="term-rail-card-badge">' + escapeHtml(t('rail.status.review')) + '</span>';
  }
  html += '</div>';
  for (var i = 0; i < rows.length; i++) {
    html += '<div class="term-rail-card-row"><span>' + escapeHtml(rows[i][0]) +
      '</span><b>' + escapeHtml(rows[i][1]) + '</b></div>';
  }
  st.card.innerHTML = html;
  st.card.hidden = false;

  // Anchor the card's top to the cell, clamped so it never leaves the rail.
  var railH = st.el.clientHeight || 0;
  var cardH = st.card.offsetHeight || 0;
  var centre = cell.offsetTop + (cell.offsetHeight / 2);
  var top = centre - (cardH / 2);
  if (top < 0) top = 0;
  if (railH - cardH < 0) {
    top = 0;
  } else if (top > railH - cardH) {
    top = railH - cardH;
  }
  st.card.style.top = Math.round(top) + 'px';
}
