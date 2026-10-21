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
     header: it is the control group's second item, after the hamburger.

     It is the window's TICK, on every viewport, and that is a deliberate
     replacement rather than a new duty. On desktop it was decoration: a monitor
     drawing that did nothing when pressed. On mobile it opened a menu of the
     server's sessions — a switch list reached through a picture of a monitor,
     which is a hidden door, and every window carried its own copy of it. The dock
     already lists this tab's windows and raising one is a click on its tab, so the
     menu's remaining value (sessions with no window yet) is not worth a second
     meaning for a glyph whose obvious reading is "pick this one".

     So the glyph does the one thing its shape suggests: it ticks the window into
     the dock's selection, exactly as the dock's own tick does. The two are the
     same control seen from two places, which is why they drive the same
     component. */
  var icon = header.querySelector('.shell-header-icon');
  if (icon) {
    icon.classList.add('shell-header-icon-btn');
    icon.setAttribute('role', 'button');
    icon.setAttribute('tabindex', '0');
    icon.title = t('win.aria.select');
    icon.setAttribute('aria-label', t('win.aria.select'));
    icon._switchWin = win;
    var activate = function (e) {
      e.preventDefault();
      e.stopPropagation();
      /* The tick lives in the dock's selection, and the dock is keyed by its tab
         (see dockKeyForWindow: `win:<id>`, or `sess:<id>` for a session with no
         window). A window whose id the dock has not drawn a tab for yet (a
         placeholder mid-connect) simply has nowhere to record the tick, so the
         press is a no-op rather than an error: the window becomes tickable the
         moment the dock draws its tab. */
      var key = (typeof dockKeyForWindow === 'function') ? dockKeyForWindow(win) : '';
      if (!_dockBatch || !key) return;
      _dockBatch.toggleSelection(key, win);
    };
    icon.addEventListener('click', activate);
    icon.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      activate(e);
    });
    /* Don't let the header's drag handler treat this as a window drag. */
    icon.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    /* The affordance no longer changes with the viewport — the glyph is a tick
       everywhere — so nothing has to be re-applied on a rotate. The hook stays as
       a no-op only if some caller still expects it; there is none, so it is gone
       and `closeSessionSwitchMenu` no longer runs from here. */
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
  /* No `_syncSwitcherAffordance` call: the header glyph used to change meaning at
     the touch breakpoint (a switch menu on mobile, decoration on desktop), so a
     rotate had to repaint its role and title. It is a tick everywhere now, and a
     tick does not depend on the viewport. */
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
