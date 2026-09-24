/**
 * Review mode in the browser: the pending queue and the count that surfaces it.
 *
 * There is no worklist modal. A decision is always made against one command
 * line, in one session's panel, and the header's pill is what tells a human a
 * request arrived at all — including when the window is minimized and only the
 * session tab bar is on screen. The terminal's own panel is the whole UI.
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
  (approvals || []).forEach(function (it) {
    var req = it.request || {};
    if (req.state !== 'pending' || !req.session_id) return;
    perSession[req.session_id] = (perSession[req.session_id] || 0) + 1;
  });
  window._approvalCounts = perSession;
  applyApprovalCounts();
}

/** refreshApprovalCounts re-reads the queue when no panel is open to do it.
 *  Called when a push arrives, so a request is visible without any navigation. */
function refreshApprovalCounts() {
  fetch('/api/approvals')
    .then(function (r) { return r.json(); })
    .then(function (j) { keepApprovalCounts(j.approvals || []); })
    .catch(function () {});
}

/** applyApprovalCounts paints the stored counts onto every surface that shows
 *  one: the window's lock (which also opens the queue the count refers to), the
 *  session tab bar (the only signal a minimized window has) and the session tile. */
function applyApprovalCounts() {
  var counts = window._approvalCounts || {};
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
    var w = tab._win;
    var n = (w && counts[w._parentSid]) || 0;
    var badge = tab.querySelector('.session-tab-review');
    if (!badge) return;
    badge.hidden = !n;
    badge.textContent = String(n);
    tab.classList.toggle('has-review', !!n);
  });
  document.querySelectorAll('.conn-tile.sess-tile').forEach(function (tile) {
    var n = counts[tile.getAttribute('data-sid')] || 0;
    var badge = tile.querySelector('.sess-review');
    if (!badge) return;
    badge.hidden = !n;
    badge.textContent = tCount('review.badge.one', 'review.badge.other', { count: n });
  });
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
  fetch('/api/approvals')
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
  } else if (kindLabel) {
    // The summary is the operation, so it takes the same place the command does.
    body = '<code class="approval-code approval-code-op">' + escapeHtml(req.summary || kindLabel) + '</code>';
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
  fetch('/api/approvals/' + encodeURIComponent(id) + (approve ? '/approve' : '/reject'), {
    method: 'POST'
  }).then(function (r) {
    if (!r.ok) return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
    return r.json();
  }).then(function () {
    if (win) loadReviewQueue(win); else refreshApprovalCounts();
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
    session_id: payload.session_id
  });
  // The badge is a count, so a transition is enough to make it wrong. Re-reading
  // the queue keeps it exact instead of guessing +/- 1 here — and any panel that
  // is open shows the same queue, so it is refreshed from the same pass.
  refreshApprovalCounts();
  document.querySelectorAll('.shell-window').forEach(function (w) {
    var sheet = w.querySelector('.shell-review-sheet');
    if (sheet && !sheet.hidden) loadReviewQueue(w);
  });
}
