function _channelNeighbor(win, sid) {
  var kids = win.querySelector('.shell-channel-tabs').querySelectorAll('.shell-channel-tab');
  var found = false;
  var prev = null;
  for (var i = 0; i < kids.length; i++) {
    if (kids[i].getAttribute('data-chsid') === sid) { found = true; continue; }
    if (found) return kids[i].getAttribute('data-chsid');
    prev = kids[i].getAttribute('data-chsid');
  }
  return prev || null;
}

/** Create a channel tab + xterm instance for sessionId. */
function createChannelTab(win, sessionId, optLabel, optReadOnlyHistory) {
  if (!win._channels) { win._channels = {}; win._channelCount = 0; win._channelSeq = 0; }
  if (win._channels[sessionId]) { switchChannelTab(win, sessionId); return; }
  var label = optLabel || ('shell-' + (win._channelSeq + 1));
  var urlIndex = win._channelSeq + 1;
  var readOnlyHistory = !!optReadOnlyHistory;

  var tabsBar = win.querySelector('.shell-channel-tabs');
  var body = win.querySelector('.shell-channel-body');
  // The add control is a split button (the "+" is wrapped with its caret half),
  // so the anchor for insertBefore is the wrapper inside the tab bar, not the "+"
  // itself: inserting before a nested node throws NotFoundError, which showed up
  // as a shell that was created on the server but never got a tab.
  var addBtn = tabsBar ? tabsBar.querySelector('.shell-channel-tab-add') : null;
  var addAnchor = addBtn && addBtn.parentNode && addBtn.parentNode !== tabsBar ? addBtn.parentNode : addBtn;
  var emptyEl = win.querySelector('.shell-channel-empty');

  // Hide empty state, restore body
  if (emptyEl) emptyEl.classList.remove('show');
  if (body) body.style.display = '';

  // Create tab
  var tab = document.createElement('div');
  tab.className = 'shell-channel-tab active';
  tab.setAttribute('data-chsid', sessionId);
  tab.innerHTML = '<span class="shell-channel-tab-label">' + escapeHtml(label || 'shell') + '</span>' +
    '<button type="button" class="shell-channel-tab-copy" title="Copy URL" data-i18n-title="common.copyUrl" aria-label="Copy URL" data-i18n-aria="common.copyUrl">' + SVG_COPY_12 + '</button>' +
    '<span class="shell-channel-tab-ended" style="display:none" title="Session ended" data-i18n-title="session.ended" data-i18n="channel.endedTab">end</span>' +
    '<button type="button" class="shell-channel-tab-close" title="Close shell" data-i18n-title="channel.close">&times;</button>';
  applyI18n(tab);
  if (addAnchor) tabsBar.insertBefore(tab, addAnchor);

  // Deactivate all other tabs
  tabsBar.querySelectorAll('.shell-channel-tab').forEach(function(t) {
    if (t !== tab) t.classList.remove('active');
  });

  // Create instance
  var inst = document.createElement('div');
  inst.className = 'shell-channel-instance active';
  inst.setAttribute('data-chsid', sessionId);
  body.appendChild(inst);
  // Hide other instances
  body.querySelectorAll('.shell-channel-instance').forEach(function(el) {
    if (el !== inst) el.classList.remove('active');
  });

  // Create xterm
  var term = new Terminal({
    cursorBlink: true,
    fontSize: 13,
    fontFamily: 'Consolas, Monaco, monospace',
    theme: {
      background: '#1e1e1e',
      foreground: '#d4d4d4',
      cursor: '#d4d4d4',
      cursorAccent: '#1e1e1e',
      selectionBackground: '#264f7844'
    },
    scrollback: 100000
  });
  // Hide until first fit to prevent garbled flash.
  inst.style.visibility = 'hidden';
  term.open(inst);
  // Show the "back to latest" button when the user scrolls this channel away from the tail.
  var _vp = term.element && term.element.querySelector('.xterm-viewport');
  if (_vp) {
    _vp.addEventListener('scroll', function () {
      if (win._activeChannelSid === sessionId) updateTermScrollButton(win);
    }, { passive: true });
  }
  attachXtermViewportScrollbarGuard(term);
  attachTermBrowserChordShield(term);

  // Store channel state. A readOnly-history channel renders already-exited
  // output from a live session container (pipe commands that finished); it is
  // read-only (no input sent) so it never interferes with concurrent MCP I/O.
  win._channels[sessionId] = {
    term: term,
    instEl: inst,
    tabEl: tab,
    readOnlyHistory: readOnlyHistory,
    streamDone: readOnlyHistory,
    endedMarkPrinted: false,
    historyLoaded: false,
    fitRo: null,
    urlIndex: urlIndex
  };
  win._activeChannelSid = sessionId;
  win._channelCount += 1;
  win._channelSeq += 1;

  // Deactivate previous tabs
  Object.keys(win._channels).forEach(function(sid) {
    if (sid !== sessionId && win._channels[sid].tabEl) {
      win._channels[sid].tabEl.classList.remove('active');
    }
  });

  // Setup input gate
  setupShellOnDataUserGate(win, term);

  // onData
  term.onData(function (data) {
    if (win._inputClosed) return;
    // Review mode does NOT block this path: this is the human's keyboard.
    //
    // The gate reviews what the AI sends, and the AI's surface is MCP. Blocking
    // the operator's own typing locked them out of the session they were
    // watching the moment they turned review on — they could not even interrupt
    // a running command. A reviewer who cannot touch the terminal is not
    // reviewing it, they are locked out of it.
    //
    // `win._approvalMode` is still read elsewhere (the title bar shows the
    // state), so nothing here needs it.
    var ch = win._channels[sessionId];
    if (!ch) return;
    if (ch.streamDone) {
      if (data === '\r' || data === '\n' || data === '\r\n') { closeChannelTab(win, sessionId); }
      return;
    }
    if (!win._shellUserInputReady) return;
    if (!window._uiWS || window._uiWS.readyState !== 1) return;
    try {
      // The keystrokes go out as the string xterm produced: JSON encodes the
      // text and the server takes its UTF-8 bytes, so no base64 layer is needed.
      window._uiWS.send(JSON.stringify({ type: 'input', id: sessionId, d: data, nl: false }));
    } catch (e) {}
  });

  // Bootstrap history + watch. Show after first fit to prevent garbled flash.
  try { fitShellTerminal(term, inst, win, false); } catch (eFit) {}
  inst.style.visibility = '';
  bootstrapShellFullHistory(sessionId, term).finally(function () {
    var chLoaded = win._channels && win._channels[sessionId];
    if (chLoaded) chLoaded.historyLoaded = true;
    if (win._readOnly || (chLoaded && chLoaded.readOnlyHistory)) {
      // DEAD/read-only channels (or a live container's exited-history channel)
      // have no live terminal_done event; render the end marker after the
      // persisted output is restored.
      showTerminalEndedMarker(win, chLoaded);
    } else {
      queueTerminalWatch(sessionId);
    }
    try {
      var ro = attachShellTerminalResizeObserverForChannel(win, term, inst, sessionId);
      win._channels[sessionId].fitRo = ro;
      shellTermSyncViewportAfterBufferChange(term, inst, win);
    } catch (eBoot) {}
    updateTermScrollButton(win);
  });

  // Tab click → switch
  tab.addEventListener('click', function(e) {
    if (e.target.closest('.shell-channel-tab-close') || e.target.closest('.shell-channel-tab-copy')) return;
    switchChannelTab(win, sessionId);
  });

  // Copy this shell's resource URL
  var tabCopyBtn = tab.querySelector('.shell-channel-tab-copy');
  tabCopyBtn.addEventListener('mousedown', function(e) { e.stopPropagation(); });
  tabCopyBtn.addEventListener('click', function(e) {
    e.preventDefault();
    e.stopPropagation();
    var url = resourceUrlShell(win._parentSid || win._sid || '', urlIndex);
    copyTextToClipboard(url).then(function () { showCopyToast(); }).catch(function () { showCopyToast(t('toast.copy.failed')); });
  });

  // Close button
  var closeBtn = tab.querySelector('.shell-channel-tab-close');
  closeBtn.addEventListener('click', function(e) {
    e.stopPropagation();
    closeChannelTab(win, sessionId);
  });

  // Focus
  requestAnimationFrame(function() { try { term.focus(); } catch(e) {} });

  return term;
}

/** Switch active channel tab. */
function switchChannelTab(win, sessionId) {
  var ch = win._channels && win._channels[sessionId];
  if (!ch) return;
  if (win._activeChannelSid === sessionId) return;
  win._activeChannelSid = sessionId;
  win._shellUserInputReady = true;  // explicit switch = user intends to type

  // Tabs
  var tabsBar = win.querySelector('.shell-channel-tabs');
  if (tabsBar) {
    tabsBar.querySelectorAll('.shell-channel-tab').forEach(function(t) {
      t.classList.toggle('active', t.getAttribute('data-chsid') === sessionId);
    });
  }
  // Instances
  var body = win.querySelector('.shell-channel-body');
  if (body) {
    body.querySelectorAll('.shell-channel-instance').forEach(function(el) {
      el.classList.toggle('active', el.getAttribute('data-chsid') === sessionId);
    });
  }

  // Fit + focus
  requestAnimationFrame(function() {
    requestAnimationFrame(function() {
      fitShellTerminal(ch.term, ch.instEl, win, true);
      try { ch.term.focus(); } catch(e) {}
    });
  });
  updateTermScrollButton(win);
}

/** Close a channel tab. Terminates the shell process and cleans up. */
function closeChannelTab(win, sessionId) {
  var ch = win._channels && win._channels[sessionId];
  if (!ch) return;

  ch.streamDone = true;

  // Unsubscribe
  sendTerminalWatch(sessionId, false);

  // DEAD tabs are read-only views; closing one must not release the persisted
  // shell or session. Live tabs retain the existing close-shell behavior.
  if (!win._readOnly) {
    fetch('/api/shells/' + encodeURIComponent(sessionId), { method: 'DELETE' }).catch(function() {});
  }

  // Dispose xterm
  try {
    if (ch.fitRo) { ch.fitRo.unobserve(ch.instEl); }
  } catch(e) {}
  try { ch.term.dispose(); } catch(e) {}

  // Remove DOM
  if (ch.tabEl && ch.tabEl.parentNode) ch.tabEl.parentNode.removeChild(ch.tabEl);
  if (ch.instEl && ch.instEl.parentNode) ch.instEl.parentNode.removeChild(ch.instEl);

  // Update state
  delete win._channels[sessionId];
  win._channelCount = Math.max(0, win._channelCount - 1);
  if (win._channelCount === 0) win._channelSeq = 0;

  // Switch or show empty
  if (win._activeChannelSid === sessionId) {
    var neighbor = _channelNeighbor(win, sessionId);
    if (neighbor && win._channels[neighbor]) {
      switchChannelTab(win, neighbor);
    } else if (win._channelCount === 0) {
      win._activeChannelSid = null;
      _showChannelEmpty(win, true);
    }
  }
  updateTermScrollButton(win);
}

function _showChannelEmpty(win, show) {
  var el = win.querySelector('.shell-channel-empty');
  if (el) el.classList.toggle('show', !!show);
  var body = win.querySelector('.shell-channel-body');
  if (body) body.style.display = show ? 'none' : '';
}

/** "+" button / empty-state click handler: create the default shell.
 *  One press is the common case, so it stays one press; the caret beside it is
 *  where the mode menu lives. */
function addChannelClick(win) {
  createChannel(win, 'pty', '');
}

/**
 * The caret half of the add control: opens the mode menu. Pressing "+" itself
 * opens the default shell instead — the menu is for the two choices that need a
 * decision (mode, and a command), not for the common case.
 *
 * `anchorEl` is the button that was pressed and MUST be passed: both carets exist
 * in the DOM at once (footer tab strip and empty state), so re-querying picked the
 * footer one and the menu appeared at the bottom of the window no matter which
 * caret was pressed.
 *
 * Reuses the session-switch menu's markup so the dismissal rules (outside click,
 * Escape, resize) come from one pattern.
 */
function openChannelAddMenu(win, anchorEl) {
  if (!win._parentSid || win._readOnly) return;
  if (typeof _channelAddMenu !== 'undefined' && _channelAddMenu) {
    var same = _channelAddMenu._win === win;
    closeChannelAddMenu();
    if (same) return; // second press toggles shut
  }
  var anchor = anchorEl || win.querySelector('.shell-channel-add-caret') || win.querySelector('.shell-channel-empty-caret');
  if (!anchor) return;
  var el = document.createElement('div');
  el.className = 'shell-window-switch-menu shell-channel-add-menu';
  el.setAttribute('role', 'menu');
  /* Literal t() keys, not a computed key: the i18n test only sees static
     arguments, and an orphan-key check is worth more than the loop. */
  [
    { label: t('channel.add.pty'), mode: 'pty' },
    { label: t('channel.add.pipe'), mode: 'pipe' }
  ].forEach(function (item) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'shell-switch-item shell-channel-add-item';
    b.setAttribute('role', 'menuitem');
    b.setAttribute('data-channel-mode', item.mode);
    var label = document.createElement('span');
    label.className = 'shell-switch-label';
    label.textContent = item.label;
    b.appendChild(label);
    b.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      closeChannelAddMenu();
      promptChannelCommand(win, item.mode);
    });
    el.appendChild(b);
  });
  document.body.appendChild(el);
  _channelAddMenu = el;
  el._win = win;

  /* Fixed positioning against the pressed button's viewport rect. Opens below when
     there is room (the empty state sits mid-window), and flips above when there is
     not — the footer button is on the bottom edge. */
  var r = anchor.getBoundingClientRect();
  var maxLeft = Math.max(8, window.innerWidth - el.offsetWidth - 8);
  el.style.left = Math.max(8, Math.min(r.left, maxLeft)) + 'px';
  var below = r.bottom + 4;
  if (below + el.offsetHeight > window.innerHeight - 8) below = r.top - el.offsetHeight - 4;
  el.style.top = Math.max(8, below) + 'px';
  anchor.classList.add('active');

  /* Deferred so the click that opened the menu does not reach the closing
     listener on the same tick. */
  setTimeout(function () {
    document.addEventListener('click', _onChannelAddMenuDocClick, true);
    document.addEventListener('keydown', _onChannelAddMenuKey);
    window.addEventListener('resize', closeChannelAddMenu);
  }, 0);
}

var _channelAddMenu = null;

function closeChannelAddMenu() {
  if (_channelAddMenu) {
    _channelAddMenu.remove();
    _channelAddMenu = null;
  }
  document.removeEventListener('click', _onChannelAddMenuDocClick, true);
  document.removeEventListener('keydown', _onChannelAddMenuKey);
  window.removeEventListener('resize', closeChannelAddMenu);
  Array.prototype.forEach.call(document.querySelectorAll('.shell-channel-add-caret.active, .shell-channel-empty-caret.active, .shell-channel-tab-add.active, .shell-channel-empty-add.active'), function (b) {
    b.classList.remove('active');
  });
}

function _onChannelAddMenuDocClick(e) {
  if (!_channelAddMenu) return;
  if (_channelAddMenu.contains(e.target)) return;
  if (e.target.closest && e.target.closest('.shell-channel-add-caret, .shell-channel-empty-caret, .shell-channel-tab-add, .shell-channel-empty-add')) return;
  closeChannelAddMenu();
}

function _onChannelAddMenuKey(e) {
  if (e.key === 'Escape') closeChannelAddMenu();
}

/** promptChannelCommand asks for the command a custom shell runs, then opens it.
 *  pty + empty command is the login shell; a command runs that command with a
 *  TTY. pipe + command is a line-oriented run-to-exit command, and pipe without
 *  one is refused by the server (there is no login shell to fall back on), so it
 *  is required there. */
function promptChannelCommand(win, mode) {
  openCommandPrompt({
    title: mode === 'pipe' ? t('channel.cmd.titlePipe') : t('channel.cmd.titlePty'),
    placeholder: t('channel.cmd.placeholder'),
    required: mode === 'pipe',
    onSubmit: function (cmd) { createChannel(win, mode, cmd); }
  });
}

/** createChannel POSTs a new shell channel and opens its tab.
 *
 * `command` is a typed command LINE, not an executable: it is split into an
 * executable plus args here, because the REST API takes argv. A line sent whole
 * would be a single argv element and fail with "executable file not found". */
function createChannel(win, mode, command) {
  if (!win._parentSid || win._readOnly) return Promise.resolve(null);
  var body = { rows: 24, cols: 80, mode: mode || 'pty' };
  var argv = splitCommandLine((command || '').trim());
  if (argv.length) {
    body.command = argv[0];
    if (argv.length > 1) body.args = argv.slice(1);
  }
  return fetch('/api/sessions/' + encodeURIComponent(win._parentSid) + '/shells', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  }).then(function (r) {
    if (!r.ok) return r.text().then(function (txt) { throw new Error(txt); });
    return r.json();
  }).then(function (j) {
    createChannelTab(win, j.shell_id || j.session_id);
    return j;
  }).catch(function (err) {
    var msg = String(err && err.message ? err.message : err).trim();
    console.error('add channel failed', err);
    showCopyToast(t('channel.add.failed', { msg: msg }));
    return null;
  });
}

/** Wire up channel tab bar events (called once per window). */

/**
 * Wire the review lock and its panel.
 *
 * The lock is one control carrying the mode (its open/closed glyph), and the panel
 * it opens holds the switch and the queue. The panel floats over the window, so
 * opening it never resizes the PTY — an earlier version was a flex sibling and
 * reflowed the terminal on every open.
 */
function wireReviewBar(win) {
  var btn = win.querySelector('.shell-review-lock');
  var sheet = win.querySelector('.shell-review-sheet');
  if (btn && sheet) {
    btn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      setReviewSheetOpen(win, sheet.hidden);
    });
  }
  if (sheet) {
    var close = sheet.querySelector('.shell-review-sheet-close');
    if (close) close.addEventListener('click', function (e) {
      e.preventDefault(); e.stopPropagation();
      setReviewSheetOpen(win, false);
    });
    var refresh = sheet.querySelector('.shell-review-refresh');
    if (refresh) refresh.addEventListener('click', function (e) {
      e.preventDefault(); e.stopPropagation();
      loadReviewQueue(win);
    });
    var tg = sheet.querySelector('.shell-review-toggle');
    if (tg) tg.addEventListener('change', function (e) {
      e.preventDefault(); e.stopPropagation();
      applyReviewToggle(win, tg);
    });
  }
  // Escape closes the panel, like any other overlay in the app.
  win.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && sheet && !sheet.hidden) {
      e.stopPropagation();
      setReviewSheetOpen(win, false);
    }
  });
}

/** updateReviewLockTitle composes the lock's tooltip: the mode, then the count
 *  if there is one. Both are read off the window, so the two writers (a mode
 *  change and a count change) say the same thing instead of racing each other.
 *  `_approvalCounts` lives in approval.js, which loads after this module, hence
 *  the guard. */
function updateReviewLockTitle(win) {
  if (!win) return;
  var btn = win.querySelector('.shell-review-lock');
  if (!btn) return;
  var n = (window._approvalCounts || {})[win._parentSid] || 0;
  btn.title = (win._approvalMode ? t('review.lock.on') : t('review.lock.off')) +
    (n ? t('review.lock.waitingCount', { count: n }) : t('review.lock.clickOpen'));
}

/** setReviewSheetOpen shows or hides the queue panel.
 *
 * No refit, by design: the panel is absolutely positioned over the terminal and
 * takes no height from it, so opening it never reflows the PTY. */
function setReviewSheetOpen(win, open) {
  var sheet = win.querySelector('.shell-review-sheet');
  if (!sheet) return;
  sheet.hidden = !open;
  var btn = win.querySelector('.shell-review-lock');
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) loadReviewQueue(win);
}

/** applyReviewToggle flips the gate from the panel's own switch. It reuses the
 *  same server call the session card uses, so both entrances share one write
 *  path and its rules.
 *
 *  One person is enough: whoever turns review on is the reviewer. Counting
 *  approvers is left to a future permission model, because termcp cannot prove
 *  who an approver is until accounts exist — a threshold over claimed names
 *  would look like security without being any.
 */
function applyReviewToggle(win, tg) {
  var sid = win._parentSid;
  if (!sid || !tg) return;
  tg.disabled = true;
  var turningOff = !!win._approvalMode;
  var p;
  if (turningOff) {
    p = confirmDialog({
      title: t('review.disable.title'),
      message: t('review.disable.msg'),
      okText: t('review.disable.ok')
    }).then(function (ok) {
      if (!ok) return null;
      return setSessionApproval(sid, false).then(function (r) {
        // Turning it off cancels everything pending, so the count this window
        // carries is about to be wrong. The server's frame fixes the mode; the
        // number is ours to drop.
        clearApprovalCount(sid);
        return r;
      });
    });
  } else {
    p = setSessionApproval(sid, true);
  }
  return p.catch(function (err) {
    showCopyToast(t('review.toast.failed', { msg: String(err.message || err) }));
  }).then(function () {
    tg.disabled = false;
  });
}

// loadReviewQueue / renderApprovalItem / decideApproval live in approval.js:
// the queue is rendered once for both the terminal's panel and the session
// tile's badge, so the two cannot drift.
function _wireChannelTabBar(win) {
  var addBtn = win.querySelector('.shell-channel-tab-add');
  if (addBtn && !addBtn._chWired) {
    addBtn._chWired = true;
    addBtn.addEventListener('click', function() { addChannelClick(win); });
  }
  var emptyBtn = win.querySelector('.shell-channel-empty-add');
  if (emptyBtn && !emptyBtn._chWired) {
    emptyBtn._chWired = true;
    emptyBtn.addEventListener('click', function() { addChannelClick(win); });
  }
  var menuBtn = win.querySelector('.shell-channel-add-caret');
  if (menuBtn && !menuBtn._chWired) {
    menuBtn._chWired = true;
    menuBtn.addEventListener('click', function(e) { e.preventDefault(); openChannelAddMenu(win, menuBtn); });
  }
  var emptyMenuBtn = win.querySelector('.shell-channel-empty-caret');
  if (emptyMenuBtn && !emptyMenuBtn._chWired) {
    emptyMenuBtn._chWired = true;
    emptyMenuBtn.addEventListener('click', function(e) { e.preventDefault(); openChannelAddMenu(win, emptyMenuBtn); });
  }
}
/** applyApprovalModeFromSnapshot seeds a window's review UI, then corrects it
 *  from the server.
 *
 *  The snapshot is a cache and can be stale for a session created after it was
 *  taken (or gated a moment ago). Trusting it made a gated session render as
 *  "Review: off", so typing looked accepted while the server dropped every byte:
 *  the failure was silent on both ends. The cache still gives the window an
 *  immediate, usually-correct state; the fetch is what makes it true. */
function applyApprovalModeFromSnapshot(win, sessionId) {
  var list = window._lastSessionsSnapshot || [];
  var found = false;
  for (var i = 0; i < list.length; i++) {
    if (list[i] && list[i].id === sessionId) {
      win._approvalNeed = list[i].approval_need || 1;
      applyApprovalMode(win, !!list[i].approval_mode);
      found = true;
      break;
    }
  }
  if (!found) applyApprovalMode(win, false);
  refreshApprovalState(win, sessionId);
}

/** refreshApprovalState reads the authoritative gate for one session and applies
 *  it to an open window. A failure leaves the cached state in place: the server
 *  is still the thing enforcing the gate, so a failed read must not silently
 *  unlock the UI. */
function refreshApprovalState(win, sessionId) {
  if (!win || !sessionId) return;
  fetch(sessionAPI(sessionId, '/approval'))
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (j) {
      if (!j || !win.parentNode) return;
      // Only apply to the window that asked: the user may have closed it and
      // opened another for the same session in the meantime.
      if (win._parentSid !== sessionId) return;
      win._approvalNeed = j.need || 1;
      applyApprovalMode(win, !!j.approval_mode);
    })
    .catch(function () {});
}

/** paintReviewLabels redraws every review label that encodes the mode, from the
 *  state the window already holds. Split out of applyApprovalMode so the language
 *  switch can repaint copy without refetching anything: a redraw that hits the
 *  network blanks the queue while offline and flickers while not. */
function paintReviewLabels(win) {
  if (!win) return;
  var on = !!win._approvalMode;
  var btn = win.querySelector('.shell-review-lock');
  if (btn) {
    btn.classList.toggle('is-on', on);
    // The glyph IS the state, so it has to change: a closed lock left on an
    // ungated session would claim a gate that is not there.
    var ic = btn.querySelector('.shell-review-lock-icon');
    if (ic) ic.innerHTML = on ? SVG_LOCK_CLOSED : SVG_LOCK_OPEN;
    btn.setAttribute('aria-label', on ? t('review.aria.on') : t('review.aria.off'));
    updateReviewLockTitle(win);
  }
  var sheet = win.querySelector('.shell-review-sheet');
  if (!sheet) return;
  // A checkbox, so the state is `checked` rather than a label: the box reflects
  // the mode, and the text next to it names what the mode is.
  var tg = sheet.querySelector('.shell-review-toggle');
  if (tg) {
    tg.checked = !!on;
    tg.disabled = false;
  }
  var txt = sheet.querySelector('.shell-review-switch-text');
  if (txt) txt.textContent = on ? t('review.on') : t('review.off');
}

/** applyApprovalMode turns the typing gate on or off for one window.
 *
 *  The window keeps rendering output either way: approval gates input, not the
 *  view. The title-bar lock carries the state — closed and amber while the AI's
 *  writes wait for a decision, open and neutral while they do not — and the tab
 *  bar carries how many are waiting (see applyApprovalCounts). */
function applyApprovalMode(win, on) {
  if (!win) return;
  win._approvalMode = !!on;
  paintReviewLabels(win);
  // The number is not on the lock: it belongs to the pane tabs, where it can say
  // WHICH face is waiting (a file write vs a command) instead of only how many.
  applyApprovalCounts();
  win.classList.toggle('win-approval', !!on);
  // Turning review on or off changes what the queue means (dropped on off), so
  // an open panel re-reads it rather than showing the previous mode's list.
  var sheet = win.querySelector('.shell-review-sheet');
  if (sheet && !sheet.hidden) loadReviewQueue(win);
}

/** ResizeObserver wrapper that tracks which channel SID to resize. */
function attachShellTerminalResizeObserverForChannel(win, term, termEl, sessionId) {
  var ro = new ResizeObserver(function() {
    scheduleShellRemoteResize(win, term, termEl, sessionId);
  });
  ro.observe(termEl);
  return ro;
}

/** Floating "back to latest output" button — shown only when the active channel is scrolled away from the live tail. */
function setupTermScrollFab(win) {
  if (win._scrollFab) return win._scrollFab;
  var body = win.querySelector('.shell-channel-body');
  if (!body) return null;
  var fab = document.createElement('button');
  fab.type = 'button';
  fab.className = 'shell-term-scroll-fab';
  fab.setAttribute('data-i18n-title', 'term.scroll.latest');
  fab.setAttribute('data-i18n-aria', 'term.scroll.latest');
  fab.title = t('term.scroll.latest');
  fab.setAttribute('aria-label', t('term.scroll.latest'));
  fab.innerHTML = '<svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 12V3M4 8l4 4 4-4"/></svg>';
  fab.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  fab.addEventListener('click', function (e) {
    e.preventDefault(); e.stopPropagation();
    var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
    var term = ch ? ch.term : null;
    if (term) shellTermScrollToBottom(term, true);
    fab.classList.remove('visible');
    try { if (term) term.focus(); } catch (e0) {}
  });
  body.appendChild(fab);
  win._scrollFab = fab;
  return fab;
}

/** Show/hide the scroll FAB based on whether the active channel is at the live tail. */
function updateTermScrollButton(win) {
  var fab = win._scrollFab;
  if (!fab) return;
  var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
  var term = ch ? ch.term : null;
  if (!term) { fab.classList.remove('visible'); return; }
  fab.classList.toggle('visible', !xtermViewportNearBottom(term, 64));
}

/** Initialize shell window UI (header buttons, drag, resize, tab switching) — no xterm. */
function _initShellWindowUI(win, connLabel, sessionId, clickEvent) {
  var header = win.querySelector('.shell-window-header');
  var collapseBtn = win.querySelector('.shell-window-collapse-btn');
  var closeBtn = win.querySelector('.close-btn');
  var minBtn = win.querySelector('.shell-window-min-btn');
  bindShellWindowMouseToFront(win);
  if (collapseBtn) collapseBtn.style.display = '';
  if (closeBtn) {
    closeBtn.setAttribute('data-i18n-title', 'session.delete.title');
    closeBtn.title = t('session.delete.title');
  }
  bindShellWindowMinButton(win, minBtn);
  var titleCopyBtn = win.querySelector('.title-copy-btn');
  if (titleCopyBtn) {
    titleCopyBtn.style.display = '';
    titleCopyBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    titleCopyBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      copyTextToClipboard(resourceUrlSession(sessionId)).then(function () { showCopyToast(); }).catch(function () { showCopyToast(t('toast.copy.failed')); });
    });
  }

  bindShellWindowCloseButton(win, closeBtn);

  win._inputClosed = false;
  setupTermScrollFab(win);

  collapseBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  collapseBtn.addEventListener('click', function (e) {
    e.preventDefault(); e.stopPropagation();
    if (win.classList.contains('shell-window-collapsed')) expandShellWindow(win);
    else collapseShellWindow(win);
  });

  setupShellWindowDrag(win, header);
  setupShellWindowResize(win);
  setupMobileSessionSwitcher(win);

  // --- Header tab switching (term / fw / file / notify panels) ---
  // Inject the Notifications tab here so both the pending and connected
  // window templates get it without duplicating the tab-bar markup.
  var tabBarEl = win.querySelector('.shell-tab-bar');
  if (tabBarEl && !tabBarEl.querySelector('.shell-tab-btn[data-stab="notify"]')) {
    var ntfTabBtn = document.createElement('button');
    ntfTabBtn.type = 'button';
    ntfTabBtn.className = 'shell-tab-btn';
    ntfTabBtn.setAttribute('data-stab', 'notify');
    ntfTabBtn.setAttribute('data-i18n-title', 'win.notify.tip');
    ntfTabBtn.title = t('win.notify.tip');
    ntfTabBtn.innerHTML = '<svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><path d="M3.5 6a3.5 3.5 0 017 0c0 2.5 1 3.5 1 3.5H2.5s1-1 1-3.5z"/><path d="M5.8 11.5a1.2 1.2 0 002.4 0"/></svg>';
    tabBarEl.appendChild(ntfTabBtn);
  }
  var tabBtns = win.querySelectorAll('.shell-tab-btn');
  var terminalWrap = win.querySelector('.shell-terminal-wrap');
  var tabPanels = win.querySelectorAll('.shell-tab-panel');
  tabBtns.forEach(function(btn) {
    btn.addEventListener('click', function(e) {
      e.preventDefault(); e.stopPropagation();
      var tab = this.getAttribute('data-stab');
      tabBtns.forEach(function(b) { b.classList.toggle('active', b === btn); });
      if (terminalWrap) terminalWrap.style.display = tab === 'term' ? '' : 'none';
      tabPanels.forEach(function(p) { p.style.display = p.getAttribute('data-stab') === tab ? 'flex' : 'none'; });
      if (tab === 'term') {
        var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
        if (ch && ch.term && ch.instEl) {
          requestAnimationFrame(function() {
            requestAnimationFrame(function() {
              fitShellTerminal(ch.term, ch.instEl, win, true);
              try { ch.term.focus(); } catch(e2) {}
            });
          });
        }
      }
      if (tab === 'fw') refreshShellFwList(win);
      if (tab === 'notify') refreshShellNtfList(win);
    });
  });

  // Forward "add" button
  var fwAddBtn = win.querySelector('.shell-fw-add-btn');
  if (fwAddBtn) {
    fwAddBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      openForwardModal(sessionId, connLabel || 'internal');
    });
  }
  var fwRefreshBtn = win.querySelector('.shell-fw-refresh-btn');
  if (fwRefreshBtn) {
    fwRefreshBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      var svg = this.querySelector('svg');
      if (svg) {
        svg.style.transition = 'transform 0.6s ease';
        svg.style.transform = 'rotate(360deg)';
        setTimeout(function() { svg.style.transition = 'none'; svg.style.transform = 'rotate(0deg)'; }, 1220);
      }
      loadForwards();
    });
  }

  // Refresh forwardings list for this window (from global cache)
  function refreshShellFwList(w) {
    w = w || win;
    var listEl = w.querySelector('.shell-fw-list');
    if (!listEl) return;
    var cfg = connLabel || 'internal';
    var fwds = (window._lastForwards || []).filter(function(f) { return forwardMatchesConfig(f, cfg); });
    if (!fwds.length) { listEl.innerHTML = '<div style="padding:16px;color:#8b949e;text-align:center">' + escapeHtml(t('fw.empty')) + '</div>'; return; }
    listEl.innerHTML = fwds.map(function(f){
      var dirLabel = f.direction;
      var dirColor = dirLabel === 'local' ? '#3fb950' : (dirLabel === 'dynamic' ? '#58a6ff' : '#d29922');
      return '<div style="display:flex;align-items:center;gap:10px;padding:10px 12px;border-bottom:1px solid #21262d;transition:background .1s" onmouseover="this.style.background=\'#161b22\'" onmouseout="this.style.background=\'\'">' +
        '<span style="font-size:0.65rem;font-weight:600;text-transform:uppercase;padding:1px 5px;border-radius:3px;color:' + dirColor + ';border:1px solid ' + dirColor + ';flex-shrink:0;min-width:42px;text-align:center">' + escapeHtml(dirLabel) + '</span>' +
        '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:0.78rem"><span style="color:#8b949e">' + escapeHtml(f.listen_addr) + '</span> <span style="color:#484f58">→</span> <span style="color:#c9d1d9">' + escapeHtml(f.target_addr) + '</span></span>' +
        '<button class="shell-fw-del-btn" data-fwid="' + escapeHtml(f.forward_id) + '" style="padding:2px 6px;font-size:0.68rem;border:1px solid transparent;border-radius:3px;background:transparent;color:#484f58;cursor:pointer;flex-shrink:0" onmouseover="this.style.borderColor=\'#f85149\';this.style.color=\'#f85149\'" onmouseout="this.style.borderColor=\'transparent\';this.style.color=\'#484f58\'">✕</button>' +
        '</div>';
    }).join('');
    listEl.querySelectorAll('.shell-fw-del-btn').forEach(function(btn) {
      btn.addEventListener('click', function(e) {
        e.stopPropagation();
        deleteForward(this.getAttribute('data-fwid')).catch(function(){});
      });
    });
    listEl.scrollTop = listEl.scrollHeight;
  }
  win._refreshFwList = function() { refreshShellFwList(win); };

  // Refresh notifications list for this window (from global cache).
  function refreshShellNtfList(w) {
    w = w || win;
    var listEl = w.querySelector('.shell-ntf-list');
    if (!listEl) return;
    var rules = (window._lastNotifications || []).filter(function(n) { return n.session_id === sessionId; });
    if (!rules.length) {
      // This tab lists wake-up RULES (the ones the notify tool registers), not the
      // toasts that appear in the corner. Those are two different things, and an
      // empty panel beside a toast that just popped reads as a bug — so the empty
      // state says which thing is empty.
      listEl.innerHTML = '<div style="padding:16px;color:#8b949e;text-align:center;line-height:1.6">'
        + escapeHtml(t('ntf.empty'))
        + '<div style="margin-top:6px;font-size:0.72rem;color:#6e7681">'
        + escapeHtml(t('ntf.emptyHint'))
        + '</div></div>';
      return;
    }
    listEl.innerHTML = rules.map(function(n){
      var evColor = n.event === 'exit' ? '#f85149' : (n.event === 'silence' ? '#d29922' : '#3fb950');
      var extra = (n.event === 'silence' && n.silence_seconds) ? ' ' + n.silence_seconds + 's' : '';
      var chLabel = n.channel === 'sampling' ? t('ntf.channel.sampling') : t('ntf.channel.resource');
      var unregisterTip = t('ntf.unregister');
      return '<div style="display:flex;align-items:center;gap:10px;padding:10px 12px;border-bottom:1px solid #21262d" onmouseover="this.style.background=\'#161b22\'" onmouseout="this.style.background=\'\'">' +
        '<span style="font-size:0.65rem;font-weight:600;text-transform:uppercase;padding:1px 5px;border-radius:3px;color:' + evColor + ';border:1px solid ' + evColor + ';flex-shrink:0;min-width:56px;text-align:center">' + escapeHtml(n.event) + escapeHtml(extra) + '</span>' +
        '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:0.78rem"><span style="color:#8b949e">' + escapeHtml(chLabel) + '</span> <span style="color:#484f58">·</span> <span style="color:#c9d1d9">' + escapeHtml(n.shell_id) + '</span></span>' +
        '<button class="shell-ntf-del-btn" data-ntfid="' + escapeHtml(n.rule_id) + '" title="' + unregisterTip + '" style="padding:2px 6px;font-size:0.68rem;border:1px solid transparent;border-radius:3px;background:transparent;color:#484f58;cursor:pointer;flex-shrink:0" onmouseover="this.style.borderColor=\'#f85149\';this.style.color=\'#f85149\'" onmouseout="this.style.borderColor=\'transparent\';this.style.color=\'#484f58\'">✕</button>' +
        '</div>';
    }).join('');
    listEl.querySelectorAll('.shell-ntf-del-btn').forEach(function(btn) {
      btn.addEventListener('click', function(e) {
        e.stopPropagation();
        deleteNotification(this.getAttribute('data-ntfid')).then(function(){ loadNotifications(); }).catch(function(){});
      });
    });
  }
  win._refreshNtfList = function() { refreshShellNtfList(win); };
  var ntfRefreshBtn = win.querySelector('.shell-ntf-refresh-btn');
  if (ntfRefreshBtn) {
    ntfRefreshBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      var svg = this.querySelector('svg');
      if (svg) {
        svg.style.transition = 'transform 0.6s ease';
        svg.style.transform = 'rotate(360deg)';
        setTimeout(function() { svg.style.transition = 'none'; svg.style.transform = 'rotate(0deg)'; }, 1220);
      }
      loadNotifications();
    });
  }

  // File operations for this shell window
  var filePathInput = win.querySelector('.shell-file-path');
  var fileListing = win.querySelector('.shell-file-listing');
  var fileUploadInput = win.querySelector('.shell-file-upload-input');
  var fileCtxMenu = win.querySelector('.shell-file-ctx-menu');

  // Dismiss context menu on outside click. Scoped to the window: without
  // removeEventListener every opened window left a click handler (and its
  // closed-over DOM) attached to document forever.
  var onFileCtxOutsideClick = function(e) {
    if (!fileCtxMenu || fileCtxMenu.style.display === 'none') return;
    if (!fileCtxMenu.contains(e.target)) fileCtxMenu.style.display = 'none';
  };
  document.addEventListener('click', onFileCtxOutsideClick);
  addWinDisposable(win, function() {
    document.removeEventListener('click', onFileCtxOutsideClick);
  });

  function shellFileBrowse() {
    var path = filePathInput.value.trim() || '/';
    if (!fileListing) return;
    fileListing.innerHTML = '<div style="padding:8px;color:#8b949e">' + escapeHtml(t('common.loading')) + '</div>';
    if (fileCtxMenu) fileCtxMenu.style.display = 'none';
    fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?path=' + encodeURIComponent(path))
      .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function(data) { renderShellFileList(data, path); })
      .catch(function(e) { fileListing.innerHTML = '<div style="padding:8px;color:#f85149">' + escapeHtml(t('file.loadFailed', { msg: String(e.message||e) })) + '</div>'; });
  }

  var _ctxPath = '', _ctxIsDir = false;

  function fileMenuAction(action) {
    if (fileCtxMenu) fileCtxMenu.style.display = 'none';
    var path = _ctxPath;
    if (!path) return;
    var base = path.replace(/\/+$/, '');
    var name = base.split('/').pop() || '';

    if (action === 'download') {
      window.open('/api/sessions/' + encodeURIComponent(sessionId) + '/files/download?path=' + encodeURIComponent(path), '_blank');
    } else if (action === 'delete') {
      if (!confirm(t('file.deleteConfirm', { path: path }))) return;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?path=' + encodeURIComponent(path), {method:'DELETE'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert(t('toast.delete.failed', { msg: e.message })); });
    } else if (action === 'rename') {
      var newName = prompt(t('file.renamePrompt', { name: name }), name);
      if (!newName || newName === name) return;
      var parts = base.split('/'); parts.pop();
      var to = (parts.join('/') || '') + '/' + newName;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?from=' + encodeURIComponent(path) + '&to=' + encodeURIComponent(to), {method:'PUT'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert(t('toast.rename.failed', { msg: e.message })); });
    }
  }

  function showFileMenu(e, path, isDir) {
    e.preventDefault();
    e.stopPropagation();
    _ctxPath = path;
    _ctxIsDir = isDir;
    if (!fileCtxMenu) return;
    fileCtxMenu.querySelectorAll('.file-ctx-item').forEach(function(it) {
      it.style.display = (it.dataset.action === 'download' && isDir) ? 'none' : '';
    });
    fileCtxMenu.style.display = 'block';
    // Position near cursor, within viewport
    var x = (e.touches && e.touches[0] ? e.touches[0].clientX : e.clientX) || e.pageX || 100;
    var y = (e.touches && e.touches[0] ? e.touches[0].clientY : e.clientY) || e.pageY || 100;
    var mw = fileCtxMenu.offsetWidth || 150;
    var mh = fileCtxMenu.offsetHeight || 120;
    if (x + mw > window.innerWidth) x = window.innerWidth - mw - 4;
    if (y + mh > window.innerHeight) y = window.innerHeight - mh - 4;
    fileCtxMenu.style.left = Math.max(0, x) + 'px';
    fileCtxMenu.style.top = Math.max(0, y) + 'px';
  }

  if (fileCtxMenu) {
    fileCtxMenu.addEventListener('click', function(e) {
      var it = e.target.closest('.file-ctx-item');
      if (!it) return;
      fileMenuAction(it.dataset.action);
    });
  }

  // Event delegation for file detail action buttons
  fileListing.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-file-action]');
    if (!btn) return;
    e.stopPropagation();
    var path = btn.getAttribute('data-file-path') || btn.parentElement.getAttribute('data-file-path');
    if (!path) return;
    var action = btn.getAttribute('data-file-action');
    if (action === 'download') {
      window.open('/api/sessions/' + encodeURIComponent(sessionId) + '/files/download?path=' + encodeURIComponent(path), '_blank');
    } else if (action === 'delete') {
      if (!confirm(t('file.deleteConfirm', { path: path }))) return;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?path=' + encodeURIComponent(path), {method:'DELETE'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert(t('toast.delete.failed', { msg: e.message })); });
    } else if (action === 'rename') {
      var pre = btn.getAttribute('data-file-name') || path.split('/').pop() || '';
      var nm = prompt(t('common.rename'), pre);
      if (!nm || nm === pre) return;
      var pp = path.replace(/\/+$/, '').split('/'); pp.pop();
      var to = (pp.join('/') || '') + '/' + nm;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?from=' + encodeURIComponent(path) + '&to=' + encodeURIComponent(to), {method:'PUT'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert(t('toast.rename.failed', { msg: e.message })); });
    }
  });

  var _longPressTimer = null;
  function renderShellFileList(data, currentPath) {
    filePathInput.value = currentPath;
    if (!data.is_dir) {
      // File detail view — show metadata with action buttons
      var nm = data.name || currentPath;
      var detailHtml = '<div class="shell-file-detail" data-file-path="' + escapeHtml(currentPath) + '">';
      detailHtml += '<div style="font-size:0.85rem;font-weight:600;margin-bottom:4px;display:flex;align-items:center;gap:6px"><span>📄</span><span style="word-break:break-all">' + escapeHtml(nm) + '</span></div>';
      detailHtml += '<div style="font-size:0.75rem;color:#8b949e;margin-bottom:2px">' + escapeHtml(t('file.size', { size: formatSize(data.size||0) })) + '</div>';
      if (data.mod_time) detailHtml += '<div style="font-size:0.75rem;color:#8b949e;margin-bottom:2px">' + escapeHtml(t('file.modified', { time: fmtTime(data.mod_time) })) + '</div>';
      detailHtml += '<div class="shell-file-detail-actions">';
      detailHtml += '<button class="sf-dl" data-file-action="download" data-file-path="' + escapeHtml(currentPath) + '">' + escapeHtml(t('common.download')) + '</button>';
      detailHtml += '<button data-file-action="rename" data-file-path="' + escapeHtml(currentPath) + '" data-file-name="' + escapeHtml(nm) + '">' + escapeHtml(t('common.rename')) + '</button>';
      detailHtml += '<button class="sf-del" data-file-action="delete" data-file-path="' + escapeHtml(currentPath) + '">' + escapeHtml(t('common.delete')) + '</button>';
      detailHtml += '</div></div>';
      fileListing.innerHTML = detailHtml;
      return;
    }
    var children = data.children || [];
    if (!children.length) { fileListing.innerHTML = '<div style="padding:8px;color:#8b949e">' + escapeHtml(t('file.empty')) + '</div>'; return; }
    var basePath = currentPath.replace(/\/+$/, '');
    fileListing.innerHTML = children.map(function(c) {
      var icon = c.is_dir ? '📁' : '📄';
      var fullPath = basePath + '/' + c.name;
      return '<div class="shell-file-row" data-path="' + escapeHtml(fullPath) + '" data-isdir="' + (c.is_dir?'1':'0') + '" data-name="' + escapeHtml(c.name) + '" style="display:flex;align-items:center;gap:6px;padding:4px 8px;cursor:pointer;border-bottom:1px solid #30363d;white-space:nowrap">' +
        '<span style="flex-shrink:0">' + icon + '</span><span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis">' + escapeHtml(c.name) + '</span>' +
        (!c.is_dir ? '<span style="flex-shrink:0;color:#8b949e;font-size:0.7rem;margin-right:2px">' + formatSize(c.size||0) + '</span>' : '') +
        '<button class="shell-file-menu-btn" title="' + escapeHtml(t('file.menu')) + '" style="flex-shrink:0">&vellip;</button>' +
        '</div>';
    }).join('');
    // Bind row events
    fileListing.querySelectorAll('.shell-file-row').forEach(function(row) {
      var path = row.getAttribute('data-path');
      var isDir = row.getAttribute('data-isdir') === '1';
      row.addEventListener('click', function(e) {
        if (e.target.closest('.shell-file-menu-btn')) return;
        filePathInput.value = path;
        shellFileBrowse();
      });
      var btn = row.querySelector('.shell-file-menu-btn');
      if (btn) btn.addEventListener('click', function(e) { showFileMenu(e, path, isDir); });
      row.addEventListener('contextmenu', function(e) { showFileMenu(e, path, isDir); });
      row.addEventListener('touchstart', function(e) {
        if (e.target.closest('.shell-file-menu-btn')) return;
        clearTimeout(_longPressTimer);
        _longPressTimer = setTimeout(function() { showFileMenu(e, path, isDir); }, 600);
      }, {passive:true});
      row.addEventListener('touchend', function() { clearTimeout(_longPressTimer); });
      row.addEventListener('touchmove', function() { clearTimeout(_longPressTimer); });
    });
  }

  if (win.querySelector('.shell-file-go')) win.querySelector('.shell-file-go').addEventListener('click', function(e) {
    var svg = this.querySelector('svg');
    if (svg) {
      svg.style.transition = 'transform 0.6s ease';
      svg.style.transform = 'rotate(360deg)';
      setTimeout(function() { svg.style.transition = 'none'; svg.style.transform = 'rotate(0deg)'; }, 1220);
    }
    shellFileBrowse();
  });
  if (win.querySelector('.shell-file-up')) win.querySelector('.shell-file-up').addEventListener('click', function() {
    var p = filePathInput.value.trim().replace(/\/+$/, '') || '/';
    if (p === '/') return;
    var parts = p.split('/'); parts.pop();
    filePathInput.value = parts.join('/') || '/';
    shellFileBrowse();
  });
  if (filePathInput) filePathInput.addEventListener('keydown', function(e) { if (e.key === 'Enter') shellFileBrowse(); });
  if (win.querySelector('.shell-file-upload-btn') && fileUploadInput) {
    win.querySelector('.shell-file-upload-btn').addEventListener('click', function() { fileUploadInput.click(); });
    fileUploadInput.addEventListener('change', function() {
      var file = this.files && this.files[0];
      if (!file) return;
      var path = (filePathInput.value.trim().replace(/\/+$/, '') || '') + '/' + file.name;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files/upload?path=' + encodeURIComponent(path), {method:'POST',body:file})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { console.error(e); });
    });
  }

  /* Exposed so reapplyWindowLanguage can redraw this window's file list from the
     path it already shows (no navigation, no request beyond the listing). */
  win._shellFileBrowse = shellFileBrowse;

  requestAnimationFrame(function () {
    var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
    if (ch && ch.term) try { ch.term.focus(); } catch (e1) {}
  });
}

/** After clicking an entry: show placeholder + spinner immediately to prevent double-connect. */
  // Shared shell-window tab panels (Forwardings + Files)
  var SHELL_WINDOW_PANELS_HTML =       '<div class="shell-tab-panel" data-stab="fw" style="display:none;flex-direction:column;flex:1;min-height:0;overflow:hidden;padding:12px 16px">' +
      '<div style="display:flex;align-items:center;margin-bottom:10px;flex-shrink:0">' +
      '<span style="font-size:0.82rem;font-weight:600;color:#e6edf3" data-i18n="fw.panel.title">Port Forwardings</span>' +
      '<button type="button" class="shell-fw-refresh-btn" title="Refresh" data-i18n-title="common.refresh" style="width:24px;height:24px;margin-left:auto;padding:0;border:1px solid #484f58;border-radius:4px;background:transparent;color:#888;cursor:pointer;display:flex;align-items:center;justify-content:center;margin-right:4px;transition:color .12s,background .12s,border-color .12s" onmouseover="this.style.color=\'#ccc\';this.style.borderColor=\'#666\'" onmouseout="this.style.color=\'#888\';this.style.borderColor=\'#484f58\'" onmousedown="this.style.background=\'rgba(255,255,255,.06)\'" onmouseup="this.style.background=\'transparent\'"><svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M1.5 7A5.5 5.5 0 0111.3 3.8M12.5 7A5.5 5.5 0 012.7 10.2"/><path d="M12.5 1.5v2.5H10M1.5 12.5v-2.5H4"/></svg></button>' +
      '<button type="button" class="shell-fw-add-btn" title="Add forwarding" data-i18n-title="fw.add" style="width:24px;height:24px;padding:0;border:1px solid #2ea043;border-radius:4px;background:transparent;color:#3fb950;cursor:pointer;display:flex;align-items:center;justify-content:center"><svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M7 3v8M3 7h8"/></svg></button>' +
      '</div>' +
      '<div class="shell-fw-list" style="flex:1;min-height:0;overflow:auto;font-size:0.78rem;color:#c9d1d9;border:1px solid #30363d;border-radius:6px;background:#0d1117" data-i18n="common.loading">Loading...</div>' +
    '</div>' +
    '<div class="shell-tab-panel" data-stab="file" style="display:none;flex-direction:column;flex:1;min-height:0;overflow:hidden;padding:12px 16px 12px">' +
      '<div style="font-size:0.82rem;font-weight:600;color:#e6edf3;margin-bottom:8px" data-i18n="win.files.tab">Files</div>' +
      '<div style="display:flex;gap:0;margin-bottom:6px;align-items:stretch;flex-shrink:0;border:1px solid #484f58;overflow:hidden">' +
      '<button type="button" class="shell-file-up" title="Parent" data-i18n-title="file.parent" style="width:30px;height:30px;padding:0;border:none;border-right:1px solid #484f58;background:#2d2d2d;color:#aaa;cursor:pointer;display:flex;align-items:center;justify-content:center;flex-shrink:0"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M2 10l5-5 5 5"/></svg></button>' +
      '<input type="text" class="shell-file-path" placeholder="/" style="flex:1;min-width:0;border:none;outline:none;font-family:ui-monospace,monospace;font-size:0.78rem;padding:0 8px;background:#1e1e1e;color:#d4d4d4">' +
      '<input type="file" class="shell-file-upload-input" style="display:none">' +
      '<button type="button" class="shell-file-upload-btn" title="Upload" data-i18n-title="common.upload" style="width:30px;height:30px;padding:0;border:none;border-left:1px solid #484f58;background:#2d2d2d;color:#aaa;cursor:pointer;display:flex;align-items:center;justify-content:center;flex-shrink:0"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M7 2v8M4 5.5l3-3 3 3M2 10v1.5a1 1 0 001 1h8a1 1 0 001-1V10"/></svg></button>' +
      '<button type="button" class="shell-file-go" title="Load" data-i18n-title="file.load" style="width:30px;height:30px;padding:0;border:none;border-left:1px solid #484f58;background:#2d2d2d;color:#aaa;cursor:pointer;display:flex;align-items:center;justify-content:center;flex-shrink:0"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M1 7a6 6 0 0111.2-3.8M13 7a6 6 0 01-11.2 3.8"/><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M13 2v2.5h-2.5M1 12v-2.5h2.5"/></svg></button>' +
      '</div>' +
      '<div class="shell-file-listing" style="flex:1;min-height:0;overflow:auto;font-size:0.78rem;font-family:ui-monospace,monospace;border:1px solid #484f58;background:#1e1e1e;color:#d4d4d4"></div>' +
        '<div class="shell-file-ctx-menu" style="display:none"><div class="file-ctx-item" data-action="download" data-i18n="common.download">Download</div><div class="file-ctx-item" data-action="rename" data-i18n="common.rename">Rename</div><div class="file-ctx-item" data-action="delete" class="file-ctx-danger" data-i18n="common.delete">Delete</div></div>' +
    '</div>' +
    '<div class="shell-tab-panel" data-stab="notify" style="display:none;flex-direction:column;flex:1;min-height:0;overflow:hidden;padding:12px 16px">' +
      '<div style="display:flex;align-items:center;margin-bottom:10px;flex-shrink:0">' +
      '<span style="font-size:0.82rem;font-weight:600;color:#e6edf3" title="Wake-up rules for this shell (notify tool)" data-i18n-title="win.notify.tip" data-i18n="win.notify.tab">Notifications</span>' +
      '<button type="button" class="shell-ntf-refresh-btn" title="Refresh" data-i18n-title="common.refresh" style="width:24px;height:24px;margin-left:auto;padding:0;border:1px solid #484f58;border-radius:4px;background:transparent;color:#888;cursor:pointer;display:flex;align-items:center;justify-content:center;transition:color .12s,background .12s,border-color .12s" onmouseover="this.style.color=\'#ccc\';this.style.borderColor=\'#666\'" onmouseout="this.style.color=\'#888\';this.style.borderColor=\'#484f58\'" onmousedown="this.style.background=\'rgba(255,255,255,.06)\'" onmouseup="this.style.background=\'transparent\'"><svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M1.5 7A5.5 5.5 0 0111.3 3.8M12.5 7A5.5 5.5 0 012.7 10.2"/><path d="M12.5 1.5v2.5H10M1.5 12.5v-2.5H4"/></svg></button>' +
      '</div>' +
      '<div class="shell-ntf-list" style="flex:1;min-height:0;overflow:auto;font-size:0.78rem;color:#c9d1d9;border:1px solid #30363d;border-radius:6px;background:#0d1117" data-i18n="common.loading">Loading...</div>' +
    '</div>' +
    '</div>'; // end SHELL_WINDOW_PANELS_HTML

function openPendingShellWindow(connName, clickEvent, abortCtl) {
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

  var termEl = win.querySelector('.shell-channel-body');
  var header = win.querySelector('.shell-window-header');
  var closeBtn = win.querySelector('.close-btn');
  var minBtn = win.querySelector('.shell-window-min-btn');
  positionShellWindowFromClick(win, clickEvent);
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
  return win;
}

function finalizePendingShellWindow(win, connLabel, sessionId, shellId, clickEvent) {
  if (!win || !win.parentNode || !sessionId) return;
  win._connectAbort = null;
  win._placeholder = false;
  win._pendingConnName = '';

  win._sid = sessionId;
  win._parentSid = sessionId;
  win._primaryShellId = shellId || sessionId;
  win._connName = connLabel || '';
  var sidEl = win.querySelector('.shell-sid-copy .sid');
  if (sidEl) sidEl.textContent = stripSessionPrefix(sessionId);
  var pend = win.querySelector('.shell-pending');
  if (pend) pend.remove();
  _wireChannelTabBar(win);
  _initShellWindowUI(win, connLabel, sessionId, clickEvent);
  wireReviewBar(win);
  // Seed the review UI here as well as in openShellWindow. This is the path a
  // session created in this tab takes, and without it _approvalMode stays
  // undefined, so typing is forwarded and the server silently drops it.
  applyApprovalModeFromSnapshot(win, sessionId);
  createChannelTab(win, win._primaryShellId);
  refreshSessionTabbar();
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
function openShellWindow(connLabel, sessionId, clickEvent, opts) {
  opts = opts || {};
  var readOnly = !!opts.readOnly;
  if (typeof Terminal === 'undefined') { alert(t('alert.xterm.failed')); return; }
  if (getShellWindowBySid(sessionId)) {
    focusSessionWindow(connLabel, sessionId, clickEvent, opts);
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

  positionShellWindowFromClick(win, clickEvent);
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
  _initShellWindowUI(win, connLabel, sessionId, clickEvent);
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
        exited.forEach(function(s) { createChannelTab(win, s.shell_id || s.id, null, true); });
        return;
      }
      var tabs = shells.filter(function(s) { return readOnly || s.status === 'running'; });
      if (tabs.length > 0) {
        if (!win._primaryShellId) {
          win._primaryShellId = (tabs[0].shell_id || tabs[0].id);
        }
        tabs.forEach(function(s) { createChannelTab(win, s.shell_id || s.id); });
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

/** POST /api/sessions/start then open shell window. command/mode may be '' to use server defaults (login shell, profile default_mode). */
function startSessionAndOpenShell(connName, clickEvt, opt) {
  opt = opt || {};
  var dup = getPendingShellWindowForConn(connName);
  if (dup) {
    bringShellWindowToFront(dup);
    return Promise.resolve(null);
  }
  var ac = new AbortController();
  var pendingWin = openPendingShellWindow(connName, clickEvt, ac);
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
        finalizePendingShellWindow(pendingWin, connName || 'session', sid, shellId, clickEvt);
      } else {
        openShellWindow(connName || 'session', sid, clickEvt);
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
      // opened rather than ignored.
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
