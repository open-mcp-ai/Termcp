function sessionTabbarEl() {
  return document.getElementById('session-tabbar');
}

function sessionTabLabel(win) {
  if (win._placeholder) return (win._pendingConnName || t('tab.connecting')) + '…';
  if (win._connName) return win._connName + ' · ' + (win._sid ? stripSessionPrefix(win._sid) : '');
  return win._sid ? stripSessionPrefix(win._sid) : t('tab.session');
}

/** Open-order sequence number (matches the shell-window-N counter). */
function sessionTabNum(win) {
  var m = /^shell-window-(\d+)$/.exec(win.id || '');
  return m ? parseInt(m[1], 10) : 0;
}

/** Rebuild tabs from open .shell-window elements; the topmost (max z-index) is active. */
function refreshSessionTabbar() {
  var bar = sessionTabbarEl();
  if (!bar) return;
  var wins = allShellWins();
  // Keep the bar (and its layout controls) visible in tile mode even with no windows,
  // so the tiled overlay always offers an exit; float mode hides it when empty.
  bar.classList.toggle('hidden', wins.length === 0 && _layoutMode !== 'tile');
  if (!bar.classList.contains('hidden')) restoreSessionTabbarPos(bar);
  var topWin = null;
  if (_layoutMode === 'tile') {
    topWin = (_paneActiveWin && isTiledWin(_paneActiveWin)) ? _paneActiveWin : null;
  } else {
    var topZ = -1;
    wins.forEach(function (w) {
      if (isTiledWin(w)) return;
      /* A minimized window keeps its inline z-index but is not on screen, so it
         must not win the "which tab is active" test. */
      if (w.classList.contains('win-minimized')) return;
      /* A full-screen window's z-index usually comes from bringShellWindowToFront;
         when it does not (e.g. a resize cleared it), fall back to the stylesheet's
         value. Ties are real at that point — same computed z-index — and the one
         later in the DOM paints on top, so >= picks the visible one. */
      var z = parseInt(w.style.zIndex, 10);
      if (isNaN(z)) z = FULLSCREEN_Z;
      if (z >= topZ) { topZ = z; topWin = w; }
    });
  }
  // Reuse existing tab nodes keyed by window id to keep hover state and avoid flicker.
  var scroll = document.getElementById('session-tabs-scroll');
  if (!scroll) return;
  var byId = {};
  Array.prototype.slice.call(scroll.children).forEach(function (t) {
    if (t._win && t._win.parentNode) byId[t._win.id] = t;
    else t.remove();
  });
  var order = [];
  wins.forEach(function (w) {
    var tab = byId[w.id];
    if (!tab) {
      tab = document.createElement('button');
      tab.type = 'button';
      tab.className = 'session-tab';
      tab.setAttribute('role', 'tab');
      tab._win = w;
      tab.innerHTML = '<span class="session-tab-num"></span>' +
        '<span class="session-tab-label"></span>' +
        '<span class="session-tab-close" role="button" title="Close window (session keeps running)" data-i18n-title="tab.closeWindow" aria-label="Close window" data-i18n-aria="tab.aria.closeWindow">' +
        '<svg viewBox="0 0 12 12" width="10" height="10"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg></span>';
      tab.addEventListener('click', function (e) {
        if (e.target.closest('.session-tab-close')) return;
        /* Same entry point as the session tiles and the mobile switcher: it
           restores a minimized window and expands a collapsed one before
           raising it. */
        focusSessionWindow(w._connName || '', w._sid, null, null);
      });
      tab.addEventListener('mousedown', function (e) {
        if (e.button === 1) { // middle-click closes the window
          e.preventDefault();
          closeShellWindow(w);
        }
      });
      var x = tab.querySelector('.session-tab-close');
      x.addEventListener('click', function (e) {
        e.preventDefault(); e.stopPropagation();
        closeShellWindow(w);
      });
    }
    tab.querySelector('.session-tab-num').textContent = sessionTabNum(w);
    tab.querySelector('.session-tab-label').textContent = sessionTabLabel(w);
    tab.title = sessionTabLabel(w);
    tab.classList.toggle('active', w === topWin);
    tab.setAttribute('aria-selected', w === topWin ? 'true' : 'false');
    order.push(tab);
  });
  order.forEach(function (t) { scroll.appendChild(t); });
}

// Keep the tab bar in sync as shell windows are opened/closed anywhere.
(function () {
  var c = shellWindowsEl();
  if (!c || typeof MutationObserver === 'undefined') return;
  new MutationObserver(function () { refreshSessionTabbar(); }).observe(c, { childList: true });
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
        if (typeof w._syncSwitcherAffordance === 'function') w._syncSwitcherAffordance();
        if (w.classList.contains('win-hidden') || w.classList.contains('win-minimized')) return;
        _fitWinSoon(w);
      });
      refreshSessionTabbar();
    });
  }
  window.addEventListener('resize', onViewportChange);
  window.addEventListener('orientationchange', onViewportChange);
})();

/** Ensure the tiled overlay is visible (e.g. first window after restoring a saved tile preference). */
function openPaneWorkspace() {
  var ws = paneWorkspaceEl();
  if (!ws) return;
  ws.classList.add('open');
  ws.setAttribute('aria-hidden', 'false');
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
  refreshSessionTabbar();
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

function hideAllWindows() {
  _winsHidden = true;
  var ws = paneWorkspaceEl();
  if (ws && _layoutMode === 'tile') { ws.classList.remove('open'); ws.setAttribute('aria-hidden', 'true'); }
  allShellWins().forEach(function (w) {
    if (!isTiledWin(w)) w.classList.add('win-hidden');
  });
  updateSessionTabbarToggle();
}

function showAllWindows() {
  _winsHidden = false;
  var ws = paneWorkspaceEl();
  if (ws && _layoutMode === 'tile') { ws.classList.add('open'); ws.setAttribute('aria-hidden', 'false'); applyPaneGrid(); }
  allShellWins().forEach(function (w) { w.classList.remove('win-hidden'); _fitWinSoon(w); });
  updateSessionTabbarToggle();
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

/** Drag the floating session tab bar by its left grip.
 *  The bar is centred with left:50% + translateX(-50%), so a drag switches it to
 *  an explicit left/top and drops the centring transform. The position is kept in
 *  localStorage and re-applied after re-renders, which only touch the inner tab list. */
function setupSessionTabbarDrag(bar) {
  if (!bar || bar._termcpDragBound) return;
  var grip = document.getElementById('session-tabbar-grip');
  if (!grip) return;
  bar._termcpDragBound = true;

  function clamp(left, top) {
    var r = bar.getBoundingClientRect();
    var maxLeft = Math.max(0, window.innerWidth - r.width);
    var maxTop = Math.max(0, window.innerHeight - r.height);
    return { left: Math.max(0, Math.min(left, maxLeft)), top: Math.max(0, Math.min(top, maxTop)) };
  }
  function place(left, top) {
    bar.style.transform = 'none';
    bar.style.left = left + 'px';
    bar.style.top = top + 'px';
  }

  var drag = { active: false, id: null, dx: 0, dy: 0 };
  function onMove(e) {
    if (!drag.active || e.pointerId !== drag.id) return;
    e.preventDefault();
    var p = clamp(e.clientX - drag.dx, e.clientY - drag.dy);
    place(p.left, p.top);
  }
  function finish(e) {
    if (!drag.active || (e && e.pointerId !== drag.id)) return;
    drag.active = false;
    bar.classList.remove('dragging');
    document.body.style.userSelect = '';
    if (grip.hasPointerCapture && grip.hasPointerCapture(drag.id)) {
      try { grip.releasePointerCapture(drag.id); } catch (e2) {}
    }
    var r = bar.getBoundingClientRect();
    try { localStorage.setItem('termcp_tabbar_pos', JSON.stringify({ left: r.left, top: r.top })); } catch (e3) {}
    drag.id = null;
    window.removeEventListener('pointermove', onMove);
    window.removeEventListener('pointerup', finish);
    window.removeEventListener('pointercancel', finish);
    window.removeEventListener('blur', finish);
  }
  grip.addEventListener('pointerdown', function (e) {
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    e.preventDefault();
    e.stopPropagation();
    var r = bar.getBoundingClientRect();
    drag.active = true;
    drag.id = e.pointerId;
    drag.dx = e.clientX - r.left;
    drag.dy = e.clientY - r.top;
    bar.classList.add('dragging');
    document.body.style.userSelect = 'none';
    if (grip.setPointerCapture) {
      try { grip.setPointerCapture(drag.id); } catch (e2) {}
    }
    window.addEventListener('pointermove', onMove, { passive: false });
    window.addEventListener('pointerup', finish);
    window.addEventListener('pointercancel', finish);
    window.addEventListener('blur', finish);
  });
}

/** Re-apply a saved tab bar position (called after the bar is shown or re-rendered). */
function restoreSessionTabbarPos(bar) {
  var saved;
  try { saved = JSON.parse(localStorage.getItem('termcp_tabbar_pos') || 'null'); } catch (e) { return; }
  if (!saved || typeof saved.left !== 'number' || typeof saved.top !== 'number') return;
  var r = bar.getBoundingClientRect();
  var maxLeft = Math.max(0, window.innerWidth - r.width);
  var maxTop = Math.max(0, window.innerHeight - r.height);
  bar.style.transform = 'none';
  bar.style.left = Math.max(0, Math.min(saved.left, maxLeft)) + 'px';
  bar.style.top = Math.max(0, Math.min(saved.top, maxTop)) + 'px';
}

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

// Restore last mode preference on load; keep the toggle button and tab bar in
// sync so a restored "tile" preference is reflected before the first window opens.
try { if (localStorage.getItem('termcp_layout_mode') === 'tile') _layoutMode = 'tile'; } catch (e) {}
updateTileToggleButton();
refreshSessionTabbar();
if (sessionTabbarEl()) setupSessionTabbarDrag(sessionTabbarEl());

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
    win.style.left = '8px'; win.style.top = '8px';
    /* dvh, not vh: iOS Safari's 100vh includes the collapsing address bar, so a
       vh-based maximize leaves the bottom of the terminal cut off. */
    win.style.width = 'calc(100vw - 16px)'; win.style.height = 'calc(100dvh - 16px)';
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

/** Initial placement for a new .shell-window (caller must have incremented shellWindowCount).
 *  On mobile the window fills the viewport and CSS owns its geometry (see
 *  applyWindowViewportMode). Otherwise it starts at 640x480, clamped to the
 *  viewport: a phone in landscape is only ~390px tall, so a fixed 480px window
 *  would hang off the bottom with its toolbar unreachable. */
function positionShellWindowFromClick(win, clickEvent) {
  if (isMobileViewport()) { applyWindowViewportMode(win); return; }
  var MARGIN = 8;
  var winW = Math.min(640, window.innerWidth - MARGIN * 2);
  var winH = Math.min(480, window.innerHeight - MARGIN * 2);
  win.style.width = winW + 'px';
  win.style.height = winH + 'px';
  var th = 21;
  var left, top;
  if (clickEvent && typeof clickEvent.clientX === 'number') {
    left = Math.max(MARGIN, Math.min(clickEvent.clientX - winW / 2, window.innerWidth - winW - MARGIN));
    top = Math.max(MARGIN, Math.min(clickEvent.clientY - th, window.innerHeight - winH - MARGIN));
  } else {
    var off = (shellWindowCount - 1) % 5;
    left = Math.max(MARGIN, 80 + off * 24);
    top = Math.max(MARGIN, 60 + off * 24);
  }
  win.style.left = left + 'px';
  win.style.top = top + 'px';
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

/** "x" button: delete the session — same as the session tile's x. A pending
 *  window has no session yet, so it just cancels the connect. */
function bindShellWindowCloseButton(win, closeBtn) {
  if (!closeBtn || closeBtn._termcpCloseBound) return;
  closeBtn._termcpCloseBound = true;
  closeBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  closeBtn.addEventListener('click', function (e) {
    e.preventDefault(); e.stopPropagation();
    var sid = win && win._sid;
    if (!sid || win._placeholder) { closeShellWindow(win); return; }
    confirmDialog({
      title: t('session.delete.title'),
      message: t('session.delete.message', { name: stripSessionPrefix(sid) }),
      okText: t('common.delete'),
      danger: true
    }).then(function (ok) {
      if (!ok) return;
      fetch('/api/sessions/' + encodeURIComponent(sid), { method: 'DELETE' })
        .then(function (r) {
          if (!r.ok && r.status !== 204) return r.json().then(function (er) { throw new Error((er && er.error) || 'HTTP ' + r.status); });
          var w = getShellWindowBySid(sid) || win;
          if (w) closeShellWindow(w);
        })
        .catch(function (err) { showCopyToast(t('toast.delete.failed', { msg: (err.message || err) })); });
    });
  });
}

function getShellWindowBySid(sessionId) {
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    if (wins[i]._sid === sessionId) return wins[i];
  }
  return null;
}

function findShellWindowByChannelSid(sessionId) {
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    if (wins[i]._channels && wins[i]._channels[sessionId]) return wins[i];
  }
  return null;
}

/** The one entry point for "the user picked this session". Every trigger routes
 *  here: the session tiles, the mobile switcher, and the session tab bar. It
 *  normalises the three states a window can be in (minimized, collapsed, tiled)
 *  before raising it, so callers never have to know about any of them. */
function focusSessionWindow(connLabel, sessionId, clickEvent, opts) {
  opts = opts || {};
  var ex = getShellWindowBySid(sessionId);
  if (!ex) {
    openShellWindow(connLabel, sessionId, clickEvent, opts);
    return;
  }
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
  if (opts.readOnly) lockWindowReadonly(ex);
  /* The tab bar's own "hide all" is a global state; showing one window has to
     clear it or the window we just raised stays invisible. */
  if (_winsHidden) showAllWindows();
  var ch = ex._activeChannelSid && ex._channels && ex._channels[ex._activeChannelSid];
  var t = ch ? ch.term : ex._term;
  if (t) { try { t.focus(); } catch (e) {} }
}


// Session + ssh profile the shared forward modal is currently bound to, set by
// openForwardModal and read by createForward.
var _fwdSshCfg = '';
var _fwdSessionId = '';

/* Language switch: three idempotent redraws of state this module already holds
   (no requests, no window rebuilds). */
onLangChange(function () {
  updateTileToggleButton();
  updateSessionTabbarToggle();
  refreshSessionTabbar();
});
