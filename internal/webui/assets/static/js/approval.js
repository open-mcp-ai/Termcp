/**
 * Review mode in the browser: the pending queue and the count that surfaces it.
 *
 * Session-bound decisions remain available in that session's terminal panel. The
 * header worklist also shows every pending request, so an operator has one place
 * to see the whole queue; both surfaces render the same rows and call the same
 * decision endpoint.
 *
 * The chips show a named key rather than the byte it produces, because the whole
 * point of approving a key is that a human can see what it does. The names come
 * from the server's own whitelist (session/keys.go); an unknown key is rendered
 * as its raw name rather than hidden, so a decision is never made blind.
 */
var KEY_LABELS = {
  'ctrl+c': 'Ctrl+C', 'enter': 'Enter', 'tab': 'Tab', 'esc': 'Esc',
  'up': 'Up', 'down': 'Down', 'left': 'Left', 'right': 'Right',
  'backspace': 'Bksp', 'delete': 'Del', 'home': 'Home', 'end': 'End',
  'ctrl+d': 'Ctrl+D', 'ctrl+z': 'Ctrl+Z', 'ctrl+l': 'Ctrl+L',
  'ctrl+u': 'Ctrl+U', 'ctrl+w': 'Ctrl+W'
};

/** keyLabel renders a key name for display. */
function keyLabel(key) {
  return KEY_LABELS[key] || key;
}

/** approvalChipsHtml renders the named keys attached to a request.
 *
 *  Enter is deliberately left out. It is the key that ends a command line, so
 *  it is already visible in what the request IS — a queued command — and a chip
 *  saying "Enter" next to a command is a chip that never varies. Only keys that
 *  do something a human could not infer are shown (Ctrl+C interrupting a
 *  process, Tab completing, an arrow moving the cursor). A request whose only
 *  key was Enter therefore renders no chips at all. */
function approvalChipsHtml(keys) {
  return (keys || []).filter(function (k) { return k !== 'enter'; }).map(function (k) {
    return '<span class="approval-chip">' + escapeHtml(keyLabel(k)) + '</span>';
  }).join('');
}

/** keepApprovalCounts stores pending counts and repaints every surface that
 *  shows one. It takes the payload the queue read already fetched, so one
 *  request serves both the panel and the badges. */
function keepApprovalCounts(approvals) {
  var perSession = {};
  var totalCount = 0;
  (approvals || []).forEach(function (it) {
    var req = it.request || {};
    if (req.state !== 'pending') return;
    totalCount += 1;
    if (!req.session_id) return;
    if (req.session_id === 'policy') {
      return;
    }
    perSession[req.session_id] = (perSession[req.session_id] || 0) + 1;
  });
  window._approvalCounts = perSession;
  window._approvalTotal = totalCount;
  applyApprovalCounts();
}

/** refreshApprovalCounts re-reads the queue when no panel is open to do it.
 *  Called when a push arrives, so a request is visible without any navigation. */
function refreshApprovalCounts() {
  fetch(apiPath('/api/approvals'))
    .then(function (r) { return r.json(); })
    .then(function (j) { keepApprovalCounts(j.approvals || []); })
    .catch(function () {});
}

/** applyApprovalCounts paints the stored counts onto every surface that shows
 *  one: the window's lock (which also opens the queue the count refers to), the
 *  session tab bar (the only signal a minimized window has) and the session tile. */
function applyApprovalCounts() {
  var counts = window._approvalCounts || {};
  var policyCount = window._approvalTotal || 0;
  var policyBadge = document.getElementById('approval-count');
  if (policyBadge) {
    policyBadge.hidden = !policyCount;
    policyBadge.textContent = String(policyCount);
  }
  document.querySelectorAll('.shell-window').forEach(function (w) {
    var n = counts[w._parentSid] || 0;
    // The lock carries the mode; the number goes on the tabs. Its title is the one
    // place both can be read together, so it is composed rather than overwritten —
    // a count-only title would erase the mode it sits on.
    // The title is composed in one place (terminal-view.js), so a count change
    // and a mode change cannot write two different sentences to the same tooltip.
    if (typeof updateReviewLockTitle === 'function') updateReviewLockTitle(w);
    // The count lives on the lock, and ONLY there.
    //
    // An earlier version badged the pane tab each request belonged to (a file
    // write badged Files, a forward badged Forwardings). That was wrong in use:
    // those tabs show the file listing and the forward list, not the queue, so a
    // dot there is a promise the tab cannot keep — you click it and see nothing
    // waiting. The lock is the one control that opens the queue, so it is the one
    // place a count means "there is something to decide, right here".
    var num = w.querySelector('.shell-review-count');
    if (num) {
      // Both the visibility AND the number: showing a badge without writing its
      // text left a red "0" sitting on the button, a count that says "nothing
      // waiting" next to a queue that has one — the worst of both.
      num.hidden = !n;
      num.textContent = String(n);
    }
    // Deliberately no panel refresh here: loadReviewQueue ends in
    // keepApprovalCounts, so refreshing the panel from a paint would loop the
    // two through each other's fetches. onApprovalEvent does it for a push.
  });
  // The tab bar is the only signal a minimized window has: the terminal inside
  // it is display:none, so its pill cannot be seen.
  document.querySelectorAll('.session-tab').forEach(function (tab) {
    var sid = tab._entry ? tab._entry.sid : (tab._win && tab._win._parentSid);
    var n = (sid && counts[sid]) || 0;
    var badge = tab.querySelector('.session-tab-review');
    if (!badge) return;
    /* Guarded writes: this runs after every refresh of the tab strip (and the
       refresh itself runs several times per arrangement click), so an
       unconditional assignment churns hidden/textContent on every pass — a
       replace-and-reinsert the strip renders as a flicker even with zero new
       output. */
    var hide = !n;
    if (badge.hidden !== hide) badge.hidden = hide;
    var text = String(n);
    if (badge.textContent !== text) badge.textContent = text;
    if (tab.classList.contains('has-review') !== !!n) tab.classList.toggle('has-review', !!n);
  });
  document.querySelectorAll('.conn-tile.sess-tile').forEach(function (tile) {
    var n = counts[tile.getAttribute('data-sid')] || 0;
    var badge = tile.querySelector('.sess-review');
    if (!badge) return;
    badge.hidden = !n;
    badge.textContent = tCount('review.badge.one', 'review.badge.other', { count: n });
  });
  if (typeof blackwallPaint === 'function') blackwallPaint();
}

/** clearApprovalCount drops one session's count. Turning review off cancels
 *  everything pending server-side, so the badges would otherwise keep pointing at
 *  requests that no longer exist. */
function clearApprovalCount(sid) {
  if (!sid) return;
  var counts = window._approvalCounts || {};
  if (!counts[sid]) return;
  delete counts[sid];
  applyApprovalCounts();
}

/** loadReviewQueue fills one window's panel with that session's queue, and is
 *  also what keeps the counts exact: the panel is the one place the full list is
 *  read, so a decision made in another tab lands in every badge from here. */
function loadReviewQueue(win) {
  var sheet = win.querySelector('.shell-review-sheet');
  if (!sheet) return;
  var list = sheet.querySelector('.shell-review-list');
  if (!list) return;
  fetch(apiPath('/api/approvals'))
    .then(function (r) { return r.json(); })
    .then(function (j) {
      var all = j.approvals || [];
      // Only what still needs a decision: the panel is a queue, not a log. A
      // decided request stays out of it, which is what makes the row
      // disappearing read as "handled".
      var mine = all.filter(function (it) {
        return it.request && it.request.session_id === win._parentSid && it.request.state === 'pending';
      });
      list.innerHTML = mine.length
        ? mine.map(renderApprovalItem).join('')
        : '<div class="shell-review-empty">' + escapeHtml(t('review.empty')) + '</div>';
      list.querySelectorAll('[data-approve],[data-reject]').forEach(function (b) {
        b.addEventListener('click', function (e) {
          e.preventDefault();
          e.stopPropagation();
          decideApproval(win, b.getAttribute('data-approve') || b.getAttribute('data-reject'),
            !!b.getAttribute('data-approve'));
        });
      });
      keepApprovalCounts(all);
    })
    .catch(function () {
      list.innerHTML = '<div class="shell-review-empty">' + escapeHtml(t('review.loadFailed')) + '</div>';
    });
}

/** installPolicyApprovalMenu adds the approval queue entry to the native header.
 *  It is created here rather than in index.html so the page shell stays below
 *  its payload budget. Settings are a separate top-level menu. */
function installPolicyApprovalMenu() {
  var existing = document.getElementById('approval-menu');
  if (existing) return existing;
  var nav = document.querySelector('.app-header-nav');
  if (!nav) return null;
  var menu = document.createElement('details');
  menu.className = 'clients-menu approval-menu';
  menu.id = 'approval-menu';
  menu.innerHTML =
    '<summary class="clients-btn approval-btn lang-btn app-control app-menu-trigger" title="Policy approvals" data-i18n-title="approvals.title" aria-label="Policy approvals" data-i18n-aria="approvals.title">' +
      '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 1.75l5.25 2.5v4.2c0 3.05-2.25 5.15-5.25 5.8-3-.65-5.25-2.75-5.25-5.8v-4.2z"/><path d="M5.5 7.9l1.65 1.65 3.4-3.45"/></svg>' +
      '<span class="clients-label" data-i18n="approvals.title">Approvals</span>' +
      '<span class="approval-header-count" id="approval-count" hidden>0</span>' +
    '</summary>' +
    '<div class="clients-panel policy-approval-panel lang-menu app-menu-panel">' +
      '<div class="policy-approval-head"><div><strong data-i18n="approvals.title">Approvals</strong><p data-i18n="approvals.queueHint">Pending requests from terminal sessions and instance policy wait here.</p></div>' +
        '<button type="button" id="policy-approval-refresh" class="btn" title="Refresh" data-i18n-title="common.refresh" data-i18n-aria="common.refresh"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M13 8a5 5 0 11-1.5-3.5"/><path d="M13 2.5V5h-2.5"/></svg></button>' +
      '</div>' +
      '<p id="policy-approval-error" class="err" hidden></p><div id="policy-approval-list"></div>' +
      '<div id="policy-approval-empty" class="policy-approval-empty" data-i18n="approvals.empty">Nothing is waiting for approval.</div>' +
    '</div>';
  var anchor = document.getElementById('clients-menu');
  nav.insertBefore(menu, anchor && anchor.parentNode === nav ? anchor : null);
  if (typeof applyI18n === 'function') applyI18n(menu);
  return menu;
}

/** loadPolicyApprovals fills the one native worklist with every pending request.
 *  Session windows keep their local review panel, but the header queue is the
 *  complete view an operator can open from anywhere. */
function loadPolicyApprovals() {
  var menu = installPolicyApprovalMenu();
  var list = document.getElementById('policy-approval-list');
  var empty = document.getElementById('policy-approval-empty');
  var error = document.getElementById('policy-approval-error');
  var refresh = document.getElementById('policy-approval-refresh');
  if (!menu || !list || !empty || !error) return Promise.resolve();
  error.hidden = true;
  error.textContent = '';
  if (refresh) refresh.disabled = true;
  return fetch(apiPath('/api/approvals'))
    .then(function (r) {
      if (!r.ok) return r.text().then(function (txt) { throw new Error(txt || ('HTTP ' + r.status)); });
      return r.json();
    })
    .then(function (j) {
      var all = j.approvals || [];
      var mine = all.filter(function (it) {
        return it.request && it.request.state === 'pending';
      });
      list.innerHTML = mine.map(renderApprovalItem).join('');
      empty.hidden = mine.length > 0;
      list.hidden = mine.length === 0;
      list.querySelectorAll('[data-approve],[data-reject]').forEach(function (b) {
        b.addEventListener('click', function (e) {
          e.preventDefault();
          e.stopPropagation();
          decideApproval(null, b.getAttribute('data-approve') || b.getAttribute('data-reject'),
            !!b.getAttribute('data-approve'));
        });
      });
      keepApprovalCounts(all);
    })
    .catch(function (err) {
      error.hidden = false;
      error.textContent = t('review.loadFailed') + ' ' + String(err.message || err);
    })
    .finally(function () {
      if (refresh) refresh.disabled = false;
    });
}

/** openPolicyApprovalMenu is the click target for a notification whose request
 *  is not attached to a session. */
function openPolicyApprovalMenu() {
  var menu = installPolicyApprovalMenu();
  if (!menu) return;
  menu.open = true;
  menu.scrollIntoView({ block: 'nearest' });
  loadPolicyApprovals();
}

/** KIND_LABELS names what a queued request will actually do. Review gates
 *  command lines, file transfers and port forwards through one queue, so the row
 *  has to say which it is: "write 1.2 KB to /etc/hosts" and "rm -rf /" are
 *  different kinds of decision and a reviewer needs to know which they face. */
/** kindLabelOf names what a queued request will do, in the current language. A
 *  shell command has no label on purpose: its text IS the thing being decided. */
function kindLabelOf(kind) {
  if (kind === 'file_write') return t('review.kind.fileWrite');
  if (kind === 'file_delete') return t('review.kind.fileDelete');
  if (kind === 'file_rename') return t('review.kind.fileRename');
  if (kind === 'file_mkdir') return t('review.kind.fileMkdir');
  if (kind === 'file_perm') return t('review.kind.filePerm');
  if (kind === 'file_link') return t('review.kind.fileLink');
  if (kind === 'file_truncate') return t('review.kind.fileTruncate');
  if (kind === 'forward_open') return t('review.kind.forwardOpen');
  if (kind === 'forward_close') return t('review.kind.forwardClose');
  return '';
}

/** renderApprovalItem is one row: what will run, then exactly two buttons beside
 *  it. The content is not editable — the AI wrote it, and a human's choice is to
 *  accept it or refuse it.
 *
 *  Two shapes share this row. A command line shows its text in a code box; a
 *  file or forward operation has no command text, so it shows the summary the
 *  server recorded. That summary is the whole reason the operation is
 *  reviewable: a human deciding on "write to /etc/hosts" without knowing what
 *  or how much is not reviewing anything.
 *
 *  The decision sits to the RIGHT, stacked Accept-over-Reject: two buttons
 *  stacked are about the height of the two-line box beside them, so the pair
 *  fills the row instead of leaving a gap next to a one-line entry. */
function renderApprovalItem(it) {
  var req = it.request || {};
  var pending = req.state === 'pending';
  var kind = req.kind || 'shell_input';
  var kindLabel = kindLabelOf(kind);
  var body;
  if (req.text) {
    body = '<code class="approval-code">' + escapeHtml(req.text) + '</code>';
  } else if (kind !== 'shell_input') {
    // The summary is the operation, so it takes the same place the command does.
    body = '<code class="approval-code approval-code-op">' + escapeHtml(req.summary || kindLabel || kind) + '</code>';
  } else {
    body = '';
  }
  var chips = approvalChipsHtml(req.keys);
  // The content side is one column (body + keys); the actions are a sibling of
  // it, so flex puts them on the same line and the column can shrink.
  var main = '<div class="approval-main">' + body +
    (chips ? '<div class="approval-chips">' + chips + '</div>' : '') + '</div>';
  var actions = pending
    ? '<div class="approval-actions">' +
        '<button type="button" class="btn btn-primary" data-approve="' + escapeHtml(req.id) + '">' + escapeHtml(t('review.accept')) + '</button>' +
        '<button type="button" class="btn btn-danger" data-reject="' + escapeHtml(req.id) + '">' + escapeHtml(t('review.reject')) + '</button>' +
      '</div>'
    : '<div class="approval-state">' + escapeHtml(req.state) + '</div>';
  return '<div class="approval-item' + (pending ? '' : ' approval-item-done') + '">' +
    '<div class="approval-item-head">' +
      '<span class="approval-src">' + escapeHtml(req.source || '?') + '</span>' +
      (kindLabel ? '<span class="approval-kind">' + escapeHtml(kindLabel) + '</span>' : '') +
      '<span class="approval-sid">' + escapeHtml(req.shell_id || '') + '</span>' +
    '</div>' +
    '<div class="approval-row">' + main + actions + '</div>' +
    '</div>';
}

/** decideApproval records the click. There is no name to collect: the decision
 *  is the reviewer's act as far as the server can tell, and a self-declared name
 *  would look like attribution in the audit trail without being any. */
function decideApproval(win, id, approve) {
  var sheet = win && win.querySelector('.shell-review-sheet');
  if (sheet) sheet.querySelectorAll('.approval-actions button').forEach(function (b) { b.disabled = true; });
  fetch(apiPath('/api/approvals/') + encodeURIComponent(id) + (approve ? '/approve' : '/reject'), {
    method: 'POST'
  }).then(function (r) {
    if (!r.ok) return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
    return r.json();
  }).then(function () {
    if (win) loadReviewQueue(win);
    else {
      refreshApprovalCounts();
      loadPolicyApprovals();
    }
  }).catch(function (err) {
    showCopyToast(t('review.toast.failed', { msg: String(err.message || err) }));
    if (win) loadReviewQueue(win);
  });
}

/** An approval event arrives on the same WebSocket as ui_notify; toast it and
 *  refresh the counts, so a request is visible with nothing open. */
function onApprovalEvent(payload) {
  showUiNotify({
    title: payload.title || t('review.title'),
    message: payload.message || '',
    level: payload.level || 'info',
    duration_seconds: payload.duration_seconds,
    // The click should land on the decision, not merely on the session.
    open_review: true,
    open_policy_approvals: payload.session_id === 'policy',
    session_id: payload.session_id
  });
  // The badge is a count, so a transition is enough to make it wrong. Re-reading
  // the queue keeps it exact instead of guessing +/- 1 here — and any panel that
  // is open shows the same queue, so it is refreshed from the same pass.
  refreshApprovalCounts();
  var policyMenu = document.getElementById('approval-menu');
  if (policyMenu && policyMenu.open) loadPolicyApprovals();
  document.querySelectorAll('.shell-window').forEach(function (w) {
    var sheet = w.querySelector('.shell-review-sheet');
    if (sheet && !sheet.hidden) loadReviewQueue(w);
  });
}

(function wirePolicyApprovals() {
  var menu = installPolicyApprovalMenu();
  var refresh = document.getElementById('policy-approval-refresh');
  if (refresh) refresh.addEventListener('click', loadPolicyApprovals);
  if (menu) {
    menu.addEventListener('toggle', function () {
      if (!menu.open) return;
      if (typeof closeHeaderMenus === 'function') closeHeaderMenus('approval-menu');
      loadPolicyApprovals();
    });
    document.addEventListener('click', function (e) {
      if (menu.open && !menu.contains(e.target)) menu.open = false;
    });
  }
})();
