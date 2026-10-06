
/** The window's link state, in one place: connecting while the dial is open,
 *  live once a session exists, dead once it is read-only history. The marker is a
 *  6px dot beside the identity rather than a worded badge: the state has to be
 *  readable at a glance across several windows, and the words are already on the
 *  tab bar and the session card. */
function setWindowLinkState(win, state) {
  if (!win) return;
  var el = win.querySelector('.shell-link-state');
  if (!el) return;
  el.className = 'shell-link-state ' + state;
  var tip = state === 'is-dead' ? t('session.dead.badgeTitle')
    : state === 'is-connecting' ? t('tab.connecting')
    : t('session.status.running');
  el.title = tip;
  el.setAttribute('role', 'img');
  el.setAttribute('aria-label', tip);
}

function openPendingShellWindow(connName, pos, abortCtl) {
  if (typeof Terminal === 'undefined') { alert(t('alert.xterm.failed')); return null; }
  var container = shellWindowsEl();
  if (!container) return null;
  shellWindowCount += 1;
  var win = document.createElement('div');
  win.className = 'shell-window';
  win.id = 'shell-window-' + shellWindowCount;
  win._sid = null;
  win._placeholder = true;
  win._pendingConnName = connName;
  win._connectAbort = abortCtl;

      win.innerHTML =
    '<div class="shell-window-header">' +
      '<span class="shell-header-icon"><svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><rect x="1.5" y="2.5" width="13" height="9" rx="1.5"/><path d="M5 14.5h6M8 11.5v3"/></svg></span>' +
      '<div class="shell-window-title-cluster">' +
      '<span class="shell-link-state"></span>' +
      '<h3 class="shell-window-title">' +
      '<span class="sw-conn">' + escapeHtml(connName) + '</span>' +
      '<span class="shell-title-id-group">' +
      '<span class="shell-sid-copy">' +
      '<span class="sid">…</span>' +
      '<button type="button" class="title-copy-btn" style="display:none" title="Copy URL" data-i18n-title="common.copyUrl" aria-label="Copy URL" data-i18n-aria="common.copyUrl">' + SVG_COPY_12 + '</button>' +
      '</span>' +
      '</span>' +
      '</h3>' +
      '</div>' +
      '<div class="shell-tab-bar">' +
        // The lock leads the tab strip: it is the session-wide gate, and the
        // tabs to its right are the faces it governs.
        SHELL_REVIEW_LOCK_HTML +
        '<button type="button" class="shell-tab-btn active" data-stab="term" title="Terminal" data-i18n-title="win.term.tab"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><rect x="1" y="2" width="12" height="10" rx="1.5"/><path d="M4 5l2 2-2 2"/><line x1="8" y1="9" x2="11" y2="9"/></svg></button>' +
        '<button type="button" class="shell-tab-btn" data-stab="fw" title="Forwardings" data-i18n-title="win.fw.tab"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3"><rect x="3.5" y="3.5" width="7" height="7" rx="1"/><path d="M1 7h3.5M9.5 7h3.5M7 1v3.5M7 9.5v3.5" stroke-linecap="round"/></svg></button>' +
        '<button type="button" class="shell-tab-btn" data-stab="file" title="Files" data-i18n-title="win.files.tab"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><path d="M1 3v9a1 1 0 001 1h10a1 1 0 001-1V4.5a1 1 0 00-1-1h-5L5.5 2H2a1 1 0 00-1 1z"/></svg></button>' +
        '</div>' +
      '<div class="shell-window-header-btns">' +
        '<button type="button" class="shell-window-max-btn" title="Maximize (dblclick header)" data-i18n-title="win.max.tip"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M2 5V2h3M10 7v3H7M2 2l3.5 3.5M10 10L6.5 6.5"/></svg></button><button type="button" class="shell-window-collapse-btn" title="Collapse" data-i18n-title="win.collapse">' +
          '<svg class="ic-d" viewBox="0 0 12 8" width="12" height="8"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M1 2l5 4 5-4"/></svg>' +
          '<svg class="ic-u" viewBox="0 0 12 8" width="12" height="8" style="display:none"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M1 6l5-4 5 4"/></svg>' +
        '</button>' +
        '<button type="button" class="shell-window-min-btn" title="Minimize (session keeps running)" data-i18n-title="win.minimize">' +
          '<svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 6h8"/></svg>' +
        '</button>' +
        '<button type="button" class="close-btn" title="Close (cancel connect)" data-i18n-title="win.closeCancel">' +
          '<svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg>' +
        '</button>' +
      '</div>' +
    '</div>' +
    '<div class="shell-window-content"><div class="shell-terminal-wrap">' +
      '<div class="shell-channel-body">' +
        '<div class="shell-pending"><div class="shell-pending-spinner" aria-hidden="true"></div><div><span data-i18n="win.connecting.label">Connecting</span> <strong>' + escapeHtml(connName) + '</strong>…</div></div>' +
      '</div>' +
      '<div class="shell-window-footer">' +
        '<div class="shell-channel-tabs">' +
          '<span class="shell-split">' +
            '<button type="button" class="shell-channel-tab-add" title="New shell channel" data-i18n-title="channel.add">+</button>' +
            '<button type="button" class="shell-channel-add-caret" aria-haspopup="menu" title="New shell channel · choose mode" data-i18n-title="channel.addMenu">▾</button>' +
          '</span>' +
        '</div>' +
      '</div>' +
      '<div class="shell-channel-empty">' +
        '<span data-i18n="channel.empty">No shell channels</span>' +
        /* Split control, same as the footer "+": label presses for the default
           shell, caret opens the mode menu. */
        '<span class="shell-split">' +
          '<button type="button" class="shell-channel-empty-add" data-i18n="channel.newShell">+ New Shell</button>' +
          '<button type="button" class="shell-channel-empty-caret" aria-haspopup="menu" title="New shell channel · choose mode" data-i18n-title="channel.addMenu">▾</button>' +
        '</span>' +
      '</div>' +
    '</div>' +
SHELL_REVIEW_SHEET_HTML +
SHELL_WINDOW_PANELS_HTML +
    SHELL_RESIZE_HANDLES_HTML;

  /* The template above carries English fallbacks plus data-i18n* keys; fill them
     here so a window created after a language switch is not born in the previous
     language. */
  applyI18n(win);
  setWindowLinkState(win, 'is-connecting');

  var termEl = win.querySelector('.shell-channel-body');
  var header = win.querySelector('.shell-window-header');
  var closeBtn = win.querySelector('.close-btn');
  var minBtn = win.querySelector('.shell-window-min-btn');
  positionShellWindowFromClick(win, pos);
  /* First mount only — never re-append this node to reorder; use bringShellWindowToFront (z-index). */
  container.appendChild(win);

  bindShellWindowMouseToFront(win);
  bindShellWindowMinButton(win, minBtn);
  bindShellWindowCloseButton(win, closeBtn);
  bindShellWindowMaxButton(win);
  setupShellWindowDrag(win, header);
  setupShellWindowResize(win);
  /* The header's switcher and drawer buttons must work while the connection is
     still being established: waiting for a slow or failing dial to be able to
     switch sessions (or reach another profile) is exactly when they matter. The
     session menu only lists established windows, so a pending one contributes
     nothing to it — but it can still open it. */
  setupMobileSessionSwitcher(win);
  bringShellWindowToFront(win);
  if (_layoutMode === 'tile') { tileWindow(win); openPaneWorkspace(); }
  else { _layoutMaybeAutoTile(); }
  if (_winsHidden) showAllWindows();
  /* The card's "connecting" state lives in this window, so opening and closing
     one is what changes it. Painted here rather than only on the session frame:
     a dial that fails never produces one, and a card left on "connecting" for a
     connection that already gave up is the same wrong answer as a red lamp under
     a live session. */
  if (typeof repaintNodeStates === 'function') repaintNodeStates();
  return win;
}

function finalizePendingShellWindow(win, connLabel, sessionId, shellId, pos, opt) {
  opt = opt || {};
  if (!win || !win.parentNode || !sessionId) return;
  win._connectAbort = null;
  win._placeholder = false;
  win._pendingConnName = '';
  setWindowLinkState(win, 'is-live');

  win._sid = sessionId;
  win._parentSid = sessionId;
  win._primaryShellId = shellId || sessionId;
  win._connName = connLabel || '';
  var sidEl = win.querySelector('.shell-sid-copy .sid');
  if (sidEl) sidEl.textContent = stripSessionPrefix(sessionId);
  var pend = win.querySelector('.shell-pending');
  if (pend) pend.remove();
  _wireChannelTabBar(win);
  _initShellWindowUI(win, connLabel, sessionId);
  wireReviewBar(win);
  // Seed the review UI here as well as in openShellWindow. This is the path a
  // session created in this tab takes, and without it _approvalMode stays
  // undefined, so typing is forwarded and the server silently drops it.
  applyApprovalModeFromSnapshot(win, sessionId);
  createChannelTab(win, win._primaryShellId, { index: opt.index > 0 ? opt.index : 1 });
  refreshSessionTabbar();
  // The placeholder that made this profile read "connecting" is gone; repaint so
  // the card shows the session it just got (or, on the failure path below, stops
  // showing a dial that is no longer running).
  if (typeof repaintNodeStates === 'function') repaintNodeStates();
}

/**
/**
 * The review lock, beside the session title.
 *
 * One control, icon-only, because the title bar is the most crowded row on the
 * window and a phone has no room for a labelled pill there. The glyph is the
 * same closed/open lock the session card uses, so one shape means one thing: is
 * this session's AI gated. It opens the panel, which holds the switch and the
 * queue.
 *
 * It replaces the label the pill carried. "Review: off" told a reader the mode,
 * but the mode is also legible from the panel and from the session card, while
 * the width it cost was paid on every session that never uses review. The queue
 * count moved to the tab bar (see renderTabBadges), where it can say WHICH face
 * has work waiting instead of only how much.
 */
var SHELL_REVIEW_LOCK_HTML =
  '<button type="button" class="shell-review-lock" aria-expanded="false" aria-label="Review" data-i18n-aria="review.title">' +
    '<span class="shell-review-lock-icon" aria-hidden="true">' + SVG_LOCK_OPEN + '</span>' +
    '<span class="shell-review-count" hidden>0</span>' +
  '</button>';

var SHELL_RESIZE_HANDLES_HTML =
  '<div class="shell-window-resize-handle" data-corner="nw" title="Drag to resize" data-i18n-title="win.resize"></div>' +
  '<div class="shell-window-resize-handle" data-corner="ne" title="Drag to resize" data-i18n-title="win.resize"></div>' +
  '<div class="shell-window-resize-handle" data-corner="sw" title="Drag to resize" data-i18n-title="win.resize"></div>' +
  '<div class="shell-window-resize-handle" data-corner="se" title="Drag to resize" data-i18n-title="win.resize"></div>';

/**
 * The review panel: a floating card over the terminal's top-right corner.
 *
 * It floats rather than taking height on purpose. As a flex sibling it pushed
 * the terminal up by its own height, so every open reflowed the PTY and moved
 * the lines you were reading — the wrong trade for a queue that is checked
 * often. Its height is capped and it scrolls, so it never covers the whole
 * screen either.
 *
 * It holds the switch and the queue, and nothing else. There is no composer:
 * the commands in the queue are written by the AI, not typed here, and a human's
 * job is to accept or refuse them.
 *
 * The switch is a small track-and-knob rather than a full-size "Turn on review"
 * button: the mode is one bit, and a padded button for it read as the panel's
 * primary action while taking a row of its own. The footer button carries the
 * same state, so it stays legible with the panel closed.
 */
var SHELL_REVIEW_SHEET_HTML =
  '<div class="shell-review-sheet" hidden>' +
    '<div class="shell-review-sheet-h">' +
      '<span class="shell-review-sheet-title" data-i18n="review.title">Review</span>' +
      '<label class="shell-review-switch" title="Review every write the AI sends to this session" data-i18n-title="review.switch.tip">' +
        '<input type="checkbox" class="shell-review-toggle" role="switch" aria-label="Review mode" data-i18n-aria="review.mode">' +
        '<span class="shell-review-switch-track" aria-hidden="true"></span>' +
        '<span class="shell-review-switch-text" data-i18n="review.off">Review</span>' +
      '</label>' +
      '<button type="button" class="shell-review-sheet-close" title="Close" aria-label="Close" data-i18n-title="common.close" data-i18n-aria="common.close">' +
        '<svg viewBox="0 0 12 12" width="11" height="11" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg>' +
      '</button>' +
    '</div>' +
    '<div class="shell-review-sheet-b">' +
      '<div class="shell-review-hint" data-i18n="review.hint">While review is on, every write the AI sends to this session waits here until a human accepts it.</div>' +
      '<div class="shell-review-queue">' +
        '<div class="shell-review-queue-h">' +
          '<span data-i18n="review.waiting">Waiting for review</span>' +
          '<button type="button" class="shell-review-refresh" title="Refresh" data-i18n-title="common.refresh">' +
            '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M13 8a5 5 0 11-1.5-3.5"/><path d="M13 2.5V5h-2.5"/></svg>' +
          '</button>' +
        '</div>' +
        '<div class="shell-review-list"></div>' +
      '</div>' +
    '</div>' +
  '</div>';
function openShellWindow(connLabel, sessionId, pos, opts) {
  opts = opts || {};
  var readOnly = !!opts.readOnly;
  if (typeof Terminal === 'undefined') { alert(t('alert.xterm.failed')); return; }
  if (getShellWindowBySid(sessionId)) {
    focusSessionWindow(connLabel, sessionId, pos, opts);
    return;
  }
  var container = shellWindowsEl();
  if (!container) return;
  shellWindowCount += 1;
  var win = document.createElement('div');
  win.className = 'shell-window';
  win.id = 'shell-window-' + shellWindowCount;
  win._sid = sessionId;
  win._readOnly = readOnly;

      win.innerHTML =
    '<div class="shell-window-header">' +
      '<span class="shell-header-icon"><svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><rect x="1.5" y="2.5" width="13" height="9" rx="1.5"/><path d="M5 14.5h6M8 11.5v3"/></svg></span>' +
      '<div class="shell-window-title-cluster">' +
      '<span class="shell-link-state"></span>' +
      '<h3 class="shell-window-title">' +
      (connLabel ? ('<span class="sw-conn">' + escapeHtml(connLabel) + '</span>') : '') +
      '<span class="shell-title-id-group">' +
      '<span class="shell-sid-copy">' +
      '<span class="sid">' + escapeHtml(stripSessionPrefix(sessionId)) + '</span>' +
      '<button type="button" class="title-copy-btn" title="Copy URL" data-i18n-title="common.copyUrl" aria-label="Copy URL" data-i18n-aria="common.copyUrl">' + SVG_COPY_12 + '</button>' +
      '</span>' +
      '</span>' +
      '</h3>' +
      '</div>' +
      '<div class="shell-tab-bar">' +
        // The lock leads the tab strip: it is the session-wide gate, and the
        // tabs to its right are the faces it governs.
        SHELL_REVIEW_LOCK_HTML +
        '<button type="button" class="shell-tab-btn active" data-stab="term" title="Terminal" data-i18n-title="win.term.tab"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><rect x="1" y="2" width="12" height="10" rx="1.5"/><path d="M4 5l2 2-2 2"/><line x1="8" y1="9" x2="11" y2="9"/></svg></button>' +
        '<button type="button" class="shell-tab-btn" data-stab="fw" title="Forwardings" data-i18n-title="win.fw.tab"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3"><rect x="3.5" y="3.5" width="7" height="7" rx="1"/><path d="M1 7h3.5M9.5 7h3.5M7 1v3.5M7 9.5v3.5" stroke-linecap="round"/></svg></button>' +
        '<button type="button" class="shell-tab-btn" data-stab="file" title="Files" data-i18n-title="win.files.tab"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><path d="M1 3v9a1 1 0 001 1h10a1 1 0 001-1V4.5a1 1 0 00-1-1h-5L5.5 2H2a1 1 0 00-1 1z"/></svg></button>' +
        '</div>' +
      '<div class="shell-window-header-btns">' +
        '<button type="button" class="shell-window-max-btn" title="Maximize (dblclick header)" data-i18n-title="win.max.tip"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M2 5V2h3M10 7v3H7M2 2l3.5 3.5M10 10L6.5 6.5"/></svg></button><button type="button" class="shell-window-collapse-btn" title="Collapse" data-i18n-title="win.collapse">' +
          '<svg class="ic-d" viewBox="0 0 12 8" width="12" height="8"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M1 2l5 4 5-4"/></svg>' +
          '<svg class="ic-u" viewBox="0 0 12 8" width="12" height="8" style="display:none"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M1 6l5-4 5 4"/></svg>' +
        '</button>' +
        '<button type="button" class="shell-window-min-btn" title="Minimize (session keeps running)" data-i18n-title="win.minimize">' +
          '<svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 6h8"/></svg>' +
        '</button>' +
        '<button type="button" class="close-btn" title="Delete session" data-i18n-title="session.delete.title">' +
          '<svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg>' +
        '</button>' +
      '</div>' +
    '</div>' +
    '<div class="shell-window-content"><div class="shell-terminal-wrap">' +
      '<div class="shell-channel-body"></div>' +
      '<div class="shell-window-footer">' +
        '<div class="shell-channel-tabs">' +
          '<span class="shell-split">' +
            '<button type="button" class="shell-channel-tab-add" title="New shell channel" data-i18n-title="channel.add">+</button>' +
            '<button type="button" class="shell-channel-add-caret" aria-haspopup="menu" title="New shell channel · choose mode" data-i18n-title="channel.addMenu">▾</button>' +
          '</span>' +
        '</div>' +
      '</div>' +
      '<div class="shell-channel-empty">' +
        '<span data-i18n="channel.empty">No shell channels</span>' +
        /* Split control, same as the footer "+": label presses for the default
           shell, caret opens the mode menu. */
        '<span class="shell-split">' +
          '<button type="button" class="shell-channel-empty-add" data-i18n="channel.newShell">+ New Shell</button>' +
          '<button type="button" class="shell-channel-empty-caret" aria-haspopup="menu" title="New shell channel · choose mode" data-i18n-title="channel.addMenu">▾</button>' +
        '</span>' +
      '</div>' +
    '</div>' +
SHELL_REVIEW_SHEET_HTML +
SHELL_WINDOW_PANELS_HTML +
    SHELL_RESIZE_HANDLES_HTML;

  /* The template above carries English fallbacks plus data-i18n* keys; fill them
     here so a window created after a language switch is not born in the previous
     language. */
  applyI18n(win);
  setWindowLinkState(win, readOnly ? 'is-dead' : 'is-live');

  positionShellWindowFromClick(win, pos);
  /* First mount only — never re-append this node to reorder; use bringShellWindowToFront (z-index). */
  container.appendChild(win);
  bindShellWindowMaxButton(win);
  bringShellWindowToFront(win);
  if (_layoutMode === 'tile') { tileWindow(win); openPaneWorkspace(); }
  else { _layoutMaybeAutoTile(); }
  if (_winsHidden) showAllWindows();
  win._parentSid = sessionId;
  win._connName = connLabel || '';
  win._inputClosed = readOnly;
  _wireChannelTabBar(win);
  wireReviewBar(win);
  // Seed the approval UI from the snapshot the page already has. Without this the
  // Approve button stays hidden until the next session-list frame arrives, which
  // can be a long time after the window opens.
  applyApprovalModeFromSnapshot(win, sessionId);
  _initShellWindowUI(win, connLabel, sessionId);
  // _initShellWindowUI resets the input gate; enforce read-only DEAD mode after.
  win._inputClosed = readOnly;

  // Restore all peer shells. readOnly DEAD views include exited snapshot shells.
  fetch(sessionAPI(sessionId, '/shells'))
    .then(function(r) { if (!r.ok) return []; return r.json().then(function(j) { return j.shells || []; }); })
    .then(function(shells) {
      var running = shells.filter(function(s) { return s.status === 'running'; });
      var exited = shells.filter(function(s) { return s.status !== 'running'; });
      // Live container with only exited history shells (pipe commands that
      // finished): show them as read-only history tabs so the web UI is not
      // empty, while keeping the ability to open a new shell. Running parents
      // with zero shells still render the exited history instead of a blank view.
      if (running.length === 0 && exited.length > 0 && !readOnly) {
        exited.forEach(function(s) { createChannelTab(win, s.shell_id || s.id, { index: s.index || 0, readOnlyHistory: true }); });
        return;
      }
      var tabs = shells.filter(function(s) { return readOnly || s.status === 'running'; });
      if (tabs.length > 0) {
        if (!win._primaryShellId) {
          win._primaryShellId = (tabs[0].shell_id || tabs[0].id);
        }
        tabs.forEach(function(s) { createChannelTab(win, s.shell_id || s.id, { index: s.index || 0 }); });
        // Restore last active tab.
        var lastActive = window._shellLastActive && window._shellLastActive[sessionId];
        if (lastActive && win._channels[lastActive]) switchChannelTab(win, lastActive);
        if (readOnly) lockWindowReadonly(win);
      } else {
        _showChannelEmpty(win, true);
        if (readOnly) lockWindowReadonly(win);
      }
    }).catch(function() {
      if (readOnly) { _showChannelEmpty(win, true); lockWindowReadonly(win); }
      else addChannelClick(win);
    });
}

/** POST /api/sessions/start then open shell window. command/mode may be '' to use server defaults (login shell, profile default_mode).
 *
 *  pos is the window's placement: a {x, y} client position, or null for the
 *  centred cascade. Callers that went through the host drawer pass null — that
 *  drawer is pinned to the left edge while the click is handled, so a position
 *  taken from that click would land the window against the left wall. */
function startSessionAndOpenShell(connName, pos, opt) {
  opt = opt || {};
  var dup = getPendingShellWindowForConn(connName);
  if (dup) {
    bringShellWindowToFront(dup);
    return Promise.resolve(null);
  }
  var ac = new AbortController();
  var pendingWin = openPendingShellWindow(connName, pos, ac);
  if (!pendingWin) {
    return Promise.reject(new Error('Could not open terminal window'));
  }
  var body = {
    command: opt.command != null ? opt.command : '',
    args: opt.args || [],
    mode: opt.mode != null ? opt.mode : '',
    ssh_config: connName,
    rows: 24,
    cols: 80
  };
  if (opt.name) body.name = opt.name;
  else if (connName) body.name = connName;
  return fetch('/api/sessions', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal: ac.signal })
    .then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || r.status); });
      return r.json();
    })
    .then(function (j) {
      var sid = j && j.session_id;
      var shellId = j && (j.shell_id || j.session_id);
      if (!sid) throw new Error('No session_id in response');
      if (!shellId) throw new Error('No shell_id in response');
      pendingWin._connectAbort = null;
      if (pendingWin.parentNode) {
        finalizePendingShellWindow(pendingWin, connName || 'session', sid, shellId, pos, { index: j.index || 0 });
      } else {
        openShellWindow(connName || 'session', sid, pos);
      }
      return j;
    })
    .catch(function (err) {
      if (pendingWin && pendingWin.parentNode && pendingWin._placeholder) {
        closeShellWindow(pendingWin);
      }
      return Promise.reject(err);
    });
}


/** Mobile session switcher.
 *
 *  The floating session tab bar is hidden on touch devices, which leaves the
 *  full-screen terminal with no way to reach another open session. Two controls
 *  go into the window header, in this order:
 *
 *    - the hamburger opens the connections drawer. It sits leftmost because
 *      switching connection is the outer scope of switching session — the same
 *      reason desktop apps put the sidebar toggle first.
 *    - the monitor glyph opens the session menu, for the current connection.
 *
 *  Both are touch-only: desktop has the tab bar for sessions and expands entries
 *  in place, so there the glyph stays an inert icon (it does not even take button
 *  semantics, which would advertise an action that is not there) and the
 *  hamburger is hidden.
 *
 *  The menu is a module-level singleton rather than a per-window node, because a
 *  per-window menu is built from whatever the DOM held when that window was
 *  created, so two windows would disagree about which sessions exist. One menu,
 *  rebuilt on every open, cannot go stale.
 *
 *  It is appended to <body>, not into the window: #pane-workspace carries a
 *  backdrop-filter, which makes it the containing block for position:fixed
 *  descendants, so a menu parented inside a window would be positioned against
 *  the pane grid (and clipped by its overflow) instead of the viewport. */
