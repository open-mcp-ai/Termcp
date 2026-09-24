function uiWebSocketURL() {
  var s = location.protocol === 'https:' ? 'wss' : 'ws';
  return s + '://' + location.host + '/api/ui/ws';
}

function wsUiSend(obj) {
  if (window._uiWS && window._uiWS.readyState === 1) {
    window._uiWS.send(JSON.stringify(obj));
  }
}

/**
 * xterm may emit focus-report sequences on attach; forwarding them to tmux/vim causes noise.
 * Do not forward onData until the user interacts in the terminal (mousedown / keydown / paste / compositionend).
 */
function setupShellOnDataUserGate(win, term) {
  // Per-window gate: once any terminal has been interacted with, all are unlocked.
  if (win._shellUserInputReady) return;
  win._shellUserInputReady = false;
  var el = term.element;
  if (!el) return;
  if (win._shellInputUnlockAC) {
    try { win._shellInputUnlockAC.abort(); } catch (e0) {}
    win._shellInputUnlockAC = null;
  }
  var ac = new AbortController();
  win._shellInputUnlockAC = ac;
  var opts = { capture: true, signal: ac.signal };
  function unlock() {
    win._shellUserInputReady = true;
    try { ac.abort(); } catch (e1) {}
    if (win._shellInputUnlockAC === ac) win._shellInputUnlockAC = null;
  }
  el.addEventListener('mousedown', unlock, opts);
  el.addEventListener('keydown', unlock, opts);
  el.addEventListener('paste', unlock, opts);
  el.addEventListener('compositionend', unlock, opts);
}

/**
 * xterm registers mousedown on the root .xterm and always calls preventDefault() — that cancels the
 * browser default for scrollbar thumb/track. Events from the viewport bubble .xterm → stop there in the gutter only.
 */
function attachXtermViewportScrollbarGuard(term) {
  try {
    var root = term && term.element;
    if (!root) return;
    var vp = root.querySelector('.xterm-viewport');
    if (!vp || vp._termcpSbGuard) return;
    vp._termcpSbGuard = true;
    var gutterPx = 28;
    vp.addEventListener('mousedown', function (e) {
      try {
        var r = vp.getBoundingClientRect();
        if (r.width <= 0) return;
        if (e.clientX > r.right - gutterPx) e.stopPropagation();
      } catch (e0) {}
    }, false);
  } catch (e1) {}
}

/**
 * Browsers may handle Ctrl+W (close tab) before the terminal; we preventDefault on keydown then let xterm parse (e.g. vim Ctrl+W).
 * Some browsers/OS may still intercept; not 100% guaranteed.
 */
function attachTermBrowserChordShield(term) {
  if (typeof term.attachCustomKeyEventHandler !== 'function') return;
  term.attachCustomKeyEventHandler(function (event) {
    if (event.type !== 'keydown') return true;
    if (!event.ctrlKey || event.altKey || event.metaKey) return true;
    var k = event.key;
    if (k === 'w' || k === 'W') {
      try { event.preventDefault(); } catch (e) {}
    }
    return true;
  });
}

function sendTerminalWatch(sid, add) {
  if (!sid) return;
  wsUiSend({ type: add ? 'watch_add' : 'watch_remove', id: sid });
}

function flushPendingTerminalWatches() {
  if (!window._pendingTerminalWatch) return;
  Object.keys(window._pendingTerminalWatch).forEach(function (sid) {
    sendTerminalWatch(sid, true);
  });
  window._pendingTerminalWatch = Object.create(null);
}

function resubscribeAllTerminalWatches() {
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    if (w._channels) {
      Object.keys(w._channels).forEach(function(sid) {
        var ch = w._channels[sid];
        if (!ch.streamDone) sendTerminalWatch(sid, true);
      });
    } else if (w._sid && !w._streamDone) {
      sendTerminalWatch(w._sid, true);
    }
  }
}

function queueTerminalWatch(sid) {
  if (!sid) return;
  if (!window._pendingTerminalWatch) window._pendingTerminalWatch = Object.create(null);
  if (window._uiWS && window._uiWS.readyState === 1) {
    sendTerminalWatch(sid, true);
  } else {
    window._pendingTerminalWatch[sid] = true;
  }
}

/** Single WebSocket: session list + terminals + input; exponential backoff (manual refresh uses startUIWebSocket). */
function connectUIWebSocket() {
  clearRetryTimer(window._sessionListSSERetryTimer);
  window._sessionListSSERetryTimer = null;
  var old = window._uiWS;
  window._uiWS = null;
  if (old) {
    try { old.close(); } catch (e) {}
  }
  var ws = new WebSocket(uiWebSocketURL());
  window._uiWS = ws;

  ws.onmessage = function (ev) {
    try {
      var j = JSON.parse(ev.data);
      if (j.type === 'sessions') {
        // The session registry retains DEAD entries, so the same snapshot
        // reconciles running and read-only windows without an archive cache.
        applySessionsSnapshot(j.sessions || []);
        if (j.connections) renderConnGrid(j.connections, '');
        // Notification rules share this wake signal (notify.Manager onChange →
        // sessionHub.broadcast), so keep the rule panels in sync too.
        loadNotifications();
      }
      else if (j.type === 'terminal') {
        var win = getShellWindowBySid(j.id);
        if (!win) win = findShellWindowByChannelSid(j.id);
        if (win) {
          var ch = win._channels && win._channels[j.id];
          var tm = ch ? ch.term : win._term;
          var done = ch ? ch.streamDone : win._streamDone;
          if (tm && !done) {
            var bytes = textToBytes(j.d);
            tm.write(bytes, function () {
              requestAnimationFrame(function () {
                shellTermScrollToBottomIfStuck(tm, true);
              });
            });
          }
        }
      } else if (j.type === 'terminal_done') {
        var win2 = getShellWindowBySid(j.id);
        if (!win2) win2 = findShellWindowByChannelSid(j.id);
        if (win2) {
          var ch2 = win2._channels && win2._channels[j.id];
          if (ch2) {
            // End-of-stream for one channel: a whole-session disconnect (DEAD)
            // or a single child-shell exit. Print the marker on EVERY ending
            // channel and stop its stream. Never close/delete the shell here —
            // reconciliation handles tab lifecycle (DEAD sessions lock read-only
            // and keep tabs; an exited child is pruned by syncWindowTabs).
            // Deliberately not gated on streamDone/_lockedReadonly: the DEAD
            // sessions frame may lock the window before the per-shell terminal_done
            // frames arrive, and gating would print exactly one shell's marker.
            // A per-channel endedMarkPrinted flag guards against re-print if a
            // watch re-subscribes after a reconnect and re-emits terminal_done.
            ch2.streamDone = true;
            showTerminalEndedMarker(win2, ch2);
          } else if (!win2._streamDone) {
            win2._streamDone = true;
            try {
              win2._term.writeln('\r\n\x1b[33m' + t('term.ended') + '\x1b[0m', function () {
                shellTermScrollToBottomIfStuck(win2._term, true);
              });
            } catch (e2) {
              try { win2._term.writeln('\r\n\x1b[33m' + t('term.ended') + '\x1b[0m'); } catch (e3) {}
              shellTermScrollToBottomIfStuck(win2._term, true);
            }
          }
        }
      } else if (j.type === 'ui_notify') {
        // Pushed by the MCP notify_user tool: toast + system notification +
        // optional session-card highlight.
        showUiNotify(j);
      }
    } catch (e) { console.error(e); }
  };

  ws.onopen = function () {
    window._sessionListSSEBackoff = 1000;
    // Avoid redrawing from a stale snapshot: first _lastSessionsSnapshot may be undefined→[] and clear tiles
    // before the first sessions frame arrives.
    setLoadBanner(document.getElementById('session-load-banner'), t('banner.syncing'));
    flushPendingTerminalWatches();
    resubscribeAllTerminalWatches();
  };

  ws.onerror = function () {};

  ws.onclose = function () {
    if (window._uiWS !== ws) return;
    window._uiWS = null;
    if (window._sessionListSSERetryTimer) return;
    var delay = window._sessionListSSEBackoff || 1000;
    window._sessionListSSEBackoff = sseBackoffNext(delay);
    renderSessionGrid(window._lastSessionsSnapshot || [], t('banner.reconnecting', { s: Math.ceil(delay / 1000) }));
    window._sessionListSSERetryTimer = setTimeout(function () {
      window._sessionListSSERetryTimer = null;
      connectUIWebSocket();
    }, delay);
  };
}

function startUIWebSocket() {
  window._sessionListSSEBackoff = 1000;
  connectUIWebSocket();
}

/** Register a window-scoped teardown. Every global listener, timer or
 *  abortable request owned by a shell window goes through here so that
 *  closing the window can never leave one behind (the old code cleaned
 *  channel tabs by hand and silently leaked per-window document listeners). */
function addWinDisposable(win, disposeFn) {
  if (!win || typeof disposeFn !== 'function') return;
  if (!win._disposables) win._disposables = [];
  win._disposables.push(disposeFn);
}

/** Run every window-scoped teardown exactly once, LIFO. */
function disposeWinResources(win) {
  if (!win || !win._disposables || !win._disposables.length) return;
  var list = win._disposables;
  win._disposables = [];
  for (var i = list.length - 1; i >= 0; i--) {
    try { list[i](); } catch (e) {}
  }
}

function closeShellWindow(win) {
  // Remember active tab for reconnect.
  if (win._parentSid && win._activeChannelSid) {
    if (!window._shellLastActive) window._shellLastActive = {};
    window._shellLastActive[win._parentSid] = win._activeChannelSid;
  }
  win._inputClosed = true;
  if (win._connectAbort) {
    try { win._connectAbort.abort(); } catch (e) {}
    win._connectAbort = null;
  }
  // Clean up channel tabs
  var usedChannels = false;
  if (win._channels) {
    Object.keys(win._channels).forEach(function(sid) {
      var ch = win._channels[sid];
      sendTerminalWatch(sid, false);
      if (ch.fitRo && ch.instEl) { try { ch.fitRo.unobserve(ch.instEl); } catch(e) {} }
      if (ch.term) { try { ch.term.dispose(); } catch(e) {} }
      if (sid === win._sid) usedChannels = true;
    });
    win._channels = {};
  }
  if (win._sid) {
    if (!usedChannels) sendTerminalWatch(win._sid, false);
    if (window._pendingTerminalWatch && window._pendingTerminalWatch[win._sid]) {
      delete window._pendingTerminalWatch[win._sid];
    }
  }
  if (win._shellResizeSyncTimer) {
    clearRetryTimer(win._shellResizeSyncTimer);
    win._shellResizeSyncTimer = null;
  }
  if (win._shellInputUnlockAC) {
    try { win._shellInputUnlockAC.abort(); } catch (e) {}
    win._shellInputUnlockAC = null;
  }
  // Release everything registered through addWinDisposable (document-level
  // listeners, drag/resize finish hooks, pending requests).
  disposeWinResources(win);
  win.remove();
}

function setupShellWindowDrag(win, header) {
  if (!header || win._termcpShellDragBound) return;
  win._termcpShellDragBound = true;
  var drag = { active: false, startX: 0, startY: 0, startLeft: 0, startTop: 0 };
  function onMove(e) {
    if (!drag.active) return;
    win.style.left = (drag.startLeft + e.clientX - drag.startX) + 'px';
    win.style.top = (drag.startTop + e.clientY - drag.startY) + 'px';
  }
  function onUp() {
    if (!drag.active) return;
    drag.active = false;
    document.body.style.cursor = '';
    document.removeEventListener('mousemove', onMove);
    document.removeEventListener('mouseup', onUp);
  }
  header.addEventListener('mousedown', function (e) {
    if (e.target.closest('button')) return;
    if (isTiledWin(win)) return; // panes are laid out by the grid, not draggable
    e.preventDefault();
    bringShellWindowToFront(win);
    drag.active = true;
    drag.startX = e.clientX; drag.startY = e.clientY;
    var rect = win.getBoundingClientRect();
    drag.startLeft = rect.left; drag.startTop = rect.top;
    document.body.style.cursor = 'move';
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  });
}

function setupShellWindowResize(win) {
  if (win._termcpShellResizeBound) return;
  var handle = win.querySelector('.shell-window-resize-handle');
  if (!handle) return;
  win._termcpShellResizeBound = true;
  var resizing = false, pointerId = null;
  var startX = 0, startY = 0, startW = 0, startH = 0;
  var pendingW = 0, pendingH = 0, frame = 0;

  function activeChannel() {
    return (win._activeChannelSid && win._channels && win._channels[win._activeChannelSid]) || null;
  }
  function fitActiveChannel(syncRemote) {
    var ch = activeChannel();
    if (ch && ch.term && ch.instEl) return fitShellTerminal(ch.term, ch.instEl, win, syncRemote);
    if (win._term && win._shellTermEl) return fitShellTerminal(win._term, win._shellTermEl, win, syncRemote);
    return false;
  }
  function applyPending(syncRemote) {
    if (pendingW > 0 && pendingH > 0) {
      win.style.width = pendingW + 'px';
      win.style.height = pendingH + 'px';
      /* Measure after the new size is committed, not the previous layout. */
      void win.offsetHeight;
    }
    fitActiveChannel(syncRemote);
  }
  function scheduleApply() {
    if (frame) return;
    frame = window.requestAnimationFrame(function () {
      frame = 0;
      if (resizing) applyPending(false);
    });
  }
  function onMove(e) {
    if (!resizing || e.pointerId !== pointerId) return;
    e.preventDefault();
    pendingW = Math.max(320, startW + e.clientX - startX);
    pendingH = Math.max(240, startH + e.clientY - startY);
    scheduleApply();
  }
  function finish() {
    if (!resizing) return;
    resizing = false;
    if (frame) {
      window.cancelAnimationFrame(frame);
      frame = 0;
    }
    /* Drop any drag-time sync first; the final fit below schedules one
       remote resize for the actual released dimensions. */
    if (win._shellResizeSyncTimer) {
      clearRetryTimer(win._shellResizeSyncTimer);
      win._shellResizeSyncTimer = null;
    }
    applyPending(true);
    if (pointerId !== null && handle.hasPointerCapture && handle.hasPointerCapture(pointerId)) {
      try { handle.releasePointerCapture(pointerId); } catch (e) {}
    }
    pointerId = null;
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
    win.classList.remove('shell-window-resizing');
    window.removeEventListener('pointermove', onMove);
    window.removeEventListener('pointerup', onUp);
    window.removeEventListener('pointercancel', finish);
    window.removeEventListener('blur', finish);
  }
  function onUp(e) {
    if (!resizing || e.pointerId !== pointerId) return;
    finish();
  }
  handle.addEventListener('pointerdown', function (e) {
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    e.preventDefault();
    e.stopPropagation();
    bringShellWindowToFront(win);
    var rect = win.getBoundingClientRect();
    resizing = true;
    pointerId = e.pointerId;
    startX = e.clientX; startY = e.clientY;
    startW = rect.width; startH = rect.height;
    pendingW = startW; pendingH = startH;
    document.body.style.cursor = 'nwse-resize';
    document.body.style.userSelect = 'none';
    win.classList.add('shell-window-resizing');
    if (handle.setPointerCapture) {
      try { handle.setPointerCapture(pointerId); } catch (e2) {}
    }
    /* Pointer capture keeps the drag alive outside the handle; blur and
       pointercancel clean up when the browser loses the pointer instead. */
    window.addEventListener('pointermove', onMove, { passive: false });
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', finish);
    window.addEventListener('blur', finish);
  });
}

function postResize(win, optSid) {
  var sid = optSid || win._activeChannelSid || win._sid;
  if (!sid) return;
  var ch = win._channels && win._channels[sid];
  var term = ch ? ch.term : win._term;
  var done = ch ? ch.streamDone : win._streamDone;
  if (!term || done) return;
  wsUiSend({ type: 'resize', id: sid, rows: term.rows, cols: term.cols });
}

/** Debounced PTY sync on container resize (local fit + optional remote resize via WebSocket). */
function scheduleShellRemoteResize(win, term, termEl, optSid) {
  var sid = optSid || win._activeChannelSid || win._sid;
  if (!win || !sid) return;
  var ch = win._channels && win._channels[sid];
  var done = ch ? ch.streamDone : win._streamDone;
  if (done) return;
  if (win._shellResizeSyncTimer) clearRetryTimer(win._shellResizeSyncTimer);
  win._shellResizeSyncTimer = setTimeout(function () {
    win._shellResizeSyncTimer = null;
    postResize(win, sid);
  }, 200);
}

/**
 * Fit columns to .xterm-viewport width (scrollbar-aware), align cell size with .xterm-screen; when syncRemote, queue PTY resize over WebSocket.
 * @returns {boolean} true if term.resize ran (grid changed).
 */
function fitShellTerminal(term, container, win, syncRemote) {
  if (!container || !container.isConnected || !term) return false;
  var vp = term.element && term.element.querySelector('.xterm-viewport');
  var w = container.clientWidth;
  if (vp && vp.clientWidth > 0) w = vp.clientWidth;
  var h = container.clientHeight;
  if (w <= 0 || h <= 0) return false;
  var fontSize = (typeof term.getOption === 'function' && term.getOption('fontSize')) || 13;
  var fontFamily = (typeof term.getOption === 'function' && term.getOption('fontFamily')) || 'Consolas, Monaco, monospace';
  var measure = document.createElement('span');
  measure.style.cssText = 'position:absolute;visibility:hidden;top:0;left:0;white-space:pre;font:' + fontSize + 'px ' + fontFamily;
  measure.textContent = 'M';
  document.body.appendChild(measure);
  var charWidth = measure.offsetWidth || 8;
  var lineHeight = Math.ceil(fontSize * 1.35) || 16;
  document.body.removeChild(measure);
  var screenEl = term.element && term.element.querySelector('.xterm-screen');
  var prevCols = term.cols;
  var prevRows = term.rows;
  if (screenEl && prevCols > 0 && prevRows > 0) {
    var cw = screenEl.clientWidth / prevCols;
    var lh = screenEl.clientHeight / prevRows;
    if (cw > 0) charWidth = cw;
    if (lh > 0) lineHeight = lh;
  }
  var cols = Math.max(2, Math.floor(w / charWidth));
  var rows = Math.max(2, Math.floor((h - 4) / lineHeight));
  /* Avoid term.resize when grid unchanged — xterm resets viewport scroll on resize; spurious RO/focus jitter was jumping scroll to top. */
  var changed = cols !== prevCols || rows !== prevRows;
  if (changed) {
    term.resize(cols, rows);
  }
  /* Drag onMove uses syncRemote false (no PTY spam); onUp may match last grid so term.resize is skipped — still must queue PTY sync. */
  if (syncRemote && win && !win._streamDone) scheduleShellRemoteResize(win);
  return changed;
}


/** First wheel (capture) or first viewport scroll: syncScrollArea — cold start can desync native scroll from buffer. */
function attachXtermViewportFirstInteractionSync(term) {
  try {
    var el = term && term.element;
    if (!el || el._termcpFirstInteractionSync) return;
    el._termcpFirstInteractionSync = true;
    var kick = function () {
      try {
        var c = term._core;
        if (c && c.viewport && typeof c.viewport.syncScrollArea === 'function') {
          c.viewport.syncScrollArea();
        }
      } catch (e) {}
    };
    el.addEventListener('wheel', kick, { passive: true, capture: true, once: true });
    var vp = el.querySelector('.xterm-viewport');
    if (vp) vp.addEventListener('scroll', kick, { passive: true, once: true });
  } catch (e0) {}
}

/**
 * Terminal bytes arrive as a JSON string; xterm wants a Uint8Array.
 *
 * TextEncoder produces the UTF-8 bytes of that string, which is exactly what
 * the server wrote. A byte sequence that is not valid UTF-8 cannot survive a
 * JSON string, so the server guarantees whole characters (it stores bytes in
 * log.bin and only converts on the way out). xterm's own decoder then stitches
 * a multi-byte character split across two frames, because it keeps a partial
 * sequence between writes.
 */
function textToBytes(text) {
  return new TextEncoder().encode(text || '');
}

var SHELL_HISTORY_CHUNK = 512 * 1024;
/**
 * Soft cap on how much prior output we paint into xterm (protects browser / renderer).
 * Full retained bytes may be larger on the server; a future dedicated “history viewer” UI
 * can page through GET /api/shells/{id}/output-range without this embedded-terminal limit.
 */
var SHELL_HISTORY_DISPLAY_CAP = 32 * 1024 * 1024;

function fetchShellOutputRange(shellId, qs) {
  // Path id is shell_id (channel id used by tabs/WS watch).
  return fetch('/api/shells/' + encodeURIComponent(shellId) + '/output-range?' + qs).then(function (r) {
    if (!r.ok) throw new Error('output-range ' + r.status);
    return r.json();
  });
}

/** Load retained server buffer from byte 0 into xterm in order (chunked HTTP), then live output follows over WS. */
function bootstrapShellFullHistory(sessionId, term) {
  var off = 0;
  var total = -1;
  var shown = 0;
  function one() {
    var capLeft = SHELL_HISTORY_DISPLAY_CAP - shown;
    if (capLeft <= 0) {
      try {
        term.write('\r\n\x1b[33m' + t('term.history.cap', { mb: Math.round(SHELL_HISTORY_DISPLAY_CAP / (1024 * 1024)) }) + '\x1b[0m\r\n');
      } catch (e0) {}
      return Promise.resolve();
    }
    var max = SHELL_HISTORY_CHUNK;
    if (total >= 0) max = Math.min(max, total - off);
    max = Math.min(max, capLeft);
    if (max <= 0) return Promise.resolve();
    var prevOff = off;
    return fetchShellOutputRange(sessionId, 'start=' + off + '&max=' + max).then(function (j) {
      var T = Number(j.total);
      if (isFinite(T) && T >= 0) total = T;
      var d = j.d || '';
      if (d) {
        var u = textToBytes(d);
        try { term.write(u); } catch (e1) {}
        shown += u.length;
      }
      var end = Number(j.end);
      if (!isFinite(end)) end = off;
      off = end;
      if (off === prevOff) return Promise.resolve();
      if (total === 0) return Promise.resolve();
      if (off >= total) return Promise.resolve();
      if (shown >= SHELL_HISTORY_DISPLAY_CAP) {
        try { term.write('\r\n\x1b[33m' + t('term.history.omitted') + '\x1b[0m\r\n'); } catch (e2) {}
        return Promise.resolve();
      }
      return one();
    });
  }
  return one().catch(function () { return Promise.resolve(); });
}

/** True if xterm viewport is at/near bottom — used only before following new output with scrollToBottom. */
function xtermViewportNearBottom(term, marginPx) {
  marginPx = marginPx == null ? 48 : marginPx;
  try {
    var el = term && term.element && term.element.querySelector('.xterm-viewport');
    if (!el) return true;
    var sh = el.scrollHeight;
    var ch = el.clientHeight;
    var st = el.scrollTop;
    if (sh <= ch + 2) return true;
    return st + ch >= sh - marginPx;
  } catch (e) {
    return true;
  }
}

/** After remote writes: scroll to bottom only if user was already at bottom (sticky tail). */
function shellTermScrollToBottomIfStuck(term, flush) {
  if (!term) return;
  if (!xtermViewportNearBottom(term, 64)) return;
  shellTermScrollToBottom(term, flush);
}

/** Scroll to buffer bottom using xterm APIs only (do not set viewport scrollTop — it desyncs from the renderer). */
function shellTermScrollToBottom(term, flush) {
  if (!term) return;
  function apply() {
    try {
      var core = term._core;
      if (core && core.viewport && typeof core.viewport.syncScrollArea === 'function') {
        core.viewport.syncScrollArea();
      }
    } catch (e0) {}
    try {
      if (typeof term.scrollToBottom === 'function') {
        term.scrollToBottom();
      }
    } catch (e) {}
    try {
      var core2 = term._core;
      if (core2 && core2.viewport && typeof core2.viewport.syncScrollArea === 'function') {
        core2.viewport.syncScrollArea();
      }
    } catch (e1) {}
  }
  function applyTwice() {
    apply();
    requestAnimationFrame(function () {
      apply();
      requestAnimationFrame(apply);
    });
  }
  if (flush) {
    applyTwice();
    return;
  }
  if (term._stbRaf) return;
  term._stbRaf = requestAnimationFrame(function () {
    term._stbRaf = 0;
    applyTwice();
  });
}

/**
 * After chunked history paint: align viewport scroll metrics + one PTY sync.
 * ResizeObserver is not active during bootstrap — registering fit/resize while term.write streams history desyncs native scroll from xterm's buffer.
 */
function shellTermSyncViewportAfterBufferChange(term, termEl, win) {
  if (!term) return;
  try { fitShellTerminal(term, termEl, win, false); } catch (e0) {}
  try { shellTermScrollToBottom(term, true); } catch (e1) {}
  requestAnimationFrame(function () {
    try {
      var c = term._core;
      if (c && c.viewport && typeof c.viewport.syncScrollArea === 'function') c.viewport.syncScrollArea();
    } catch (e2) {}
    try {
      if (typeof term.refresh === 'function') term.refresh(0, term.rows - 1);
    } catch (e3) {}
    requestAnimationFrame(function () {
      try {
        var c2 = term._core;
        if (c2 && c2.viewport && typeof c2.viewport.syncScrollArea === 'function') c2.viewport.syncScrollArea();
      } catch (e4) {}
      try { fitShellTerminal(term, termEl, win, true); } catch (e5) {}
      attachXtermViewportFirstInteractionSync(term);
    });
  });
}

function getPendingShellWindowForConn(connName) {
  if (!connName) return null;
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    if (w._placeholder && w._pendingConnName === connName) return w;
  }
  return null;
}

// ---- Shell channel tabs (SSH multiplexing) ----

/** Get sibling tab next to the given sessionId after removal (or null). */
