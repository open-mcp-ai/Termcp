/** The two plates, each with the selection set and the DOM ids it owns. `dead`
 *  splits the snapshot: the sessions plate renders the running half, the archive
 *  the other. One table holds both plates whole — card markup, actions, pruning
 *  and the selection sets themselves all come from here. */
var _sessionRegions = [
  { dead: false, ids: new Set(), gridId: 'session-grid', countId: 'session-batch-count', selAllId: 'session-sel-all', delId: 'session-del-btn', stopId: 'session-stop-btn' },
  { dead: true, ids: new Set(), gridId: 'archive-grid', countId: 'archive-batch-count', selAllId: 'archive-sel-all', delId: 'archive-del-btn', stopId: 'archive-stop-btn' }
];

/** A session is over once it stops running; the registry retains it for the
 *  archive rather than dropping it. */
function isDeadSession(s) { return !(s && s.status === 'running'); }

/* ---- sessions still being dialled ----
 *
 * A connect that is still in progress has no session yet: the server registers
 * one only when the dial succeeds, so it is absent from every snapshot and the
 * Sessions plate cannot show it. The only place a dial exists before that is the
 * placeholder window that was opened for it, and that window IS the dial — it
 * holds the AbortController whose abort is the only thing that can stop the
 * handshake server-side (see startSessionAndOpenShell).
 *
 * These synthesise a card for each placeholder so the connecting work is visible
 * in the plate and therefore selectable by the same select-all and batch actions
 * as everything else. The card carries a prefixed id rather than a real session
 * id: it must never be mistaken for something the server can look up, because
 * `terminate` and `DELETE` would both 404 on it — cancelling the dial means
 * aborting the request that made it, not calling an endpoint.
 */
var PENDING_ID_PREFIX = 'pending:';

/** The synthetic card id for a dial in progress, keyed by the profile it dials.
 *  One placeholder per profile exists at a time (startSessionAndOpenShell
 *  refuses a duplicate), so the profile name is a stable identity for the card:
 *  the same value across re-renders, which is what lets a selection survive
 *  the frequent session frames that repaint this grid. */
function pendingCardId(connName) { return PENDING_ID_PREFIX + (connName || ''); }

/** Is this card id one of the synthesised connecting cards? */
function isPendingCardId(id) {
  return typeof id === 'string' && id.indexOf(PENDING_ID_PREFIX) === 0;
}

/** Every dial in progress, as snapshot-shaped records so the plate renders them
 *  through the same card markup as a real session. `status` is 'running'
 *  because the connect IS in progress — the card belongs to the live plate, and
 *  it is what makes it land in a select-all. */
function pendingSessionRecords() {
  if (typeof allShellWins !== 'function') return [];
  var out = [];
  allShellWins().forEach(function (w) {
    if (!w || !w._placeholder) return;
    var conn = w._pendingConnName || '';
    out.push({
      id: pendingCardId(conn),
      name: conn || t('tab.connecting'),
      status: 'running',
      _pending: true,
      _pendingConn: conn
    });
  });
  return out;
}

/** The list every plate reads: the server's snapshot plus the dials the server
 *  cannot know about yet. One place composes them, so the running plate, both
 *  regions' membership, and pruning all see the same list. */
function sessionsWithPending() {
  return (window._lastSessionsSnapshot || []).concat(pendingSessionRecords());
}

/** Stop a dial in progress. This is the batch form of the placeholder window's
 *  own close button: aborting the request is what makes the server's context
 *  end and the handshake stop (Manager.Create rechecks the context before it
 *  registers anything, so an aborted dial never leaves a session behind).
 *
 *  Returns true when a placeholder was found and cancelled. */
function cancelPendingConn(connName) {
  if (typeof getPendingShellWindowForConn !== 'function') return false;
  var win = getPendingShellWindowForConn(connName);
  if (!win) return false;
  closeShellWindow(win);
  return true;
}

/** Cancel every dial currently in progress, returning how many were stopped.
 *  Used by both batch actions: a connecting card has no session to terminate or
 *  delete, so whichever action is chosen, cancelling is the whole effect. */
function cancelAllPendingConns() {
  var cancelled = 0;
  pendingSessionRecords().forEach(function (p) {
    if (cancelPendingConn(p._pendingConn)) cancelled++;
  });
  return cancelled;
}

/** Sessions belonging to a region, from the last snapshot, so a region only ever
 *  sees the half it renders. */
function sessionsForRegion(region, snapshot) {
  return (snapshot || []).filter(function (s) { return s && s.id && isDeadSession(s) === region.dead; });
}

/** Drop from both plates' selections every id that is absent from `present`
 *  (a map keyed by session id). One pass, because a session that leaves the
 *  registry must leave both sets: it is gone from both plates.
 *
 *  `present` must come from the composed list (sessionsWithPending), not the raw
 *  snapshot: a dial in progress is absent from the server's snapshot by
 *  definition, so pruning against that alone would clear its tick on the next
 *  repaint — the selection would not survive the frame that renders the card. */
function pruneSessionSelections(present) {
  if (!present) return;
  _sessionRegions.forEach(function (region) {
    region.ids.forEach(function (id) { if (!present[id]) region.ids.delete(id); });
  });
}

/** Show a region's trash only once it has a selection, and report whether its
 *  checkbox is fully checked. */
function updateSessionBatchBar() {
  var snapshot = sessionsWithPending();
  _sessionRegions.forEach(function (region) {
    var countEl = document.getElementById(region.countId);
    var delBtn = document.getElementById(region.delId);
    var selAllBtn = document.getElementById(region.selAllId);
    var members = sessionsForRegion(region, snapshot);
    var selected = members.filter(function (s) { return region.ids.has(s.id); });
    var count = selected.length;
    var allSelected = members.length > 0 && count >= members.length;
    if (selAllBtn) {
      selAllBtn.classList.toggle('all-checked', allSelected);
      selAllBtn.title = allSelected ? t('section.batch.clearSelection') : t('section.batch.selectAll');
      selAllBtn.disabled = members.length === 0;
    }
    if (countEl) {
      if (count > 0) {
        countEl.style.display = '';
        countEl.textContent = t('batch.count.selected', { count: count });
      } else {
        countEl.style.display = 'none';
        countEl.textContent = '';
      }
    }
    if (delBtn) {
      // Hidden, not disabled: the trash IS the "something is selected" signal,
      // so it must be absent rather than greyed out.
      delBtn.hidden = count === 0;
      delBtn.title = count > 0
        ? tCount('batch.del.title.one', 'batch.del.title.other', { count: count })
        : t('section.batch.deleteSelected');
    }
    /* Stopping is offered for the live plate only: an archived session has
       already stopped, and cancelling a dial is the same action, so the archive
       (which never holds a pending card) has no use for this key. It appears on
       the same terms as the trash — with a selection. */
    var stopBtn = region.stopId ? document.getElementById(region.stopId) : null;
    if (stopBtn) {
      stopBtn.hidden = count === 0;
      stopBtn.title = count > 0
        ? tCount('batch.disconnect.title.one', 'batch.disconnect.title.other', { count: count })
        : t('section.batch.disconnectSelected');
    }
  });
}

// The list lives in window._lastSessionsSnapshot, written by
// applySessionsSnapshot and read by every pass below; the banner message is the
// only thing a caller varies.
function renderSessionGrid(bannerMsg) {
  setLoadBanner(document.getElementById('session-load-banner'), bannerMsg);
  // `all` carries the dials in progress as well: a connecting card has to be
  // rendered by the same pass (and be selectable by the same controls) as a
  // real session, or select-all would reach only the sessions the server
  // already knows about.
  var all = sessionsWithPending();
  // One renderer for both plates: the split is which half each region owns, so
  // the card markup, rename, copy, lock and delete paths cannot drift apart.
  _sessionRegions.forEach(function (region) { renderTileGrid(region, all); });
  var present = {};
  all.forEach(function (s) { if (s && s.id) present[s.id] = true; });
  pruneSessionSelections(present);
  updateSessionBatchBar();
}

/** Render one region's tiles, from the half of the snapshot that region owns.
 *  The card itself is identical in both, so a session keeps its look when it
 *  moves from running to over. */
function renderTileGrid(region, all) {
  var grid = document.getElementById(region.gridId);
  if (!grid) return;
  var selectedIds = region.ids;
  grid.innerHTML = '';
  // Unified card: DEAD sessions are the same tile as running ones (the registry
  // retains them); a DEAD tile opens its terminal window in read-only mode.
  var rows = all.filter(function (s) { return isDeadSession(s) === region.dead; });
  /* A rowless plate says why it is rowless instead of being a blank strip. It
     only appears once a snapshot has arrived: before the first frame every plate
     is trivially empty, and "no live sessions" would be a claim about a list the
     page has not read yet. */
  if (rows.length === 0 && window._lastSessionsSnapshot) grid.appendChild(sessionPlateEmpty(region));
  rows.forEach(function (s) {
    var sid = s.id || '';
    // This plate renders one kind of session, so the card's DEAD state follows
    // from which plate built it — the same reason the card markup is shared.
    var dead = region.dead;
    var isSelected = selectedIds.has(sid);
    var tile = document.createElement('div');
    var tileClass = 'conn-tile sess-tile' + (dead ? ' archived' : '') + (isSelected ? ' batch-selected' : '');
    tile.className = tileClass;
    tile.setAttribute('data-sid', sid);
    // Re-apply a pending notify_user highlight across re-renders.
    if (sid && _uiNotifHighlights && _uiNotifHighlights[sid]) {
      tile.classList.add('sess-notified');
    }
    // A dial in progress: the card shows the profile it is dialling and a
    // spinner instead of the live/dead lamp. Everything else on the card is
    // suppressed below rather than branched here, so the two kinds of card
    // cannot drift apart in structure.
    var pending = !!s._pending;
    var nm = String((s.name || '').trim());
    var entryLine = pending
      ? (nm || t('tab.connecting'))
      : (nm && nm.indexOf('session-') !== 0 ? nm : displaySessionShort(s));
    /* Status lamp at the left of the id: green while the session is up, red once
       it is over. The reason still travels in the tooltip. A connecting card has
       neither state yet, so it gets the spinner the pending window already uses
       and says what it is doing in the same tooltip slot. */
    var statusText = pending
      ? t('session.status.connecting')
      : (dead
          ? t('session.status.dead', { reason: reasonLabel(s.reason) })
          : t('session.status.running'));
    var statusIc = pending
      ? '<span class="sess-status-ic is-connecting" title="' +
        escapeHtml(statusText) + '" role="img" aria-label="' +
        escapeHtml(statusText) + '"></span>'
      :
        '<span class="sess-status-ic ' + (dead ? 'is-dead' : 'is-live') + '" title="' +
        escapeHtml(statusText) + '" role="img" aria-label="' +
        escapeHtml(statusText) + '"></span>';

    /* Approval lock: a state indicator that is also the switch. It sits in the
       meta row with the lamp and the id, which is already the line that reads
       "what state is this session in". Only live sessions can be gated — a DEAD
       session has no input to gate. */
    var gated = !!s.approval_mode;
    var lockHtml = (!dead && !pending)
      ? '<button type="button" class="sess-lock' + (gated ? ' is-on' : '') + '" ' +
          'title="' + escapeHtml(gated ? t('review.disableTip') : t('review.enableTip')) + '" ' +
          'aria-pressed="' + (gated ? 'true' : 'false') + '" ' +
          'aria-label="' + escapeHtml(t('review.mode')) + '">' +
          (gated ? SVG_LOCK_CLOSED : SVG_LOCK_OPEN) +
        '</button>'
      : '';

    // The corner key cancels the dial rather than deleting a session: there is
    // nothing on the server to delete yet, and the abort is what stops the
    // handshake.
    var actionCornerHtml = pending
      ? '<button type="button" class="sess-x" title="' + escapeHtml(t('session.cancelDial.title')) + '" ' +
        'aria-label="' + escapeHtml(t('session.cancelDial.title')) + '">' +
        '<svg viewBox="0 0 12 12" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg>' +
        '</button>'
      :
        '<button type="button" class="sess-x" title="Delete session" data-i18n-title="session.delete.title" aria-label="Delete session" data-i18n-aria="session.delete.title">' +
        '<svg viewBox="0 0 12 12" width="13" height="13" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg>' +
        '</button>';

    /* The name line is the rename control, and the sid line copies the session
       URL. Neither exists for a dial in progress: there is no session to rename
       and no id to address yet, so a connecting card keeps both elements but
       leaves them inert (no role, no tab stop, and the sid slot carries the
       status word in place of an id that does not exist). The elements are kept
       rather than dropped so the two cards stay the same shape — the CSS grid
       that places them then needs no second variant. */
    var nameRowHtml =
      '<div class="sess-name-row">' +
      '<input type="checkbox" class="sess-checkbox" title="Select session" data-i18n-title="session.aria.select" aria-label="Select session" data-i18n-aria="session.aria.select"' + (isSelected ? ' checked' : '') + '>' +
      (pending
        ? '<div class="sess-entry-line">' + escapeHtml(entryLine) + '</div>'
        : '<div class="sess-entry-line" role="button" tabindex="0" title="Rename session" data-i18n-title="session.aria.rename" aria-label="Rename session" data-i18n-aria="session.aria.rename">' + escapeHtml(entryLine) + '</div>') +
      '</div>';
    /* The lamp sits in the meta row, immediately before the sid: that row is the
       line that reads "what state is this session in". A connecting card swaps
       the lamp for the spinner and the id for the status word, but keeps both
       slots in this one row, so the row reads the same way on either card. The
       sid span is written out in both branches rather than hoisted into a
       variable, because the row's own markup is what a test pins. */
    tile.innerHTML =
      '<div class="conn-tile-stack">' +
      actionCornerHtml +
      '<div class="icon-wrap" title="' + escapeHtml(dead ? t('session.openHistory') : t('session.openTerminal')) + '"><span class="sess-terminal-ic">' + terminalIconImgHtml() + '</span></div>' +
      '</div>' +
      '<div class="sess-tile-body">' +
      nameRowHtml +
      '<div class="sess-meta-row">' +
      statusIc +
      lockHtml +
      (pending
        ? '<span class="sess-sid-line is-pending">' + escapeHtml(statusText) + '</span>'
        : '<span class="sess-sid-line" role="button" tabindex="0" title="Copy session URL" data-i18n-title="session.aria.copyUrl" aria-label="Copy session URL" data-i18n-aria="session.aria.copyUrl">' + escapeHtml(sid) + '</span>') +
      '</div>' +
      reviewBadgeHtml(sid) +
      '</div>' +
      '<div class="sess-fwd-info" style="display:none"></div>';

    /* A pending card's tooltip says what it is doing; the real card's names what
       opening it will do. */
    tile.title = pending
      ? statusText + ' · ' + escapeHtml(s._pendingConn || '')
      : (dead ? t('session.openHistory.tip') : t('session.openTerminal')) + ' · ' + sid;

    var lockBtn = tile.querySelector('.sess-lock');
    if (lockBtn) {
      lockBtn.onclick = function (e) {
        e.preventDefault();
        e.stopPropagation();
        toggleSessionApproval(s, gated, lockBtn);
      };
    }

    var sx = tile.querySelector('.sess-x');
    var delConf = {
      title: t('session.delete.title'),
      message: t('session.delete.message', { name: entryLine }),
      okText: t('common.delete'),
      danger: true
    };
    if (sx) {
      sx.onclick = function (e) {
        e.preventDefault();
        e.stopPropagation();
        /* Cancelling a dial needs no confirmation: the user is undoing their own
           click, nothing is destroyed, and the abort is immediate on purpose.
           Confirming would put a dialog between the user and the one gesture
           whose whole point is "stop now". */
        if (pending) {
          cancelPendingConn(s._pendingConn);
          renderSessionGrid('');
          return;
        }
        confirmDialog(delConf).then(function (ok) {
          if (!ok) return;
          fetch(apiPath('/api/sessions/') + encodeURIComponent(s.id), { method: 'DELETE' })
            .then(function (r) {
              if (!r.ok && r.status !== 204) return r.json().then(function (er) { throw new Error((er && er.error) || 'HTTP ' + r.status); });
              var w = getShellWindowBySid(s.id);
              if (w) closeShellWindow(w);
            })
            .catch(function (err) { showCopyToast(t('toast.delete.failed', { msg: String(err.message || err) })); });
        });
      };
    }

    var checkbox = tile.querySelector('.sess-checkbox');
    if (checkbox) {
      checkbox.addEventListener('mousedown', function (e) { e.stopPropagation(); });
      checkbox.addEventListener('click', function (e) {
        // Do NOT preventDefault() here: that would revert the checkbox toggle.
        e.stopPropagation();
        if (checkbox.checked) selectedIds.add(sid);
        else selectedIds.delete(sid);
        tile.classList.toggle('batch-selected', checkbox.checked);
        updateSessionBatchBar();
      });
    }

    /* The sid copies what the button next to it used to: the session URL. The
       button went because the id is already the thing you aim at. */
    var sidEl = tile.querySelector('.sess-sid-line');
    if (sidEl) {
      var doCopy = function (e) {
        e.preventDefault();
        e.stopPropagation();
        copyTextToClipboard(resourceUrlSession(sid)).then(function () { showCopyToast(); }).catch(function () { showCopyToast(t('toast.copy.failed')); });
      };
      sidEl.addEventListener('mousedown', function (e) { e.stopPropagation(); });
      sidEl.addEventListener('click', doCopy);
      sidEl.addEventListener('keydown', function (e) {
        if (e.key !== 'Enter' && e.key !== ' ') return;
        doCopy(e);
      });
    }

    var nameEl = tile.querySelector('.sess-entry-line');
    var doRename = function (e) {
      e.preventDefault();
      e.stopPropagation();
      /* A connecting card renders its name as plain text (no role/tabindex), so
         this handler is unreachable for it — the guard is here because the name
         element is found by class on both kinds of card, and renaming a
         synthetic id would PATCH an endpoint that cannot resolve it. */
      if (pending || !nameEl.hasAttribute('role')) return;
      var cur = String((s.name || '').trim());
      if (cur.indexOf('session-') === 0) cur = '';
      var input = prompt(t('session.prompt.rename'), cur);
      if (input === null) return;
      var newName = input.trim();
      if (!newName || (cur && newName === cur)) { showCopyToast(t('session.toast.unchanged')); return; }
      fetch(apiPath('/api/sessions/') + encodeURIComponent(sid), {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: newName })
      })
        .then(function (r) {
          if (!r.ok) return r.json().then(function (er) { throw new Error((er && er.error) || 'HTTP ' + r.status); });
          showCopyToast(t('session.toast.renamed'));
          // Local optimistic refresh; the server broadcast reconciles shortly.
          var snap = (window._lastSessionsSnapshot || []).slice();
          for (var i = 0; i < snap.length; i++) {
            if (snap[i] && snap[i].id === sid) { snap[i].name = newName; }
          }
          applySessionsSnapshot(snap);
        })
        .catch(function (err) { showCopyToast(t('toast.rename.failed', { msg: (err.message || err) })); });
    };
    /* The name is the rename control: the pencil it replaced sat next to the sid
       and read as "edit the id", which is not what it did. */
    nameEl.addEventListener('mousedown', function (e) { e.stopPropagation(); });
    nameEl.addEventListener('click', doRename);
    nameEl.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      doRename(e);
    });

    tile.onclick = function (e) {
      if (e.target.closest('.sess-x') || e.target.closest('.sess-checkbox') || e.target.closest('.sess-entry-line') || e.target.closest('.sess-sid-line') || e.target.closest('.sess-status-ic')) return;
      /* A connecting card has no session to open. Its click raises the pending
         window instead, which is the one actionable thing that exists for a dial
         in progress and the same thing the connection list does for it. */
      if (pending) {
        var pw = typeof getPendingShellWindowForConn === 'function' ? getPendingShellWindowForConn(s._pendingConn) : null;
        if (pw) bringShellWindowToFront(pw);
        return;
      }
      clearSessNotified(sid); // opening the session acknowledges its notification highlight
      /* The card names the position, so a click on the session grid still opens
         the terminal at the pointer; a keyboard activation has none and centres. */
      focusSessionWindow(s.name || '', s.id, windowPositionFromClick(e), dead ? { readOnly: true } : null);
    };
    if (!dead && !pending) {
      var fwdInfo = tile.querySelector('.sess-fwd-info');
      var fwds = (window._lastForwards || []).filter(function(f) { return f.ssh_config === s.name; });
      if (fwds.length > 0) {
        fwdInfo.style.display = 'block';
        fwdInfo.textContent = tCount('fwd.count.one', 'fwd.count.other', { count: fwds.length }) + ': ' + fwds.map(function(f){ return f.listen_addr + '\u2192' + f.target_addr; }).join(', ');
      }
    }
    grid.appendChild(tile);
  });
  applyI18n(grid);
}

/** A plate with no rows reads as a terminal that printed nothing. The sentence
 *  is the catalog's (one per plate), and the node replaces what would otherwise
 *  be an empty grid — the plate would still be there, saying nothing about why. */
function sessionPlateEmpty(region) {
  var el = document.createElement('div');
  el.className = 'sess-plate-empty';
  el.textContent = region.dead ? t('plate.archive.empty') : t('plate.sessions.empty');
  return el;
}

/** reviewBadgeHtml builds a session card's pending-review badge from the counts
 *  already in hand.
 *
 *  The grid is rebuilt from scratch on every session frame, and submitting a
 *  request emits one of those frames (the manager's list-change broadcast). A
 *  badge painted only by a later pass was therefore wiped by the very frame that
 *  announced the request: measured in a browser, the tab bar showed "1" while
 *  the card stayed empty. Building the badge into the tile is what makes it
 *  survive its own re-render. */
function reviewBadgeHtml(sid) {
  var n = (window._approvalCounts || {})[sid] || 0;
  if (!n) return '<div class="sess-review" hidden></div>';
  return '<div class="sess-review">' + escapeHtml(tCount('review.badge.one', 'review.badge.other', { count: n })) + '</div>';
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
  /* The header lamp tracks the same transition: a window that just became
     read-only must not keep claiming a live link. */
  if (typeof setWindowLinkState === 'function') setWindowLinkState(win, 'is-dead');
  if (win._deadBadgeAdded) return;
  var title = win.querySelector('.shell-window-title');
  if (!title) return;
  win._deadBadgeAdded = true;
  var st = document.createElement('span');
  st.className = 'shell-dead-badge';
  st.title = t('session.dead.badgeTitle');
  st.textContent = t('session.dead.badge');
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
    /* The chip is repainted, not revealed: it carries a state, and "ended" is
       only one of them, so the node is always present and its text is written
       by whoever changes the state. */
    var sid = ch.tabEl.getAttribute('data-chsid');
    if (sid) shellStatusSet(win, sid, 'ended');
  }
  if (ch.term) {
    try {
      ch.term.writeln('\r\n\x1b[33m' + t('term.ended') + '\x1b[0m', function () {
        shellTermScrollToBottomIfStuck(ch.term, true);
      });
    } catch (e) {
      try { ch.term.writeln('\r\n\x1b[33m' + t('term.ended') + '\x1b[0m'); } catch (e2) {}
      shellTermScrollToBottomIfStuck(ch.term, true);
    }
  }
}

/**
 * toggleSessionApproval flips a session's approval gate.
 *
 * Turning it on asks for the threshold first: the number of approvers is the
 * real control (termcp authenticates a deployment with one token, so it cannot
 * prove who an approver is), and defaulting it silently to 1 would hide that.
 * Turning it off cancels everything pending server-side, hence the confirmation.
 */
function toggleSessionApproval(s, gated, btn) {
  if (!s || !s.id) return;
  if (gated) {
    confirmDialog({
      title: t('review.disable.title'),
      message: t('review.disable.msgSession', { session: displaySessionShort(s) }),
      okText: t('review.disable.ok')
    }).then(function (ok) {
      if (!ok) return;
      setSessionApproval(s.id, false).catch(function (e) {
        showCopyToast(t('review.toast.failed', { msg: String(e.message || e) }));
      });
    });
    return;
  }
  setSessionApproval(s.id, true).catch(function (e) {
    showCopyToast('Failed: ' + String(e.message || e));
  });
}

/** setSessionApproval PATCHes the gate. The session-list frame re-renders the
 *  card, so no local state is kept here — the server is the single source.
 *
 *  No reviewer count is sent: review has one reviewer, so a number would be a
 *  field with a single valid value.
 */
function setSessionApproval(sessionID, enabled) {
  var body = enabled ? { enabled: true } : { enabled: false };
  return fetch(sessionAPI(sessionID, '/approval'), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  }).then(function (r) {
    if (!r.ok) return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
    return r.json();
  }).then(function (j) {
    showCopyToast(enabled ? t('review.toast.on') : t('review.toast.off'));
    // The server broadcasts the new session list (Manager.EnableApproval), and the
    // WebSocket frame re-renders the cards. Nothing is refreshed locally: reading
    // back here would be a second source of truth for the same state.
    return j;
  });
}

function applySessionsSnapshot(sessions) {
  window._lastSessionsSnapshot = sessions;
  renderSessionGrid('');
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
    // Approval mode is part of the session snapshot, so an open window reflects a
    // switch flipped in another tab without needing its own request.
    w._approvalNeed = s.approval_need || 1;
    applyApprovalMode(w, !!s.approval_mode);
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
  pruneSessionSelections(liveMap);
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
  _sessionRegions.forEach(function (region) { region.ids.delete(sid); });
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
      if (!existing[sid]) createChannelTab(win, sid, { index: s.index || 0 });
    });
    return;
  }
  var running = shells.filter(function(s) { return s.status === 'running'; });
  var exited = shells.filter(function(s) { return s.status !== 'running'; });
  // Add tabs for shells the window does not show yet. An already-exited shell is
  // opened read-only: its stream is over, and a pipe channel exists precisely to
  // run to exit, so its tab is the output the user asked for — not a corpse to
  // sweep away.
  exited.forEach(function(s) {
    var sid = s.shell_id || s.id;
    if (!existing[sid]) createChannelTab(win, sid, { index: s.index || 0, readOnlyHistory: true });
  });
  // Primary shell tab is created on explicit open (session create / restore).
  // Sync only adds additional shells so closing a tab does not recreate it on
  // the next refresh.
  running.forEach(function(s) {
    var sid = s.shell_id || s.id;
    if (!existing[sid] && sid !== win._primaryShellId) createChannelTab(win, sid, { index: s.index || 0 });
  });
  // Prune tabs for shells the server no longer lists at all — closed elsewhere
  // (MCP shell_close, another window). A shell that merely exited stays listed
  // with its buffer, so its tab stays too; the primary (root) shell is never
  // pruned here, because during teardown its exit can surface in a still-RUNNING
  // list frame before the session flips to DEAD.
  Object.keys(existing).forEach(function(sid) {
    if (sid === win._primaryShellId) return;
    var found = shells.some(function(s) { return (s.shell_id || s.id) === sid; });
    if (!found && existing[sid]) closeChannelTab(win, sid);
  });
}

/* Language switch: re-render the tiles from the snapshot in memory (no request). */
onLangChange(function () { renderSessionGrid(''); });

