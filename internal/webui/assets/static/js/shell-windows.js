function sessionTabbarEl() {
  return document.getElementById('sessdock');
}

/* The dock's own element, named for what it is now. sessionTabbarEl is kept as
 * the alias every existing caller already uses — the tab row is the dock's
 * collapsed state, not a second control. */
function sessdockEl() {
  return document.getElementById('sessdock');
}

/** The bare tab row's note.
 *
 *  The dock lists one tab per RUNNING SESSION plus one per window with no session
 *  (see dockEntries), so an empty row means there is nothing live to show at all —
 *  no session on the server and no window in this page. The note stays a plain
 *  label: there is nothing behind the row to open, so dressing it as a control
 *  would promise a list the page does not have. It is built here rather than
 *  declared in the markup because the row's shape is decided at paint time (the
 *  strip may already hold tabs), and it lives in the dock BAR rather than inside
 *  the strip: the strip is a role=tablist, and the only thing a tablist may hold
 *  is a tab. */
function paintSessdockEmpty(empty) {
  var bar = document.getElementById('sessdock-bar');
  if (!bar) return;
  var marker = document.getElementById('sessdock-empty');
  if (!empty) {
    if (marker) marker.remove();
    var strip = document.getElementById('session-tabs-scroll');
    if (strip) strip.style.display = '';
    return;
  }
  if (marker) return;
  marker = document.createElement('span');
  marker.id = 'sessdock-empty';
  marker.className = 'sessdock-empty';
  marker.setAttribute('data-i18n', 'dock.empty');
  /* A child of the BAR that is taken out of the flow.
   *
   * The bar is a wrapping flex row in which the strip claims `flex: 1 1 100%` to
   * force the batch keys onto row 2. So a third participant becomes a THIRD ROW:
   * measured with no windows open, the note alone claimed a full-width line above
   * the strip, the bar grew to 82px inside a 68px dock, and the dock — which has
   * no overflow — let it spill past its own border. Two rows plus a label is three
   * rows, however small the label is.
   *
   * It cannot go inside the strip either: that is a role=tablist whose only legal
   * child is a tab. So it stays a child of the bar and is positioned over row 1 by
   * CSS, which is where it belongs — it stands in for the tabs, and it must not
   * compete with them for the layout. */
  bar.insertBefore(marker, document.getElementById('session-tabs-scroll'));
  applyI18n(marker);
  /* The empty strip is taken OUT of the row while the note stands in for it. Both
     are `flex: 1 1 0%` in the same slot, so leaving the strip in place splits the
     width between an empty tablist and a sentence — and the sentence, being the
     thing with words in it, is the one that ends up truncated. The strip has no
     content to keep; it is rebuilt when the first tab arrives. */
  var strip = document.getElementById('session-tabs-scroll');
  if (strip) strip.style.display = 'none';
}

/** The glyph inside a dock tab's tick: the window's own KIND, not always a monitor.
 *
 *  It was a hard-coded monitor drawing, so every tab in the bar looked like a
 *  terminal — including the extension cards, which are not terminals and carry no
 *  session at all. With an extension window open the bar showed two identical
 *  monitor icons for two unrelated things, and the only way to tell them apart was
 *  to read the label.
 *
 *  The tick is also the tab's whole identity mark, so this is the one place the
 *  distinction has to be drawn: the bar is the list of open windows, and a list of
 *  windows that cannot be told apart by shape is not doing its job. The extension
 *  glyph is the plate's own (two overlapping squares) — the same shape the column
 *  header wears, so the tab, the header and the card all agree.
 *
 *  Drawn here rather than cloned from the window's DOM: the window may be minimised,
 *  and a clone would tie the tab's markup to whatever the header happens to hold. */
function dockTickGlyph(w) {
  if (w && w._extName) {
    return '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round">' +
      '<rect x="1.5" y="1.5" width="8" height="8" rx="1.5"/><rect x="6.5" y="6.5" width="8" height="8" rx="1.5"/><path d="M4 14.5v1.5M14.5 4H16"/></svg>';
  }
  return '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><rect x="1.5" y="2.5" width="13" height="9" rx="1.5"/><path d="M5 14.5h6M8 11.5v3"/></svg>';
}

function sessionTabLabel(win) {
  if (win._placeholder) return (win._pendingConnName || t('tab.connecting')) + '…';
  if (win._connName) return win._connName + ' · ' + (win._sid ? stripSessionPrefix(win._sid) : '');
  return win._sid ? stripSessionPrefix(win._sid) : t('tab.session');
}

/** The label of a session that has NO window on this page.
 *
 *  A tab used to exist only for an open window, so every label could be read off
 *  the window (`_connName · shortId`). A session started elsewhere — by an agent
 *  over MCP (`pty`, `session_start`), by a script through POST /api/sessions, or
 *  by a connect whose window this page then closed — has no window to read, so
 *  its label comes from the record. The shape stays the same as a window's, or
 *  the two kinds of tab would be read as two kinds of thing.
 */
function sessionDockLabel(s) {
  if (!s) return t('tab.session');
  var id = stripSessionPrefix(s.id);
  var conn = String(s.ssh_config || '');
  var name = String(s.name || '').trim();
  /* A name the user chose is the identity; the default name IS the profile's,
     which the profile half of the label already shows. */
  if (name && name.indexOf('session-') !== 0 && name !== conn) return name + ' · ' + id;
  return (conn ? conn + ' · ' : '') + id;
}

/** The thing a window-less tab stands for: the session record, shaped like the
 *  window the click would open.
 *
 *  The dock's batch row, a tab's context menu and the review badges all key off a
 *  window's `_sid`/`_parentSid`. Giving each window-less session a stand-in with
 *  the same two fields is what keeps ONE code path for all three instead of a
 *  second branch in each of them (and the second branch is where one of the three
 *  would eventually forget). `_windowless` is the one field that says there is no
 *  DOM node to raise or close. */
function dockSessionTarget(s) {
  return { id: 'sess:' + s.id, _sid: s.id, _parentSid: s.id, _windowless: true };
}

/* Sessions whose window changed while nobody was looking at it.
 *
 *  ONE RULE, and it is about the window: a tab pulses when something happened in it
 *  and its window does not have the reader's attention. Not "an agent opened this",
 *  not "nobody has clicked it yet" — those were separate states answering a question
 *  about the SESSION ("has this page seen it?"), and they had a flaw a reader noticed
 *  immediately: a tab could sit lit forever because no one ever clicked it, which on
 *  a bright accent theme reads as a permanently highlighted tab rather than as
 *  something asking to be looked at.
 *
 *  Attention is defined in dockWindowWatched: on screen, top of its layout, page in
 *  front. Anything less — minimized, buried, collapsed, background tab — is a window
 *  that can print without a pair of eyes on it, and the mark is what says so.
 *
 *  The set is in memory, not in localStorage: a reload is a fresh look. */
var _dockFresh = Object.create(null);

/** dockMarkFresh flags a session as having output nobody has looked at. */
function dockMarkFresh(sid) {
  if (!sid) return;
  if (_dockFresh[sid]) return;
  _dockFresh[sid] = true;
  /* The bar is not rebuilt per output frame — it is rebuilt per sessions frame —
     so the flag has to reach the DOM on its own or the pulse starts on the next
     unrelated repaint, which can be minutes away. The tab is looked up rather than
     repainted: one class is the whole change. */
  var tab = dockTabForSid(sid);
  if (tab) tab.classList.add('is-fresh');
}

/** dockClearFresh drops the mark for a session the human is now looking at. */
function dockClearFresh(sid) {
  if (!sid || !_dockFresh[sid]) return;
  delete _dockFresh[sid];
  var tab = dockTabForSid(sid);
  if (tab) tab.classList.remove('is-fresh');
}

function dockTabForSid(sid) {
  var strip = document.getElementById('session-tabs-scroll');
  if (!strip || !sid) return null;
  var tabs = strip.children;
  for (var i = 0; i < tabs.length; i++) {
    var t = tabs[i];
    if (t && t._entry && t._entry.sid === sid) return t;
  }
  return null;
}

/** Is this window the one the reader is looking at right now?
 *
 *  Watched means: on screen (not minimized, not hidden, not collapsed to its
 *  header) and the page itself is not in a background tab. Anything less is
 *  "somewhere else", and a tab for it has something to report.
 *
 *  THE TWO LAYOUTS ANSWER "ON SCREEN" DIFFERENTLY, and that difference is the
 *  whole point of this predicate:
 *
 *    - FLOATING: windows overlap. Exactly one is legible, so `win-active` (the
 *      raised one) is the right question. A buried window's output is genuinely
 *      news the reader has not seen.
 *
 *    - TILED: the grid exists so that every pane is visible AT THE SAME TIME.
 *      `pane-active` marks the pane that owns the KEYBOARD, not the one the eye
 *      can see — they are different questions and conflating them made a pane
 *      sitting in plain sight report its own output as unseen. Reproduced on a
 *      live instance: a `blackbox` session beside a second pane, both fully
 *      visible in the grid, the second holding the keyboard. One output frame
 *      and the visible `blackbox` tab started pulsing; it pulsed forever,
 *      because the only way to clear the mark was to take the keyboard away
 *      from the pane the operator was typing in. Tiled panes are therefore all
 *      watched, with one exception: a MAXIMIZED pane (`.pane-maxed`) covers its
 *      neighbours, so those neighbours are as buried as any floating window.
 *
 *  Being on screen is measured from the DOM (`offsetParent` / `getClientRects`),
 *  not from a class: a collapsed header, a `display:none` ancestor and a window
 *  moved off-grid all mean the same thing to the reader — the terminal is not
 *  there to look at. */
function dockWindowWatched(win) {
  if (!win || !win.parentNode) return false;
  if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return false;
  if (win.classList.contains('win-minimized') || win.classList.contains('win-hidden')) return false;
  if (win.classList.contains('shell-window-collapsed')) return false;
  if (isTiledWin(win)) {
    /* A maximized pane is the only tiled thing that hides its neighbours. */
    var maxed = document.querySelector('.shell-window.pane-maxed');
    if (maxed && maxed !== win) return false;
    return winHasPixels(win);
  }
  return win.classList.contains('win-active');
}

/** Does this element actually occupy pixels on screen?
 *
 *  `getClientRects()` is empty for a `display:none` ancestor, for a zero-sized
 *  box and for a detached node — one question, one answer, with no reliance on a
 *  class name a future layout change might rename. (`offsetParent` would answer
 *  "hidden" for every `position:fixed` element too, which is a different bug.) */
function winHasPixels(el) {
  if (!el || !el.getClientRects) return false;
  return el.getClientRects().length > 0;
}

/** dockNoteOutput is called for every terminal frame that reached a window.
 *
 *  The two halves have to be one function: "mark unless watched, clear if watched"
 *  is what makes the pulse stop on its own for someone already looking at the window,
 *  without the calling code having to decide either way. Called from the frame path,
 *  so it early-returns on the steady state (already marked, or already watched and
 *  unmarked) — a pulse that is already on costs one property read. */
function dockNoteOutput(win) {
  if (!win || win._extName || !win._sid) return;
  if (dockWindowWatched(win)) dockClearFresh(win._sid);
  else dockMarkFresh(win._sid);
}

/** One dock entry per RUNNING session, plus one per window that has no session.
 *
 *  The dock used to be built from `allShellWins()` alone, so it listed the windows
 *  this page opened and nothing else. Every other way a session can start — an
 *  agent over MCP, a script through POST /api/sessions, a connect whose window was
 *  closed here — produced a live session the page could not see, address or stop,
 *  and a session you cannot see is a session you cannot take back.
 *
 *  So the identity is the SESSION, not the window:
 *
 *    sess:<id>  a running session, with or without a window on this page;
 *    win:<id>   a window that carries no session — a dial in flight, or an
 *               extension's status card.
 *
 *  Keying the window case by its session is what makes a tab survive its window:
 *  closing the window of a running session leaves the same tab in place (now
 *  window-less) instead of dropping one tab and adding another, and a tick placed
 *  on it follows the session rather than the piece of chrome.
 *
 *  Two things are deliberately NOT here. A session that is over belongs to the
 *  archive column, the ledger of finished sessions. A subsystem's own connection
 *  (an extension's `ext:` session) belongs to the extension column — the same rule
 *  the plates apply, so one filter keeps both lists honest.
 *
 *  Order: windows that carry no session first (a dial in flight is the one thing
 *  you just did), then every SESSION by creation time, whether or not this page
 *  holds its window.
 *  The sort matters more than it looks — the snapshot comes from a Go map, so
 *  ListAll order is random, and rendering it as it arrives would reshuffle the tabs
 *  on every frame. A bar that jumps under the pointer is unusable for clicking.
 *  Which is also why the ORDER does not depend on having a window: pressing a
 *  window-less tab opens one, and grouping windows ahead of sessions made the whole
 *  bar shift one place at that moment. The reader presses the third tab and the
 *  first one appears to stop pulsing, because the first place now holds the tab
 *  they just pressed. Position follows the session's creation time, full stop.
 */
function dockEntries() {
  var snapshot = window._lastSessionsSnapshot || [];
  var byId = Object.create(null);
  snapshot.forEach(function (s) { if (s && s.id) byId[s.id] = s; });
  var bare = [];
  var entries = [];
  var covered = Object.create(null);
  /* How many panels each session occupies, counted from the same pass that builds
     the entries: a session split across windows is still ONE tab, and the tab has
     to show that there is more than one place to look (see openSessionPanelsMenu). */
  var panels = Object.create(null);
  allShellWins().forEach(function (w) {
    /* A status card carries the extension's NAME in `_sid`, so it is not a session
       and must not claim one's tab. */
    var sid = (!w._extName && w._sid) ? w._sid : '';
    if (!sid) { bare.push({ key: 'win:' + w.id, win: w, sid: '', session: null }); return; }
    panels[sid] = (panels[sid] || 0) + 1;
    /* ONE TAB PER SESSION. Splitting a session's channels across windows does not
       split the session: each window is just another panel, and the tab raises a
       panel that still has a channel to show. */
    if (covered[sid]) return;
    covered[sid] = true;
    entries.push({ key: 'sess:' + sid, win: getShellWindowBySid(sid) || w, sid: sid, session: byId[sid] || null });
  });
  entries.forEach(function (e) { e.panels = panels[e.sid] || 0; });
  snapshot.forEach(function (s) {
    if (!s || !s.id || s.status !== 'running') return;
    if (covered[s.id]) return;
    /* sessions.js loads AFTER this file, and the page's first paint runs before
       it, so the predicate is asked for by name rather than assumed present. */
    if (typeof isHiddenSession === 'function' && isHiddenSession(s)) return;
    entries.push({ key: 'sess:' + s.id, win: null, sid: s.id, session: s });
  });
  entries.sort(function (a, b) {
    /* A session the snapshot has not reported yet (the window of a dial in flight)
       sorts last rather than first: it is the newest thing on the bar, and it will
       take its place from created_at on the frame after it exists. Sorting it first
       would shift every other tab one place for one frame. */
    var ta = a.session ? (Number(a.session.created_at) || 0) : Infinity;
    var tb = b.session ? (Number(b.session.created_at) || 0) : Infinity;
    if (ta !== tb) return ta < tb ? -1 : 1;
    return String(a.sid) < String(b.sid) ? -1 : 1;
  });
  /* A dial in flight is a WINDOW with no session, so it cannot sort by
     created_at. It used to be prefixed here (bare.concat) — the left end of the
     bar — and the moment the connection landed the entry gained created_at and
     fell to the sorted tail: the tab teleported from first to last on the
     frame the reader was watching for. It is APPENDED instead: the dial is the
     newest thing on the bar, the same slot the session it becomes will hold,
     so connecting moves nothing. (This is also what the sort's own comment
     always said — "it is the newest thing on the bar" — the concat just
     contradicted it.) */
  return entries.concat(bare);
}

/** The dock key of an open window, or '' when the dock has no tab for it.
 *
 *  Two spellings, one definition — and the tick has two entry points (the dock's
 *  own glyph and the window header's monitor glyph). A key spelled twice is a
 *  selection that works from one of them and silently does nothing from the other
 *  (which is how the header glyph lost its tick when the dock started keying by
 *  session).
 */
function dockKeyForWindow(win) {
  if (!win || !win.id) return '';
  if (!win._extName && win._sid) return 'sess:' + win._sid;
  return 'win:' + win.id;
}
/** Rebuild tabs from the dock's entries; the topmost (max z-index) window is active.
 *
 * The dock is permanent: zero entries is a legal state and the bar must stay,
 * because it is the one place the open sessions are listed. The old floating tab
 * bar hid itself when empty, which is exactly the trap this avoids — a control
 * that removes itself cannot be the one you need to see that nothing is open.
 *
 * An entry is a WINDOW or a running session with no window (see dockEntries):
 * the page used to list only the first kind, which made every session an agent
 * started over MCP invisible here. */
function refreshSessionTabbar() {
  /* The one hооk every open, close, raise, minimize, split and merge already runs
     through: the arrangement this tab shows is saved here rather than at each of
     those call sites. Debounced, so a burst of repaints costs one write. */
  savePanelLayoutSoon();
  var bar = sessionTabbarEl();
  if (!bar) return;
  var wins = allShellWins();
  var entries = dockEntries();
  // The empty marker rides the tab strip's place: it says why the row is bare
  // (no windows) without pretending a window exists.
  paintSessdockEmpty(entries.length === 0);
  /* The sweep key answers "is an archived session still on screen", which changes
     for reasons a click handler cannot see: a session dies on its own and its
     window turns read-only, and both of those arrive here. Painting it in the one
     hook every open/close/raise already runs through is what keeps it from
     claiming there is nothing to sweep while a read-only window sits on the page. */
  paintCloseArchivedKey();
  var topWin = null;
  if (!_winsHidden && _layoutMode === 'tile') {
    topWin = (_paneActiveWin && isTiledWin(_paneActiveWin)) ? _paneActiveWin : null;
  } else if (!_winsHidden) {
    var topZ = -1;
    wins.forEach(function (w) {
      if (isTiledWin(w)) return;
      /* A minimized window keeps its inline z-index but is not on screen, so it
         must not win the "which tab is active" test. */
      if (w.classList.contains('win-minimized') || w.classList.contains('win-hidden')) return;
      /* A full-screen window's z-index usually comes from bringShellWindowToFront;
         when it does not (e.g. a resize cleared it), fall back to the stylesheet's
         value. Ties are real at that point — same computed z-index — and the one
         later in the DOM paints on top, so >= picks the visible one. */
      var z = parseInt(w.style.zIndex, 10);
      if (isNaN(z)) z = fullscreenRung();
      if (z >= topZ) { topZ = z; topWin = w; }
    });
  }
  // Reuse existing tab nodes keyed by window id to keep hover state and avoid flicker.
  /* The raised window is the one the eye is in, and that has to be legible from
     the window itself: before this, the only difference between the front window
     and the rest was an inline z-index the stylesheet cannot read. Tiled panes
     keep their own marker (.pane-active, painted by setActivePane), so they are
     left out. Both painters run before the tab list is rebuilt so an early
     return below cannot leave a stale mark on a closed window. */
  wins.forEach(function (w) {
    w.classList.toggle('win-active', w === topWin && !isTiledWin(w));
  });
  /* The grid is rebuilt from scratch on every session frame, so the tile marker
     is re-painted here too: the tab bar refresh is the one hook that every open,
     close and raise already runs through. The Blackwall reads the same windows,
     so it follows — closing the last AI-driven terminal must let the backdrop
     settle. Guarded: this runs once at load, before ui-socket.js is evaluated. */
  if (typeof paintSessionTileWindowMarkers === 'function') paintSessionTileWindowMarkers();
  if (typeof blackwallPaint === 'function') blackwallPaint();
  /* A full-screen window covers the header anyway; the class tells the header to
     step out of the way entirely (base.css) instead of floating over it when one
     of its own menus is open. A window only counts while it is actually on
     screen — a minimised or hidden one leaves the header reachable. */
  var fs = wins.some(function (w) {
    return w.classList.contains('win-fullscreen') &&
      !w.classList.contains('win-minimized') &&
      !w.classList.contains('win-hidden');
  });
  document.body.classList.toggle('has-fullscreen-window', fs);
  var scroll = document.getElementById('session-tabs-scroll');
  if (!scroll) return;
  var validKeys = Object.create(null);
  entries.forEach(function (e) { validKeys[e.key] = true; });
  /* Reuse by KEY: a window-less tab has no window to key on, and a window that
     gains a tab later must land on the tab it already had. Any tab whose entry
     is no longer in the list (a session that died, a window that closed) is
     removed. */
  var byKey = {};
  Array.prototype.slice.call(scroll.children).forEach(function (t) {
    if (t._dockKey && validKeys[t._dockKey] && (t._entry && t._entry.win ? t._entry.win.parentNode : true)) {
      byKey[t._dockKey] = t;
    } else {
      t.remove();
    }
  });
  var order = [];
  entries.forEach(function (entry) {
    var w = entry.win;
    var tab = byKey[entry.key];
    if (!tab) {
      tab = document.createElement('button');
      tab.type = 'button';
      tab.className = 'session-tab';
      tab.setAttribute('role', 'tab');
      tab.innerHTML =
        /* The tick is a MONITOR glyph, and it is the tab's own control rather than a
           mode indicator: pressing it ticks the tab for a batch close while the
           rest of the tab keeps raising the window. That is what lets this bar have
           no selection mode at all — the same arrangement the archive's rows use.
           It is a real button, so it is reachable by keyboard and readable to a
           screen reader; `tabindex` keeps it out of the tab order's way because the
           tab itself is already the focusable item. */
        '<button type="button" class="session-tab-tick" tabindex="-1" title="Select this window" data-i18n-title="dock.tick.title" aria-label="Select this window" data-i18n-aria="dock.tick.title">' +
        dockTickGlyph(w) +
        '</button>' +
        '<span class="session-tab-label"></span>' +
        '<span class="session-tab-review" hidden data-i18n-title="tab.reviewWaiting"></span>' +
        '<span class="session-tab-panels" hidden></span>' +
        '<span class="session-tab-close" role="button" title="Close window (session keeps running)" data-i18n-title="tab.closeWindow" aria-label="Close window" data-i18n-aria="tab.aria.closeWindow">' +
        '<svg viewBox="0 0 12 12" width="10" height="10"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg></span>';
      tab.addEventListener('click', function (e) {
        if (e.target.closest('.session-tab-close')) return;
        /* The monitor glyph is the tick. Anything else on the tab opens its
           window — always, with no mode to check, because the two actions have
           two separate targets now. */
        if (e.target.closest('.session-tab-tick')) {
          if (_dockBatch) _dockBatch.toggleSelection(entry.key, tab);
          return;
        }
        /* The tab owns its window, so no lookup is needed — which is what makes
           this correct for both of the cases a session lookup would get wrong: a
           connecting window has no session id yet (simultaneous dials would both
           resolve to the first null id), and a status card carries the
           extension's NAME in _sid, so resolving it as a session would dial a
           terminal for it. Both are raised directly. A window-less tab is the
           other way round: it has a session and no window, so its click OPENS
           one (through focusSessionWindow, which opens when none exists). */
        var cur = tab._entry;
        /* Pressing the tab is the reader looking at it, before the window even
           exists — so the pulse stops on the press, not on the next frame. */
        if (cur && cur.sid) dockClearFresh(cur.sid);
        tab.classList.remove('is-fresh');
        if (!cur || !cur.win) {
          if (cur && cur.session) {
            focusSessionWindow(cur.session.name || '', cur.session.id, windowPositionFromClick(e), null);
          }
          return;
        }
        /* Spread across panels: the tab is one session with several windows, so the
           press asks WHICH one instead of guessing (see openSessionPanelsMenu). A
           single window keeps the direct raise — a menu for one row is an extra
           press for nothing. */
        if (cur.panels > 1) { openSessionPanelsMenu(cur, tab); return; }
        focusShellWindow(cur.win);
      });
      tab.addEventListener('mousedown', function (e) {
        if (e.button === 1) { // middle-click drops the WINDOW; the session keeps running
          e.preventDefault();
          /* The fast form of the tab's x, and deliberately only that: a window-less
             tab has no window to drop, and turning the same gesture into "stop the
             session" would put an irreversible act behind a middle click. */
          var cur = tab._entry;
          if (cur && cur.win) closeShellWindow(cur.win);
        }
      });
      var x = tab.querySelector('.session-tab-close');
      x.addEventListener('click', function (e) {
        e.preventDefault(); e.stopPropagation();
        closeDockEntry(tab._entry);
      });
      /* A tab is the only place a session is listed now that the session plate is
         gone, so the per-session actions that have no home on the window's own
         header have to be reachable from here: the approval gate and the copy of
         the session URL. They go behind a context menu rather than onto the tab's
         face — a tab is 28px tall and already carries a label, a review badge and
         a close key, and a lock glyph among them would be a fourth unlabelled icon
         in a row of them.

         Rename is NOT here any more: it lives on the window's own title (see
         bindWindowTitleRename), because a right-click has no meaning on a phone —
         and on a phone the dock is hidden entirely, so this menu does not exist
         there at all. The menu keeps the verb for a session with no window open,
         whose title there is nothing to click. */
      tab.addEventListener('contextmenu', function (e) {
        e.preventDefault();
        openSessionTabMenu(tab._entry, e.clientX, e.clientY);
      });
    }
    tab._entry = entry;
    tab._dockKey = entry.key;
    tab._win = w || null;
    /* The label and the tooltip are the SESSION's, not the window's: a tab for a
       session started elsewhere has no window to read, and the same session must
       read the same whether or not this page happens to hold its window.
       Guarded writes: this refresh runs several times per arrangement click
       (directly, and again through the pane/container MutationObservers), and
       assigning textContent unconditionally tears the text node out and puts a
       new one back — a strip-wide replace the eye reads as a flicker even with
       no new output. */
    var label = entry.session ? sessionDockLabel(entry.session) : sessionTabLabel(w);
    var labelEl = tab.querySelector('.session-tab-label');
    if (labelEl && labelEl.textContent !== label) labelEl.textContent = label;
    if (tab.title !== label) tab.title = label;
    /* "Active" is about the SESSION, not about which of its panels happens to be
       on top: a session split across panels is one tab, and looking at any of
       them is looking at that session. dockKeyForWindow answers that — every panel
       is keyed by the session it shows. */
    var tabActive = !!w && !!topWin && dockKeyForWindow(topWin) === entry.key;
    /* classList.toggle with a force argument still writes the class attribute when
       the presence does not change, and this loop runs several times per
       arrangement click. Reading first keeps an unchanged tab byte-identical, so
       the strip is not style-invalidated for news it does not carry. */
    if (tab.classList.contains('active') !== tabActive) tab.classList.toggle('active', tabActive);
    var selVal = tabActive ? 'true' : 'false';
    if (tab.getAttribute('aria-selected') !== selVal) tab.setAttribute('aria-selected', selVal);
    /* A window-less entry is still a session with a state, so the tab says which:
       no window here is a fact about the page, not about the session. */
    if (tab.classList.contains('is-windowless') !== !w) tab.classList.toggle('is-windowless', !w);
    /* More than one panel: say so, because the press will open a menu instead of
       raising a window — a tab that asks a question with no sign it will is a
       surprise. Hidden on the ordinary one-window session. */
    var panelsEl = tab.querySelector('.session-tab-panels');
    if (panelsEl) {
      var panelCount = entry.panels || 0;
      var panelsHidden = panelCount < 2;
      if (panelsEl.hidden !== panelsHidden) panelsEl.hidden = panelsHidden;
      if (panelCount > 1) {
        var countText = String(panelCount);
        if (panelsEl.textContent !== countText) panelsEl.textContent = countText;
        /* Rendered from t() at paint time, like the label: the tooltip follows a
           language switch on the next rebuild instead of needing its own hook. */
        var countTip = t('dock.panels.tip');
        if (panelsEl.title !== countTip) panelsEl.title = countTip;
      }
    }
    /* The rebuild reads the fresh set rather than trusting the class it left behind:
       this loop is the one place a tab is created, and a flag set by an output frame
       while the tab did not exist yet (a session that printed before its first
       sessions frame) has no class to inherit.
       The set is CLEARED here rather than only at the click: raising, restoring or
       un-minimizing a window is also the reader looking at it, and every one of those
       paths repaints this bar. Deleting the flag (instead of calling
       dockClearFresh) keeps it out of the strip while the strip is being rebuilt. */
    if (entry.sid && w && dockWindowWatched(w)) delete _dockFresh[entry.sid];
    /* NO WINDOW HERE IS THE LOUDEST CASE OF "NOBODY IS WATCHING": the session is
       running and there is nothing on this page showing it. Marking only on frames
       missed it entirely — frames reach windows, and a session started over MCP has
       none, so the tabs that most needed the pulse were the only ones that never got
       it. The mark is set here rather than at creation for the same reason the rest
       of the rule lives in this loop: the snapshot is the one thing every session
       passes through. */
    if (entry.sid && !w) _dockFresh[entry.sid] = true;
    var tabFresh = !!(entry.sid && _dockFresh[entry.sid]);
    if (tab.classList.contains('is-fresh') !== tabFresh) tab.classList.toggle('is-fresh', tabFresh);
    if (!w) {
      if (tab.getAttribute('data-sid') !== entry.sid) tab.setAttribute('data-sid', entry.sid);
    } else if (tab.getAttribute('data-sid') !== null) {
      tab.removeAttribute('data-sid');
    }
    /* The batch component owns both the mode and the ticked set, and paints the
       ticked class over the grid it was handed (this strip). All this loop does is
       expose the key the component indexes by and show the empty tick slot while
       the mode is on — the slot itself must exist in the DOM at all times, or the
       tab's width would change under the pointer the moment selection starts. */
    if (tab.getAttribute('data-win-id') !== entry.key) tab.setAttribute('data-win-id', entry.key);
    /* The tick is always visible on every tab: there is no mode to reveal it in,
       and a glyph that appeared and disappeared would change the tab's width under
       the pointer whenever a selection started or ended. */
    order.push(tab);
  });
  /* Reorder ONLY when the strip differs. appendChild on a node that is already
     in the DOM removes and re-inserts it, so the previous unconditional pass
     churned every tab on every refresh — and this refresh runs SIX times per
     arrangement click (directly, plus the pane-grid, container and visibility
     observers), which is why the strip flickered on layout changes even when
     no terminal produced output. With the guard, an arrangement click that
     does not add, remove or reorder tabs performs no strip writes at all;
     a real change still lands because the loop above creates/moves nodes
     before this comparison runs. */
  var kids = scroll.children;
  var stripDiffers = kids.length !== order.length;
  if (!stripDiffers) {
    for (var i = 0; i < order.length; i++) {
      if (kids[i] !== order[i]) { stripDiffers = true; break; }
    }
  }
  if (stripDiffers) order.forEach(function (t) { scroll.appendChild(t); });
  /* The bar's keys read the list that was just rendered, so they are refreshed
     here rather than on a timer: a count painted from a previous frame is exactly
     the bug this ordering prevents. */
  if (_dockBatch) _dockBatch.update();
  // A minimized window shows nothing but its tab, so the tab is where its queue
  // has to be visible. This runs at load time too (this module loads before
  // approval.js), hence the guard: the counts arrive with the first queue read.
  if (typeof applyApprovalCounts === 'function') applyApprovalCounts();
}

/** The session behind a dock entry: the record for a window-less tab, or the
 *  last snapshot's copy for a window.
 *
 *  Read through the snapshot rather than off the window for the same reason
 *  sessionForTabWindow does it: a window's `_sid` is not always a session id, so
 *  a match is what proves there is a session to rename, gate or stop. */
function dockEntrySession(entry) {
  if (!entry) return null;
  if (entry.win) return sessionForTabWindow(entry.win);
  /* A window-less entry was built FROM the snapshot, so its record is the one
     already in hand; a lookup would just find the same object again. */
  return entry.session || null;
}

/** The tab's x: stop the SESSION, not one of its panels.
 *
 *  The dock lists SESSIONS — a session split across panels is still one tab (it
 *  only reports how many panels it has). So its x is a session-level key, and it
 *  must ask the session-level question and, on yes, take the whole session down at
 *  once. It used to delegate to the window-level close, which is panel-aware: on a
 *  split session that closed one panel silently and left the reader to press it
 *  again for each of the others, so "close this session" turned into an unclear
 *  number of clicks, each one looking like it had done nothing.
 *
 *  The window header's own x keeps the panel-level meaning (see
 *  closeSessionWindowFromUI): there the control sits ON a panel, so "this panel" is
 *  what it can honestly mean. */
function closeDockEntry(entry) {
  if (!entry) return Promise.resolve('cancelled');
  var s = dockEntrySession(entry);
  if (entry.win) {
    /* A status card and a dial in flight have no session: askCloseSession drops
       the window for both, which is all there is to do. */
    return askCloseSession(entry.win._extName ? null : entry.sid, entry.win);
  }
  if (!s) return Promise.resolve('cancelled');
  return askCloseSession(s.id, dockSessionTarget(s));
}

/** Close every panel showing a session.
 *
 *  Stopping a session removes what all of its panels were views OF, so they go
 *  together: leaving the others on screen would show transcripts of a session that
 *  is no longer there, and the reader would have to close each one by hand. */
function closeSessionPanels(sid) {
  sessionPanelWindows(sid).forEach(function (w) {
    /* The session-level terminate already stopped every channel of this session.
       These windows are losing a VIEW, not closing a shell, so drop each channel
       view directly: `closeChannelTab` would post a per-shell archive for shells
       the session teardown has already handled. Dropping the view also detaches
       the channel's disposable, so releasing the window cannot re-run it. */
    Object.keys(w._channels || {}).forEach(function (chSid) { _dropChannelView(w, chSid); });
    closeShellWindow(w);
  });
}

/** The session data a tab's window stands for, or null when there is none yet.
 *
 *  A tab's window carries `_sid` for a real session, but a connecting window has
 *  no id yet and a status card carries the extension's NAME in the same slot. So
 *  the id is looked up in the last snapshot rather than trusted: only a session
 *  the server reported has anything to rename, gate or copy. */
function sessionForTabWindow(w) {
  if (!w) return null;
  var sid = w._sid;
  if (!sid) return null;
  var snap = window._lastSessionsSnapshot || [];
  for (var i = 0; i < snap.length; i++) {
    if (snap[i] && snap[i].id === sid) return snap[i];
  }
  return null;
}

/** Rename a session by id.
 *
 *  Extracted from the card's name-click handler so the tab menu and a card (if
 *  one ever comes back) cannot drift: the prompt, the empty-name guard, the
 *  PATCH and the optimistic local rename are one implementation.
 */
function renameSessionById(s) {
  if (!s || !s.id) return;
  var cur = String((s.name || '').trim());
  if (cur.indexOf('session-') === 0) cur = '';
  var input = prompt(t('session.prompt.rename'), cur);
  if (input === null) return;
  var newName = input.trim();
  if (!newName || (cur && newName === cur)) { showCopyToast(t('session.toast.unchanged')); return; }
  fetch(apiPath('/api/sessions/') + encodeURIComponent(s.id), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: newName })
  })
    .then(function (r) {
      if (!r.ok) return r.json().then(function (er) { throw new Error((er && er.error) || 'HTTP ' + r.status); });
      showCopyToast(t('session.toast.renamed'));
      // Local optimistic refresh; the server broadcast reconciles shortly.
      var snap = (window._lastSessionsSnapshot || []).slice();
      for (var i = 0; i < snap.length; i++) {
        if (snap[i] && snap[i].id === s.id) { snap[i].name = newName; }
      }
      applySessionsSnapshot(snap);
    })
    .catch(function (err) { showCopyToast(t('toast.rename.failed', { msg: (err.message || err) })); });
}

/** Copy a session's URL. */
function copySessionUrl(s) {
  if (!s || !s.id) return;
  copyTextToClipboard(resourceUrlSession(s.id))
    .then(function () { showCopyToast(); })
    .catch(function () { showCopyToast(t('toast.copy.failed')); });
}

/** Make a terminal window's own title the rename control.
 *
 *  The session plate's card had "click the name to rename". When the plate went,
 *  the rename survived only as an item in the dock tab's RIGHT-CLICK menu — a
 *  gesture with no meaning on a phone, and one nothing on the page announces. The
 *  window's title bar is where a name is read, and it is already on screen on
 *  every viewport, so that is where the control belongs.
 *
 *  The name shown is the session's, not the window's `_connName`: a window is
 *  opened from a label, but the session may have been renamed since — here or by
 *  an agent — so the snapshot is what the title reads (paintWindowTitleName). A
 *  window whose session is not in the snapshot yet (a dial in progress, an
 *  extension's status card) gets no control at all rather than a prompt that would
 *  PATCH an id that cannot resolve. */
function bindWindowTitleRename(win) {
  if (!win || win._termcpTitleRenameBound) return;
  var el = win.querySelector('.shell-window-title .sw-conn');
  if (!el) return;
  win._termcpTitleRenameBound = true;
  /* The label gets its own span so a repaint can rewrite the text without
     removing the pencil that sits beside it. */
  var nameEl = document.createElement('span');
  nameEl.className = 'sw-conn-name';
  nameEl.textContent = el.textContent;
  el.textContent = '';
  el.appendChild(nameEl);
  /* The pencil is the affordance: a bare name that renames on click is the same
     hidden door the context menu was. It is a span, not a button, because the
     header's drag handler skips `button` and the name has to keep dragging the
     window. */
  var pencil = document.createElement('span');
  pencil.className = 'sw-conn-pencil';
  pencil.setAttribute('aria-hidden', 'true');
  pencil.innerHTML = '<svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M11.5 2.5l2 2L5 13l-3 1 1-3z"/></svg>';
  el.appendChild(pencil);
  el.setAttribute('role', 'button');
  el.setAttribute('tabindex', '0');
  /* Declarative, not a t() call: the label is read here but rewritten on every
     language switch by applyI18n, which is the hook every other control on this
     header uses. A t() call would freeze it in the language the window was opened
     in. */
  el.setAttribute('data-i18n-title', 'session.aria.rename');
  el.setAttribute('data-i18n-aria', 'session.aria.rename');
  applyI18n(el);
  /* The header drag starts on mousedown, so the press must not reach it. The
     dblclick guard is separate on purpose: the header's own dblclick maximizes,
     and stopping the CLICK does not stop the dblclick that follows it — a double
     tap on the name would full-screen the window and prompt twice. */
  el.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  el.addEventListener('dblclick', function (e) { e.stopPropagation(); });
  var rename = function (e) {
    e.preventDefault();
    e.stopPropagation();
    var s = sessionForTabWindow(win);
    if (!s) return;
    renameSessionById(s);
  };
  el.addEventListener('click', rename);
  el.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    rename(e);
  });
  /* A window that already has its session in the snapshot paints now rather than
     waiting for the next frame — otherwise a restored window shows its dial label
     until something unrelated emits a session frame. */
  paintWindowTitleName(win, sessionForTabWindow(win));
}

/** The name a window's title shows: the session's own, else the profile it dialled.
 *
 *  A default name (`session-…`) IS the profile's, so the profile half already says
 *  it — the same rule sessionDockLabel applies, so a window and its tab cannot
 *  disagree about what the session is called. Neither of those may leave the title
 *  empty: a session with no profile (an internal one, or one whose profile was
 *  deleted) still shows its default name, and a window with no snapshot record
 *  keeps the label it was opened with. */
function windowTitleName(s, win) {
  var name = String((s && s.name) || '').trim();
  if (name && name.indexOf('session-') !== 0) return name;
  var conn = String((s && s.ssh_config) || '').trim();
  if (conn) return conn;
  if (name) return name;
  return String((win && win._connName) || '');
}

/** Repaint a window's title name from the session snapshot. Called from the
 *  session frame, so a rename made anywhere (this title, a tab's menu, an agent)
 *  lands on the window itself rather than only on its tab. */
function paintWindowTitleName(win, s) {
  if (!win || !s) return;
  var el = win.querySelector('.sw-conn-name');
  if (el) el.textContent = windowTitleName(s, win);
}

/** The per-session menu behind a tab's right-click.
 *
 *  A tab is the only place a session is listed now, so this is the only way to
 *  reach the things that are not "switch to it": the approval gate, the rename,
 *  and the session URL. A context menu rather than icons on the tab's face — the
 *  tab is 28px of label, badge and close key, and a fourth unlabelled glyph in
 *  that row would be a mystery button.
 *
 *  It takes a dock ENTRY rather than a window: a session started by an agent has
 *  no window here, and that is exactly the case where the menu cannot be replaced
 *  by "open it and use the other one".
 *
 *  One menu element, built on demand and reused: a per-tab menu would be one
 *  stale closure per window, and this page opens and closes windows all day. */
/** Every open window showing this session, in the workspace's own order.
 *
 *  Panels are peers. The order is not a home-first policy: it is simply the order
 *  the windows occupy across the floating container and tiled grid, so splitting
 *  or merging does not give one window special identity. */
function sessionPanelWindows(sid) {
  if (!sid) return [];
  return allShellWins().filter(function (w) { return !w._extName && w._sid === sid; });
}

/* --- the workspace this tab had on screen ----------------------------------
 *
 * Which channel sits in which panel is not the server's to know: a session owns
 * its shells, not the arrangement a reader put them in. So a reload rebuilt one
 * window per session from the shell list alone, and a session that had been split
 * across two panels came back merged — the split was silently undone.
 *
 * The arrangement is kept in sessionStorage, which is the one store that belongs to
 * a single TAB. The daemon is a shared instance: another browser, another tab and an
 * agent can all be attached at once, and a layout stored on the server would be one
 * record all of them overwrite, so the second tab's reload would rearrange the
 * first tab's screen. A tab's screen is the tab's business, and it dies with the tab
 * — which is also when the arrangement stops meaning anything.
 *
 * Only the GROUPING is stored, never content: session id, which shells a panel
 * showed, which one was in front, and where the panel sat. Everything else is
 * re-read from the server on load, and a shell that no longer exists is left out.
 */
var PANEL_LAYOUT_KEY = 'termcp_panel_layout';
var _panelLayoutSaveTimer = 0;
/** Saves are gated on restore: the first repaints of a fresh page have no windows
 *  yet, and writing while async shell lists are arriving would erase panels not
 *  rebuilt yet. */
var _panelLayoutRestored = false;
var _panelLayoutRestoring = false;

/** Remember the open panels. Debounced, because the callers are hot paths (every
 *  tab-bar repaint is an open, a close, a raise or a frame). */
function savePanelLayoutSoon() {
  if (!_panelLayoutRestored || typeof setTimeout !== 'function') return;
  if (_panelLayoutSaveTimer) clearTimeout(_panelLayoutSaveTimer);
  _panelLayoutSaveTimer = setTimeout(function () {
    _panelLayoutSaveTimer = 0;
    savePanelLayout();
  }, 250);
}

function savePanelLayout() {
  if (!_panelLayoutRestored || _panelLayoutRestoring) return;
  var panels = [];
  allShellWins().forEach(function (win) {
    /* A dial in flight and an extension's status card are not session panels:
       neither has a session id and shells to name. */
    if (!win._parentSid || win._placeholder || win._extName) return;
    var shells = Object.keys(win._channels || {});
    if (!shells.length) return;
    var rect = isTiledWin(win) ? win._floatRect : win.style;
    function px(value) { var n = parseFloat(value); return isFinite(n) ? n : null; }
    panels.push({
      sid: win._parentSid,
      conn: win._connName || '',
      shells: shells,
      active: win._activeChannelSid || '',
      min: win.classList.contains('win-minimized'),
      rect: rect ? { left: px(rect.left), top: px(rect.top), width: px(rect.width), height: px(rect.height) } : null
    });
  });
  try { sessionStorage.setItem(PANEL_LAYOUT_KEY, JSON.stringify({ v: 1, panels: panels })); } catch (e) {}
}

function loadPanelLayout() {
  var raw = '';
  try { raw = sessionStorage.getItem(PANEL_LAYOUT_KEY) || ''; } catch (e) { return []; }
  if (!raw) return [];
  var doc = null;
  try { doc = JSON.parse(raw); } catch (e) { return []; }
  if (!doc || doc.v !== 1 || !Array.isArray(doc.panels)) return [];
  return doc.panels;
}

/** Put a floating window back where it was. A tiled pane is laid out by the grid
 *  and a phone's window by the stylesheet, so both keep their own geometry. */
function applySavedPanelRect(win, rect) {
  if (!win || !rect || isMobileViewport()) return;
  var target = isTiledWin(win) ? (win._floatRect || (win._floatRect = {})) : win.style;
  ['left', 'top', 'width', 'height'].forEach(function (key) {
    if (typeof rect[key] === 'number' && isFinite(rect[key])) target[key] = Math.round(rect[key]) + 'px';
  });
  if (!isTiledWin(win)) clampShellWindowIntoContainer(win);
}

/** Rebuild the panels this tab had open before the reload.
 *
 *  Called with the session snapshot, because a saved panel is only worth rebuilding
 *  if its session still exists — and the panel's shells are then read from the
 *  server, so a channel closed elsewhere leaves nothing behind and a panel left with
 *  no shells is not reopened. Runs once per page load. */
function restorePanelLayout(sessionById) {
  if (_panelLayoutRestored) return;
  var saved = loadPanelLayout().filter(function (p) {
    return p && typeof p.sid === 'string' && Object.prototype.hasOwnProperty.call(sessionById, p.sid) &&
      Array.isArray(p.shells) && p.shells.some(function (id) { return typeof id === 'string' && id; });
  }).map(function (p) {
    return {
      sid: p.sid,
      conn: typeof p.conn === 'string' ? p.conn : '',
      shells: p.shells.filter(function (id) { return typeof id === 'string' && id; }),
      active: typeof p.active === 'string' ? p.active : '',
      min: p.min === true,
      rect: p.rect
    };
  });
  /* An empty record is a real answer: the reader closed everything, and a reload
     must not bring it back. */
  _panelLayoutRestored = true;
  if (!saved.length) { savePanelLayoutSoon(); return; }
  _panelLayoutRestoring = true;
  /* Fetch concurrently, then build in the saved order. Completion order is not
     window order: letting whichever HTTP response wins append first would make the
     workspace reshuffle across reloads. */
  Promise.all(saved.map(function (p) {
    return fetch(sessionAPI(p.sid, '/shells'))
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) { return j && j.shells ? { panel: p, session: sessionById[p.sid], shells: j.shells } : null; })
      .catch(function () { return null; });
  })).then(function (results) {
    var assigned = Object.create(null), restoredSessions = Object.create(null);
    results.forEach(function (result) {
      if (!result) return;
      var p = result.panel, s = result.session;
      /* If the reader opened this session manually while the shell lists loaded,
         their new window wins over the stale saved layout. Further panels from the
         same saved arrangement are allowed after its first panel is rebuilt. */
      if (!restoredSessions[p.sid] && sessionPanelWindows(p.sid).length) return;
      var live = {};
      result.shells.forEach(function (sh) { live[sh.shell_id || sh.id] = sh; });
      var wanted = p.shells.filter(function (id) {
        if (!live[id] || assigned[id]) return false;
        assigned[id] = true;
        return true;
      });
      if (!wanted.length) return;
      var head = live[wanted[0]];
      var win = openShellWindow(p.conn || s.ssh_config || s.name || '', p.sid, null, {
        shell: wanted[0],
        index: head.index || 0,
        readOnly: s.status !== 'running',
        channelReadOnly: head.status !== 'running'
      });
      if (!win) return;
      restoredSessions[p.sid] = true;
      for (var i = 1; i < wanted.length; i++) {
        createChannelTab(win, wanted[i], {
          index: live[wanted[i]].index || 0,
          readOnlyHistory: live[wanted[i]].status !== 'running'
        });
      }
      if (p.active && win._channels[p.active]) switchChannelTab(win, p.active);
      applySavedPanelRect(win, p.rect);
      if (p.min) minimizeShellWindow(win);
    });
    _panelLayoutRestoring = false;
    savePanelLayoutSoon();
  });
}

/** The channels a panel shows, as the labels their tabs carry.
 *
 *  A panel is identified by what is IN it: two windows of one session have the
 *  same title (`profile · session`), so a list of titles would be a list of
 *  identical rows. The channel labels are what differs — they are also what the
 *  tabs in the panel say, so the menu reads as a summary of the window. */
function panelChannelLabel(win) {
  var ids = win && win._channels ? Object.keys(win._channels) : [];
  if (!ids.length) return t('channel.empty');
  var indexes = ids.map(function (sid) {
    var ch = win._channels[sid];
    return ch && ch.urlIndex ? ch.urlIndex : 0;
  });
  indexes.sort(function (a, b) { return a - b; });
  return indexes.map(function (n) { return n ? 'shell-' + n : 'shell'; }).join(' · ');
}

/** The dock tab of a session spread across several panels: pick which one.
 *
 *  A session can be split — a channel tab dragged out gets a panel of its own —
 *  and it keeps ONE dock tab, because it is one session. Raising a window then
 *  has an answer the page cannot guess: the reader dragged a channel out to look
 *  at it separately, and pressing the tab is how they say "take me back there".
 *
 *  So the press opens a menu UPWARDS from the tab (the dock is the bottom edge of
 *  the page) with one row per panel, named by the channels it holds. One panel
 *  keeps the old behaviour and raises the window directly: a menu for a single
 *  choice is an extra press for nothing. */
function openSessionPanelsMenu(entry, tabEl) {
  var sid = entry && entry.sid;
  if (!sid) return;
  var open = document.getElementById('sess-tab-menu');
  if (open && open.dataset.sid === sid) { closeSessionTabMenu(); return; } // second press toggles
  closeSessionTabMenu();

  var panels = sessionPanelWindows(sid);
  if (panels.length < 2) return;
  var current = topVisibleShellWindow();

  var menu = document.createElement('div');
  menu.className = 'sess-tab-menu sess-panels-menu';
  menu.id = 'sess-tab-menu';
  menu.setAttribute('role', 'menu');
  menu.dataset.sid = sid;
  menu.dataset.win = entry.key;

  panels.forEach(function (w) {
    var isCurrent = w === current;
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'sess-tab-menu-item' + (isCurrent ? ' is-on' : '');
    b.setAttribute('role', 'menuitem');
    b.setAttribute('aria-current', isCurrent ? 'true' : 'false');
    b.innerHTML = dockTickGlyph(w) + '<span>' + escapeHtml(panelChannelLabel(w)) + '</span>' +
      (isCurrent ? '<span class="sess-tab-menu-dot" aria-hidden="true"></span>' : '');
    b.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      closeSessionTabMenu();
      if (entry.sid) dockClearFresh(entry.sid);
      focusShellWindow(w);
    });
    menu.appendChild(b);
  });

  document.body.appendChild(menu);
  /* Fixed, measured after mounting, and opening UPWARDS: the anchor is a tab in the
     dock's bottom bar, so there is no room below it by definition. */
  var r = menu.getBoundingClientRect();
  var vw = window.innerWidth || 0;
  var tab = tabEl && tabEl.getBoundingClientRect ? tabEl.getBoundingClientRect() : null;
  var left = tab ? tab.left : 8;
  if (left + r.width > vw - 8) left = Math.max(8, vw - r.width - 8);
  var top = tab ? tab.top - r.height - 6 : 8;
  if (top < 8) top = Math.min(8 + (window.innerHeight || 0) - r.height - 8, (tab ? tab.bottom + 6 : 8));
  menu.style.left = left + 'px';
  menu.style.top = Math.max(8, top) + 'px';
  var first = menu.querySelector('.sess-tab-menu-item');
  if (first) first.focus();
}

function openSessionTabMenu(entry, x, y) {
  var w = entry && entry.win;
  var s = dockEntrySession(entry);
  closeSessionTabMenu();
  /* A window with no session behind it (a dial in progress, a status card) has
     nothing here, so the menu is not offered at all rather than shown empty. */
  if (!s) return;
  var menu = document.createElement('div');
  menu.className = 'sess-tab-menu';
  menu.id = 'sess-tab-menu';
  menu.setAttribute('role', 'menu');
  menu.dataset.win = entry.key;

  var gated = !!s.approval_mode;
  var items = [
    {
      key: 'lock',
      icon: gated ? SVG_LOCK_CLOSED : SVG_LOCK_OPEN,
      label: t('review.mode'),
      /* Two literal calls rather than one ternary argument: the i18n scanner reads
         t('key'), so a key that only appears inside a conditional looks unused —
         and "looks unused" is how a live string gets deleted as an orphan. */
      hint: gated ? t('review.disableTip') : t('review.enableTip'),
      on: gated,
      run: function () {
        /* The card passed the pressed state it had painted; the menu reads it
           from the snapshot instead, so a gate flipped elsewhere is reflected. */
        toggleSessionApproval(s, gated, null);
      }
    },
    {
      key: 'rename',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M11.5 2.5l2 2L5 13l-3 1 1-3z"/></svg>',
      label: t('session.aria.rename'),
      run: function () { renameSessionById(s); }
    },
    {
      key: 'copy',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="5.5" y="5.5" width="9" height="9" rx="1"/><path d="M10.5 3.5v-1a1 1 0 00-1-1h-7a1 1 0 00-1 1v7a1 1 0 001 1h1"/></svg>',
      label: t('session.aria.copyUrl'),
      run: function () { copySessionUrl(s); }
    }
  ];

  items.forEach(function (it) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'sess-tab-menu-item' + (it.on ? ' is-on' : '');
    b.setAttribute('role', 'menuitem');
    b.setAttribute('aria-pressed', it.on ? 'true' : 'false');
    if (it.hint) b.title = it.hint;
    b.innerHTML = it.icon + '<span>' + escapeHtml(it.label) + '</span>' +
      (it.on ? '<span class="sess-tab-menu-dot" aria-hidden="true"></span>' : '');
    b.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      closeSessionTabMenu();
      it.run();
    });
    menu.appendChild(b);
  });

  document.body.appendChild(menu);
  /* Placed after it is in the DOM so the menu has a measured size to fit: near
     the window's bottom edge it opens upwards instead of off-screen. */
  var r = menu.getBoundingClientRect();
  var vw = window.innerWidth || 0;
  var vh = window.innerHeight || 0;
  var left = x, top = y;
  if (left + r.width > vw - 8) left = Math.max(8, vw - r.width - 8);
  if (top + r.height > vh - 8) top = Math.max(8, vh - r.height - 8);
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';
  var first = menu.querySelector('.sess-tab-menu-item');
  if (first) first.focus();
}

function closeSessionTabMenu() {
  var old = document.getElementById('sess-tab-menu');
  if (old) old.remove();
}

/* Dismissal: a click anywhere, Escape, or a resize (the anchor is a moving tab,
   so a menu that survived a resize would sit over the wrong window). Registered
   once, on the document, rather than once per menu. */
document.addEventListener('mousedown', function (e) {
  if (e.target.closest && e.target.closest('.sess-tab-menu')) return;
  /* The tab that owns the open panels menu is not "anywhere": a second press on it
     toggles the menu shut (see openSessionPanelsMenu), and closing on the mousedown
     would leave nothing for the click to toggle. */
  var menu = document.getElementById('sess-tab-menu');
  var tab = e.target.closest ? e.target.closest('.session-tab') : null;
  if (menu && menu.dataset.sid && tab && tab._entry && tab._entry.sid === menu.dataset.sid) return;
  closeSessionTabMenu();
}, true);
document.addEventListener('keydown', function (e) {
  if (e.key === 'Escape') closeSessionTabMenu();
});
window.addEventListener('resize', closeSessionTabMenu);

// Keep the tab bar in sync as shell windows are opened/closed anywhere.
//
// The session plate is repainted from the same signal, and that is not cosmetic:
// a dial in progress exists as a placeholder window and nothing else, so the
// window appearing or disappearing is the ONLY event that can add or remove its
// connecting card. Watching the container is what makes the card show up when
// the dial starts and clear when it ends — without this the plate is only
// repainted by session frames, which a still-connecting dial never produces.
(function () {
  var c = shellWindowsEl();
  if (!c || typeof MutationObserver === 'undefined') return;
  new MutationObserver(function () {
    refreshSessionTabbar();
    if (typeof renderSessionGrid === 'function') renderSessionGrid('');
  }).observe(c, { childList: true });
})();

// Fixed right-side window controls, separated from the dock strip by its border:
// eye (show/hide) · layout toggle (float<->tile, highlighted while tiled) · strategy select.
(function () {
  var actions = document.getElementById('session-tabbar-actions');
  if (!actions) return;
  var dock = document.getElementById('sessdock-bar');
  if (!document.getElementById('dock-close-archived') && dock) {
    var broom = document.createElement('button');
    broom.type = 'button';
    broom.id = 'dock-close-archived';
    broom.className = 'ext-key ext-key-icon';
    broom.setAttribute('data-i18n-title', 'dock.closeArchived.title');
    broom.setAttribute('data-i18n-aria', 'dock.closeArchived.title');
    broom.title = t('dock.closeArchived.title');
    broom.setAttribute('aria-label', t('dock.closeArchived.title'));
    broom.disabled = true;
    /* The archive's own glyph — the stratified stack — not a broom. This key
       closes the archived windows that are OPEN, which is the same object the
       archive column draws, so it speaks the column's vocabulary instead of
       introducing a cleaning metaphor the page uses nowhere else. */
    broom.innerHTML = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3.2 12.8h9.6v2H3.2z"/><path d="M4.4 8.6h7.2v4.2H4.4z"/><path d="M5.6 4.4h4.8v4.2H5.6z"/><path d="M6.8 1.2h2.4v3.2H6.8z"/></svg>';
    broom.addEventListener('click', function (e) { e.preventDefault(); closeArchivedWindows(); });
    /* The broom belongs to the dock strip, not the arrangement controls. Keep it
       immediately before their vertical divider: the space it cleans up is in the
       strip, and the separator marks where layout controls begin. */
    dock.insertBefore(broom, actions);
    paintCloseArchivedKey();
  }
  if (!document.getElementById('session-tabbar-toggle')) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.id = 'session-tabbar-toggle';
    btn.className = 'session-tabbar-toggle';
    btn.addEventListener('click', function () { toggleAllWindowsVisible(); });
    actions.appendChild(btn);
    updateSessionTabbarToggle();
  }
  if (!document.getElementById('session-tabbar-tile')) {
    var tile = document.createElement('button');
    tile.type = 'button';
    tile.id = 'session-tabbar-tile';
    tile.className = 'session-tabbar-tile';
    tile.addEventListener('click', function () { setLayoutMode(_layoutMode === 'tile' ? 'float' : 'tile'); });
    actions.appendChild(tile);
    updateTileToggleButton();
  }
  /* The arrangement key is a menu, not a <select>. A native select drops the
     operating system's own list — a white slab with system chrome — into a bar
     built from the terminal's palette, and it looks like nothing else in the
     page. It also cannot show which choice is current while closed without a
     label wide enough for the widest one, and the bar has no width to spare.
     So the trigger shows the current choice and the list is markup, drawn with
     the same panel and row rules as the header's language menu (.lang-menu /
     .lang-opt); only the trigger is shaped like the keys beside it. */
  if (!document.getElementById('session-tabbar-strategy')) {
    var wrap = document.createElement('span');
    wrap.className = 'session-tabbar-strategy';
    var trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.id = 'session-tabbar-strategy';
    trigger.className = 'session-tabbar-select';
    trigger.setAttribute('aria-haspopup', 'true');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('data-i18n-title', 'pane.strategy.title');
    var label = document.createElement('span');
    label.id = 'session-tabbar-strategy-label';
    var caret = document.createElement('span');
    caret.className = 'session-tabbar-caret';
    caret.setAttribute('aria-hidden', 'true');
    trigger.appendChild(label);
    trigger.appendChild(caret);

    var menu = document.createElement('span');
    menu.className = 'lang-menu session-tabbar-menu hidden';
    menu.setAttribute('role', 'menu');
    menu.innerHTML =
      '<button type="button" class="lang-opt" role="menuitemradio" data-strategy="auto" data-i18n="pane.strategy.auto">Auto</button>' +
      '<button type="button" class="lang-opt" role="menuitemradio" data-strategy="cols" data-i18n="pane.strategy.cols">Columns</button>' +
      '<button type="button" class="lang-opt" role="menuitemradio" data-strategy="rows" data-i18n="pane.strategy.rows">Rows</button>' +
      '<button type="button" class="lang-opt" role="menuitemradio" data-strategy="pairs" data-i18n="pane.strategy.pairs">2 cols</button>';
    var opts = menu.querySelectorAll('.lang-opt');
    for (var i = 0; i < opts.length; i++) {
      opts[i].addEventListener('click', function () {
        choosePaneStrategy(this.getAttribute('data-strategy'));
      });
    }
    if (typeof applyI18n === 'function') applyI18n(menu);
    wrap.appendChild(trigger);
    wrap.appendChild(menu);
    actions.appendChild(wrap);

    try {
      var saved = localStorage.getItem('termcp_pane_strategy');
      if (saved) _paneStrategy = saved;
    } catch (e) {}
    paintPaneStrategy();

    trigger.addEventListener('click', function (e) {
      e.stopPropagation();
      setPaneStrategyOpen(!_paneStrategyOpen);
    });
    /* One menu at a time, and only this one closes on an outside click: the
       header's own menus live under .app-header and never overlap the dock. */
    document.addEventListener('click', function (e) {
      if (!_paneStrategyOpen) return;
      if (e.target.closest && e.target.closest('.session-tabbar-strategy')) return;
      setPaneStrategyOpen(false);
    });
    document.addEventListener('keydown', function (e) {
      if (!_paneStrategyOpen || e.key !== 'Escape') return;
      setPaneStrategyOpen(false);
      try { trigger.focus(); } catch (err) {}
    });
  }
})();

/** Whether the arrangement menu is showing. Kept beside the element it drives
 *  rather than in localStorage: an open menu is a transient, not a preference. */
var _paneStrategyOpen = false;

function setPaneStrategyOpen(open) {
  var menu = document.querySelector('.session-tabbar-menu');
  var trigger = document.getElementById('session-tabbar-strategy');
  if (!menu || !trigger) return;
  _paneStrategyOpen = !!open;
  menu.classList.toggle('hidden', !_paneStrategyOpen);
  trigger.setAttribute('aria-expanded', _paneStrategyOpen ? 'true' : 'false');
}

/** Draw the trigger and the checked row from _paneStrategy. Both the closed
 *  trigger and the open list must agree with the arrangement actually applied,
 *  so there is one function that reads the variable rather than two call sites
 *  that could drift. */
function paintPaneStrategy() {
  var label = document.getElementById('session-tabbar-strategy-label');
  if (!label) return;
  var opts = document.querySelectorAll('.session-tabbar-menu .lang-opt');
  for (var i = 0; i < opts.length; i++) {
    var on = opts[i].getAttribute('data-strategy') === _paneStrategy;
    opts[i].classList.toggle('active', on);
    opts[i].setAttribute('aria-checked', on ? 'true' : 'false');
    /* The trigger's word IS the chosen row's word, read from the row rather
       than translated again: one key, one rendering, so the closed key and the
       open list cannot disagree — and switching language needs no second pass. */
    if (on) label.textContent = opts[i].textContent;
  }
}

/** Apply one arrangement. The stored value is written before the grid is laid
 *  out, so a reload restores what is on screen rather than the previous shape. */
function choosePaneStrategy(value) {
  _paneStrategy = value;
  _paneColFr = [];
  try { localStorage.setItem('termcp_pane_strategy', value); } catch (e) {}
  paintPaneStrategy();
  setPaneStrategyOpen(false);
  applyPaneGrid();
}

/** Toggle button reflects the current mode: grid icon when floating, float icon when tiled; active (blue) while tiled. */
function updateTileToggleButton() {
  var b = document.getElementById('session-tabbar-tile');
  if (!b) return;
  var tiled = _layoutMode === 'tile';
  b.classList.toggle('active', tiled);
  b.title = tiled ? t('layout.tip.float') : t('layout.tip.tile');
  b.innerHTML = tiled
    ? '<svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><path d="M2 5V2h3M12 9v3H9M2 2l4 4M12 12L8 8"/></svg>'
    : '<svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.3"><rect x="1" y="1" width="5" height="5" rx="1"/><rect x="8" y="1" width="5" height="5" rx="1"/><rect x="1" y="8" width="5" height="5" rx="1"/><rect x="8" y="8" width="5" height="5" rx="1"/></svg>';
}

// ---- pane workspace: tiled mode, maximize, auto-merge ----
var _layoutMode = 'float';        // 'float' | 'tile'
var _paneActiveWin = null;        // highlighted pane in tile mode
var _paneMaxedWin = null;         // pane temporarily filling the grid
var _paneStrategy = 'auto';        // auto | cols | rows | pairs (iTerm2-style arrangements)
var _paneColFr = [];               // grid column ratios (unitless fr)
var _autoTileArmed = true;        // re-arms when floating count drops to/below threshold
var AUTO_TILE_THRESHOLD = 0;      // 0 disables auto-tiling; panes are entered manually only

function paneGridEl() { return document.getElementById('pane-grid'); }
function paneWorkspaceEl() { return document.getElementById('pane-workspace'); }

function allShellWins() {
  var out = [];
  var sel = '.shell-window';
  var c = shellWindowsEl();
  if (c) Array.prototype.push.apply(out, c.querySelectorAll(sel));
  var g = paneGridEl();
  if (g) Array.prototype.push.apply(out, g.querySelectorAll(sel));
  return out;
}

function isTiledWin(win) { return !!(win && win.parentNode && win.parentNode.id === 'pane-grid'); }

function tileWindow(win) {
  if (isTiledWin(win) || !win.classList.contains('shell-window')) return;
  win._floatRect = { left: win.style.left, top: win.style.top, width: win.style.width, height: win.style.height };
  /* The grid owns geometry from here on. A .win-fullscreen pane inside the grid
     is a contradiction: the class says "the stylesheet decides my box" (inset 0,
     100vw/100dvh, z-index of the fullscreen rung), while the grid says "I decide
     your cell". The class lingers from a window that went full-screen on a narrow
     viewport, and it POISONED the whole layout: the grid cell could not shrink it
     (100vw inline-free width came from the fullscreen rule), its z-index beat its
     neighbours', and refreshSessionTabbar counted it as a fullscreen window —
     which hid the page header while tiled. Save the narrow-viewport rect before
     dropping the class, exactly like applyWindowViewportMode does, so untile can
     put the window back the way the operator had it. */
  if (win.classList.contains('win-fullscreen')) {
    win._desktopRect = {
      left: win.style.left,
      top: win.style.top,
      width: win.style.width,
      height: win.style.height
    };
    win.classList.remove('win-fullscreen');
  }
  win.classList.add('tiled');
  paneGridEl().appendChild(win);
  setActivePane(win, false);
}

function untileWindow(win) {
  if (!isTiledWin(win)) return;
  if (_paneMaxedWin === win) _paneMaxedWin = null;
  win.classList.remove('tiled', 'pane-active', 'pane-maxed');
  var r = win._floatRect || {};
  win.style.left = r.left || '';
  win.style.top = r.top || '';
  win.style.width = r.width || '';
  win.style.height = r.height || '';
  shellWindowsEl().appendChild(win);
  bringShellWindowToFront(win);
  /* The restored inline rect describes a DESKTOP floating window. On a narrow
     viewport that geometry would beat the fullscreen media query (inline styles
     win the cascade), leaving a 640x480 box hanging off a phone screen. The
     viewport-mode pass re-evaluates and restores fullscreen when the viewport is
     narrow — the same call every floating window gets on a resize. */
  applyWindowViewportMode(win);
  /* The pane stopped being a pane; if the grid still names it active, that name
     now points outside the grid and applyPaneGrid's guard would silently pick a
     different pane while _paneActiveWin stayed stale. Clear it here where the
     demotion happens, not in whoever notices next. */
  if (_paneActiveWin === win) _paneActiveWin = null;
  _fitWinSoon(win);
}

function setActivePane(win, focusTerm) {
  var g = paneGridEl();
  if (g) Array.prototype.forEach.call(g.children, function (w) { w.classList.remove('pane-active'); });
  _paneActiveWin = win;
  if (win && isTiledWin(win)) win.classList.add('pane-active');
  refreshSessionTabbar();
  if (focusTerm) {
    var ch = win && win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
    var t = ch ? ch.term : (win && win._term);
    if (t) { try { t.focus(); } catch (e) {} }
  }
}

/** Column/row counts per strategy (iTerm2-style): auto = square-ish grid, cols = side-by-side, rows = stacked, pairs = two columns. */
function paneGridShape(n) {
  switch (_paneStrategy) {
    case 'cols':  return { cols: n, rows: 1 };
    case 'rows':  return { cols: 1, rows: n };
    case 'pairs': return { cols: 2, rows: Math.ceil(n / 2) };
    default:      return { cols: Math.ceil(Math.sqrt(n)), rows: Math.ceil(n / Math.ceil(Math.sqrt(n))) };
  }
}

function applyPaneGrid() {
  var g = paneGridEl();
  if (!g) return;
  /* A minimized pane is display:none, so it is not a grid item at all and must
     not be counted when sizing the grid — otherwise the layout reserves an
     empty cell for a window nobody can see. */
  var wins = Array.prototype.filter.call(g.children, function (w) {
    return w.classList.contains('shell-window') && !w.classList.contains('win-minimized');
  });
  var n = wins.length;
  var shape = paneGridShape(Math.max(1, n));
  var cols = Math.max(1, shape.cols);
  if (_paneColFr.length !== cols) _paneColFr = new Array(cols).fill(1);
  g.style.gridTemplateColumns = _paneColFr.map(function (fr) { return fr + 'fr'; }).join(' ');
  g.style.gridTemplateRows = 'repeat(' + Math.max(1, shape.rows) + ', 1fr)';
  var maxed = _paneMaxedWin && _paneMaxedWin.parentNode === g ? _paneMaxedWin : null;
  wins.forEach(function (w) { w.classList.toggle('pane-maxed', w === maxed); });
  if (!_paneActiveWin || _paneActiveWin.parentNode !== g || _paneActiveWin.classList.contains('win-minimized')) {
    setActivePane(wins[0] || null, false);
  }
  wins.forEach(_fitWinSoon);
}

function _fitWinSoon(win) {
  requestAnimationFrame(function () {
    if (!win._channels) return;
    Object.keys(win._channels).forEach(function (sid) {
      var ch = win._channels[sid];
      if (ch && ch.term && ch.instEl) {
        try { fitShellTerminal(ch.term, ch.instEl, win, true); } catch (e) {}
      }
    });
  });
}

/* Viewport changes: rotate the phone, resize the browser, or cross the mobile
   breakpoint. Two things must happen, and only one of them is a plain fit:
     1. every floating window re-evaluates full-screen mode, since a window that
        carries desktop inline geometry (640x480) otherwise keeps it after a
        rotate to the mobile breakpoint, because inline styles beat media queries;
     2. the terminal refits, or xterm keeps the row/column count from the old size.
   Coalesced through rAF: mobile browsers fire resize continuously while the
   address bar collapses. */
(function () {
  var pending = 0;
  function onViewportChange() {
    if (pending) return;
    pending = window.requestAnimationFrame(function () {
      pending = 0;
      var mobile = isMobileViewport();
      if (mobile !== _wasMobileViewport) {
        _wasMobileViewport = mobile;
        /* Crossing into a narrow viewport drops tile mode: arrangement controls
           are hidden on mobile (the buttons are meaningless when every terminal
           is full-screen, leaving ~180px slivers of text in a grid) and leaving
           tile active would strand the operator in a grid with no visible toggle
           back. Untiling restores the floating container, and the loop below
           immediately promotes every restored window to .win-fullscreen. */
        if (mobile && _layoutMode === 'tile') {
          setLayoutMode('float');
        }
        allShellWins().forEach(function (w) {
          if (isTiledWin(w)) return;
          if (mobile) {
            applyWindowViewportMode(w);
          } else {
            /* Leaving narrow: drop full-screen and re-seat the window as a
               floating one. The window's own desktop rect is restored when one
               was remembered (applyWindowViewportMode saved it before clearing
               inline styles): a window the operator moved and sized on the
               desktop returns to THAT place, not to a fresh cascade. A window
               opened while narrow has no memory, so it is centred as any new
               desktop window is. Either way it ends up draggable and clamped
               into the now-wider viewport. */
            w.classList.remove('win-fullscreen');
            if (!w._maxed) {
              if (w._desktopRect && w._desktopRect.width) {
                ['left', 'top', 'width', 'height'].forEach(function (key) {
                  w.style[key] = w._desktopRect[key];
                });
                clampShellWindowIntoContainer(w);
              } else {
                positionShellWindowFromClick(w, null);
              }
            }
          }
        });
      } else if (mobile) {
        /* Same mode, new dimensions: nothing to re-seat, but keep the class. */
        allShellWins().forEach(function (w) { if (!isTiledWin(w)) applyWindowViewportMode(w); });
      }
      /* Refit after the layout settles; a window measured while hidden or
         mid-resize reports 0 and fitShellTerminal silently declines. */
      allShellWins().forEach(function (w) {
        if (w.classList.contains('win-hidden') || w.classList.contains('win-minimized')) return;
        _fitWinSoon(w);
      });
      refreshSessionTabbar();
    });
  }
  window.addEventListener('resize', onViewportChange);
  window.addEventListener('orientationchange', onViewportChange);
})();

/** Open the tiled overlay unless the operator has hidden all windows. */
function openPaneWorkspace() {
  var ws = paneWorkspaceEl();
  if (!ws) return;
  ws.classList.toggle('open', !_winsHidden);
  ws.setAttribute('aria-hidden', _winsHidden ? 'true' : 'false');
  applyPaneGrid();
}

function setLayoutMode(mode) {
  /* An empty tiling is a TRAP, not a mode: the workspace overlay would open
     (z-index of the workspace rung, pointer-events on) over the whole page with
     not one pane inside it, and every click on the main page would land on the
     grid instead of the connection form, the extensions, the archive — all of
     it. The close-the-last-pane observer cannot save this case either, because
     nothing changes while the overlay sits there empty: no childList mutation
     ever fires. So "tile" with zero windows simply refuses: the mode stays
     float, the preference is not persisted, and the strip keeps its windows.
     Toggling the button with nothing to arrange is a no-op, which is also what
     it looks like. */
  if (mode === 'tile' && allShellWins().length === 0) {
    updateTileToggleButton();
    return;
  }
  /* The arrangement CONTROLS are hidden on a narrow viewport (the buttons are
     meaningless when every terminal is the full-screen layer), so a mode that
     arrives while narrow has no visible way back — and a tile preference saved
     on the desktop would restore itself on the phone with the toggle gone. The
     viewport owns the answer here: narrow means one terminal on screen, so
     requests to tile are answered with float instead of an unreachable grid.
     (applyWindowViewportMode and the resize handler already keep every window
     fullscreen while narrow; this only stops the layout state from disagreeing
     with what the reader can see.) */
  if (mode === 'tile' && isNarrowViewport()) {
    _layoutMode = 'float';
    try { localStorage.setItem('termcp_layout_mode', 'float'); } catch (e) {}
    document.body.classList.remove('tile-mode');
    updateTileToggleButton();
    return;
  }
  _layoutMode = mode === 'tile' ? 'tile' : 'float';
  try { localStorage.setItem('termcp_layout_mode', _layoutMode); } catch (e) {}
  document.body.classList.toggle('tile-mode', _layoutMode === 'tile');
  updateTileToggleButton();
  var ws = paneWorkspaceEl();
  if (!ws) return;
  if (_layoutMode === 'tile') {
    allShellWins().forEach(function (w) { if (!isTiledWin(w)) tileWindow(w); });
    _paneColFr = [];
    openPaneWorkspace();
  } else {
    _paneMaxedWin = null;
    Array.prototype.slice.call(paneGridEl().children).forEach(function (w) { if (w.classList.contains('shell-window')) untileWindow(w); });
    applyPaneGrid();
    ws.classList.remove('open');
    ws.setAttribute('aria-hidden', 'true');
  }
  syncShellWindowVisibility();
}

/** Auto-merge: more than AUTO_TILE_THRESHOLD floating windows tiles everything once; re-arms at/below threshold. */
function _layoutMaybeAutoTile() {
  if (AUTO_TILE_THRESHOLD <= 0) return; // disabled
  if (_layoutMode === 'tile') { applyPaneGrid(); return; }
  var floating = allShellWins().filter(function (w) { return !isTiledWin(w); }).length;
  if (floating <= AUTO_TILE_THRESHOLD) _autoTileArmed = true;
  else if (_autoTileArmed) {
    _autoTileArmed = false;
    setLayoutMode('tile');
  }
}

// Toolbar lives in the session tab bar; see the action-cluster init above.

// ---- hide/show all terminal windows (main-page focus mode) ----
var _winsHidden = false;

/** Last viewport mode seen, so the resize handler can tell a rotate (same mode,
 *  new size) from a breakpoint crossing (mode flip). Seeded on first load. */
var _wasMobileViewport = isNarrowViewport();

/* Layout changes move windows between containers. Reapply the global hide
   choice after each move: floats hide individually, panes hide with their
   workspace and must not retain a float's display:none marker. */
function syncShellWindowVisibility() {
  var ws = paneWorkspaceEl();
  var showPanes = _layoutMode === 'tile' && !_winsHidden;
  if (ws) {
    ws.classList.toggle('open', showPanes);
    ws.setAttribute('aria-hidden', showPanes ? 'false' : 'true');
  }
  allShellWins().forEach(function (w) {
    w.classList.toggle('win-hidden', _winsHidden && !isTiledWin(w));
    if (!_winsHidden && !w.classList.contains('win-minimized')) _fitWinSoon(w);
  });
  if (showPanes) applyPaneGrid();
  updateSessionTabbarToggle();
  refreshSessionTabbar();
}

function hideAllWindows() {
  _winsHidden = true;
  syncShellWindowVisibility();
}

function showAllWindows() {
  _winsHidden = false;
  syncShellWindowVisibility();
}

function toggleAllWindowsVisible() { _winsHidden ? showAllWindows() : hideAllWindows(); }

function updateSessionTabbarToggle() {
  var btn = document.getElementById('session-tabbar-toggle');
  if (!btn) return;
  btn.title = _winsHidden ? t('win.showAll') : t('win.hideAll');
  btn.setAttribute('aria-label', btn.title);
  btn.innerHTML = _winsHidden
    ? '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><path d="M1 8s2.5-5 7-5 7 5 7 5-2.5 5-7 5-7-5-7-5z"/><circle cx="8" cy="8" r="2.5"/></svg>'
    : '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><path d="M1 8s2.5-5 7-5 7 5 7 5-2.5 5-7 5-7-5-7-5z"/><path d="M3.5 11.5l9-7"/></svg>';
}

/** Publish the dock's height for the stylesheet, which cannot measure.
 *
 *  The dock is pinned to the bottom of the viewport and the tiled overlay
 *  (.pane-workspace) covers the viewport, so without this the overlay would sit
 *  on top of the dock: the pane-layout keys would be invisible, which is the
 *  one thing the dock exists to offer.
 *
 *  This is the same arrangement the header already uses (--app-header-h): the
 *  overlay insets itself by a measured length instead of the layout guessing,
 *  so there is no "push vs overlay" rule to keep in sync. A ResizeObserver
 *  rather than a resize listener, because the dock changes height when a
 *  language switch rewrites its labels or a web font lands. */
function syncDockDepth() {
  var el = sessdockEl();
  if (!el) return;
  var r = el.getBoundingClientRect();
  var h = isFinite(r.height) ? Math.max(0, r.height) : 0;
  document.documentElement.style.setProperty('--dock-h', h + 'px');
}

/* A language switch relabels the dock's own strings. The empty note carries a
   data-i18n key and is repainted by applyI18n, but the row may also have been
   rebuilt with it absent, so the refresh is the one pass that settles both. */
onLangChange(refreshSessionTabbar);

// Keep the grid recomputed as panes are added/removed.
(function () {
  var g = paneGridEl();
  if (!g || typeof MutationObserver === 'undefined') return;
  new MutationObserver(function () {
    if (_layoutMode !== 'tile') return;
    // Closing the last pane leaves an empty overlay with nothing to act on:
    // drop back to float mode so the main page becomes reachable again.
    if (!g.querySelector('.shell-window')) { setLayoutMode('float'); return; }
    applyPaneGrid();
    // applyPaneGrid skips setActivePane (and its tabbar refresh) when the active
    // pane is unaffected, so closed non-active panes would leave stale tabs.
    refreshSessionTabbar();
  }).observe(g, { childList: true });
})();

/* closeShellWindow is reached from a dozen paths (session tiles, the archive,
   reconnect, the window's own buttons), so the header's has-fullscreen-window
   flag cannot rely on each of them refreshing the tab bar. The container is the
   one thing every removal goes through, and refreshSessionTabbar recomputes the
   flag from the windows that remain. */
(function () {
  var c = shellWindowsEl();
  if (!c || typeof MutationObserver === 'undefined') return;
  new MutationObserver(function () { refreshSessionTabbar(); }).observe(c, { childList: true });
})();

// Restore last mode preference on load; keep the toggle button and tab bar in
// sync so a restored "tile" preference is reflected before the first window opens.
// A narrow viewport never restores tile: the arrangement controls are hidden
// there, and the restored grid would be unreachable — see setLayoutMode.
try { if (localStorage.getItem('termcp_layout_mode') === 'tile' && !isNarrowViewport()) _layoutMode = 'tile'; } catch (e) {}
updateTileToggleButton();
refreshSessionTabbar();

/* Publish the header's depth before any window can open the tiled workspace, then
   republish whenever the header itself changes size — a viewport resize that wraps
   its nav, a language switch that rewrites the labels, or a web font landing after
   first paint. A resize listener would miss the last two, which is why the observer
   watches the header and not the window. */
syncAppHeaderDepth();
(function () {
  if (typeof ResizeObserver === 'undefined') return;
  var header = document.querySelector('.app-header');
  if (header) new ResizeObserver(syncAppHeaderDepth).observe(header);
})();

/* The same arrangement for the dock. It is measured rather than assumed because
   it is what the tiled overlay insets itself by, and the tab row's height
   follows the font and the language. */
syncDockDepth();
(function () {
  if (typeof ResizeObserver === 'undefined') return;
  var dock = sessdockEl();
  if (dock) new ResizeObserver(syncDockDepth).observe(dock);
})();
window.addEventListener('resize', syncDockDepth);

/** Toggle maximize: floating window fills the viewport; a pane fills the grid. */
function toggleShellWindowMax(win) {
  if (!win || !win.classList.contains('shell-window')) return;
  if (isTiledWin(win)) {
    _paneMaxedWin = (_paneMaxedWin === win) ? null : win;
    applyPaneGrid();
    setActivePane(win, true);
    return;
  }
  /* Full-screen mode owns its geometry through CSS; letting a maximize toggle
     write inline width/left here would override the stylesheet and un-fullscreen
     the window. On mobile the window is already screen-sized, so it is a no-op. */
  if (win.classList.contains('win-fullscreen') || isMobileViewport()) return;
  if (win._maxed) {
    win._maxed = false;
    var r = win._maxSavedRect || {};
    win.style.left = r.left || ''; win.style.top = r.top || '';
    win.style.width = r.width || ''; win.style.height = r.height || '';
    var maxBtn = win.querySelector('.shell-window-max-btn');
    if (maxBtn) { maxBtn.setAttribute('data-i18n-title', 'win.max.tip'); maxBtn.title = t('win.max.tip'); }
  } else {
    win._maxed = true;
    win._maxSavedRect = { left: win.style.left, top: win.style.top, width: win.style.width, height: win.style.height };
    /* The maximized window sits in the PAGE, not on the screen: it is inset by the
       measured header and dock, the same two depths the tiled layer uses.
     *
     * It was `8px / 100vw / 100dvh`, i.e. the full viewport with a margin — so
     * maximizing covered the page header and the dock. The dock is where the
     * layout keys live (tile, untile, restore), and covering it while a window is
     * maximized hides the controls that get you out; the header is page chrome the
     * terminals belong to. "Maximize" means "use the work area", and the work area
     * is what those two custom properties already describe.
     *
     * The gutter is 8px on all four sides, and the dock keeps its own 8px of
     * clearance under the window rather than sitting flush against it. */
    var insetTop = 'var(--app-header-h, 0px)';
    var insetBottom = 'var(--dock-h, 0px)';
    win.style.left = '8px';
    win.style.top = 'calc(' + insetTop + ' + 8px)';
    /* dvh, not vh: iOS Safari's 100vh includes the collapsing address bar, so a
       vh-based maximize leaves the bottom of the terminal cut off. */
    win.style.width = 'calc(100vw - 16px)';
    win.style.height = 'calc(100dvh - ' + insetTop + ' - ' + insetBottom + ' - 16px)';
    var maxBtnR = win.querySelector('.shell-window-max-btn');
    if (maxBtnR) { maxBtnR.setAttribute('data-i18n-title', 'win.restore.tip'); maxBtnR.title = t('win.restore.tip'); }
  }
  bringShellWindowToFront(win);
  _fitWinSoon(win);
}

function bindShellWindowMaxButton(win) {
  var btn = win.querySelector('.shell-window-max-btn');
  var header = win.querySelector('.shell-window-header');
  if (btn && !btn._termcpMaxBound) {
    btn._termcpMaxBound = true;
    btn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    btn.addEventListener('click', function (e) { e.preventDefault(); e.stopPropagation(); toggleShellWindowMax(win); });
  }
  if (header && !header._termcpDblMaxBound) {
    header._termcpDblMaxBound = true;
    header.addEventListener('dblclick', function (e) {
      if (e.target.closest('button')) return;
      /* No maximize toggle on mobile: the window already fills the screen, and a
         double tap there is a browser zoom gesture. */
      if (win.classList.contains('win-fullscreen') || isMobileViewport()) return;
      toggleShellWindowMax(win);
    });
  }
}

// Esc restores a maximized floating window / maxed pane.
document.addEventListener('keydown', function (e) {
  if (e.key !== 'Escape') return;
  if (_paneMaxedWin && isTiledWin(_paneMaxedWin)) { _paneMaxedWin = null; applyPaneGrid(); return; }
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    if (wins[i]._maxed && !isTiledWin(wins[i])) { toggleShellWindowMax(wins[i]); return; }
  }
});

/** The page's one definition of "narrow": a viewport the desktop window model
 *  does not fit.
 *
 *  Width leads: at or below 768px — the same breakpoint the stylesheet's narrow
 *  blocks use — a floating 640x480 window is either clipped or leaves no room to
 *  read, so the window becomes the screen. Height joins it for the touch phone
 *  held sideways: ~844px wide clears the width test but only ~390px tall, and a
 *  desktop box that tall is a sliver; touch capability is the gate there, so a
 *  short DESKTOP strip (a half-screen terminal beside an editor) keeps its
 *  draggable window and a mouse. The `no-touch` escape hatch lives in the CSS
 *  blocks, not here — this predicate is what the JS asks, and it must agree with
 *  the stylesheet it drives.
 *
 *  Deliberately NOT a device query alone: pointer capability never changes while
 *  the window is open, so a device-only test made the mode blind to the resize
 *  it exists to answer (a phone unfolded to tablet width kept its full-screen
 *  window; a desktop window dragged narrow never got one). */
function isTouchDevice() {
  if (!window.matchMedia) return false;
  try {
    return window.matchMedia('(pointer: coarse)').matches &&
           window.matchMedia('(hover: none)').matches;
  } catch (e) {
    return false;
  }
}

function isNarrowViewport() {
  var w = window.innerWidth || (document.documentElement && document.documentElement.clientWidth) || 0;
  var h = window.innerHeight || (document.documentElement && document.documentElement.clientHeight) || 0;
  if (w > 0 && w <= 768) return true;
  return h > 0 && h <= 520 && isTouchDevice();
}

/** Kept as the name every caller and test knows. It IS the narrow test: the
 *  full-screen window is a property of the viewport, not of the device. */
function isMobileViewport() {
  return isNarrowViewport();
}

/** Mobile shows one terminal filling the viewport (RFC: full-screen layer).
 *
 *  Geometry comes from the stylesheet here, never from inline styles: an inline
 *  width/height wins over a media query, so writing the desktop 640x480 box would
 *  silently defeat the full-screen rule. Any inline geometry left over from a
 *  desktop-sized window (or an earlier viewport) is cleared, and tiled panes are
 *  left alone — they are laid out by the pane grid, not by this. */
function applyWindowViewportMode(win) {
  if (!win || isTiledWin(win)) return;
  if (!isMobileViewport()) {
    win.classList.remove('win-fullscreen');
    return;
  }
  /* Save the desktop geometry before clearing inline styles, so leaving narrow
     viewport can restore the exact size and position the operator arranged
     instead of cascading to the centre line again. */
  if (win.style.width && win.style.height) {
    win._desktopRect = {
      left: win.style.left,
      top: win.style.top,
      width: win.style.width,
      height: win.style.height
    };
  }
  win.classList.add('win-fullscreen');
  win.style.width = '';
  win.style.height = '';
  win.style.left = '';
  win.style.top = '';
  /* Clear the inline z-index too: the full-screen rule in the stylesheet owns it,
     and a stale one from floating mode would defeat that rule. */
  win.style.zIndex = '';
  /* A desktop maximize rect cannot be restored meaningfully at this size, and a
     stale "maximized" flag would make the first restore jump back to 640x480. */
  win._maxSavedRect = null;
  win._maxed = false;
}

/** Keep a window on the centre lines its placement chose after its content settled
 *  on a size of its own. Placement centres a box of the session default (640x480,
 *  clamped to the viewport); a card that measures a fraction of that would hang
 *  below and right of the middle, so the real box is moved back onto the same two
 *  centre lines — clamped by the same 8px margin and the same header floor.
 *
 *  The shift is always measured from where placement left the window, not from where
 *  the window is now: a card whose readout arrives late is re-centred from the same
 *  base, and two shifts never stack. A reader's drag writes geometry of its own, so
 *  callers stop asking once the window has been taken over.
 *
 *  Self-contained on purpose: the placement test runs positionShellWindowFromClick
 *  on its own, so neither function may depend on a shared constant. */
function recentreShellWindowForSize(win) {
  if (!win || isMobileViewport()) return;
  var realW = win.offsetWidth;
  var realH = win.offsetHeight;
  if (!realW || !realH) return;
  var size = realW + 'x' + realH;
  if (win._centredSize === size) return;
  if (!win._placedAt) {
    win._placedAt = { left: parseFloat(win.style.left), top: parseFloat(win.style.top) };
  }
  var base = win._placedAt;
  if (isNaN(base.left) || isNaN(base.top)) return;
  var MARGIN = 8;
  var defW = Math.min(640, window.innerWidth - MARGIN * 2);
  var defH = Math.min(480, window.innerHeight - MARGIN * 2);
  if (realW >= defW && realH >= defH) return;
  var floor = Math.max(MARGIN, appHeaderBottom());
  win.style.left = Math.max(MARGIN,
    Math.min(base.left + (defW - realW) / 2, window.innerWidth - realW - MARGIN)) + 'px';
  win.style.top = Math.max(floor,
    Math.min(base.top + (defH - realH) / 2, window.innerHeight - realH - MARGIN)) + 'px';
  win._centredSize = size;
}

/** Fallback placement for a new .shell-window (caller must have incremented shellWindowCount).
 *
 *  Takes an explicit position — {x, y} in client coordinates, or null for the
 *  centred cascade — and never an event. The host drawer is pinned to the left
 *  edge while its click is still being handled, so a caller that passed the
 *  click itself would place the window against the screen's left wall, under
 *  the drawer (the clamp below keeps it on screen, which is exactly what pinned
 *  it there). Callers pass a position only when the click is a real target.
 *
 *  On mobile the window fills the viewport and CSS owns its geometry (see
 *  applyWindowViewportMode); the cascade steps aside for the same reason.
 *
 *  Both placements stop below the page header (see appHeaderBottom): a window
 *  whose title bar lands in the header band hides the wordmark and the page's own
 *  controls, and a click that names a point up there still has to resolve to a
 *  reachable window. */
function positionShellWindowFromClick(win, pos) {
  if (isMobileViewport()) { applyWindowViewportMode(win); return; }
  var MARGIN = 8;
  /* The header is page chrome, so it outranks the cosmetic margin: the window's
     leading edge goes below the header even when that is further down than 8px. */
  var FLOOR = Math.max(MARGIN, appHeaderBottom());
  var winW = Math.min(640, window.innerWidth - MARGIN * 2);
  var winH = Math.min(480, window.innerHeight - MARGIN * 2);
  win.style.width = winW + 'px';
  win.style.height = winH + 'px';
  /* A null position means "centre it", not "guess": the caller decides, so a
     guessed coordinate can never place the window against the edge again.
     Consecutive windows cascade from the centre rather than stacking exactly,
     which decides the top window on a click that never named one. */
  if (!pos) {
    var off = (shellWindowCount - 1) % 5;
    var centreL = Math.round((window.innerWidth - winW) / 2) + off * 24;
    var centreT = Math.round((window.innerHeight - winH) / 2) + off * 24;
    win.style.left = Math.max(MARGIN, Math.min(centreL, window.innerWidth - winW - MARGIN)) + 'px';
    win.style.top = Math.max(FLOOR, Math.min(centreT, window.innerHeight - winH - MARGIN)) + 'px';
    return;
  }
  var th = 21;
  win.style.left = Math.max(MARGIN, Math.min(pos.x - winW / 2, window.innerWidth - winW - MARGIN)) + 'px';
  win.style.top = Math.max(FLOOR, Math.min(pos.y - th, window.innerHeight - winH - MARGIN)) + 'px';
}

/** The pointer position a click event names, or null when there is none (a
 *  keyboard activation) or the window is not placed at the pointer at all — the
 *  centring decision every host-list entry point shares. */
function windowPositionFromClick(ev) {
  if (!ev || typeof ev.clientX !== 'number') return null;
  return { x: ev.clientX, y: ev.clientY };
}

/** The y coordinate a floating window may not rise above: the page header's lower
 *  edge. Measured rather than restated as a constant because the header is not a
 *  fixed height — its nav wraps at narrow widths and a language switch rewrites
 *  the labels inside it. Returns 0 (the viewport top, i.e. the old behaviour) when
 *  the page has no header at all, which is also what an unmeasured page gets. */
function appHeaderBottom() {
  var el = document.querySelector('.app-header');
  if (!el) return 0;
  var r = el.getBoundingClientRect();
  return isFinite(r.bottom) ? Math.max(0, r.bottom) : 0;
}

/** Publish that edge for the stylesheet, which cannot measure: .pane-workspace
 *  insets its own top by this variable, so the tiled grid stops below the header
 *  and the header — the way back to the page — stays readable and clickable in
 *  tile mode. Kept as a variable rather than a duplicated constant so the CSS
 *  floor and the drag floor cannot drift apart. */
function syncAppHeaderDepth() {
  document.documentElement.style.setProperty('--app-header-h', appHeaderBottom() + 'px');
}

/** Pull a floating window back inside the windows container after a drag.
 *
 *  A drag tracks the cursor with no bound — that is what makes it feel direct —
 *  but it also lets the window be released past the edge, and a window released
 *  fully off-screen has no header left to grab: the only way back would be the
 *  session tab bar. Clamping on release rather than during the move keeps the
 *  pointer and the window locked together while it matters, and settles the
 *  position once the drag is over.
 *
 *  Bounded by the container, not the viewport: the container is what windows are
 *  positioned against, so if it ever gains an inset this stays correct. The
 *  leading edge is the page header's lower edge, never the viewport top — a window
 *  released over the header would cover the page's own chrome, and the header is
 *  the only way back to it. When the window is larger than the remaining area on
 *  an axis the leading edge wins, which keeps the title and its close button
 *  reachable. */
function clampShellWindowIntoContainer(win) {
  if (!win || isTiledWin(win)) return;
  var host = win.parentNode;
  if (!host || typeof host.getBoundingClientRect !== 'function') return;
  var box = host.getBoundingClientRect();
  var areaW = host.clientWidth || box.width;
  var areaH = host.clientHeight || box.height;
  /* A container with no area cannot say where "inside" is. Measured mid-resize it
     can report 0, and every clamp below would then resolve to the container's own
     origin — moving the window for a reason that does not exist. */
  if (!areaW || !areaH) return;
  var r = win.getBoundingClientRect();
  /* NO LAYOUT BOX, NOTHING TO CLAMP. A hidden, minimized, or not-yet-placed window
     measures 0x0 at 0,0, and clamping that rect would set left/top to the
     container's top-left corner: the window would come back from the tab bar at
     the corner instead of where the reader left it. This is reachable now that the
     clamp also runs on focus (see bringShellWindowToFront), which is a path a
     hidden window can take. */
  if (!r.width || !r.height) return;
  /* The leading edge is the container's own top, or the header's lower edge when
     that is further down; the trailing edge still counts from the container. */
  var floor = Math.max(box.top, appHeaderBottom());
  var maxLeft = Math.max(box.left, box.left + areaW - r.width);
  var maxTop = Math.max(floor, box.top + areaH - r.height);
  var left = Math.min(Math.max(box.left, r.left), maxLeft);
  var top = Math.min(Math.max(floor, r.top), maxTop);
  if (left !== r.left) win.style.left = left + 'px';
  if (top !== r.top) win.style.top = top + 'px';
}

/** Idempotent: z-order on mousedown + focus only when clicking wrap padding (not .xterm / not header). */
function bindShellWindowMouseToFront(win) {
  if (win._termcpMouseToFrontBound) return;
  win._termcpMouseToFrontBound = true;
  win.addEventListener('mousedown', function (e) {
    bringShellWindowToFront(win);
    var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
    var t = ch ? ch.term : win._term;
    if (!t) return;
    if (e.target.closest('.shell-window-header')) return;
    if (!e.target.closest('.xterm')) {
      try { t.focus(); } catch (e2) {}
    }
  });
}

/** "-" button: minimize — hide the window, keep the session running and the
 *  window restorable (see minimizeShellWindow). Destroying the window outright
 *  was the old behavior and made the session unreachable from the UI. */
function bindShellWindowMinButton(win, minBtn) {
  if (!minBtn || minBtn._termcpMinBound) return;
  minBtn._termcpMinBound = true;
  minBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  minBtn.addEventListener('click', function (e) {
    e.preventDefault(); e.stopPropagation();
    minimizeShellWindow(win);
  });
}

/** Minimize: hide the window but keep it alive — DOM, xterm, and the terminal
 *  watch all survive, so the session stays reachable from the switcher and the
 *  session tiles. This is deliberately NOT closeShellWindow, which destroys the
 *  window: a minimized window that has been removed from the DOM cannot be
 *  listed or restored by anything.
 *
 *  Uses .win-minimized rather than the tab bar's .win-hidden, because that one
 *  is a single global flag (_winsHidden): restoring one window would restore
 *  every hidden window, and minimizing one would hide them all. */
function minimizeShellWindow(win) {
  if (!win || win.classList.contains('win-minimized')) return;
  win.classList.add('win-minimized');
  win._minimized = true;
  /* In tile mode the grid has to drop this pane's cell, or the layout keeps
     reserving space for a window that is no longer drawn. */
  if (isTiledWin(win)) applyPaneGrid();
  refreshSessionTabbar();
}

/** Undo minimizeShellWindow. Idempotent. */
function restoreShellWindow(win) {
  if (!win || !win._minimized) return;
  win.classList.remove('win-minimized');
  win._minimized = false;
  if (isTiledWin(win)) applyPaneGrid();
  _fitWinSoon(win);
}

/** Expand a collapsed window (the header's chevron). Idempotent, so the
 *  switcher and the session tiles can call it before focusing. */
function expandShellWindow(win) {
  if (!win || !win.classList.contains('shell-window-collapsed')) return;
  win.style.height = win._savedH || '';
  win.style.minHeight = '';
  win.classList.remove('shell-window-collapsed');
  var btn = win.querySelector('.shell-window-collapse-btn');
  if (btn) {
    var icD = btn.querySelector('.ic-d');
    var icU = btn.querySelector('.ic-u');
    if (icD && icU) { icD.style.display = ''; icU.style.display = 'none'; }
    btn.setAttribute('data-i18n-title', 'win.collapse');
    btn.title = t('win.collapse');
  }
  var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
  if (ch && ch.term && ch.instEl) fitShellTerminal(ch.term, ch.instEl, win, true);
}

/** Collapse a window to its header. Idempotent. */
function collapseShellWindow(win) {
  if (!win || win.classList.contains('shell-window-collapsed')) return;
  win._savedH = win.style.height || (win.getBoundingClientRect().height + 'px');
  win.style.height = 'auto';
  win.style.minHeight = '0';
  win.classList.add('shell-window-collapsed');
  var btn = win.querySelector('.shell-window-collapse-btn');
  if (btn) {
    var icD = btn.querySelector('.ic-d');
    var icU = btn.querySelector('.ic-u');
    if (icD && icU) { icD.style.display = 'none'; icU.style.display = ''; }
    btn.setAttribute('data-i18n-title', 'win.expand');
    btn.title = t('win.expand');
  }
}

/** The window header's x: close THIS panel, or ask to archive the session if it is
 *  the LAST one.
 *
 *  This is the panel-level key — it sits on a panel, so "this panel" is what it can
 *  honestly mean. The dock tab's x is the session-level one and takes the whole
 *  session down at once (see closeDockEntry). */
function closeSessionWindowFromUI(win) {
  if (!win) return;
  var sid = win._parentSid || win._sid;
  if (!sid || win._placeholder || win._extName) { closeShellWindow(win); return; }
  if (sessionPanelWindows(sid).length > 1) {
    closeShellWindow(win);
    return;
  }
  askCloseSession(sid, win);
}

/** The dock's broom: close every window whose session is already over.
 *
 *  What it closes is WINDOWS, not records. A session that has ended is retained in
 *  the archive and its window is a read-only view of a transcript that is already
 *  filed there; the workspace keeps showing it until someone closes it, one panel
 *  at a time. This is the sweep for that residue, and it destroys nothing: the
 *  archive rows, their transcripts and their export all stay where they are.
 *
 *  That is also why this key carries no confirmation and no destructive colour.
 *  The archive column owns erasing records, and it asks first. A key here that did
 *  the same thing would put the irreversible act on the bar the reader uses to
 *  tidy, which is the one place they are not expecting to lose anything.
 *
 *  Its enablement comes from the PRESENCE of such a window, never from the ticked
 *  set: a sweep is not a selection, and a key that quietly required one would look
 *  broken on a page where nobody has ticked anything.
 */
function closeArchivedWindows() {
  var doomed = archivedSessionWindows();
  if (!doomed.length) {
    showCopyToast(t('dock.closeArchived.none'));
    return;
  }
  doomed.forEach(function (w) { closeShellWindow(w); });
  /* The strip is repainted here rather than left to the removal observer: every
     close is synchronous up to its fetch, and the count and the empty note read
     off the entry list, which is rebuilt in this pass. */
  refreshSessionTabbar();
  showCopyToast(tCount('dock.closeArchived.done.one', 'dock.closeArchived.done.other', { count: doomed.length }));
}

/** The open windows that show a session that is over.
 *
 *  Read from the session snapshot, not from `win._readOnly`: the flag says the
 *  window was LOCKED, which is also true of a card the operator pinned while its
 *  session still runs, and those are not swept. */
function archivedSessionWindows() {
  return allShellWins().filter(function (w) {
    /* A dial in flight has no session yet, and an extension's status card has no
       session at all: neither is a view of an ended session, and closing one would
       interrupt a connection that is still being made. */
    return !!w._parentSid && !w._placeholder && !w._extName && isSessionDeadInWindows(w._parentSid);
  });
}

/** Enable the broom only while there is something for it to sweep. */
function paintCloseArchivedKey() {
  var btn = document.getElementById('dock-close-archived');
  if (!btn) return;
  btn.disabled = archivedSessionWindows().length === 0;
}

/** "x" button: this panel's close, which asks at session scope only when it is the
 *  last panel left (see closeSessionWindowFromUI). */
function bindShellWindowCloseButton(win, closeBtn) {
  if (!closeBtn || closeBtn._termcpCloseBound) return;
  closeBtn._termcpCloseBound = true;
  closeBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  closeBtn.addEventListener('click', function (e) {
    e.preventDefault(); e.stopPropagation();
    closeSessionWindowFromUI(win);
  });
}

/** Whether a session id belongs to an archived (not running) session, read from
 *  the last snapshot. Absent from the snapshot means "unknown": treat it as live,
 *  because asking before disconnecting a running session is the safe mistake while
 *  silently erasing one is not recoverable.
 */
function isSessionDeadInWindows(sid) {
  var snap = window._lastSessionsSnapshot || [];
  for (var i = 0; i < snap.length; i++) {
    if (snap[i] && snap[i].id === sid) return isDeadSession(snap[i]);
  }
  return false;
}

/* ---- Closing several windows at once ----------------------------------------
 * The dock is the only list of open sessions, so it is also where a batch of them
 * is closed — and it uses the SAME component as the two side columns, because that
 * component exists precisely so a column's batch row cannot drift from its
 * neighbours'. Its grid is the tab strip, its cards are the tabs, and the id it
 * indexes by is the window id the tab already carries.
 *
 * "Close" means STOP AND ARCHIVE. The page offers no way to erase a session
 * directly any more: a running session has a transcript worth keeping, and erasing
 * one is archive-then-delete — a deliberate act with its own home in the archive
 * column, where the record being destroyed is the thing on screen.
 */
var _dockBatch = (typeof createBatchBar === 'function') ? createBatchBar({
  bodyId: 'sessdock',
  /* alwaysOn: every tab carries its own tick (the monitor glyph), so a tab click
     still raises its window and the batch needs no mode of its own. This is the
     same arrangement the archive column uses, and for the same reason — a switch
     that only exists to make things clickable is chrome in front of a list that is
     already clickable. */
  alwaysOn: true,
  selectingHostId: 'sessdock',
  toggleId: null,
  countId: 'dock-toggle-count',
  invertId: 'dock-sel-invert',
  /* No split key: this bar has no "open" verb for a wide half — clicking a tab
     already does that — so the action key is a plain one and there is no caret or
     menu. The component takes null ids for all three and skips them. */
  splitId: null, mainId: null, caretId: null, menuId: null,
  exportId: null,
  deleteId: 'dock-close-ticked',
  actionIds: ['dock-min-ticked', 'dock-restore-ticked', 'dock-close-ticked'],
  gridId: 'session-tabs-scroll',
  cardSelector: '.session-tab',
  nameAttr: 'data-win-id',
  selectKey: 'nethub.batch.select',
  names: function () {
    return (typeof dockTabKeys === 'function') ? dockTabKeys() : [];
  },
  /* The tab is its own painter: the component toggles `batch-selected`, and the
     dock's CSS keys the tick off that class. */
  render: function () { refreshSessionTabbar(); },
  tipFor: function (id) { return id; },
  /* The close key answers while nothing is ticked by not being there at all. The
     component only DISABLES an action key, which is right for a mode-switched row
     whose keys hold their length in both states — but on this bar the key would be
     a permanently visible dead control, and the ticked tabs are already the signal
     that there is a batch to act on. */
  onUpdate: function (n) {
    /* The close key dims rather than disappears, the same contract the archive's
       verbs follow: a key that vanishes takes its own existence with it, and the
       dock's count beside it is what explains the dimming. */
    var count = document.getElementById('dock-toggle-count');
    if (count) {
      /* Shown at every value, including zero. Hiding the readout while the count
         was zero meant the dock's number disappeared the moment you cleared the
         batch and reappeared on the next tick, so the row's width changed under the
         pointer — and, worse, a zero that is absent cannot be told apart from a
         counter that is broken, which is exactly how this one was reported. */
      count.hidden = false;
      count.textContent = String(n);
      count.classList.toggle('is-empty', n === 0);
    }
    /* Every window's header glyph is the SAME tick seen from the window side, so it
       is repainted from the one set here, on every change. Without this the glyph
       was a control that produced no feedback at all: the press recorded a tick the
       user could only confirm by finding the dock and reading a number on it. The
       class it flips is the one the stylesheet already had for the state
       (`.shell-header-icon-btn.active`) and that nothing ever set. */
    /* Read off the dock's own tabs rather than off the component: this hook runs
       while the component is still being constructed, so `_dockBatch` is not yet
       bound, and the strip is the painted truth anyway — a tab is marked exactly
       when its window is in the set. */
    var ticked = Object.create(null);
    /* Whether the selection holds anything either key can act on. Read in the same
       pass: a ticked PLACEHOLDER (still dialling) or a status card has no window,
       so a row of those leaves both keys dead — and a live key that does nothing is
       the one thing this bar's own comments forbid. */
    var minable = 0, restorable = 0;
    Array.prototype.slice.call(document.querySelectorAll('#session-tabs-scroll [data-win-id]')).forEach(function (t) {
      if (!t.classList.contains('batch-selected')) return;
      ticked[t.getAttribute('data-win-id')] = 1;
      var w = t._entry && t._entry.win;
      if (!w) return;
      if (w.classList.contains('win-minimized')) restorable++;
      else minable++;
    });
    /* After paintActions, which only knows "nothing ticked". */
    var minKey = document.getElementById('dock-min-ticked');
    if (minKey) minKey.disabled = minable === 0;
    var restoreKey = document.getElementById('dock-restore-ticked');
    if (restoreKey) restoreKey.disabled = restorable === 0;
    Array.prototype.slice.call(document.querySelectorAll('.shell-header-icon-btn')).forEach(function (b) {
      var key = dockKeyForWindow(b._switchWin);
      var on = !!(key && ticked[key]);
      b.classList.toggle('active', on);
      b.setAttribute('aria-pressed', String(on));
    });
  },
  onDelete: function (ids) { dockCloseEntries(ids); }
}) : null;

/** MINIMIZE / RESTORE for the ticked tabs.
 *
 *  They act on the WINDOW behind a tab, never on the session: a tab with no
 *  window has nothing to hide, and pretending otherwise would be a lie about a
 *  session that keeps running either way. Both helpers are the same two calls the
 *  window's own title-bar key makes, so there is one minimizer in this file, not
 *  two that can drift.
 */
function dockTickedWindows(keys) {
  var out = [];
  (keys || []).forEach(function (key) {
    var entry = dockEntryByKey(key);
    if (entry && entry.win) out.push(entry.win);
  });
  return out;
}
function dockMinimizeSelected() {
  if (!_dockBatch) return;
  /* Idempotent per window (minimizeShellWindow returns early when already
     minimized), so a batch that mixes the two states needs no filtering here. */
  dockTickedWindows(_dockBatch.selected()).forEach(minimizeShellWindow);
}
function dockRestoreSelected() {
  if (!_dockBatch) return;
  dockTickedWindows(_dockBatch.selected()).forEach(restoreShellWindow);
  /* restoreShellWindow deliberately does not repaint the strip — a single window's
     own key is followed by a focus that does it — so a batch has to, or the tabs
     keep the minimized styling until the next frame. */
  refreshSessionTabbar();
}

(function wireDockWindowKeys() {
  function wire() {
    var min = document.getElementById('dock-min-ticked');
    var restore = document.getElementById('dock-restore-ticked');
    if (min && !min._termcpWired) {
      min._termcpWired = true;
      min.addEventListener('click', function (e) { e.preventDefault(); dockMinimizeSelected(); });
    }
    if (restore && !restore._termcpWired) {
      restore._termcpWired = true;
      restore.addEventListener('click', function (e) { e.preventDefault(); dockRestoreSelected(); });
    }
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', wire);
  else wire();
})();


/** The dock keys currently listed, in bar order.
 *
 *  Read off the tab strip rather than a registry: every tab carries its key in
 *  data-win-id, the strip is rebuilt from the live entry list on each frame, and
 *  a batch must act on exactly what the user can see — not on a second list that
 *  could hold a tab that has gone.
 *
 *  The key is `win:<id>` for a tab backed by an open window and `sess:<id>` for a
 *  session with no window here. Both forms exist because the tick and the close
 *  act on two different things: a window is a DOM node to raise or drop, a
 *  window-less session is a record to open or stop.
 */
function dockTabKeys() {
  var scroll = document.getElementById('session-tabs-scroll');
  if (!scroll) return [];
  var out = [];
  Array.prototype.slice.call(scroll.children).forEach(function (t) {
    var id = t && t.getAttribute && t.getAttribute('data-win-id');
    if (id) out.push(id);
  });
  return out;
}

/** The dock entry a tab key belongs to, or null once its tab has gone. */
function dockEntryByKey(key) {
  var scroll = document.getElementById('session-tabs-scroll');
  if (!scroll) return null;
  for (var i = 0; i < scroll.children.length; i++) {
    var t = scroll.children[i];
    if (t && t.getAttribute && t.getAttribute('data-win-id') === key) return t._entry || null;
  }
  return null;
}

/** Close the ticked dock entries, one confirm for the whole batch.
 *
 *  The dialog names how many sessions will be stopped, because that is the part
 *  the tab row cannot show: closing a window looks the same as stopping a session,
 *  and only one of them keeps the output.
 */
function dockCloseEntries(keys) {
  var entries = (keys || []).map(dockEntryByKey).filter(function (e) { return !!e; });
  if (!entries.length) return;
  /* Three kinds of entry, and the count has to describe what will ACTUALLY happen
     rather than a subset of it.
   *
   *   stopping  a session that is still behind this entry — it will be archived.
   *             A CONNECTING window belongs here: it already carries its session id
   *             (that is how the dial is addressed), and the close path below sends
   *             that id to /terminate exactly like a connected one. The predicate
   *             used to exclude `_placeholder`, so six windows — four dialling, two
   *             up — announced "stop and archive 2 sessions" and then stopped six.
   *             A confirmation whose number is wrong is worse than no number: it is
   *             read as the scope of the action being confirmed.
   *   extension  a card with no session at all: closing it drops a window.
   *   already-over  a read-only transcript view: also just a window.
   *   window-less  a running session with no window on this page: it is stopped,
   *             and there is no window to drop afterwards.
   *
   * The split is reported as one sentence with the total, because that is the
   * question the dialog is answering — "how many of my six tabs am I closing?" */
  /* `_sid` is the whole test, and `_placeholder` deliberately is NOT part of it.
   *
   * A window is a placeholder while it DIALS, and it starts with `_sid = null`; the
   * session id arrives when the dial succeeds, so a window that is "still connecting"
   * may already own a session and may not. The close path below tests `_sid` alone —
   * it sends /terminate for a real id and merely hangs up a pending dial otherwise —
   * so the count has to use the same test or the two disagree. Excluding
   * `_placeholder` was the bug: it dropped every half-connected window from the count
   * while the close still stopped their sessions, so "6 windows" announced 2. */
  var stopping = entries.filter(function (e) {
    return !!e.sid && !(e.win && e.win._extName) && !isSessionDeadInWindows(e.sid);
  });
  var msg = stopping.length
    ? tCount('dock.close.msg.live.one', 'dock.close.msg.live.other', { count: entries.length })
    : tCount('dock.close.msg.window.one', 'dock.close.msg.window.other', { count: entries.length });
  confirmDialog({
    title: t('dock.close.title'),
    message: msg,
    okText: t('dock.close.ok'),
    danger: stopping.length > 0
  }).then(function (ok) {
    if (!ok) return;
    /* Sequential on purpose: each stop is a POST the server serialises anyway, and
       one at a time keeps the failure report readable ("three archived, one
       failed") instead of a race whose loser is invisible. */
    var failed = [];
    var chain = Promise.resolve();
    entries.forEach(function (entry) {
      chain = chain.then(function () {
        var w = entry.win;
        /* `_sid` alone, and this used to exclude `_placeholder` — which meant a
           window whose dial HAD produced a session but had not finished connecting
           got its window dropped and its session left running on the server: an
           orphan with no tab, visible only in the archive later. A placeholder with
           no id yet has nothing to stop, and closeShellWindow hangs up its dial. */
        if (entry.sid && !(w && w._extName)) {
          /* Stopping the session takes all of its panels, for the same reason the
             single tab's x does: the panels were views of a session that no longer
             exists. */
          var sid = entry.sid;
          return archiveSessionById(sid).then(function () { closeSessionPanels(sid); });
        }
        /* No session behind it: dropping the window IS the close. */
        if (w) closeShellWindow(w);
      }).catch(function (err) { failed.push(String((err && err.message) || err)); });
    });
    chain.then(function () {
      if (_dockBatch) _dockBatch.setMode(false);
      if (failed.length) showCopyToast(t('toast.stop.failed', { msg: failed.join('; ') }));
      else showCopyToast(tCount('dock.close.done.one', 'dock.close.done.other', { count: entries.length }));
    });
  });
}

/** Stop one session — which ARCHIVES it, never erases it.
 *
 *  The page offers no way to erase a session directly any more. A running
 *  session has a transcript worth keeping, and erasing one is archive-then-delete:
 *  a deliberate act, with its own home in the archive column where the thing being
 *  destroyed is visible and named. Putting "delete" next to "stop" on a live tab
 *  made the irreversible choice the one you reach for when you meant the other.
 *
 *  A window whose session is already over just loses its window — there is nothing
 *  to stop, and the archive still holds the record.
 *
 *  Returns a Promise of 'archived' | 'closed' | 'cancelled'.
 */
function askCloseSession(sid, win) {
  /* Two kinds of window carry no session at all: a connecting window has no id
     yet, and a status card puts the extension's NAME in that slot. Neither may
     reach the archive endpoint — the first would post to /sessions/null, the
     second would try to stop a session named after an extension. closing the
     window IS the close for both. */
  if (!sid || (win && (win._placeholder || win._extName))) {
    if (win && !win._windowless) closeShellWindow(win);
    return Promise.resolve('closed');
  }
  var name = stripSessionPrefix(sid);
  if (isSessionDeadInWindows(sid)) {
    closeSessionPanels(sid);
    return Promise.resolve('closed');
  }
  return confirmDialog({
    title: t('session.close.title'),
    message: t('session.close.messageArchives', { name: name }),
    okText: t('session.close.ok'),
    danger: true
  }).then(function (ok) {
    if (!ok) return 'cancelled';
    return archiveSessionById(sid).then(function () {
      closeSessionPanels(sid);
      return 'archived';
    });
  });
}

/** Stop and archive one session. This is the ONLY way the page ends a session:
 *  POST /terminate is the archive move the batch bar makes, and it keeps the
 *  transcript readable in the archive column afterwards.
 */
function archiveSessionById(sid) {
  var banner = document.getElementById('session-load-banner');
  if (banner) setLoadBanner(banner, t('banner.stopping.one', { count: 1 }));
  return fetch(apiPath('/api/sessions/') + encodeURIComponent(sid) + '/terminate', { method: 'POST' })
    .then(function (r) {
      if (banner) setLoadBanner(banner, '');
      if (!r.ok && r.status !== 204) {
        return r.text().then(function (txt) { throw new Error(txt || ('HTTP ' + r.status)); });
      }
      showCopyToast(t('toast.session.stopped.one', { count: 1 }));
    })
    .catch(function (err) {
      if (banner) setLoadBanner(banner, '');
      showCopyToast(t('toast.stop.failed', { msg: String(err.message || err) }));
      throw err;
    });
}

/** Erase a session and its on-disk transcript, then drop its window. */
function deleteSessionById(sid, win) {
  return fetch(apiPath('/api/sessions/') + encodeURIComponent(sid), { method: 'DELETE' })
    .then(function (r) {
      if (!r.ok && r.status !== 204) return r.json().then(function (er) { throw new Error((er && er.error) || 'HTTP ' + r.status); });
      var w = getShellWindowBySid(sid) || win;
      if (w) closeShellWindow(w);
    })
    .catch(function (err) { showCopyToast(t('toast.delete.failed', { msg: (err.message || err) })); });
}

/** The window a session's tab raises.
 *
 *  Any window of the session will do, and every window is a peer: there is no
 *  "own" window to prefer. The one preference left is USEFULNESS — a panel that
 *  still holds channels beats one that a move emptied, because raising the empty
 *  one would look like the session had lost its terminal. */
function getShellWindowBySid(sessionId) {
  if (!sessionId) return null;
  var wins = allShellWins();
  var empty = null;
  for (var i = 0; i < wins.length; i++) {
    /* An extension's status card carries its NAME in _sid so the tab bar can label
       it, and a name is the author's to choose under the same [A-Za-z0-9_-]{1,64}
       rule that a 12-hex session id satisfies. This lookup means "a window for
       this SESSION", so a card (which has no session) is never its answer. */
    var w = wins[i];
    if (w._extName || w._sid !== sessionId) continue;
    if (w._channels && Object.keys(w._channels).length) return w;
    if (!empty) empty = w;
  }
  return empty;
}

/** The window owning a channel id, or null. */
function findShellWindowByChannelSid(sessionId) {
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    if (wins[i]._channels && wins[i]._channels[sessionId]) return wins[i];
  }
  return null;
}

/** The window a terminal frame belongs to: a channel of one window, or a window
 *  whose own id IS the channel. One pass over the windows instead of the two
 *  lookups this used to be.
 *
 * The distinction matters because this runs on the output path, where a busy
 * command delivers frames thousands of times a second: each of those lookups
 * scanned every window, so the pair cost two full DOM scans per frame (measured
 * as the largest single block of script time while output streamed). Both answers
 * come from the same array, so asking it once is the same work minus the scan. */
function shellWindowForFrame(id) {
  var wins = allShellWins();
  var bySid = null;
  for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    if (w._channels && w._channels[id]) return w;
    if (!bySid && w._sid === id) bySid = w;
  }
  return bySid;
}

/** Session tiles and the mobile switcher resolve a session, opening its window
 *  if needed. The tab bar already owns its window, including pending dials.
 *
 *  opts.shell names a channel to land on, for a caller that asked for one
 *  extension's terminal rather than for the session — several extensions can be
 *  served by one connection, so the session alone does not say which one. */
function focusSessionWindow(connLabel, sessionId, pos, opts) {
  var ex = getShellWindowBySid(sessionId);
  if (!ex) {
    openShellWindow(connLabel, sessionId, pos, opts);
    return;
  }
  focusShellWindow(ex, opts);
}

/** Shared focus behavior for session lookup and window-owned tab clicks:
 *  normalise the window's three states (minimized, collapsed, hidden behind the
 *  tab bar's global "hide all") and raise it. Callers that own the window call
 *  this directly — which is what makes the tab bar correct for a pending dial
 *  (no session id yet) and for a status card (whose _sid is the extension's
 *  NAME, so resolving it as a session would dial a terminal for it) without a
 *  lookup at all. */
function focusShellWindow(ex, opts) {
  if (!ex || !ex.parentNode) return;
  opts = opts || {};
  /* Minimized: bring it back before anything else looks at its geometry. */
  restoreShellWindow(ex);
  /* Collapsed to its header: a collapsed window cannot show the terminal the
     user just asked for, so expand it. */
  expandShellWindow(ex);
  if (isTiledWin(ex)) {
    if (_paneMaxedWin && _paneMaxedWin !== ex) { _paneMaxedWin = null; applyPaneGrid(); }
    try { ex.scrollIntoView({ block: 'nearest', inline: 'nearest' }); } catch (e) {}
  }
  bringShellWindowToFront(ex);
  /* A window that is already up switches to the requested channel directly; a
     freshly opened one does it once its tabs have been restored (openShellWindow
     reads the same option). */
  if (opts.shell && ex._channels && ex._channels[opts.shell] && typeof switchChannelTab === 'function') {
    switchChannelTab(ex, opts.shell);
  }
  if (opts.readOnly) lockWindowReadonly(ex);
  /* The tab bar's own "hide all" is a global state; showing one window has to
     clear it or the window we just raised stays invisible. */
  if (_winsHidden) showAllWindows();
  var ch = ex._activeChannelSid && ex._channels && ex._channels[ex._activeChannelSid];
  var t = ch ? ch.term : ex._term;
  if (t) { try { t.focus(); } catch (e) {} }
}

// Session the shared forward modal is currently bound to, set by
// openForwardModal and read by createForward.
var _fwdSessionId = '';

/* Language switch: three idempotent redraws of state this module already holds
   (no requests, no window rebuilds). */
onLangChange(function () {
  updateTileToggleButton();
  updateSessionTabbarToggle();
  refreshSessionTabbar();
});
