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

/* --- which panel shows which channel --------------------------------------
 *
 * A session can occupy more than one panel: a channel tab dragged out into empty
 * space gets a panel of its own. A channel is still exactly ONE view — the server
 * keeps one watch per channel id and addWatch replaces the previous reader, so
 * two windows claiming one channel would each see part of the stream.
 *
 * Every panel of a session is a peer. There is no "home window" or "guest panel":
 * any panel may adopt an unowned channel, any panel may receive a merge from any
 * other panel of the same session, and an emptied panel closes itself.
 *
 * Ownership is recorded here, as the WINDOW that shows it, because both places
 * that add tabs unprompted (openShellWindow's restore and syncWindowTabs) must
 * ask first: a channel another window already shows is simply not theirs. The
 * map holds DOM nodes rather than ids because its whole job is window identity;
 * reload restores the grouping via sessionStorage.
 */
var _channelOwner = Object.create(null);

function claimChannel(win, sid) { if (sid) _channelOwner[sid] = win; }

function releaseChannel(win, sid) {
  if (sid && _channelOwner[sid] === win) delete _channelOwner[sid];
}

/** May `win` take a channel nobody shows yet?
 *
 *  Ownership is the whole rule. A channel has exactly ONE view (the server replaces
 *  the previous reader on addWatch), so the first window that opens it owns it and
 *  every other window gets `false` from the owner check. Nothing here asks whether
 *  the window came first, was split out, or was opened from the archive: those are
 *  histories, not roles, and a panel that is a peer must not be treated as a guest
 *  because of how it was born. */
function channelFitsWindow(win, sid) {
  var owner = _channelOwner[sid];
  return !owner || owner === win;
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
  var owner = _channelOwner[sessionId];
  if (owner && owner !== win) return; // One live browser view/watch per server channel.
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
  /* Draggable so the channel can be given a panel of its own, or dropped on a
     sibling panel of the same session (see the channel-drag section below). The
     tab is the handle because that is what the operator points at when they mean
     "this channel"; dragging the terminal itself is text selection. */
  tab.draggable = true;
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
  var appearance = termcpTerminalOptions();
  var term = new Terminal({
    cursorBlink: true,
    fontSize: appearance.fontSize,
    fontWeight: appearance.fontWeight,
    fontWeightBold: appearance.fontWeightBold,
    lineHeight: appearance.lineHeight,
    letterSpacing: appearance.letterSpacing,
    /* xterm styles its own grid from this option (it injects a stylesheet
       carrying the string), so it cannot inherit --font-mono from the page.
       Ask util.js for the same list the rest of the UI uses: one stack, one
       source, and the CJK faces it contains are what keep a bare Linux box
       from falling through to a glyph-less generic monospace. */
    fontFamily: termcpMonoFontFamily(),
    /* Let the terminal canvas participate in the glass surface. Without this,
       xterm paints an opaque rectangle over the window's backdrop-filter. */
    allowTransparency: appearance.allowTransparency,
    theme: appearance.theme,
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
  /* THE WINDOW IS THIS CHANNEL'S PARENT. Its teardown is registered on the
     window, so destroying the window releases the channel through the parent's
     own list rather than through a loop that has to know what a channel is — and
     the registration travels with the channel when a drag re-parents it
     (_dropChannelView unregisters it, createChannelTab registers it on the new
     owner). One owner, one registration, one release. */
  var ch = win._channels[sessionId];
  ch.closeFromOwner = function () { closeChannelTab(win, sessionId); };
  addWinDisposable(win, ch.closeFromOwner);
  claimChannel(win, sessionId);
  win._activeChannelSid = sessionId;
  win._channelCount += 1;
  if (typeof savePanelLayoutSoon === 'function') savePanelLayoutSoon();

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
      var ro = attachShellTerminalResizeObserverForChannel(win, term, inst);
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

  // Drag handle for the split/merge gesture.
  tab.addEventListener('dragstart', function(e) {
    if (e.target.closest && e.target.closest('button')) { e.preventDefault(); return; }
    if (!canStartChannelDrag(win)) { e.preventDefault(); return; }
    _channelDrag = { win: win, sid: sessionId };
    try {
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', sessionId);
    } catch (eDT) {}
  });

  // Focus
  syncChannelTabDraggable(win);
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
  if (typeof savePanelLayoutSoon === 'function') savePanelLayoutSoon();

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

  // A read-only view has no live channel to stop, so it must not touch the server at
  // all — closing the tab only closes the view.
  //
  // For a live tab: POST .../archive, NOT DELETE. DELETE is the erase — it stops the
  // channel and drops its record, so the transcript loses the one piece of metadata
  // that addresses it and the archive cannot show the channel afterwards. Closing a
  // tab is an ordinary act ("I am done with this channel"), not a request to destroy
  // its history, and the X is the control a person presses without meaning anything
  // by it beyond that. The record now survives, carries an exit status, and appears
  // in the archive; erasure is still available on the archive's own key, where it is
  // named and confirmed.
  if (!win._readOnly) {
    fetch(apiPath('/api/shells/') + encodeURIComponent(sessionId) + '/archive', { method: 'POST' }).catch(function() {});
  }

  _dropChannelView(win, sessionId);
}

/** Tear down one channel's VIEW: xterm, DOM nodes, tab state, subscription.
 *
 * It is the half of closeChannelTab that a MOVE also needs: dragging a tab into
 * another panel must not archive the shell (the process is alive and still the
 * user's), only stop showing it here. The destination rebuilds the view from the
 * same persisted log a reopened window restores from, which is why the view is
 * torn down instead of re-parented: every listener the tab and the terminal
 * carry closes over the window that created them. */
function _dropChannelView(win, sessionId) {
  var ch = win._channels && win._channels[sessionId];
  if (!ch) return;

  ch.streamDone = true;

  // Unsubscribe. The destination subscribes again when it builds its view.
  sendTerminalWatch(sessionId, false);
  if (window._pendingTerminalWatch && window._pendingTerminalWatch[sessionId]) {
    delete window._pendingTerminalWatch[sessionId];
  }
  if (ch.statusTimer) { clearTimeout(ch.statusTimer); ch.statusTimer = 0; }
  ch.lineKind = 0;
  ch.statusState = '';

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
  // The channel leaves this window's ownership, and its teardown with it: after a
  // move the new owner is the parent, and releasing the old window must not
  // archive a shell another panel is showing.
  removeWinDisposable(win, ch.closeFromOwner);
  ch.closeFromOwner = null;
  releaseChannel(win, sessionId);

  // Switch or show empty
  if (win._activeChannelSid === sessionId) {
    win._activeChannelSid = null;
    var neighbor = _channelNeighbor(win, sessionId);
    if (neighbor && win._channels[neighbor]) {
      switchChannelTab(win, neighbor);
    } else if (win._channelCount === 0) {
      _showChannelEmpty(win, true);
    }
  }
  updateTermScrollButton(win);
  syncChannelTabDraggable(win);
  if (typeof savePanelLayoutSoon === 'function') savePanelLayoutSoon();
}

/* --- moving a channel between panels --------------------------------------
 *
 * Every window here is one session's panel (see openShellWindow), and a session
 * may be shown by several of them. Dragging any channel tab onto another window of
 * the SAME session moves the channel there; dragging a channel out into empty space
 * gives it a panel of its own only when the source has another channel left behind.
 * A drop on a window of a different session is refused — the channel belongs to the
 * connection that created it, so the panels a session can be split across are its
 * own.
 *
 * The panels are PEERS. A window whose channel was split out is not a guest of the
 * window it came from: it may take a channel nobody shows, it may receive a merge,
 * and it may be the window a session's tab raises. The one rule that decides who
 * shows what is `_channelOwner`, which is why every path here goes through it.
 *
 * The moved view is rebuilt, not re-parented (see _dropChannelView), so a move is
 * cheap for the page and lossless for the user: the new tab restores the same log.
 */
var _channelDrag = null;

/** The window a drag would land on, or `{empty:true}` for "give it its own
 *  panel", or null when the drop is a no-op. */
function canStartChannelDrag(win) {
  /* Any channel tab can be dragged: a single-channel window can merge into another
     panel of the same session. If dropped outside, the drop is cancelled so the
     source window never becomes an empty shell. */
  return !!win && win._channelCount >= 1;
}

/** Paint the grab affordance to match canStartChannelDrag: a tab that cannot be
 *  dragged must not show a drag cursor. Called wherever a window's channel count
 *  changes, since the rule is about the count and not about the tab. */
function syncChannelTabDraggable(win) {
  if (!win || !win._channels) return;
  var can = canStartChannelDrag(win);
  Object.keys(win._channels).forEach(function (sid) {
    var tab = win._channels[sid] && win._channels[sid].tabEl;
    if (tab) tab.draggable = can;
  });
}

function _channelDropTargetFor(e) {
  var drag = _channelDrag;
  if (!drag) return null;
  var node = e.target;
  var win = node && node.closest ? node.closest('.shell-window') : null;
  if (win) {
    /* Dropped back where it came from: the channel is already there. */
    if (win === drag.win) return null;
    if (!win._placeholder && !!win._readOnly === !!drag.win._readOnly &&
        win._parentSid === drag.win._parentSid) return win;
  }
  /* When dragging from a multi-channel window, an outside drop splits the channel
     off into a new panel. When dragging the ONLY channel of a window, an outside
     drop is a no-op: it cannot create a new panel, otherwise the source window
     would be emptied into a husk. */
  if (drag.win._channelCount <= 1) return null;
  /* Another session's pane, the empty workspace, anything else: the channel gets
     a panel of its own at the pointer. A channel belongs to the connection that
     created it, so it is never grafted onto a session it is not part of. */
  return { empty: true };
}

function _channelDragHint(win) {
  Array.prototype.forEach.call(document.querySelectorAll('.shell-window.channel-drop-target'), function (w) {
    if (w !== win) w.classList.remove('channel-drop-target');
  });
  if (win && !win.classList.contains('channel-drop-target')) win.classList.add('channel-drop-target');
}

function _onChannelDragOver(e) {
  if (!_channelDrag) return;
  var target = _channelDropTargetFor(e);
  _channelDragHint(target && !target.empty ? target : null);
  if (!target) return;
  e.preventDefault();
  if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
}

function _onChannelDrop(e) {
  var drag = _channelDrag;
  if (!drag) return;
  e.preventDefault();
  var target = _channelDropTargetFor(e);
  _channelDragHint(null);
  _channelDrag = null;
  if (!target) return;
  if (target.empty) detachChannelToPanel(drag.win, drag.sid, windowPositionFromClick(e));
  else moveChannelToWindow(drag.win, drag.sid, target);
}

function _onChannelDragEnd() {
  _channelDrag = null;
  _channelDragHint(null);
}

document.addEventListener('dragover', _onChannelDragOver, false);
document.addEventListener('drop', _onChannelDrop, false);
document.addEventListener('dragend', _onChannelDragEnd, false);

/** Show one channel in another window of the same session. */
function moveChannelToWindow(srcWin, sid, dstWin) {
  var ch = srcWin && srcWin._channels && srcWin._channels[sid];
  if (!ch || !dstWin || dstWin === srcWin) return;
  var index = ch.urlIndex;
  var readOnly = !!(ch.readOnlyHistory || ch.streamDone);
  _dropChannelView(srcWin, sid);
  createChannelTab(dstWin, sid, { index: index, readOnlyHistory: readOnly });
  /* When any source window loses its last channel because it was merged into
     another window of the same session, close the emptied window so no empty
     husk remains on screen. */
  if (srcWin._channelCount === 0) closeShellWindow(srcWin);
  refreshSessionTabbar();
}

/** Give one channel a panel of its own, at `pos` (a client position). The caller
 *  only does this for a multi-channel source, so the original panel is not emptied. */
function detachChannelToPanel(srcWin, sid, pos) {
  var ch = srcWin && srcWin._channels && srcWin._channels[sid];
  if (!ch || !srcWin._parentSid) return;
  var index = ch.urlIndex;
  var readOnly = !!(ch.readOnlyHistory || ch.streamDone);
  _dropChannelView(srcWin, sid);
  if (srcWin._channelCount === 0) closeShellWindow(srcWin);
  openShellWindow(srcWin._connName || '', srcWin._parentSid, pos, {
    shell: sid, index: index, readOnly: !!srcWin._readOnly, channelReadOnly: readOnly
  });
  refreshSessionTabbar();
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
  el._anchor = anchor;

  /* Fixed positioning against the pressed button's viewport rect. Recalculate
     while the owning window moves: the menu is deliberately portaled to body
     (the tiled workspace creates a fixed-position containing block), so it
     cannot travel with the window in the DOM. */
  repositionChannelAddMenu();
  anchor.classList.add('active');

  /* Deferred so the click that opened the menu does not reach the closing
     listener on the same tick. */
  setTimeout(function () {
    document.addEventListener('click', _onChannelAddMenuDocClick, true);
    document.addEventListener('keydown', _onChannelAddMenuKey);
    document.addEventListener('mousemove', scheduleChannelAddMenuPosition, true);
    window.addEventListener('resize', repositionChannelAddMenu);
    window.addEventListener('scroll', repositionChannelAddMenu, true);
  }, 0);
}

var _channelAddMenuRaf = 0;

/* The menu is a portal, so it cannot move with the window in the DOM: it is
 * re-placed from the anchor's rect whenever the pointer moves. The rect is read
 * synchronously (the window drag handler runs later in the bubble, so the
 * position it would report is this event's, not the next one) and the placement
 * is deferred to the next frame. The comparison doubles as the record of "this
 * press moved the panel": a window drag ends with a synthesized click, and that
 * click must not count as the dismissal (see _onChannelAddMenuDocClick). */
function scheduleChannelAddMenuPosition() {
  var el = _channelAddMenu;
  if (!el || !el._anchor) return;
  var r = el._anchor.getBoundingClientRect();
  var was = el._anchorAt;
  el._anchorAt = { left: r.left, top: r.top };
  if (was && (was.left !== r.left || was.top !== r.top)) el._moved = true;
  if (_channelAddMenuRaf) return;
  _channelAddMenuRaf = requestAnimationFrame(function () {
    _channelAddMenuRaf = 0;
    repositionChannelAddMenu();
  });
}

function repositionChannelAddMenu() {
  if (!_channelAddMenu || !_channelAddMenu._anchor || !_channelAddMenu._anchor.isConnected) return;
  var el = _channelAddMenu;
  var r = el._anchor.getBoundingClientRect();
  var maxLeft = Math.max(8, window.innerWidth - el.offsetWidth - 8);
  el.style.left = Math.max(8, Math.min(r.left, maxLeft)) + 'px';
  var below = r.bottom + 4;
  if (below + el.offsetHeight > window.innerHeight - 8) below = r.top - el.offsetHeight - 4;
  el.style.top = Math.max(8, below) + 'px';
}

var _channelAddMenu = null;

function closeChannelAddMenu() {
  if (_channelAddMenu) {
    _channelAddMenu.remove();
    _channelAddMenu = null;
  }
  document.removeEventListener('click', _onChannelAddMenuDocClick, true);
  document.removeEventListener('keydown', _onChannelAddMenuKey);
  document.removeEventListener('mousemove', scheduleChannelAddMenuPosition, true);
  if (_channelAddMenuRaf) cancelAnimationFrame(_channelAddMenuRaf);
  _channelAddMenuRaf = 0;
  window.removeEventListener('resize', repositionChannelAddMenu);
  window.removeEventListener('scroll', repositionChannelAddMenu, true);
  Array.prototype.forEach.call(document.querySelectorAll('.shell-channel-add-caret.active, .shell-channel-empty-caret.active, .shell-channel-tab-add.active, .shell-channel-empty-add.active'), function (b) {
    b.classList.remove('active');
  });
}

function _onChannelAddMenuDocClick(e) {
  if (!_channelAddMenu) return;
  if (_channelAddMenu.contains(e.target)) return;
  if (e.target.closest && e.target.closest('.shell-channel-add-caret, .shell-channel-empty-caret, .shell-channel-tab-add, .shell-channel-empty-add')) return;
  /* A window drag ends with a click on whatever the pointer was released over.
     That click is the gesture's full stop, not a request to dismiss: closing here
     made the menu impossible to keep while moving the panel it belongs to. */
  if (_channelAddMenu._moved) { _channelAddMenu._moved = false; return; }
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
  return fetch(apiPath('/api/sessions/') + encodeURIComponent(win._parentSid) + '/shells', {
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
  /* A narrowed scope is named in the tooltip. "Review on" is true of a session
     that only holds file writes, and a lock that says only that would have the
     reviewer believe typing is gated when it is not — the one thing a lock must
     never be vague about. "all" is the default and needs no explanation. */
  var mode = win._approvalMode ? t('review.lock.on') : t('review.lock.off');
  var scopes = win._approvalScopes;
  if (win._approvalMode && Array.isArray(scopes) && scopes.length) {
    mode += t('review.lock.scoped', { scopes: scopes.join(', ') });
  }
  btn.title = mode + (n ? t('review.lock.waitingCount', { count: n }) : t('review.lock.clickOpen'));
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
      applyApprovalMode(win, !!list[i].approval_mode, list[i].scopes);
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
      applyApprovalMode(win, !!j.approval_mode, j.scopes);
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
function applyApprovalMode(win, on, scopes) {
  if (!win) return;
  win._approvalMode = !!on;
  // What the switch covers, for the lock's tooltip. Absent/"all" means every
  // gated kind, which is the default and what an older payload reports.
  win._approvalScopes = Array.isArray(scopes) ? scopes : null;
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
function attachShellTerminalResizeObserverForChannel(win, term, termEl) {
  // THE OBSERVER MUST REFIT, NOT ONLY TELL THE REMOTE SIDE (issue #165).
  //
  // It used to call scheduleShellRemoteResize only: the PTY was told the new geometry
  // while the LOCAL grid kept its old row count. The xterm element then stayed taller
  // than the box that contains it, and the scrollbar belongs to that inner viewport —
  // so it could be scrolled to ITS bottom while the last rows sat outside the visible
  // area. That is the reported shape exactly: "the scrollbar is at the bottom but the
  // content is not", with no scroll position that reaches the tail.
  //
  // Measured in a real browser before this change: shrinking the window to 300px left
  // the container at 264px and the xterm at 408px, rows still 26 — a 144px band of
  // rows that no amount of scrolling could show. Nothing else had to happen: no font
  // change, no viewport resize, no switching. Any layout change that shrinks the
  // container does it, and the dock growing (a new session) is one.
  //
  // Coalesced through rAF because a drag emits a box per frame and term.resize is
  // expensive; the flag lives on the TERM, not the window, so two channels resizing in
  // the same frame do not cancel each other.
  var ro = new ResizeObserver(function() {
    if (term._termcpFitRaf) return;
    term._termcpFitRaf = requestAnimationFrame(function() {
      term._termcpFitRaf = 0;
      // fitShellTerminal reads the container and returns false when it is gone or
      // zero-sized, so a channel torn down mid-frame is a no-op rather than an error.
      try { fitShellTerminal(term, termEl, win, true); } catch (e) {}
    });
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

/** Read resolved CSS colors so both new and existing xterms use one palette. */
function termcpXtermTheme() {
  var css = getComputedStyle(document.documentElement);
  var defaults = {
    background: 'rgba(0, 0, 0, 0)', foreground: '#d4d4d4',
    cursor: '#6ee2ff', cursorAccent: '#0b1220',
    selectionBackground: 'rgba(110, 226, 255, .22)'
  };
  var properties = {
    background: 'background', foreground: 'foreground', cursor: 'cursor',
    cursorAccent: 'cursor-accent', selectionBackground: 'selection',
    black: 'black', red: 'red', green: 'green', yellow: 'yellow', blue: 'blue',
    magenta: 'magenta', cyan: 'cyan', white: 'white', brightBlack: 'bright-black',
    brightRed: 'bright-red', brightGreen: 'bright-green', brightYellow: 'bright-yellow',
    brightBlue: 'bright-blue', brightMagenta: 'bright-magenta', brightCyan: 'bright-cyan', brightWhite: 'bright-white'
  };
  var colors = {};
  Object.keys(properties).forEach(function (key) {
    var value = css.getPropertyValue('--xterm-' + properties[key]).trim();
    colors[key] = value || defaults[key];
  });
  // The wrap owns the tint in transparent mode, keeping one alpha layer behind
  // the text. Opaque mode lets xterm paint the same solid surface as its window.
  colors.background = _themeTerminal.transparent ? 'rgba(0, 0, 0, 0)' :
    (css.getPropertyValue('--terminal-background').trim() || css.getPropertyValue('--xterm-background').trim() || css.getPropertyValue('--bg-card').trim() || '#ffffff');
  return colors;
}

function termcpTerminalOptions() {
  var css = getComputedStyle(document.documentElement);
  function number(property, fallback) {
    var value = parseFloat(css.getPropertyValue(property));
    return isFinite(value) ? value : fallback;
  }
  function weight(property, fallback) {
    var value = css.getPropertyValue(property).trim();
    return value && isFinite(Number(value)) ? Number(value) : value || fallback;
  }
  var requestedFont = css.getPropertyValue('--terminal-font-family').trim() || css.getPropertyValue('--font-mono').trim();
  var fontQuery = weight('--terminal-font-weight', 'normal') + ' ' + number('--terminal-font-size', 13) + 'px ' + requestedFont;
  if (requestedFont && document.fonts && typeof document.fonts.check === 'function' && typeof document.fonts.load === 'function' &&
      !document.fonts.check(fontQuery, 'M中')) {
    var revision = _themeRevision;
    var key = revision + ':' + fontQuery;
    if (_terminalThemeFontRequest !== key) {
      _terminalThemeFontRequest = key;
      document.fonts.load(fontQuery, 'M中').then(function () {
        if (revision === _themeRevision && _terminalThemeFontRequest === key) updateTerminalTheme();
      }).catch(function () { /* Keep the installed fallback if a font cannot load. */ });
    }
  }
  return {
    fontFamily: termcpMonoFontFamily(),
    fontSize: number('--terminal-font-size', 13),
    fontWeight: weight('--terminal-font-weight', 'normal'),
    fontWeightBold: weight('--terminal-font-weight-bold', 'bold'),
    lineHeight: number('--terminal-line-height', 1),
    letterSpacing: number('--terminal-letter-spacing', 0),
    /* Passed to xterm, which repairs a cell it cannot read by moving the text
       colour's lightness away from the background it was painted on. `ls`
       colours an other-writable directory blue on green, and eight of the
       sixteen ANSI slots are the UI's semantic tokens — a hue picked to read
       on the page, not on another ANSI colour. Without this the light theme
       renders that cell at a ratio of 1.00 (identical colours). */
    minimumContrastRatio: number('--terminal-minimum-contrast-ratio', 4.5),
    allowTransparency: !!_themeTerminal.transparent,
    theme: termcpXtermTheme()
  };
}

var _terminalThemeFontRequest = '';

function updateTerminalTheme() {
  var options = termcpTerminalOptions();
  allShellWins().forEach(function (win) {
    Object.keys(win._channels || {}).forEach(function (sid) {
      var channel = win._channels[sid];
      if (!channel.term) return;
      var current = channel.term.options;
      var metricsChanged = current.fontFamily !== options.fontFamily || current.fontSize !== options.fontSize ||
        current.fontWeight !== options.fontWeight || current.fontWeightBold !== options.fontWeightBold ||
        current.lineHeight !== options.lineHeight || current.letterSpacing !== options.letterSpacing;
      Object.keys(options).forEach(function (key) { current[key] = options[key]; });
      if (metricsChanged && channel.instEl && typeof fitShellTerminal === 'function') {
        requestAnimationFrame(function () {
          if (win._channels && win._channels[sid] === channel) fitShellTerminal(channel.term, channel.instEl, win, true);
        });
      }
    });
  });
}

onThemeChange(updateTerminalTheme);
