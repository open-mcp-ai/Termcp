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
/** Create a channel tab + xterm instance for sessionId.
 *
 * `opt.index` is the channel's server-assigned number — the N of
 * termcp://#<session>:N, and the number the tab label shows. It must come from
 * the server rather than from a client-side counter: closing an earlier channel
 * leaves the server's numbering untouched, while a client counter would renumber
 * the survivors and make the label disagree with a locator copied earlier.
 * `opt.readOnlyHistory` marks a channel rendered from already-exited output.
 *
 * A missing index is a programming error (every caller has one). It is reported
 * rather than silently replaced by a guessed number, because a guessed number is
 * exactly the drift this parameter exists to remove. The tab is still created so
 * the channel stays usable in this browser; only its copy button is disabled. */
function createChannelTab(win, sessionId, opt) {
  opt = opt || {};
  if (!win._channels) { win._channels = {}; win._channelCount = 0; }
  if (win._channels[sessionId]) { switchChannelTab(win, sessionId); return; }
  var urlIndex = Number(opt.index);
  var haveIndex = Number.isInteger(urlIndex) && urlIndex >= 1;
  if (!haveIndex) {
    console.error('shell channel is missing its server index; the copy button will stay disabled', sessionId, opt);
  }
  var label = haveIndex ? ('shell-' + urlIndex) : 'shell';
  var readOnlyHistory = !!opt.readOnlyHistory;

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
    '<span class="shell-channel-tab-state" style="display:none"></span>' +
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
    /* xterm styles its own grid from this option (it injects a stylesheet
       carrying the string), so it cannot inherit --font-mono from the page.
       Ask util.js for the same list the rest of the UI uses: one stack, one
       source, and the CJK faces it contains are what keep a bare Linux box
       from falling through to a glyph-less generic monospace. */
    fontFamily: termcpMonoFontFamily(),
    /* Let the terminal canvas participate in the glass surface. Without this,
       xterm paints an opaque rectangle over the window's backdrop-filter. */
    allowTransparency: true,
    theme: {
      /* xterm paints this literal into its canvas. Keep it fully transparent and
         let the CSS chrome supply the tint: an alpha here would stack on top of
         the window's own layers and end up effectively opaque. */
      background: 'rgba(0, 0, 0, 0)',
      foreground: '#d4d4d4',
      /* Preserve the terminal's existing cursor and selection colours; the page's
         cyan belongs to the chrome and status accents, not to input paint. */
      cursor: '#6ee2ff',
      cursorAccent: '#0b1220',
      selectionBackground: 'rgba(110, 226, 255, 0.22)'
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
      /* A live channel that is quiet needs no event to reach its resting state:
         "nothing changed for three seconds" is already true when its history
         lands, and a shell that sends no frame at all would otherwise leave the
         chip blank until the operator touched it. Arming the idle timer here is
         the same rule the byte path uses, just started from the restored log. */
      shellStatusTouch(win, sessionId);
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

  // Copy this shell's resource URL. Without a server index there is no correct
  // locator to emit, so the button is disabled rather than copying a guess.
  var tabCopyBtn = tab.querySelector('.shell-channel-tab-copy');
  if (!haveIndex) {
    tabCopyBtn.disabled = true;
    tabCopyBtn.title = 'No channel index from the server';
  }
  tabCopyBtn.addEventListener('mousedown', function(e) { e.stopPropagation(); });
  tabCopyBtn.addEventListener('click', function(e) {
    e.preventDefault();
    e.stopPropagation();
    if (!haveIndex) return;
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
    createChannelTab(win, j.shell_id || j.session_id, { index: j.index || 0 });
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
