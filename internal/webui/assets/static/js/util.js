var shellWindowCount = 0;
var startConnName = '';
var editingConnName = '';
var _connDirty = false; // unsaved edits in the connection editor
window._pendingTerminalWatch = Object.create(null);

function escapeHtml(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

var SVG_COPY_12 = '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" fill="currentColor"><path d="M5 2.5V2a1 1 0 011-1h6a1 1 0 011 1v8a1 1 0 01-1 1h-1v.5a1 1 0 01-1 1H4a1 1 0 01-1-1V4a1 1 0 011-1h1zm1 .5H4v8h6v-8H6zm-1-1V2h6v8h-1V3.5a1 1 0 00-1-1H5z"/></svg>';
/* Approval lock glyphs. Closed = gated, open = ungated: the shape carries the
   state, so the switch reads correctly without colour. */
var SVG_LOCK_CLOSED = '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 015 0v2"/></svg>';
var SVG_LOCK_OPEN = '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 014.9-.6"/></svg>';

var SVG_CONN_QUICK = '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true" fill="currentColor"><path d="M5 3.5a.75.75 0 011.12-.65l7.5 4.5a.75.75 0 010 1.3l-7.5 4.5a.75.75 0 01-1.12-.65v-9z"/></svg>';

var SVG_CONN_EDIT = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>';

var TERMINAL_ICON_SRC = 'icons/terminal-shell.svg';

function terminalIconImgHtml() {
  return '<img class="terminal-shell-icon" src="' + TERMINAL_ICON_SRC + '" width="16" height="16" alt="" draggable="false">';
}

function copyTextToClipboard(text) {
  text = String(text || '');
  if (!text) return Promise.resolve();
  if (navigator.clipboard && navigator.clipboard.writeText) {
    return navigator.clipboard.writeText(text).catch(function () { return copyTextFallback(text); });
  }
  return copyTextFallback(text);
}

function copyTextFallback(text) {
  return new Promise(function (resolve, reject) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.cssText = 'position:fixed;left:-9999px;top:0';
    document.body.appendChild(ta);
    ta.select();
    try {
      if (document.execCommand('copy')) resolve();
      else reject(new Error('copy failed'));
    } catch (e) {
      reject(e);
    }
    document.body.removeChild(ta);
  });
}

var _copyToastTimer;
/** Split a typed command line into argv.
 *
 * The shell APIs take an executable plus an argv array, but a human types a line
 * (`ls -la`, `python -m http.server`). Passing the line as the executable sends
 * the whole thing as one argv element and the far side answers "executable file
 * not found". Quotes group a token, so paths with spaces still work. */
function splitCommandLine(line) {
  var out = [], cur = '', quote = null, started = false;
  for (var i = 0; i < line.length; i++) {
    var c = line.charAt(i);
    if (quote) {
      if (c === quote) quote = null;
      else cur += c;
    } else if (c === '"' || c === "'" || c === '`') {
      quote = c; started = true;
    } else if (c === ' ' || c === '\t') {
      if (started) { out.push(cur); cur = ''; started = false; }
    } else {
      cur += c; started = true;
    }
  }
  if (started) out.push(cur);
  return out;
}

function showCopyToast(msg) {
  msg = msg || t('toast.copied');
  var el = document.getElementById('ui-copy-toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'ui-copy-toast';
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', 'polite');
    document.body.appendChild(el);
  }
  el.textContent = msg;
  el.classList.add('ui-copy-toast-visible');
  clearTimeout(_copyToastTimer);
  _copyToastTimer = setTimeout(function () {
    el.classList.remove('ui-copy-toast-visible');
  }, 1600);
}

/** Human-readable byte size (B/KB/MB/GB), one decimal above the first unit. */
function formatSize(bytes) {
  if (!bytes || bytes < 0) return '0 B';
  var units = ['B', 'KB', 'MB', 'GB'];
  var i = 0;
  var size = bytes;
  while (size >= 1024 && i < units.length - 1) { size /= 1024; i++; }
  return (i === 0 ? size : size.toFixed(1)) + ' ' + units[i];
}

/**
 * Render a Unix-millisecond timestamp as local time.
 *
 * Every timestamp on the wire is Unix ms: manifests, log.jsonl marks, session
 * and forward metadata, file mod_time. Formatting is deliberately the client's
 * job so the server never has to serialize a time as a locale string, and
 * every field shares one unit.
 */
function fmtTime(ms) {
  if (!ms) return '';
  var d = new Date(ms);
  if (isNaN(d.getTime())) return '';
  return d.toLocaleString();
}

/* --- Server-pushed user notifications (MCP notify_user) --- */
var _uiNotifHighlights = {}; // session_id -> true; re-applied when tiles re-render

function uiNotifyStackEl() {
  var el = document.getElementById('ui-notify-stack');
  if (!el) {
    el = document.createElement('div');
    el.id = 'ui-notify-stack';
    document.body.appendChild(el);
  }
  return el;
}

function findSessTile(sid) {
  var tiles = document.querySelectorAll('.conn-tile.sess-tile');
  for (var i = 0; i < tiles.length; i++) {
    if (tiles[i].getAttribute('data-sid') === sid) return tiles[i];
  }
  return null;
}

function clearSessNotified(sid) {
  if (!sid) return;
  delete _uiNotifHighlights[sid];
  var tile = findSessTile(sid);
  if (tile) tile.classList.remove('sess-notified');
  var w = getShellWindowBySid(sid);
  if (w) w.classList.remove('shell-window-notified');
}

function markSessNotified(sid) {
  if (!sid) return;
  _uiNotifHighlights[sid] = true;
  var tile = findSessTile(sid);
  if (tile) {
    tile.classList.add('sess-notified');
    try { tile.scrollIntoView({ block: 'nearest', inline: 'nearest' }); } catch (e) {}
  }
  var w = getShellWindowBySid(sid);
  if (w) w.classList.add('shell-window-notified');
}

/** Browser system-level notification (visible with the tab in the background).
 *  Best-effort: requested once, silently skipped when not granted. */
function browserSystemNotify(n) {
  if (!('Notification' in window)) return;
  if (Notification.permission === 'granted') {
    fireBrowserNotify(n);
  } else if (Notification.permission === 'default' && typeof Notification.requestPermission === 'function') {
    // A WS-pushed notification is not a user gesture, so Chrome may keep
    // permission at 'default' instead of prompting; best-effort only.
    Notification.requestPermission().then(function (perm) {
      if (perm === 'granted') fireBrowserNotify(n);
    }).catch(function () {});
  }
}

function fireBrowserNotify(n) {
  try { new Notification(n.title || 'termcp', { body: n.message }); } catch (e) {}
}

function dismissUiNotify(card) {
  if (!card || card._dismissed) return;
  card._dismissed = true;
  if (card._timer) clearTimeout(card._timer);
  card.classList.add('ui-notify-out');
  setTimeout(function () {
    if (card.parentNode) card.parentNode.removeChild(card);
  }, 200);
  if (card._sessionId) clearSessNotified(card._sessionId);
}

function showUiNotify(n) {
  if (!n) return;
  var level = String(n.level || 'info');
  if (level !== 'info' && level !== 'success' && level !== 'warn' && level !== 'error') level = 'info';
  var title = String(n.title || 'termcp');
  var message = String(n.message || '');
  var dur = parseInt(n.duration_seconds, 10);
  if (isNaN(dur) || dur < 0) dur = 10;
  if (dur > 600) dur = 600;
  var sid = String(n.session_id || '');

  var card = document.createElement('div');
  card.className = 'ui-notify-card ntf-' + level;
  card._sessionId = sid;
  var titleEl = document.createElement('div');
  titleEl.className = 'ui-notify-title';
  titleEl.textContent = title;
  var msgEl = document.createElement('div');
  msgEl.className = 'ui-notify-msg';
  msgEl.textContent = message;
  card.appendChild(titleEl);
  card.appendChild(msgEl);
  if (sid) {
    var sessEl = document.createElement('div');
    sessEl.className = 'ui-notify-sess';
    sessEl.textContent = t('notify.sessionPrefix', { id: sid });
    card.appendChild(sessEl);
  }
  var closeBtn = document.createElement('button');
  closeBtn.type = 'button';
  closeBtn.className = 'ui-notify-close';
  closeBtn.setAttribute('aria-label', t('notify.dismiss'));
  closeBtn.textContent = '×';
  closeBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    dismissUiNotify(card);
  });
  card.appendChild(closeBtn);

  /* A notification names a session, so clicking it goes there. It reuses
     focusSessionWindow rather than reaching for the window directly: that one
     already handles every state the session can be in — no window yet (opens
     it), minimized (restores it), collapsed (expands it), tiled (raises the
     pane) — which is exactly the set of cases a "go to this session" click has
     to cover. A notification about a minimized terminal is the common case, and
     opening a second window for it would be wrong. */
  if (sid) {
    card.classList.add('ui-notify-clickable');
    card.setAttribute('role', 'button');
    card.setAttribute('tabindex', '0');
    card.title = t('session.open.tip', { sid: sid });
    var openSession = function (e) {
      if (e.target.closest('.ui-notify-close')) return;
      var snap = window._lastSessionsSnapshot || [];
      var name = '', dead = false;
      for (var i = 0; i < snap.length; i++) {
        if (snap[i] && snap[i].id === sid) {
          name = snap[i].name || '';
          dead = snap[i].status !== 'running';
          break;
        }
      }
      dismissUiNotify(card);
      // Same entrance the session tiles use, so a session with no window yet is
      // opened rather than ignored, and a DEAD one opens read-only.
      var win = focusSessionWindow(name, sid, null, dead ? { readOnly: true } : null);
      // A notification about a pending decision should land on the decision, not
      // just on the session that has one: arriving at a terminal and still having
      // to find the queue is the step this click exists to remove. Waiting for the
      // window to exist matters because a session with no window yet is created by
      // the call above, and its panel cannot be opened before it is built.
      if (n.open_review) {
        var openPanel = function () {
          var w = win || getShellWindowBySid(sid);
          if (w && !w._placeholder) { setReviewSheetOpen(w, true); return true; }
          return false;
        };
        if (!openPanel()) setTimeout(openPanel, 120);
      }
    };
    card.addEventListener('click', openSession);
    card.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      e.preventDefault();
      openSession(e);
    });
  }
  if (dur > 0) {
    var bar = document.createElement('div');
    bar.className = 'ui-notify-bar';
    bar.style.transition = 'transform ' + dur + 's linear';
    card.appendChild(bar);
    // Force a layout read so the transition has a painted start state, then animate.
    void bar.offsetWidth;
    bar.style.transform = 'scaleX(0)';
    card._timer = setTimeout(function () { dismissUiNotify(card); }, (dur + 1) * 1000);
  }
  uiNotifyStackEl().appendChild(card);

  markSessNotified(sid);
  browserSystemNotify(n);
}

/** Display: strip leading session- from name, else use id */
function displaySessionShort(s) {
  var id = String((s && s.id) || '').trim();
  var name = String((s && s.name) || '').trim();
  if (name.indexOf('session-') === 0) {
    return name.slice(8) || id || name;
  }
  return id || name || '—';
}

function stripSessionPrefix(str) {
  str = String(str || '').trim();
  if (str.indexOf('session-') === 0) return str.slice(8) || str;
  return str;
}

// Resource URLs: termcp://[entry] | termcp://#[session][:shell-index]
// Session/shell URLs ALWAYS use the short form (no entry prefix):
// session names are user-assigned and may not match any entry.
var TERMCP_SCHEME = 'termcp://';
function resourceUrlEntry(name) { return TERMCP_SCHEME + name; }
function resourceUrlSession(sid) {
  return TERMCP_SCHEME + '#' + stripSessionPrefix(sid);
}
function resourceUrlShell(sid, index) {
  return resourceUrlSession(sid) + ':' + index;
}

function shellWindowsEl() {
  return document.getElementById('shell-windows-container');
}

function setLoadBanner(bannerEl, msg) {
  if (!bannerEl) return;
  /* textContent also clears any previous close button. */
  bannerEl.textContent = msg || '';
  bannerEl.style.display = msg ? 'flex' : 'none';
  if (!msg) return;
  /* Dismissable: otherwise the banner stays until the next successful action,
     which can be a long time on a page the user has moved on from. Built here
     so every caller gets it. */
  var x = document.createElement('button');
  x.type = 'button';
  x.className = 'conn-load-banner-close';
  x.title = t('banner.dismiss');
  x.setAttribute('aria-label', t('banner.dismiss'));
  x.textContent = '\u00d7';
  x.addEventListener('click', function () { setLoadBanner(bannerEl, ''); });
  bannerEl.appendChild(x);
}

/** /api/sessions/{id}{suffix} — session-scoped REST (shells/forwards/files). Terminal I/O uses WebSocket. */
function sessionAPI(sessionId, suffix) {
  return '/api/sessions/' + encodeURIComponent(sessionId) + suffix;
}

/** SSE reconnect: backoff from prev to next cap (seconds) */
function sseBackoffNext(prev) {
  var d = prev || 1000;
  return Math.min(30000, Math.max(2000, Math.floor(d * 1.5)));
}

function clearRetryTimer(id) {
  if (!id) return;
  try { clearTimeout(id); } catch (e) {}
}

/* The stylesheet's z-index for .win-fullscreen. Full-screen windows all share
   it, so ordering them against each other needs an inline value; this is the
   value the frontmost one takes. */
var FULLSCREEN_Z = 21000;

function bringShellWindowToFront(win) {
  if (!win || !win.classList.contains('shell-window')) return;
  if (isTiledWin(win)) { setActivePane(win, false); return; }
  if (win.classList.contains('win-fullscreen')) {
    /* Full-screen windows share the stylesheet's z-index, so ordering them needs
       an inline value. The top slot is handed over, never pushed higher: the
       raised window takes FULLSCREEN_Z and every other one drops to
       FULLSCREEN_Z - 1. So the ceiling never moves and cannot overrun the
       drawer's 24000, whatever the window count or switching order. (A value
       below the tab bar's 20000 would sink the terminal behind it, which is why
       the shared floating sequence is not used here.) */
    allShellWins().forEach(function (w) {
      if (w !== win && w.classList.contains('win-fullscreen')) {
        w.style.zIndex = String(FULLSCREEN_Z - 1);
      }
    });
    win.style.zIndex = String(FULLSCREEN_Z);
    refreshSessionTabbar();
    return;
  }
  if (!window._shellZSeq) window._shellZSeq = 10050;
  window._shellZSeq += 1;
  win.style.zIndex = String(window._shellZSeq);
  refreshSessionTabbar();
}

// ---- session tab bar (multi-session switching) ----
