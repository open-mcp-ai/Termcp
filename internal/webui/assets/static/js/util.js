var shellWindowCount = 0;
var editingConnName = '';
window._pendingTerminalWatch = Object.create(null);

function escapeHtml(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

/* The monospace stack, for the one consumer that cannot read CSS: xterm takes
   fontFamily as a JS option and injects it into its own stylesheet, so the
   terminal grid was styled by a JS string while the rest of the chrome was
   styled by --font-mono in tokens.css. Two lists, one of them latin-only, and
   the terminal obeyed the latin-only one: on a machine with no Consolas or
   Monaco the grid fell through to generic `monospace` (DejaVu Sans Mono, no CJK
   at all) and every Chinese character came from the last-resort fallback — the
   wide-glyph report in issue #77.

   So CSS owns the list and this reads it, with a copy of the same list as the
   fallback for the case where the variable is missing (stylesheet not loaded
   yet, or a deployment that ships the JS without tokens.css).
   TestTerminalFontStackMatchesCSS compares the two copies entry by entry,
   because a silent drift here is invisible until someone opens the UI on a
   bare Linux box — which is exactly how the original bug survived. */
var TERMCP_MONO_FALLBACK =
  '"Sarasa Mono SC", "Sarasa Fixed SC", "Sarasa Term SC", "Sarasa Mono TC", "Sarasa Fixed TC", "Sarasa Term TC", "Sarasa Mono J", "Sarasa Mono K", "Noto Sans Mono CJK SC", "Noto Sans Mono CJK TC", "Noto Sans Mono CJK JP", "Noto Sans Mono CJK KR", "Source Han Mono SC", "Source Han Mono TC", "Source Han Mono J", "Source Han Mono K", "WenQuanYi Zen Hei Mono", "WenQuanYi Micro Hei Mono", "JetBrains Mono", SFMono-Regular, "SF Mono", Menlo, Monaco, Consolas, "Cascadia Mono", "Roboto Mono", "DejaVu Sans Mono", "Liberation Mono", "Noto Sans Mono", "Ubuntu Mono", "Droid Sans Mono", ui-monospace, NSimSun, "PingFang SC", "PingFang TC", "Hiragino Sans GB", "Heiti SC", "STHeiti", "Microsoft YaHei", "Microsoft JhengHei", "Yu Gothic", "Meiryo", "MS Gothic", "Malgun Gothic", "Apple SD Gothic Neo", "Noto Sans CJK SC", "Noto Sans CJK TC", "Noto Sans CJK JP", "Noto Sans CJK KR", "Source Han Sans SC", "Source Han Sans TC", "Noto Sans SC", "Noto Sans TC", "WenQuanYi Micro Hei", "WenQuanYi Zen Hei", "AR PL UMing CN", "Droid Sans Fallback", "Noto Color Emoji", "Apple Color Emoji", "Segoe UI Emoji", monospace';

/* The stack, preferring the stylesheet so tokens.css stays the single source.
   Whitespace is collapsed: getPropertyValue returns the declaration as written,
   and tokens.css wraps this list across lines. */
function termcpMonoFontFamily() {
  var v = '';
  try {
    if (typeof getComputedStyle === 'function' && document.documentElement) {
      var css = getComputedStyle(document.documentElement);
      v = css.getPropertyValue('--terminal-font-family') || css.getPropertyValue('--font-mono') || '';
      // A packaged web font may still be downloading. Start with the installed
      // stack so xterm measures a real face; the terminal module loads and
      // reapplies the requested face once it is ready.
      if (document.fonts && typeof document.fonts.check === 'function') {
        var size = parseFloat(css.getPropertyValue('--terminal-font-size')) || 13;
        var weight = css.getPropertyValue('--terminal-font-weight').trim() || 'normal';
        if (!document.fonts.check(weight + ' ' + size + 'px ' + v, 'M中')) v = TERMCP_MONO_FALLBACK;
      }
    }
  } catch (e) {}
  v = String(v).replace(/\s+/g, ' ').trim();
  return v || TERMCP_MONO_FALLBACK;
}

/* ---- Glass affordability (the "is this page allowed to be translucent" probe) ----

   Every backdrop-filter in this UI is a compositor effect. With a GPU that is
   free; with a CPU rasterizer — no GPU at all (Device Manager shows no driver, a
   remote desktop session, hardware acceleration switched off) — every blur is
   repainted on the CPU, and it is repainted for every frame anything moves.
   Measured on this page at 1440x900 while a terminal streamed: 17 ms/frame with a
   GPU, 63 ms/frame without one at 1x, 200 ms/frame at 2x. The main thread sat idle
   in all three runs, which is why this reads as "卡" that no profiler explains.

   Two independent signals say no. The OS preference is the official one (Windows:
   Settings > Personalization > Colors > Transparency effects, which Edge maps to
   prefers-reduced-transparency). The renderer string is the second, because a
   remote session has no GPU whatever the preference says — and asking the browser
   beats guessing from the user agent, which does not carry this at all.
   The decision lands as one class on <html>; the stylesheet that consumes it is
   the noglass.css chunk, last in app.css's manifest so a plain selector can win
   over the other chunks' backdrop-filter without !important. */

/**
 * termcpGlassSignals reports what the glass decision is made from: whether the OS
 * asks for less transparency, and whether the compositor is a CPU rasterizer.
 */
function termcpGlassSignals() {
  var sig = { reduceTransparency: false, software: false };
  try {
    sig.reduceTransparency = !!(window.matchMedia && window.matchMedia('(prefers-reduced-transparency: reduce)').matches);
  } catch (e) {}
  try {
    var c = document.createElement('canvas');
    var gl = c.getContext('webgl') || c.getContext('experimental-webgl');
    if (gl) {
      var dbg = gl.getExtension('WEBGL_debug_renderer_info');
      var name = String((dbg && gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL)) || gl.getParameter(gl.RENDERER) || '');
      /* The rasterizers that mean "no GPU": Windows' Basic Render Driver (what a
         machine with no display driver, or a session on one, reports), ANGLE's
         software backend, SwiftShader, and Mesa's llvmpipe/softpipe. Matching the
         NAME and not the vendor keeps this one check; an unrecognised renderer
         keeps the glass, because the failure that matters is a slow page, not a
         missing decoration. */
      sig.software = /basic render driver|swiftshader|llvmpipe|softpipe/i.test(name);
    }
  } catch (e) {}
  return sig;
}

/**
 * termcpSyncGlassMode applies the decision to <html> as one class and returns it.
 * Called at boot, and again if the OS preference flips while the page is open.
 */
function termcpSyncGlassMode() {
  if (!document.documentElement) return false;
  var sig = termcpGlassSignals();
  var opaque = sig.reduceTransparency || sig.software;
  document.documentElement.classList.toggle('no-glass', opaque);
  return opaque;
}

termcpSyncGlassMode();
try {
  var _glassMQ = window.matchMedia && window.matchMedia('(prefers-reduced-transparency: reduce)');
  if (_glassMQ && _glassMQ.addEventListener) _glassMQ.addEventListener('change', termcpSyncGlassMode);
} catch (e) {}

var SVG_COPY_12 = '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" fill="currentColor"><path d="M5 2.5V2a1 1 0 011-1h6a1 1 0 011 1v8a1 1 0 01-1 1h-1v.5a1 1 0 01-1 1H4a1 1 0 01-1-1V4a1 1 0 011-1h1zm1 .5H4v8h6v-8H6zm-1-1V2h6v8h-1V3.5a1 1 0 00-1-1H5z"/></svg>';

/* Approval lock glyphs. Closed = gated, open = ungated: the shape carries the
   state, so the switch reads correctly without colour. */
var SVG_LOCK_CLOSED = '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 015 0v2"/></svg>';
var SVG_LOCK_OPEN = '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 014.9-.6"/></svg>';

var SVG_CONN_QUICK = '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true" fill="currentColor"><path d="M5 3.5a.75.75 0 011.12-.65l7.5 4.5a.75.75 0 010 1.3l-7.5 4.5a.75.75 0 01-1.12-.65v-9z"/></svg>';

var SVG_CONN_EDIT = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>';

var TERMINAL_ICON_SRC = 'icons/terminal-shell.svg';

function terminalIconImgHtml() {
  return '<img class="terminal-shell-icon" data-theme-asset="icons/terminal-shell.svg" src="' + themeAssetURL(TERMINAL_ICON_SRC) + '" width="16" height="16" alt="" draggable="false">';
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
  }
  /* No else: a WS-pushed notification is not a user gesture, so a prompt opened
     from here would be ignored by Chrome and Safari. armPermissionRequest() has
     already asked on the first click, which is the only place the prompt can
     work. Someone who never granted it gets the toast and nothing else. */
}

/* --- asking for permission, from inside a gesture -------------------------
 *
 * Browser permissions that matter to a page that is not in front of the user —
 * desktop notifications, and audio for them — are only granted in response to
 * a user gesture. This page's notifications arrive from a WebSocket, which is
 * not one, so asking at arrival time silently did nothing: `Notification.
 * permission` stayed 'default' forever and every toast was screen-only. The
 * audio context had the mirror-image problem, staying 'suspended' until
 * something resumed it.
 *
 * So the ask happens on the first real input the page sees, whatever it is.
 * Once per page load, and only while permission is still undecided: nagging a
 * person who already said no is how a page gets blocked outright.
 */
var _permissionArmed = false;
function armPermissionRequest() {
  if (_permissionArmed) return;
  _permissionArmed = true;
  var ask = function () {
    document.removeEventListener('pointerdown', ask, true);
    document.removeEventListener('keydown', ask, true);
    if ('Notification' in window && Notification.permission === 'default' &&
        typeof Notification.requestPermission === 'function') {
      Notification.requestPermission().catch(function () {});
    }
    /* An AudioContext starts only when it is created inside a gesture. Creating
       one earlier and calling resume() here does not lift it — a context born
       outside a gesture stays 'suspended' no matter how many clicks follow — so
       the context is opened *now*, not at the first notification. */
    ensureNotifyAudio();
    if (_notifyAudio && _notifyAudio.state === 'suspended' && typeof _notifyAudio.resume === 'function') {
      _notifyAudio.resume().catch(function () {});
    }
  };
  document.addEventListener('pointerdown', ask, true);
  document.addEventListener('keydown', ask, true);
}

/* --- notification sound ---------------------------------------------------
 *
 * Generated rather than shipped as an audio file: two short notes cost ~30
 * lines and no asset, no fetch, and no autoplay policy puzzle (an <audio> the
 * browser never got a gesture for is as silent as no sound at all). A person
 * who does not want it mutes the tab.
 *
 * Every call is a no-op until a gesture has opened the context, which is why
 * armPermissionRequest() exists: the first click on the page is what makes this
 * work, and it is the same click that asks for notifications.
 */
var _notifyAudio = null;
function ensureNotifyAudio() {
  if (_notifyAudio || typeof AudioContext === 'undefined' && typeof webkitAudioContext === 'undefined') return _notifyAudio;
  try {
    _notifyAudio = new (typeof AudioContext !== 'undefined' ? AudioContext : webkitAudioContext)();
  } catch (e) { _notifyAudio = null; }
  return _notifyAudio;
}

/** Two ascending notes, the shape a chat app uses for "something arrived".
 *  Level changes the pitch, not the volume: an error reads as more urgent
 *  without being loud enough to startle. Returns true when it actually played,
 *  so a caller (or a test) can tell a muted context from a working one. */
function playNotifySound(level) {
  var ctx = ensureNotifyAudio();
  if (!ctx) return false;
  if (ctx.state === 'suspended' && typeof ctx.resume === 'function') ctx.resume().catch(function () {});
  if (ctx.state !== 'running') return false;
  var notes = level === 'error' || level === 'warn' ? [740, 560] : [660, 880];
  var at = ctx.currentTime;
  notes.forEach(function (hz, i) {
    var osc = ctx.createOscillator();
    var gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.value = hz;
    /* A bare oscillator clicks. Ramping the gain up and back down over ~90 ms
       is the smallest envelope that removes it, and it also keeps the sound
       from competing with whatever the person is listening to. */
    var t0 = at + i * 0.13;
    gain.gain.setValueAtTime(0, t0);
    gain.gain.linearRampToValueAtTime(0.14, t0 + 0.02);
    gain.gain.linearRampToValueAtTime(0, t0 + 0.11);
    osc.connect(gain);
    gain.connect(ctx.destination);
    osc.start(t0);
    osc.stop(t0 + 0.12);
  });
  return true;
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
  playNotifySound(level);
  browserSystemNotify(n);
}

/** Display: strip leading session- from name, else use id */
/** Download an opaque TOML document from an export route. The backend composes
 *  the document (a selection, or everything), so the browser stays a pipe for
 *  bytes: fetch, blob, hand it to the download machinery, release the URL. */
function downloadTOML(path, filename) {
  return fetch(path).then(function (r) {
    if (!r.ok) return r.text().then(function (message) { throw new Error(message || String(r.status)); });
    return r.blob();
  }).then(function (blob) {
    var url = URL.createObjectURL(blob);
    var link = document.createElement('a');
    link.href = url;
    link.download = filename;
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
  });
}

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

/**
 * Deployment path prefix, derived once from this document's own URL.
 *
 * termcp is often reached behind a reverse proxy mounted on a sub-path
 * (`https://host/termcp/`), where every root-absolute URL the page emits
 * (`/api/...`, `/static/...`, `/icons/...`) would escape the mount and hit the
 * parent site instead. Deriving the prefix from `location.pathname` keeps the
 * page working both at the root (prefix = "") and under any mount, with no
 * server-side configuration and no URL rewriting in the proxy.
 *
 * The document is always `<prefix>/` or `<prefix>/index.html`, so the prefix is
 * everything before the trailing `index.html`, or before the final path segment.
 * Normalized to "" (root) or "/sub/path" (no trailing slash).
 */
function uiBasePath() {
  var p = location.pathname || '/';
  if (p.slice(-11) === '/index.html') p = p.slice(0, -11);
  else {
    var i = p.lastIndexOf('/');
    p = i < 0 ? '' : p.slice(0, i);
  }
  return p === '/' ? '' : p;
}

/**
 * Resolve a termcp-root-absolute path against the deployment prefix:
 * apiPath('/api/version') -> '/api/version' at the root,
 *                            '/termcp/api/version' under a /termcp mount.
 * Idempotent for values that are already prefixed.
 */
function apiPath(path) {
  var p = String(path == null ? '' : path);
  if (!p) return uiBasePath();
  if (p.charAt(0) !== '/') return p;
  var base = uiBasePath();
  if (!base) return p;
  if (p === base || p.indexOf(base + '/') === 0) return p;
  return base + p;
}

/** /api/sessions/{id}{suffix} — session-scoped REST (shells/forwards/files). Terminal I/O uses WebSocket. */
function sessionAPI(sessionId, suffix) {
  return apiPath('/api/sessions/' + encodeURIComponent(sessionId) + suffix);
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

/* ---- reading the z-index ladder (z-layers.css) ----------------------------

   The page's layers are ranked in exactly one place, the stylesheet, so this is
   the only way script may name one. Two things used to be literals here: the
   full-screen rung, and the number a floating window's raise sequence starts at.
   Both are stylesheet decisions (they sit between the dock and the drawers), and a
   copy in JS cannot be kept in step with them — the copy is invisible to the CSS
   rule it is supposed to agree with, so the two silently disagree the first time
   either moves.

   The fallbacks are the same values z-layers.css declares. They exist because a
   window can be created before the stylesheet has applied (or a deployment can
   ship the JS without it), and `null` from getComputedStyle would otherwise reach
   `Number()` as NaN. TestZIndexLadderIsTheOnlySourceOfRanks fails if a fallback
   here and the rung in the stylesheet drift apart. */
function zRung(name, fallback) {
  var v = '';
  try {
    if (typeof getComputedStyle === 'function' && document.documentElement) {
      v = getComputedStyle(document.documentElement).getPropertyValue(name);
    }
  } catch (e) {}
  v = String(v || '').trim();
  var n = parseInt(v, 10);
  return Number.isFinite(n) ? n : fallback;
}

/* The stylesheet's rung for .win-fullscreen. Full-screen windows all share it, so
   ordering them against each other needs an inline value; this is the value the
   frontmost one takes. Read through a getter rather than cached: a theme swap
   rewrites the ladder, and a stale cache would rank new windows on the old one.
   The tab bar and the "which window is on top" probe both read it through
   fullscreenRung() below. */
function fullscreenZ() { return zRung('--z-fullscreen', 21000); }

/* A hard-coded constant for the full-screen rung used to live here, which was a
   second copy of a stylesheet decision with no way to disagree out loud. The
   answers come from the ladder now; these two keep the call sites readable. */
function fullscreenRung() { return fullscreenZ(); }
function windowRung() { return zRung('--z-window', 10001); }

/* A floating window's raise sequence, kept inside its own band.

   `low` is where the first window sits and `high` is the last value the sequence
   may reach. The band is [--z-window, --z-dock), because the dock is a real
   surface above the windows and a window that climbed past it would paint over
   the tab bar listing it.

   The sequence WRAPS rather than climbing: when it reaches `high` the floating
   windows are renumbered from `low` in their current order, which preserves every
   relative rank and costs one pass. Without the wrap the counter is a one-way
   ratchet — ~10k raises to overrun the dock, which is not a realistic number of
   user clicks but is a number the code should not depend on the user never
   reaching. */
function shellZBand() {
  var low = zRung('--z-window', 10001);
  var dock = zRung('--z-dock', 20000);
  return { low: low, high: Math.max(low + 16, dock - 1) };
}

function bringShellWindowToFront(win) {
  if (!win || !win.classList.contains('shell-window')) return;
  if (isTiledWin(win)) { setActivePane(win, false); return; }
  if (win.classList.contains('win-fullscreen')) {
    /* Full-screen windows share the stylesheet's z-index, so ordering them needs
       an inline value. The top slot is handed over, never pushed higher: the
       raised window takes the full-screen rung and every other one drops to one
       below it. So the ceiling never moves and cannot overrun the drawer's rung,
       whatever the window count or switching order. (A value below the dock's
       rung would sink the terminal behind the tab bar, which is why the shared
       floating sequence is not used here.) */
    var fullZ = fullscreenZ();
    allShellWins().forEach(function (w) {
      if (w !== win && w.classList.contains('win-fullscreen')) {
        w.style.zIndex = String(fullZ - 1);
      }
    });
    win.style.zIndex = String(fullZ);
    refreshSessionTabbar();
    return;
  }

  var band = shellZBand();
  if (!window._shellZSeq || window._shellZSeq < band.low || window._shellZSeq >= band.high) {
    /* Renumber from the bottom of the band, in the order the windows currently
       rank, so the raise below lands on top of all of them and every existing
       relative order survives. Full-screen and tiled windows are not in this
       sequence at all (they carry their own rung), so they are left untouched. */
    var floating = allShellWins().filter(function (w) {
      return !w.classList.contains('win-fullscreen') && !isTiledWin(w);
    });
    floating.sort(function (a, b) {
      return (parseInt(a.style.zIndex, 10) || 0) - (parseInt(b.style.zIndex, 10) || 0);
    });
    window._shellZSeq = band.low;
    floating.forEach(function (w) { w.style.zIndex = String(++window._shellZSeq); });
  }
  window._shellZSeq += 1;
  win.style.zIndex = String(window._shellZSeq);
  refreshSessionTabbar();
}

/** Session cards mirror the windows open for them, so the grid and the floating
 *  layer agree about which session is already on screen. Called from the tab bar
 *  refresh, the one path every open/close/raise already runs through. */
function paintSessionTileWindowMarkers() {
  var open = {};
  allShellWins().forEach(function (w) { if (w._parentSid) open[w._parentSid] = true; });
  document.querySelectorAll('.conn-tile.sess-tile').forEach(function (t) {
    var sid = t.getAttribute('data-sid');
    if (sid) t.classList.toggle('has-open-window', !!open[sid]);
  });
}

// ---- session tab bar (multi-session switching) ----

/* Permissions and audio both need a gesture, and this module is loaded before
   anything in the page can produce a notification, so arming here catches the
   first click or keypress. Deliberately the last statement in the file: it must
   run after the state it reads (`_permissionArmed`, `_notifyAudio`) has been
   initialised, and a module-level call placed beside those declarations would
   run before them — `var` hoists the binding but not the assignment, so the
   flag would be cleared right after being set and the gesture would be armed
   twice. */
armPermissionRequest();
