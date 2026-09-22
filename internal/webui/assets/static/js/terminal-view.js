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
  var addBtn = tabsBar ? tabsBar.querySelector('.shell-channel-tab-add') : null;
  var emptyEl = win.querySelector('.shell-channel-empty');

  // Hide empty state, restore body
  if (emptyEl) emptyEl.classList.remove('show');
  if (body) body.style.display = '';

  // Create tab
  var tab = document.createElement('div');
  tab.className = 'shell-channel-tab active';
  tab.setAttribute('data-chsid', sessionId);
  tab.innerHTML = '<span class="shell-channel-tab-label">' + escapeHtml(label || 'shell') + '</span>' +
    '<button type="button" class="shell-channel-tab-copy" title="Copy URL" aria-label="Copy URL">' + SVG_COPY_12 + '</button>' +
    '<span class="shell-channel-tab-ended" style="display:none" title="Session ended">end</span>' +
    '<button type="button" class="shell-channel-tab-close" title="Close shell">&times;</button>';
  if (addBtn) tabsBar.insertBefore(tab, addBtn);

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
    copyTextToClipboard(url).then(function () { showCopyToast(); }).catch(function () { showCopyToast('Copy failed'); });
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

/** "+" button / empty-state click handler. */
function addChannelClick(win) {
  if (!win._parentSid || win._readOnly) return;
  fetch('/api/sessions/' + encodeURIComponent(win._parentSid) + '/shells', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ rows: 24, cols: 80 })
  }).then(function(r) {
    if (!r.ok) return r.text().then(function(t) { throw new Error(t); });
    return r.json();
  }).then(function(j) {
    createChannelTab(win, j.shell_id || j.session_id);
  }).catch(function(err) { console.error('add channel failed', err); });
}

/** Wire up channel tab bar events (called once per window). */
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
  fab.title = 'Back to latest output';
  fab.setAttribute('aria-label', 'Back to latest output');
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
  if (closeBtn) closeBtn.title = 'Delete session';
  bindShellWindowMinButton(win, minBtn);
  var titleCopyBtn = win.querySelector('.title-copy-btn');
  if (titleCopyBtn) {
    titleCopyBtn.style.display = '';
    titleCopyBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    titleCopyBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      copyTextToClipboard(resourceUrlSession(sessionId)).then(function () { showCopyToast(); }).catch(function () { showCopyToast('Copy failed'); });
    });
  }

  bindShellWindowCloseButton(win, closeBtn);

  win._inputClosed = false;
  setupTermScrollFab(win);

  collapseBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
  collapseBtn.addEventListener('click', function (e) {
    e.preventDefault(); e.stopPropagation();
    var collapsing = !win.classList.contains('shell-window-collapsed');
    if (collapsing) {
      win._savedH = win.style.height || (win.getBoundingClientRect().height + 'px');
      win.style.height = 'auto';
      win.style.minHeight = '0';
    } else {
      win.style.height = win._savedH || '';
      win.style.minHeight = '';
    }
    win.classList.toggle('shell-window-collapsed');
    var icD = collapseBtn.querySelector('.ic-d');
    var icU = collapseBtn.querySelector('.ic-u');
    if (icD && icU) {
      icD.style.display = win.classList.contains('shell-window-collapsed') ? 'none' : '';
      icU.style.display = win.classList.contains('shell-window-collapsed') ? '' : 'none';
    }
    collapseBtn.title = win.classList.contains('shell-window-collapsed') ? 'Expand' : 'Collapse';
    if (!win.classList.contains('shell-window-collapsed')) {
      var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
      if (ch && ch.term && ch.instEl) fitShellTerminal(ch.term, ch.instEl, win, true);
    }
  });

  setupShellWindowDrag(win, header);
  setupShellWindowResize(win);

  // --- Header tab switching (term / fw / file / notify panels) ---
  // Inject the Notifications tab here so both the pending and connected
  // window templates get it without duplicating the tab-bar markup.
  var tabBarEl = win.querySelector('.shell-tab-bar');
  if (tabBarEl && !tabBarEl.querySelector('.shell-tab-btn[data-stab="notify"]')) {
    var ntfTabBtn = document.createElement('button');
    ntfTabBtn.type = 'button';
    ntfTabBtn.className = 'shell-tab-btn';
    ntfTabBtn.setAttribute('data-stab', 'notify');
    ntfTabBtn.title = 'Notifications';
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
    if (!fwds.length) { listEl.innerHTML = '<div style="padding:16px;color:#8b949e;text-align:center">No active forwards</div>'; return; }
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
    if (!rules.length) { listEl.innerHTML = '<div style="padding:16px;color:#8b949e;text-align:center">No active notifications</div>'; return; }
    listEl.innerHTML = rules.map(function(n){
      var evColor = n.event === 'exit' ? '#f85149' : (n.event === 'silence' ? '#d29922' : '#3fb950');
      var extra = (n.event === 'silence' && n.silence_seconds) ? ' ' + n.silence_seconds + 's' : '';
      var chLabel = n.channel === 'sampling' ? 'sampling' : 'resource';
      return '<div style="display:flex;align-items:center;gap:10px;padding:10px 12px;border-bottom:1px solid #21262d" onmouseover="this.style.background=\'#161b22\'" onmouseout="this.style.background=\'\'">' +
        '<span style="font-size:0.65rem;font-weight:600;text-transform:uppercase;padding:1px 5px;border-radius:3px;color:' + evColor + ';border:1px solid ' + evColor + ';flex-shrink:0;min-width:56px;text-align:center">' + escapeHtml(n.event) + escapeHtml(extra) + '</span>' +
        '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:0.78rem"><span style="color:#8b949e">' + escapeHtml(chLabel) + '</span> <span style="color:#484f58">·</span> <span style="color:#c9d1d9">' + escapeHtml(n.shell_id) + '</span></span>' +
        '<button class="shell-ntf-del-btn" data-ntfid="' + escapeHtml(n.rule_id) + '" title="Unregister" style="padding:2px 6px;font-size:0.68rem;border:1px solid transparent;border-radius:3px;background:transparent;color:#484f58;cursor:pointer;flex-shrink:0" onmouseover="this.style.borderColor=\'#f85149\';this.style.color=\'#f85149\'" onmouseout="this.style.borderColor=\'transparent\';this.style.color=\'#484f58\'">✕</button>' +
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
    fileListing.innerHTML = '<div style="padding:8px;color:#8b949e">Loading...</div>';
    if (fileCtxMenu) fileCtxMenu.style.display = 'none';
    fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?path=' + encodeURIComponent(path))
      .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function(data) { renderShellFileList(data, path); })
      .catch(function(e) { fileListing.innerHTML = '<div style="padding:8px;color:#f85149">Error: ' + escapeHtml(String(e.message||e)) + '</div>'; });
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
      if (!confirm('Delete ' + path + '?')) return;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?path=' + encodeURIComponent(path), {method:'DELETE'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert('Delete failed: ' + e.message); });
    } else if (action === 'rename') {
      var newName = prompt('Rename ' + name, name);
      if (!newName || newName === name) return;
      var parts = base.split('/'); parts.pop();
      var to = (parts.join('/') || '') + '/' + newName;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?from=' + encodeURIComponent(path) + '&to=' + encodeURIComponent(to), {method:'PUT'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert('Rename failed: ' + e.message); });
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
      if (!confirm('Delete ' + path + '?')) return;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?path=' + encodeURIComponent(path), {method:'DELETE'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert('Delete failed: ' + e.message); });
    } else if (action === 'rename') {
      var pre = btn.getAttribute('data-file-name') || path.split('/').pop() || '';
      var nm = prompt('Rename', pre);
      if (!nm || nm === pre) return;
      var pp = path.replace(/\/+$/, '').split('/'); pp.pop();
      var to = (pp.join('/') || '') + '/' + nm;
      fetch('/api/sessions/' + encodeURIComponent(sessionId) + '/files?from=' + encodeURIComponent(path) + '&to=' + encodeURIComponent(to), {method:'PUT'})
        .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function() { shellFileBrowse(); })
        .catch(function(e) { alert('Rename failed: ' + e.message); });
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
      detailHtml += '<div style="font-size:0.75rem;color:#8b949e;margin-bottom:2px">Size: ' + formatSize(data.size||0) + '</div>';
      if (data.mod_time) detailHtml += '<div style="font-size:0.75rem;color:#8b949e;margin-bottom:2px">Modified: ' + escapeHtml(fmtTime(data.mod_time)) + '</div>';
      detailHtml += '<div class="shell-file-detail-actions">';
      detailHtml += '<button class="sf-dl" data-file-action="download" data-file-path="' + escapeHtml(currentPath) + '">Download</button>';
      detailHtml += '<button data-file-action="rename" data-file-path="' + escapeHtml(currentPath) + '" data-file-name="' + escapeHtml(nm) + '">Rename</button>';
      detailHtml += '<button class="sf-del" data-file-action="delete" data-file-path="' + escapeHtml(currentPath) + '">Delete</button>';
      detailHtml += '</div></div>';
      fileListing.innerHTML = detailHtml;
      return;
    }
    var children = data.children || [];
    if (!children.length) { fileListing.innerHTML = '<div style="padding:8px;color:#8b949e">Empty</div>'; return; }
    var basePath = currentPath.replace(/\/+$/, '');
    fileListing.innerHTML = children.map(function(c) {
      var icon = c.is_dir ? '📁' : '📄';
      var fullPath = basePath + '/' + c.name;
      return '<div class="shell-file-row" data-path="' + escapeHtml(fullPath) + '" data-isdir="' + (c.is_dir?'1':'0') + '" data-name="' + escapeHtml(c.name) + '" style="display:flex;align-items:center;gap:6px;padding:4px 8px;cursor:pointer;border-bottom:1px solid #30363d;white-space:nowrap">' +
        '<span style="flex-shrink:0">' + icon + '</span><span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis">' + escapeHtml(c.name) + '</span>' +
        (!c.is_dir ? '<span style="flex-shrink:0;color:#8b949e;font-size:0.7rem;margin-right:2px">' + formatSize(c.size||0) + '</span>' : '') +
        '<button class="shell-file-menu-btn" title="Menu" style="flex-shrink:0">&vellip;</button>' +
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

  requestAnimationFrame(function () {
    var ch = win._activeChannelSid && win._channels && win._channels[win._activeChannelSid];
    if (ch && ch.term) try { ch.term.focus(); } catch (e1) {}
  });
}

/** After clicking an entry: show placeholder + spinner immediately to prevent double-connect. */
  // Shared shell-window tab panels (Forwardings + Files)
  var SHELL_WINDOW_PANELS_HTML =       '<div class="shell-tab-panel" data-stab="fw" style="display:none;flex-direction:column;flex:1;min-height:0;overflow:hidden;padding:12px 16px">' +
      '<div style="display:flex;align-items:center;margin-bottom:10px;flex-shrink:0">' +
      '<span style="font-size:0.82rem;font-weight:600;color:#e6edf3">Port Forwardings</span>' +
      '<button type="button" class="shell-fw-refresh-btn" title="Refresh" style="width:24px;height:24px;margin-left:auto;padding:0;border:1px solid #484f58;border-radius:4px;background:transparent;color:#888;cursor:pointer;display:flex;align-items:center;justify-content:center;margin-right:4px;transition:color .12s,background .12s,border-color .12s" onmouseover="this.style.color=\'#ccc\';this.style.borderColor=\'#666\'" onmouseout="this.style.color=\'#888\';this.style.borderColor=\'#484f58\'" onmousedown="this.style.background=\'rgba(255,255,255,.06)\'" onmouseup="this.style.background=\'transparent\'"><svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M1.5 7A5.5 5.5 0 0111.3 3.8M12.5 7A5.5 5.5 0 012.7 10.2"/><path d="M12.5 1.5v2.5H10M1.5 12.5v-2.5H4"/></svg></button>' +
      '<button type="button" class="shell-fw-add-btn" title="Add forwarding" style="width:24px;height:24px;padding:0;border:1px solid #2ea043;border-radius:4px;background:transparent;color:#3fb950;cursor:pointer;display:flex;align-items:center;justify-content:center"><svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M7 3v8M3 7h8"/></svg></button>' +
      '</div>' +
      '<div class="shell-fw-list" style="flex:1;min-height:0;overflow:auto;font-size:0.78rem;color:#c9d1d9;border:1px solid #30363d;border-radius:6px;background:#0d1117">Loading...</div>' +
    '</div>' +
    '<div class="shell-tab-panel" data-stab="file" style="display:none;flex-direction:column;flex:1;min-height:0;overflow:hidden;padding:12px 16px 12px">' +
      '<div style="font-size:0.82rem;font-weight:600;color:#e6edf3;margin-bottom:8px">Files</div>' +
      '<div style="display:flex;gap:0;margin-bottom:6px;align-items:stretch;flex-shrink:0;border:1px solid #484f58;overflow:hidden">' +
      '<button type="button" class="shell-file-up" title="Parent" style="width:30px;height:30px;padding:0;border:none;border-right:1px solid #484f58;background:#2d2d2d;color:#aaa;cursor:pointer;display:flex;align-items:center;justify-content:center;flex-shrink:0"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M2 10l5-5 5 5"/></svg></button>' +
      '<input type="text" class="shell-file-path" placeholder="/" style="flex:1;min-width:0;border:none;outline:none;font-family:ui-monospace,monospace;font-size:0.78rem;padding:0 8px;background:#1e1e1e;color:#d4d4d4">' +
      '<input type="file" class="shell-file-upload-input" style="display:none">' +
      '<button type="button" class="shell-file-upload-btn" title="Upload" style="width:30px;height:30px;padding:0;border:none;border-left:1px solid #484f58;background:#2d2d2d;color:#aaa;cursor:pointer;display:flex;align-items:center;justify-content:center;flex-shrink:0"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M7 2v8M4 5.5l3-3 3 3M2 10v1.5a1 1 0 001 1h8a1 1 0 001-1V10"/></svg></button>' +
      '<button type="button" class="shell-file-go" title="Load" style="width:30px;height:30px;padding:0;border:none;border-left:1px solid #484f58;background:#2d2d2d;color:#aaa;cursor:pointer;display:flex;align-items:center;justify-content:center;flex-shrink:0"><svg viewBox="0 0 14 14" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M1 7a6 6 0 0111.2-3.8M13 7a6 6 0 01-11.2 3.8"/><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M13 2v2.5h-2.5M1 12v-2.5h2.5"/></svg></button>' +
      '</div>' +
      '<div class="shell-file-listing" style="flex:1;min-height:0;overflow:auto;font-size:0.78rem;font-family:ui-monospace,monospace;border:1px solid #484f58;background:#1e1e1e;color:#d4d4d4"></div>' +
        '<div class="shell-file-ctx-menu" style="display:none"><div class="file-ctx-item" data-action="download">Download</div><div class="file-ctx-item" data-action="rename">Rename</div><div class="file-ctx-item" data-action="delete" class="file-ctx-danger">Delete</div></div>' +
    '</div>' +
    '<div class="shell-tab-panel" data-stab="notify" style="display:none;flex-direction:column;flex:1;min-height:0;overflow:hidden;padding:12px 16px">' +
      '<div style="display:flex;align-items:center;margin-bottom:10px;flex-shrink:0">' +
      '<span style="font-size:0.82rem;font-weight:600;color:#e6edf3">Notifications</span>' +
      '<button type="button" class="shell-ntf-refresh-btn" title="Refresh" style="width:24px;height:24px;margin-left:auto;padding:0;border:1px solid #484f58;border-radius:4px;background:transparent;color:#888;cursor:pointer;display:flex;align-items:center;justify-content:center;transition:color .12s,background .12s,border-color .12s" onmouseover="this.style.color=\'#ccc\';this.style.borderColor=\'#666\'" onmouseout="this.style.color=\'#888\';this.style.borderColor=\'#484f58\'" onmousedown="this.style.background=\'rgba(255,255,255,.06)\'" onmouseup="this.style.background=\'transparent\'"><svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M1.5 7A5.5 5.5 0 0111.3 3.8M12.5 7A5.5 5.5 0 012.7 10.2"/><path d="M12.5 1.5v2.5H10M1.5 12.5v-2.5H4"/></svg></button>' +
      '</div>' +
      '<div class="shell-ntf-list" style="flex:1;min-height:0;overflow:auto;font-size:0.78rem;color:#c9d1d9;border:1px solid #30363d;border-radius:6px;background:#0d1117">Loading...</div>' +
    '</div>' +
    '</div>'; // end SHELL_WINDOW_PANELS_HTML

function openPendingShellWindow(connName, clickEvent, abortCtl) {
  if (typeof Terminal === 'undefined') { alert('xterm failed to load'); return null; }
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
      '<button type="button" class="title-copy-btn" style="display:none" title="Copy URL" aria-label="Copy URL">' + SVG_COPY_12 + '</button>' +
      '</span>' +
      '</span>' +
      '</h3>' +
      '</div>' +
      '<div class="shell-tab-bar">' +
        '<button type="button" class="shell-tab-btn active" data-stab="term" title="Terminal"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><rect x="1" y="2" width="12" height="10" rx="1.5"/><path d="M4 5l2 2-2 2"/><line x1="8" y1="9" x2="11" y2="9"/></svg></button>' +
        '<button type="button" class="shell-tab-btn" data-stab="fw" title="Forwardings"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3"><rect x="3.5" y="3.5" width="7" height="7" rx="1"/><path d="M1 7h3.5M9.5 7h3.5M7 1v3.5M7 9.5v3.5" stroke-linecap="round"/></svg></button>' +
        '<button type="button" class="shell-tab-btn" data-stab="file" title="Files"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><path d="M1 3v9a1 1 0 001 1h10a1 1 0 001-1V4.5a1 1 0 00-1-1h-5L5.5 2H2a1 1 0 00-1 1z"/></svg></button>' +
        '</div>' +
      '<div class="shell-window-header-btns">' +
        '<button type="button" class="shell-window-max-btn" title="Maximize (dblclick header)"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M2 5V2h3M10 7v3H7M2 2l3.5 3.5M10 10L6.5 6.5"/></svg></button><button type="button" class="shell-window-collapse-btn" title="Collapse">' +
          '<svg class="ic-d" viewBox="0 0 12 8" width="12" height="8"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M1 2l5 4 5-4"/></svg>' +
          '<svg class="ic-u" viewBox="0 0 12 8" width="12" height="8" style="display:none"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M1 6l5-4 5 4"/></svg>' +
        '</button>' +
        '<button type="button" class="shell-window-min-btn" title="Minimize (session keeps running)">' +
          '<svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 6h8"/></svg>' +
        '</button>' +
        '<button type="button" class="close-btn" title="Close (cancel connect)">' +
          '<svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg>' +
        '</button>' +
      '</div>' +
    '</div>' +
    '<div class="shell-window-content"><div class="shell-terminal-wrap">' +
      '<div class="shell-channel-body">' +
        '<div class="shell-pending"><div class="shell-pending-spinner" aria-hidden="true"></div><div>Connecting <strong>' + escapeHtml(connName) + '</strong>…</div></div>' +
      '</div>' +
      '<div class="shell-channel-tabs">' +
        '<button type="button" class="shell-channel-tab-add" title="New shell channel">+</button>' +
      '</div>' +
      '<div class="shell-channel-empty">' +
        '<span>No shell channels</span>' +
        '<button type="button" class="shell-channel-empty-add">+ New Shell</button>' +
      '</div>' +
    '</div>' +
SHELL_WINDOW_PANELS_HTML +
    '<div class="shell-window-resize-handle" title="Drag to resize"></div>';

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
  createChannelTab(win, win._primaryShellId);
  refreshSessionTabbar();
}

function openShellWindow(connLabel, sessionId, clickEvent, opts) {
  opts = opts || {};
  var readOnly = !!opts.readOnly;
  if (typeof Terminal === 'undefined') { alert('xterm failed to load'); return; }
  if (getShellWindowBySid(sessionId)) {
    openOrFocusShellWindow(connLabel, sessionId, clickEvent, opts);
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
      '<button type="button" class="title-copy-btn" title="Copy URL" aria-label="Copy URL">' + SVG_COPY_12 + '</button>' +
      '</span>' +
      '</span>' +
      '</h3>' +
      '</div>' +
      '<div class="shell-tab-bar">' +
        '<button type="button" class="shell-tab-btn active" data-stab="term" title="Terminal"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><rect x="1" y="2" width="12" height="10" rx="1.5"/><path d="M4 5l2 2-2 2"/><line x1="8" y1="9" x2="11" y2="9"/></svg></button>' +
        '<button type="button" class="shell-tab-btn" data-stab="fw" title="Forwardings"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3"><rect x="3.5" y="3.5" width="7" height="7" rx="1"/><path d="M1 7h3.5M9.5 7h3.5M7 1v3.5M7 9.5v3.5" stroke-linecap="round"/></svg></button>' +
        '<button type="button" class="shell-tab-btn" data-stab="file" title="Files"><svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><path d="M1 3v9a1 1 0 001 1h10a1 1 0 001-1V4.5a1 1 0 00-1-1h-5L5.5 2H2a1 1 0 00-1 1z"/></svg></button>' +
        '</div>' +
      '<div class="shell-window-header-btns">' +
        '<button type="button" class="shell-window-max-btn" title="Maximize (dblclick header)"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" d="M2 5V2h3M10 7v3H7M2 2l3.5 3.5M10 10L6.5 6.5"/></svg></button><button type="button" class="shell-window-collapse-btn" title="Collapse">' +
          '<svg class="ic-d" viewBox="0 0 12 8" width="12" height="8"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M1 2l5 4 5-4"/></svg>' +
          '<svg class="ic-u" viewBox="0 0 12 8" width="12" height="8" style="display:none"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M1 6l5-4 5 4"/></svg>' +
        '</button>' +
        '<button type="button" class="shell-window-min-btn" title="Minimize (session keeps running)">' +
          '<svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 6h8"/></svg>' +
        '</button>' +
        '<button type="button" class="close-btn" title="Delete session">' +
          '<svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg>' +
        '</button>' +
      '</div>' +
    '</div>' +
    '<div class="shell-window-content"><div class="shell-terminal-wrap">' +
      '<div class="shell-channel-body"></div>' +
      '<div class="shell-channel-tabs">' +
        '<button type="button" class="shell-channel-tab-add" title="New shell channel">+</button>' +
      '</div>' +
      '<div class="shell-channel-empty">' +
        '<span>No shell channels</span>' +
        '<button type="button" class="shell-channel-empty-add">+ New Shell</button>' +
      '</div>' +
    '</div>' +
SHELL_WINDOW_PANELS_HTML +
    '<div class="shell-window-resize-handle" title="Drag to resize"></div>';

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

