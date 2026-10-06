var _sessionSwitchMenu = null;

function closeSessionSwitchMenu() {
  if (_sessionSwitchMenu) {
    _sessionSwitchMenu.remove();
    _sessionSwitchMenu = null;
  }
  document.removeEventListener('click', _onSessionSwitchMenuDocClick, true);
  document.removeEventListener('keydown', _onSessionSwitchMenuKey);
  window.removeEventListener('resize', closeSessionSwitchMenu);
  Array.prototype.forEach.call(document.querySelectorAll('.shell-header-icon-btn.active'), function (b) {
    b.classList.remove('active');
  });
}

function _onSessionSwitchMenuDocClick(e) {
  if (!_sessionSwitchMenu) return;
  if (_sessionSwitchMenu.contains(e.target)) return;
  if (e.target.closest && e.target.closest('.shell-header-icon-btn')) return;
  closeSessionSwitchMenu();
}

function _onSessionSwitchMenuKey(e) {
  if (e.key === 'Escape') closeSessionSwitchMenu();
}

/** Live sessions with an open terminal window. Minimized windows still count:
 *  they are alive, just hidden. Ended (read-only) views are left out. */
function openSessionSwitchMenu(anchorBtn) {
  var alreadyOpen = !!_sessionSwitchMenu;
  closeSessionSwitchMenu();
  if (alreadyOpen) return;   // second press toggles it shut

  // The menu lists SESSIONS from the server snapshot, not open windows.
  //
  // Listing windows made the menu a view of this tab's client state: a session
  // created from another terminal (or another device) existed on the server and
  // in the tile grid, but had no window here, so it never appeared. The snapshot
  // is already pushed over the WebSocket on every change, so reading it keeps
  // the menu live without a second source of truth.
  var sessions = (window._lastSessionsSnapshot || []).filter(function (s) {
    return s && s.id;
  });
  var el = document.createElement('div');
  el.className = 'shell-window-switch-menu';
  el.setAttribute('role', 'menu');
  var currentSid = anchorBtn._switchWin ? anchorBtn._switchWin._parentSid : null;
  sessions.forEach(function (s) {
    var item = document.createElement('button');
    item.type = 'button';
    item.className = 'shell-switch-item';
    item.setAttribute('role', 'menuitem');
    var dead = s.status !== 'running';
    if (dead) item.classList.add('dead');
    if (s.id === currentSid) item.classList.add('current');
    item.innerHTML =
      '<span class="shell-switch-num"></span>' +
      '<span class="shell-switch-label"></span>';
    // The number is the window's open-order slot when it has one; a session with
    // no window here has none, and an empty slot is honest about that.
    var w = getShellWindowBySid(s.id);
    item.querySelector('.shell-switch-num').textContent = w ? String(sessionTabNum(w) || '') : '';
    item.querySelector('.shell-switch-label').textContent = s.name || stripSessionPrefix(s.id);
    if (dead) item.title = 'Dead: read-only history';
    item.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      closeSessionSwitchMenu();
      // Same entrance the session tiles use, so a session with no window yet is
      // opened rather than ignored. Centred: this menu is anchored to the window
      // header, not to a point on the page the user aimed at.
      focusSessionWindow(s.name || '', s.id, null, dead ? { readOnly: true } : null);
    });
    el.appendChild(item);
  });
  if (sessions.length === 0) {
    var empty = document.createElement('div');
    empty.className = 'shell-switch-empty';
    empty.textContent = t('switch.empty');
    el.appendChild(empty);
  }
  document.body.appendChild(el);
  _sessionSwitchMenu = el;

  /* Viewport coordinates: the menu's parent is <body>, so position:fixed means
     the viewport and these numbers are the button's real position. */
  var r = anchorBtn.getBoundingClientRect();
  var maxLeft = Math.max(8, window.innerWidth - el.offsetWidth - 8);
  el.style.left = Math.max(8, Math.min(r.left, maxLeft)) + 'px';
  var maxTop = Math.max(8, window.innerHeight - el.offsetHeight - 8);
  el.style.top = Math.max(8, Math.min(r.bottom + 4, maxTop)) + 'px';
  anchorBtn.classList.add('active');

  /* Deferred: the click that opened the menu must not reach this listener, or
     it closes on the same tick. */
  setTimeout(function () {
    document.addEventListener('click', _onSessionSwitchMenuDocClick, true);
    document.addEventListener('keydown', _onSessionSwitchMenuKey);
    window.addEventListener('resize', closeSessionSwitchMenu);
  }, 0);
}

function setupMobileSessionSwitcher(win) {
  if (!win || win._termcpSwitcherBound) return;
  win._termcpSwitcherBound = true;
  var header = win.querySelector('.shell-window-header');
  var titleCluster = win.querySelector('.shell-window-title-cluster');
  if (!header || !titleCluster) return;

  /* One control group holding both header controls, so their spacing comes from
     a single gap instead of two elements' margins fighting. */
  var group = document.createElement('div');
  group.className = 'shell-header-controls';

  /* The monitor glyph moves into the group from its original spot in the
     header: it is the control group's second item, after the hamburger. */
  var icon = header.querySelector('.shell-header-icon');
  if (icon) {
    if (isMobileViewport()) {
      icon.classList.add('shell-header-icon-btn');
      icon.setAttribute('role', 'button');
      icon.setAttribute('tabindex', '0');
      icon.setAttribute('aria-haspopup', 'true');
      icon.title = t('win.aria.activeSessions');
    }
    icon._switchWin = win;
    var activate = function (e) {
      if (!isMobileViewport()) return;      // desktop: plain icon, no action
      e.preventDefault();
      e.stopPropagation();
      openSessionSwitchMenu(icon);
    };
    icon.addEventListener('click', activate);
    icon.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      activate(e);
    });
    /* Don't let the header's drag handler treat this as a window drag. */
    icon.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    /* A window can be created on desktop and then meet the touch breakpoint on a
       rotate or a devtools resize. The listeners above are already attached (they
       self-check at click time), so only the affordance and semantics need
       adding here. */
    win._syncSwitcherAffordance = function () {
      if (isMobileViewport()) {
        icon.classList.add('shell-header-icon-btn');
        icon.setAttribute('role', 'button');
        icon.setAttribute('tabindex', '0');
        icon.setAttribute('aria-haspopup', 'true');
        icon.title = t('win.aria.activeSessions');
      } else {
        closeSessionSwitchMenu();
        icon.classList.remove('shell-header-icon-btn');
        icon.removeAttribute('role');
        icon.removeAttribute('tabindex');
        icon.removeAttribute('aria-haspopup');
        icon.removeAttribute('title');
      }
    };
    group.appendChild(icon);
  }

  /* Connections drawer. Leftmost of the two: switching connection is the outer
     scope of switching session, and touch-only because desktop expands entries
     in place. */
  var drawerBtn = document.createElement('button');
  drawerBtn.type = 'button';
  drawerBtn.className = 'shell-window-drawer-btn';
  drawerBtn.title = t('win.aria.connections');
  drawerBtn.setAttribute('aria-label', t('win.aria.connections'));
  drawerBtn.innerHTML = '<svg viewBox="0 0 16 16" width="15" height="15" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><path d="M2 4h12M2 8h12M2 12h12"/></svg>';
  group.insertBefore(drawerBtn, group.firstChild);

  drawerBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  drawerBtn.addEventListener('click', function (e) {
    e.preventDefault();
    e.stopPropagation();
    closeSessionSwitchMenu();
    if (typeof window.termcpToggleEntriesDrawer === 'function') window.termcpToggleEntriesDrawer(true);
  });

  /* DOM order is the visual order here on purpose: a CSS reorder would leave the
     tab order disagreeing with what the user sees. */
  header.insertBefore(group, header.firstChild);
}

/* Re-apply the current language to one open shell window.
 *
 *  Only attributes and text are rewritten: win._channels holds live xterm
 *  instances, and every header button has a listener bound directly to its node,
 *  so re-rendering the window (win.innerHTML = ...) would drop the listeners and
 *  orphan the terminals mid-stream. Idempotent.
 *
 *  Known residuals, deliberately not chased: a native prompt/confirm/alert that is
 *  already on screen when the language changes, the session switch menu
 *  (.shell-switch-menu is rebuilt on every open) and a validation line currently
 *  on screen. Each picks up the new language the next time it is drawn. */
function reapplyWindowLanguage(win) {
  if (!win) return;
  applyI18n(win);
  updateTermScrollButton(win);
  if (typeof setWindowLinkState === 'function') {
    setWindowLinkState(win, win._placeholder ? 'is-connecting' : (win._readOnly ? 'is-dead' : 'is-live'));
  }
  /* The channel chip's text is its state, so it is written from t() at paint
     time and is not a data-i18n node; re-run the paint so a language switch does
     not leave the previous language's word on the tab. */
  if (win._channels) {
    Object.keys(win._channels).forEach(function (sid) {
      var ch = win._channels[sid];
      if (ch && ch.statusState) shellStatusPaint(ch, ch.statusState);
    });
  }
  /* These three titles encode state, so the template default from applyI18n is
     only correct for the state it was written for. */
  var maxBtn = win.querySelector('.shell-window-max-btn');
  if (maxBtn) maxBtn.title = win._maxed ? t('win.restore.tip') : t('win.max.tip');
  var collapseBtn = win.querySelector('.shell-window-collapse-btn');
  if (collapseBtn) {
    collapseBtn.title = win.classList.contains('shell-window-collapsed') ? t('win.expand') : t('win.collapse');
  }
  if (win._syncSwitcherAffordance) {
    try { win._syncSwitcherAffordance(); } catch (e) {}
  }
  if (win._refreshFwList) win._refreshFwList();
  if (win._refreshNtfList) win._refreshNtfList();
  if (win._shellFileBrowse) win._shellFileBrowse();
  /* The lock's title and its switch text encode the mode, so they are written
     from t() at paint time and are not data-i18n nodes. Repainting the labels
     (not re-applying the mode) keeps a stale mode label from surviving the
     switch without refetching the queue. */
  paintReviewLabels(win);
}

/* Language switch: replay every open window in place, so no window is rebuilt
   and no terminal loses its stream. */
onLangChange(function () { allShellWins().forEach(reapplyWindowLanguage); });
