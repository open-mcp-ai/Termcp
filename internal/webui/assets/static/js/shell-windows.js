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
 *  Order: open windows in DOM order, then window-less sessions by creation time.
 *  The sort matters more than it looks — the snapshot comes from a Go map, so
 *  ListAll order is random, and rendering it as it arrives would reshuffle the tabs
 *  on every frame. A bar that jumps under the pointer is unusable for clicking.
 */
function dockEntries() {
  var snapshot = window._lastSessionsSnapshot || [];
  var byId = Object.create(null);
  snapshot.forEach(function (s) { if (s && s.id) byId[s.id] = s; });
  var entries = [];
  var covered = Object.create(null);
  allShellWins().forEach(function (w) {
    /* A status card carries the extension's NAME in `_sid`, so it is not a session
       and must not claim one's tab. */
    var sid = (!w._extName && w._sid) ? w._sid : '';
    if (!sid) { entries.push({ key: 'win:' + w.id, win: w, sid: '', session: null }); return; }
    covered[sid] = true;
    entries.push({ key: 'sess:' + sid, win: w, sid: sid, session: byId[sid] || null });
  });
  var extra = [];
  snapshot.forEach(function (s) {
    if (!s || !s.id || s.status !== 'running') return;
    if (covered[s.id]) return;
    /* sessions.js loads AFTER this file, and the page's first paint runs before
       it, so the predicate is asked for by name rather than assumed present. */
    if (typeof isHiddenSession === 'function' && isHiddenSession(s)) return;
    extra.push(s);
  });
  extra.sort(function (a, b) {
    var ta = Number(a.created_at) || 0, tb = Number(b.created_at) || 0;
    if (ta !== tb) return ta - tb;
    return String(a.id) < String(b.id) ? -1 : 1;
  });
  extra.forEach(function (s) {
    entries.push({ key: 'sess:' + s.id, win: null, sid: s.id, session: s });
  });
  return entries;
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
  var bar = sessionTabbarEl();
  if (!bar) return;
  var wins = allShellWins();
  var entries = dockEntries();
  // The empty marker rides the tab strip's place: it says why the row is bare
  // (no windows) without pretending a window exists.
  paintSessdockEmpty(entries.length === 0);
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
        if (!cur || !cur.win) {
          if (cur && cur.session) {
            focusSessionWindow(cur.session.name || '', cur.session.id, windowPositionFromClick(e), null);
          }
          return;
        }
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
      /* A tab is the ONLY place a session is listed now that the session plate is
         gone, so the per-session actions that used to live on its card have to be
         reachable from here or they are unreachable: the approval gate, the
         rename, and copying the session URL. They go behind a context menu rather
         than onto the tab's face — a tab is 28px tall and already carries a label,
         a review badge and a close key, and a lock glyph among them would be a
         fourth unlabelled icon in a row of them. */
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
       read the same whether or not this page happens to hold its window. */
    var label = entry.session ? sessionDockLabel(entry.session) : sessionTabLabel(w);
    tab.querySelector('.session-tab-label').textContent = label;
    tab.title = label;
    tab.classList.toggle('active', !!w && w === topWin);
    tab.setAttribute('aria-selected', (!!w && w === topWin) ? 'true' : 'false');
    /* A window-less entry is still a session with a state, so the tab says which:
       no window here is a fact about the page, not about the session. */
    tab.classList.toggle('is-windowless', !w);
    if (!w) tab.setAttribute('data-sid', entry.sid);
    else tab.removeAttribute('data-sid');
    /* The batch component owns both the mode and the ticked set, and paints the
       ticked class over the grid it was handed (this strip). All this loop does is
       expose the key the component indexes by and show the empty tick slot while
       the mode is on — the slot itself must exist in the DOM at all times, or the
       tab's width would change under the pointer the moment selection starts. */
    tab.setAttribute('data-win-id', entry.key);
    /* The tick is always visible on every tab: there is no mode to reveal it in,
       and a glyph that appeared and disappeared would change the tab's width under
       the pointer whenever a selection started or ended. */
    order.push(tab);
  });
  order.forEach(function (t) { scroll.appendChild(t); });
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

/** The tab's x: close the WINDOW, or stop the SESSION when there is no window.
 *
 *  Those are two different acts and the icon cannot express the difference, so a
 *  window-less entry asks: stopping a session you cannot see the terminal of
 *  should never be one misclick away. */
function closeDockEntry(entry) {
  if (!entry) return;
  if (entry.win) { askCloseTabWindow(entry.win); return; }
  var s = dockEntrySession(entry);
  if (!s) return;
  askCloseSession(s.id, dockSessionTarget(s));
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
      hint: t(gated ? 'review.disableTip' : 'review.enableTip'),
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

// Fixed action cluster at the right end of the session tab bar (Chrome-style):
// eye (show/hide) · layout toggle (float<->tile, highlighted while tiled) · strategy select.
(function () {
  var actions = document.getElementById('session-tabbar-actions');
  if (!actions) return;
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
  if (!document.getElementById('session-tabbar-strategy')) {
    var sel = document.createElement('select');
    sel.id = 'session-tabbar-strategy';
    sel.className = 'session-tabbar-select';
    sel.setAttribute('data-i18n-title', 'pane.strategy.title');
    sel.innerHTML = '<option value="auto" data-i18n="pane.strategy.auto">Auto</option><option value="cols" data-i18n="pane.strategy.cols">Columns</option><option value="rows" data-i18n="pane.strategy.rows">Rows</option><option value="pairs" data-i18n="pane.strategy.pairs">2 cols</option>';
    applyI18n(sel);
    try {
      var saved = localStorage.getItem('termcp_pane_strategy');
      if (saved) sel.value = saved;
      _paneStrategy = sel.value;
    } catch (e) {}
    sel.addEventListener('change', function () {
      _paneStrategy = sel.value;
      _paneColFr = [];
      try { localStorage.setItem('termcp_pane_strategy', sel.value); } catch (e) {}
      applyPaneGrid();
    });
    actions.appendChild(sel);
  }
})();

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
        allShellWins().forEach(function (w) {
          if (isTiledWin(w)) return;
          if (mobile) {
            applyWindowViewportMode(w);
          } else {
            /* Leaving mobile: drop full-screen and re-seat the window as a
               floating one, since it has no inline geometry to fall back on. */
            w.classList.remove('win-fullscreen');
            if (!w._maxed) positionShellWindowFromClick(w, null);
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
var _wasMobileViewport = (function () {
  if (!window.matchMedia) return false;
  try {
    return window.matchMedia('(pointer: coarse)').matches &&
           window.matchMedia('(hover: none)').matches;
  } catch (e) {
    return false;
  }
})();

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
try { if (localStorage.getItem('termcp_layout_mode') === 'tile') _layoutMode = 'tile'; } catch (e) {}
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

/** True on touch-first devices (phones, tablets) in any orientation.
 *
 *  Deliberately NOT a width query: a phone in landscape is ~844px wide, which
 *  fails `max-width: 768px` and would leave it with a desktop floating window.
 *  Touch capability is the real signal, and it is orientation-independent.
 *  The `no-touch` escape hatch lets a narrow desktop window stay in floating
 *  mode, where a mouse-driven UI is still the right model. */
function isMobileViewport() {
  if (!window.matchMedia) return false;
  try {
    return window.matchMedia('(pointer: coarse)').matches &&
           window.matchMedia('(hover: none)').matches;
  } catch (e) {
    return false;
  }
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
  var r = win.getBoundingClientRect();
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

/** "x" button: close the session.
 *
 *  A running session is NOT destroyed by this key. It asks what to do with it,
 *  because the two outcomes are very different and the icon cannot express the
 *  difference: archiving disconnects the session and keeps its output readable
 *  in the archive (recoverable — its transcript is still there), while deleting
 *  erases the on-disk directory for good. The key used to delete outright, which
 *  made one misclick on a live terminal unrecoverable and gave the archive no way
 *  to grow other than the batch bar.
 *
 *  The dialog offers the same vocabulary the batch bar already uses for the same
 *  two actions (Stop/Archive vs Delete), and for an already-archived session it
 *  keeps the plain delete confirmation: there is nothing left to archive. */
function bindShellWindowCloseButton(win, closeBtn) {
  if (!closeBtn || closeBtn._termcpCloseBound) return;
  closeBtn._termcpCloseBound = true;
  closeBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  closeBtn.addEventListener('click', function (e) {
    e.preventDefault(); e.stopPropagation();
    var sid = win && win._sid;
    if (!sid || win._placeholder) { closeShellWindow(win); return; }
    askCloseSession(sid, win);
  });
}

/** The dock tab's x.
 *
 *  It used to call closeShellWindow outright, which destroyed the window and
 *  left the session running with nothing on the page pointing at it. That was
 *  survivable while the session list was a permanent plate: the card was still
 *  there to click. It is not survivable now — the dock is the only place the
 *  open sessions appear, so "close the window" and "lose the session" became the
 *  same gesture, and a misclick on a tab cost the user their terminal.
 *
 *  So the tab's x now asks the same question the window header's x asks, through
 *  the same dialog: cancel / archive / delete. The three cases that are NOT a
 *  session (an extension's status card, a dial still in flight) have nothing to
 *  archive or delete, so they still just close. */
function askCloseTabWindow(win) {
  if (!win) return;
  var sid = win._sid;
  if (!sid || win._placeholder || win._extName) { closeShellWindow(win); return; }
  askCloseSession(sid, win);
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
  actionIds: ['dock-close-ticked'],
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
    Array.prototype.slice.call(document.querySelectorAll('#session-tabs-scroll [data-win-id]')).forEach(function (t) {
      if (t.classList.contains('batch-selected')) ticked[t.getAttribute('data-win-id')] = 1;
    });
    Array.prototype.slice.call(document.querySelectorAll('.shell-header-icon-btn')).forEach(function (b) {
      var key = dockKeyForWindow(b._switchWin);
      var on = !!(key && ticked[key]);
      b.classList.toggle('active', on);
      b.setAttribute('aria-pressed', String(on));
    });
  },
  onDelete: function (ids) { dockCloseEntries(ids); }
}) : null;


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
          return archiveSessionById(entry.sid).then(function () { if (w) closeShellWindow(w); });
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
  if (!win || !sid || win._placeholder || win._extName) {
    if (win) closeShellWindow(win);
    return Promise.resolve('closed');
  }
  var name = stripSessionPrefix(sid);
  if (isSessionDeadInWindows(sid)) {
    closeShellWindow(win);
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
      closeShellWindow(win);
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
      showCopyToast(t('dock.close.done.one', { count: 1 }));
    })
    .catch(function (err) {
      if (banner) setLoadBanner(banner, '');
      showCopyToast(t('toast.stop.failed', { msg: String(err.message || err) }));
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

function getShellWindowBySid(sessionId) {
  if (!sessionId) return null;
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    /* An extension's status card carries its NAME in _sid so the tab bar can label
       it, and a name is the author's to choose under the same [A-Za-z0-9_-]{1,64}
       rule that a 12-hex session id satisfies. This lookup means "the window for
       this SESSION", so a card (which has no session) is never its answer —
       without this an extension named after a session id would be raised, notified
       or fed that session's output instead of the session's own window. */
    if (wins[i]._extName) continue;
    if (wins[i]._sid === sessionId) return wins[i];
  }
  return null;
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
