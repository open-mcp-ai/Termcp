var _selectedSessionIds = new Set();

function updateSessionBatchBar() {
  var countEl = document.getElementById('session-batch-count');
  var delBtn = document.getElementById('batch-del-btn');
  var selAllBtn = document.getElementById('batch-sel-all');
  var snapshot = (window._lastSessionsSnapshot || []).filter(function (s) { return s && s.id; });
  var count = _selectedSessionIds.size;
  var total = snapshot.length;
  var allSelected = total > 0 && count >= total;
  if (selAllBtn) {
    selAllBtn.classList.toggle('all-checked', allSelected);
    selAllBtn.title = allSelected ? 'Clear selection' : 'Select all';
    selAllBtn.disabled = total === 0;
  }
  if (countEl) {
    if (count > 0) {
      countEl.style.display = '';
      countEl.textContent = count + ' selected';
    } else {
      countEl.style.display = 'none';
      countEl.textContent = '';
    }
  }
  if (delBtn) {
    delBtn.disabled = count === 0;
    delBtn.title = count > 0 ? 'Delete ' + count + ' selected session' + (count === 1 ? '' : 's') : 'Delete selected';
  }
}

function renderSessionGrid(sessions, bannerMsg) {
  var grid = document.getElementById('session-grid');
  if (!grid) return;
  setLoadBanner(document.getElementById('session-load-banner'), bannerMsg);
  grid.innerHTML = '';
  // Unified grid: running + DEAD sessions now both come from /api/sessions
  // (the registry retains exited sessions). DEAD tiles open the same terminal
  // window in read-only mode — no separate history window.
  var all = (sessions || []).slice();
  if (all.length === 0) {
    _selectedSessionIds.clear();
    updateSessionBatchBar();
    var empty = document.createElement('div');
    empty.style.cssText = 'padding:8px 4px;font-size:0.85rem;color:#656d76';
    empty.textContent = 'No sessions.';
    grid.appendChild(empty);
    return;
  }
  all.forEach(function (s) {
    var dead = !(s.status === 'running');
    var sid = s.id || '';
    var isSelected = _selectedSessionIds.has(sid);
    var tile = document.createElement('div');
    var tileClass = 'conn-tile sess-tile' + (dead ? ' archived' : '') + (isSelected ? ' batch-selected' : '');
    tile.className = tileClass;
    tile.setAttribute('data-sid', sid);
    // Re-apply a pending notify_user highlight across re-renders.
    if (sid && _uiNotifHighlights && _uiNotifHighlights[sid]) {
      tile.classList.add('sess-notified');
    }
    var nm = String((s.name || '').trim());
    var entryLine = nm && nm.indexOf('session-') !== 0 ? nm : displaySessionShort(s);
    var badge = dead ? '<span class="sess-dead" title="' + escapeHtml(reasonLabel(s.reason)) + '">dead</span>' : '';

    var actionCornerHtml =
      '<button type="button" class="sess-x" title="Delete session" aria-label="Delete session">' +
        '<svg viewBox="0 0 12 12" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg>' +
      '</button>';

    tile.innerHTML =
      '<div class="conn-tile-stack">' +
      actionCornerHtml +
      '<div class="icon-wrap" title="' + (dead ? 'Open history' : 'Open terminal') + '"><span class="sess-terminal-ic">' + terminalIconImgHtml() + '</span></div>' +
      '<input type="checkbox" class="sess-checkbox" title="Select session" aria-label="Select session"' + (isSelected ? ' checked' : '') + '>' +
      '</div>' +
      '<div class="sess-tile-body">' +
      '<div class="sess-entry-line" title="' + escapeHtml(entryLine) + '">' + escapeHtml(entryLine) + badge + '</div>' +
      '<div class="sess-meta-row">' +
      '<span class="sess-sid-line" title="' + escapeHtml(sid) + '">' + escapeHtml(sid) + '</span>' +
      '<button type="button" class="sess-rename-btn" title="Rename session" aria-label="Rename session">' + SVG_PENCIL_12 + '</button>' +
      '<button type="button" class="sess-copy-btn" title="Copy URL" aria-label="Copy URL">' + SVG_COPY_12 + '</button>' +
      '</div>' +
      '</div>' +
      '<div class="sess-fwd-info" style="display:none;font-size:0.62rem;color:#656d76;margin-top:2px;text-align:center"></div>';

    tile.title = (dead ? 'Open history (read-only)' : 'Open terminal') + ' · ' + sid;

    var sx = tile.querySelector('.sess-x');
    var delConf = { title: 'Delete session', message: 'Delete "' + entryLine + '"?', okText: 'Delete', danger: true };
    if (sx) {
      sx.onclick = function (e) {
        e.preventDefault();
        e.stopPropagation();
        confirmDialog(delConf).then(function (ok) {
          if (!ok) return;
          fetch('/api/sessions/' + encodeURIComponent(s.id), { method: 'DELETE' })
            .then(function (r) {
              if (!r.ok && r.status !== 204) return r.json().then(function (er) { throw new Error((er && er.error) || 'HTTP ' + r.status); });
              var w = getShellWindowBySid(s.id);
              if (w) closeShellWindow(w);
            })
            .catch(function (err) { showCopyToast('Delete failed: ' + String(err.message || err)); });
        });
      };
    }

    var checkbox = tile.querySelector('.sess-checkbox');
    if (checkbox) {
      checkbox.addEventListener('mousedown', function (e) { e.stopPropagation(); });
      checkbox.addEventListener('click', function (e) {
        // Do NOT preventDefault() here: that would revert the checkbox toggle.
        e.stopPropagation();
        if (checkbox.checked) _selectedSessionIds.add(sid);
        else _selectedSessionIds.delete(sid);
        tile.classList.toggle('batch-selected', checkbox.checked);
        updateSessionBatchBar();
      });
    }

    var copyBtn = tile.querySelector('.sess-copy-btn');
    copyBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    copyBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      copyTextToClipboard(resourceUrlSession(sid)).then(function () { showCopyToast(); }).catch(function () { showCopyToast('Copy failed'); });
    });
    var renBtn = tile.querySelector('.sess-rename-btn');
    renBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    renBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      var cur = String((s.name || '').trim());
      if (cur.indexOf('session-') === 0) cur = '';
      var input = prompt('Rename session', cur);
      if (input === null) return;
      var newName = input.trim();
      if (!newName || (cur && newName === cur)) { showCopyToast('Name unchanged'); return; }
      fetch('/api/sessions/' + encodeURIComponent(sid), {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: newName })
      })
        .then(function (r) {
          if (!r.ok) return r.json().then(function (er) { throw new Error((er && er.error) || 'HTTP ' + r.status); });
          showCopyToast('Renamed');
          // Local optimistic refresh; the server broadcast reconciles shortly.
          var snap = (window._lastSessionsSnapshot || []).slice();
          for (var i = 0; i < snap.length; i++) {
            if (snap[i] && snap[i].id === sid) { snap[i].name = newName; }
          }
          applySessionsSnapshot(snap);
        })
        .catch(function (err) { showCopyToast('Rename failed: ' + (err.message || err)); });
    });

    tile.onclick = function (e) {
      if (e.target.closest('.sess-x') || e.target.closest('.sess-rename-btn') || e.target.closest('.sess-copy-btn') || e.target.closest('.sess-checkbox')) return;
      clearSessNotified(sid); // opening the session acknowledges its notification highlight
      openOrFocusShellWindow(s.name || '', s.id, e, dead ? { readOnly: true } : null);
    };
    if (!dead) {
      var fwdInfo = tile.querySelector('.sess-fwd-info');
      var fwds = (window._lastForwards || []).filter(function(f) { return f.ssh_config === s.name; });
      if (fwds.length > 0) {
        fwdInfo.style.display = 'block';
        fwdInfo.textContent = fwds.length + ' forward' + (fwds.length > 1 ? 's' : '') + ': ' + fwds.map(function(f){ return f.listen_addr + '\u2192' + f.target_addr; }).join(', ');
      }
    }
    grid.appendChild(tile);
  });
  var liveIds = new Set(all.map(function(s) { return s.id; }));
  _selectedSessionIds.forEach(function(id) {
    if (!liveIds.has(id)) _selectedSessionIds.delete(id);
  });
  updateSessionBatchBar();
}

/** Convert a live terminal window to read-only when its session becomes DEAD:
 *  stop streaming/input while keeping the ordinary xterm screen and tabs. Also
 *  surfaces a "dead" badge in the window title so the DEAD state is visible
 *  inside the terminal window (not only on the session tile). Idempotent. */
function lockWindowReadonly(win) {
  if (!win) return;
  win._lockedReadonly = true;
  win._readOnly = true;
  win._inputClosed = true;
  setWindowDeadBadge(win);
  var chans = win._channels || {};
  Object.keys(chans).forEach(function (sid) {
    var ch = chans[sid];
    sendTerminalWatch(sid, false);
    if (ch.term) {
      ch.streamDone = true;
      try { ch.term.options.readOnly = true; } catch (e) {}
      try { ch.term.readOnly = true; } catch (e) {}
    }
    // Channel went live→DEAD with its history already fully painted: render the end
    // marker here as well, so it shows even if the terminal_done frame was lost (e.g.
    // the WS dropped before the server emitted it). Freshly opened DEAD channels have
    // historyLoaded=false and get the marker from the bootstrap .finally instead, so
    // we never paint before their persisted output lands.
    if (ch.historyLoaded === true) showTerminalEndedMarker(win, ch);
  });
}

/** Insert (once) a compact "dead" badge into a terminal window's title right
 *  after the session-id group. Never disturbs layout — flex-shrink:0 and it
 *  sits inside the existing .shell-window-title flex row. */
function setWindowDeadBadge(win) {
  if (!win) return;
  if (win._deadBadgeAdded) return;
  var title = win.querySelector('.shell-window-title');
  if (!title) return;
  win._deadBadgeAdded = true;
  var st = document.createElement('span');
  st.className = 'shell-dead-badge';
  st.title = 'Session ended (read-only)';
  st.textContent = 'dead';
  title.appendChild(st);
}

/** Append the terminal-end marker to one channel at most once. This is the single
 *  choke point for the "ended" state: every path that observes a session/shell exiting
 *  (live terminal_done frame, live window flipping to DEAD, or a freshly opened
 *  DEAD/read-only channel after its history is restored) calls this once. It is gated
 *  on ch.endedMarkPrinted, so the marker renders exactly once per channel no matter
 *  how many events fire. */
function showTerminalEndedMarker(win, ch) {
  if (!ch || ch.endedMarkPrinted) return;
  ch.endedMarkPrinted = true;
  if (win && ch.tabEl) {
    var eb = ch.tabEl.querySelector('.shell-channel-tab-ended');
    if (eb) eb.style.display = '';
  }
  if (ch.term) {
    try {
      ch.term.writeln('\r\n\x1b[33m[Session ended]\x1b[0m', function () {
        shellTermScrollToBottomIfStuck(ch.term, true);
      });
    } catch (e) {
      try { ch.term.writeln('\r\n\x1b[33m[Session ended]\x1b[0m'); } catch (e2) {}
      shellTermScrollToBottomIfStuck(ch.term, true);
    }
  }
}

function applySessionsSnapshot(sessions) {
  window._lastSessionsSnapshot = sessions;
  renderSessionGrid(sessions, '');
  loadForwards();
  // Reconcile open ordinary shell windows with the retained registry:
  //  - running parent → leave live
  //  - DEAD parent → lock readonly, keep the window and tabs
  //  - deleted parent → close the window
  var sessionById = {};
  (sessions || []).forEach(function (s) { if (s.id) sessionById[s.id] = s; });
  // Reconcile EVERY open window — floating (shellWindowsEl) AND tiled
  // (pane-grid). Iterating only the floating container left windows for
  // deleted sessions open forever in the tiled workspace; allShellWins()
  // is the single source of truth for "all open shell windows".
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    if (!w._parentSid || w._placeholder) continue;
    var s = sessionById[w._parentSid];
    if (!s) {
      releaseSessionUIState(w._parentSid);
      closeShellWindow(w);
      continue;
    }
    if (s.status !== 'running') lockWindowReadonly(w);
  }
  // Prune invisible client-side state for sessions that have left the
  // server registry entirely, even if no window was open when they died.
  pruneDeadSessionUIState(sessionById);
  refreshAllWindowTabs();
}

/** Drop client-side maps for sessions no longer present in sessionById. */
function pruneDeadSessionUIState(liveMap) {
  if (!liveMap) return;
  if (_uiNotifHighlights) {
    Object.keys(_uiNotifHighlights).forEach(function(id) { if (!liveMap[id]) delete _uiNotifHighlights[id]; });
  }
  if (_selectedSessionIds && _selectedSessionIds.size) {
    _selectedSessionIds.forEach(function(id) { if (!liveMap[id]) _selectedSessionIds.delete(id); });
  }
  if (window._shellLastActive) {
    Object.keys(window._shellLastActive).forEach(function(id) { if (!liveMap[id]) delete window._shellLastActive[id]; });
  }
  if (window._pendingTerminalWatch) {
    Object.keys(window._pendingTerminalWatch).forEach(function(id) { if (!liveMap[id]) delete window._pendingTerminalWatch[id]; });
  }
}

/** Drop every client-side map keyed by a session id that no longer exists.
 *  Windows are the visible half of the leak; these maps are the invisible
 *  half (stale highlights, remembered tabs, queued watches). */
function releaseSessionUIState(sid) {
  if (!sid) return;
  if (_uiNotifHighlights) delete _uiNotifHighlights[sid];
  if (_selectedSessionIds) _selectedSessionIds.delete(sid);
  if (window._shellLastActive) delete window._shellLastActive[sid];
  if (window._pendingTerminalWatch) delete window._pendingTerminalWatch[sid];
}

/** Per-window tab refresh: fetch child shells for win's parent session and sync tabs.
 *  Each window is independent — no shared loop state, no cross-contamination. */
function refreshWindowTabs(win) {
  if (!win || !win._parentSid) return;
  fetch(sessionAPI(win._parentSid, '/shells'))
    .then(function(r) { if (!r.ok) return; return r.json(); })
    .then(function(j) {
      if (!j || !j.shells) return;
      syncWindowTabs(win, j.shells);
    }).catch(function() {});
}

/** Notify all open windows that the global session list changed; each window independently
 *  decides whether to refresh its own channel tabs. Covers tiled panes too. */
function refreshAllWindowTabs() {
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    refreshWindowTabs(wins[i]);
  }
}

function syncWindowTabs(win, shells) {
  var existing = win._channels || {};
  if (win._readOnly) {
    // DEAD view: render every retained shell snapshot as a read-only tab. Never
    // prune on refresh (all snapshot shells report exited) and never send input.
    (shells || []).forEach(function(s) {
      var sid = s.shell_id || s.id;
      if (!existing[sid]) createChannelTab(win, sid);
    });
    return;
  }
  var running = shells.filter(function(s) { return s.status === 'running'; });
  var exited = shells.filter(function(s) { return s.status !== 'running'; });
  // Live container whose shells have all exited: keep the exited-history tabs
  // read-only (don't recreate them from scratch each refresh).
  if (running.length === 0 && exited.length > 0) {
    (exited).forEach(function(s) {
      var sid = s.shell_id || s.id;
      if (!existing[sid]) createChannelTab(win, sid, null, true);
    });
    return;
  }
  // Add new running shells that don't have a tab yet.
  // Primary shell tab is created on explicit open (session create / restore). Sync only
  // adds additional shells so closing a tab does not recreate it on the next refresh.
  running.forEach(function(s) {
    var sid = s.shell_id || s.id;
    if (!existing[sid] && sid !== win._primaryShellId) createChannelTab(win, sid);
  });
  // Remove tabs for shells that are no longer running. The primary (root) shell
  // is never pruned here: during teardown its exit can surface in a still-RUNNING
  // list frame before the session flips to DEAD, and pruning it here would close
  // the first shell on disconnect/kill. The DEAD lock (readOnly) keeps it; child
  // shells added via "+" are pruned as before on genuine exit. Read-only history
  // channels from an exited container are also kept.
  Object.keys(existing).forEach(function(sid) {
    if (sid === win._primaryShellId) return;
    if (existing[sid] && existing[sid].readOnlyHistory) return;
    var found = running.some(function(s) { return (s.shell_id || s.id) === sid; });
    if (!found && existing[sid]) closeChannelTab(win, sid);
  });
}

