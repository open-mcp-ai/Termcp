function showModal(id) { document.getElementById(id).classList.remove('hidden'); }
function hideModal(id) { document.getElementById(id).classList.add('hidden'); }

/**
 * Only guard page leave when terminal windows are actually open.
 * Sessions keep running on the server even when windows are closed.
 */
function pageHasLiveState() {
  var openWins = (typeof allShellWins === 'function' ? allShellWins() : []).length;
  if (openWins > 0) return tCount('msg.leave.windows.one', 'msg.leave.windows.other', { count: openWins });
  return '';
}

window.addEventListener('beforeunload', function (e) {
  var live = pageHasLiveState();
  if (!live) return;
  e.preventDefault();
  // Modern browsers always show a generic dialog regardless of text; legacy
  // browsers render returnValue verbatim.
  e.returnValue = t('msg.leave.detail', { windows: live });
});

// Style-consistent replacement for the native confirm() dialog.
// opts: { title, message, okText, danger }
function confirmDialog(opts) {
  opts = opts || {};
  var title = document.getElementById('modal-confirm-title');
  var msg = document.getElementById('modal-confirm-msg');
  var okBtn = document.getElementById('modal-confirm-ok');
  var cancelBtn = document.getElementById('modal-confirm-cancel');
  var closeBtn = document.getElementById('modal-confirm-close');
  title.textContent = opts.title || t('common.confirm');
  msg.textContent = opts.message || '';
  okBtn.textContent = opts.okText || t('common.ok');
  okBtn.className = opts.danger ? 'btn btn-danger' : 'btn btn-primary';
  // Replace handlers without accumulating listeners: clone node each call.
  var freshOk = okBtn.cloneNode(true);
  okBtn.parentNode.replaceChild(freshOk, okBtn);
  var freshCancel = cancelBtn.cloneNode(true);
  cancelBtn.parentNode.replaceChild(freshCancel, cancelBtn);
  var freshClose = closeBtn.cloneNode(true);
  closeBtn.parentNode.replaceChild(freshClose, closeBtn);
  return new Promise(function (resolve) {
    function done(v) {
      hideModal('modal-confirm');
      freshOk.removeEventListener('click', onOk);
      freshCancel.removeEventListener('click', onCancel);
      freshClose.removeEventListener('click', onCancel);
      resolve(v);
    }
    function onOk() { done(true); }
    function onCancel() { done(false); }
    freshOk.addEventListener('click', onOk);
    freshCancel.addEventListener('click', onCancel);
    freshClose.addEventListener('click', onCancel);
    showModal('modal-confirm');
    try { freshOk.focus(); } catch (e) {}
  });
}

/* Style-consistent replacement for the native prompt() dialog.
 *
 * opts: { title, placeholder, required, onSubmit }. One caller-supplied action
 * and one input — no generic form builder, because the only fields a caller has
 * ever needed are these. */
function openCommandPrompt(opts) {
  opts = opts || {};
  var input = document.getElementById('modal-cmd-input');
  var errEl = document.getElementById('modal-cmd-err');
  document.getElementById('modal-cmd-title').textContent = opts.title || t('channel.cmd.titlePty');
  input.value = '';
  input.placeholder = opts.placeholder || '';
  errEl.style.display = 'none';
  errEl.textContent = '';
  var okBtn = document.getElementById('modal-cmd-run');
  var cancelBtn = document.getElementById('modal-cmd-cancel');
  var closeBtn = document.getElementById('modal-cmd-close');
  // Clone to drop a previous call's listeners (the modal is a singleton).
  var freshOk = okBtn.cloneNode(true);
  okBtn.parentNode.replaceChild(freshOk, okBtn);
  var freshCancel = cancelBtn.cloneNode(true);
  cancelBtn.parentNode.replaceChild(freshCancel, cancelBtn);
  var freshClose = closeBtn.cloneNode(true);
  closeBtn.parentNode.replaceChild(freshClose, closeBtn);
  function close() {
    hideModal('modal-cmd');
    input.removeEventListener('keydown', onKey);
  }
  function submit() {
    var v = input.value.trim();
    if (!v && opts.required) {
      errEl.textContent = t('channel.cmd.required');
      errEl.style.display = 'block';
      return;
    }
    close();
    opts.onSubmit(v);
  }
  function onOk() { submit(); }
  function onKey(e) { if (e.key === 'Enter') { e.preventDefault(); submit(); } }
  freshOk.addEventListener('click', onOk);
  freshCancel.addEventListener('click', close);
  freshClose.addEventListener('click', close);
  input.addEventListener('keydown', onKey);
  showModal('modal-cmd');
  try { input.focus(); } catch (e) {}
}

/* The entry banner carries information the user may need to read after the
   switch (why the list is empty, why a connect failed), so it remembers its
   catalog key + params instead of being re-rendered away. The message is only
   replayed while the banner is actually on screen, so any later successful load
   that clears the banner also retires the remembered one. */
var _connBanner = null; // { key, params } | null

function rememberConnBanner(key, params) {
  _connBanner = key ? { key: key, params: params || null } : null;
}

/** The remembered banner in the current language, or '' when it is not shown. */
function connBannerText() {
  var el = document.getElementById('conn-load-banner');
  if (!el || el.style.display === 'none') return '';
  return _connBanner ? t(_connBanner.key, _connBanner.params) : '';
}

/** One access node's runtime state.
 *
 *  The server has no per-host health field: a profile is a stored recipe, and
 *  the only evidence of reachability the page ever receives is the sessions it
 *  is carrying. So the state is DERIVED, in this order:
 *
 *    connecting — a session window for this profile is still dialling
 *    online     — the profile has at least one running session
 *    offline    — the profile HAS been connected (it has a session record, live
 *                 or retained as DEAD) and nothing is running on it now
 *    idle       — the profile has no session record at all: it has never been
 *                 dialled, so Termcp has no evidence either way
 *
 *  "idle" and "offline" are deliberately separate words. Having no session is
 *  not evidence that the machine is down — it is the absence of evidence, and a
 *  fresh profile, a host reached from another machine, or one simply not tried
 *  yet all land there. Only a profile Termcp has actually held a connection to
 *  can be called disconnected. What this column answers is "do I have a live way
 *  in", not "is the box up"; probing every stored host on render would be a
 *  network round-trip per card per session frame.
 *
 *  The link from a session back to its profile: the profile is stamped on the
 *  session by the server (`ssh_config`), because everything else on the record
 *  that could look like one is a display value. A session's NAME is whatever the
 *  user (or the launch dialog) typed and can be renamed at any moment; a window's
 *  `_connName` exists only on the page that opened the window, so it cannot see a
 *  session started by an agent or by another tab. Attributing by name made the
 *  card fall back to "offline" the moment a session was renamed or started under
 *  another name — a live session under a red lamp. The old spellings are kept
 *  only as a fallback for records written before the field existed. */
function sessionBelongsToNode(s, name, wins) {
  if (s.ssh_config) return s.ssh_config === name;
  if (s.name === name) return true;
  for (var j = 0; j < wins.length; j++) {
    if (wins[j] && wins[j]._connName === name && wins[j]._sid === s.id) return true;
  }
  return false;
}

function runningSessionsForNode(name) {
  var sess = window._lastSessionsSnapshot || [];
  var wins = (typeof allShellWins === 'function') ? allShellWins() : [];
  var out = [];
  for (var i = 0; i < sess.length; i++) {
    var s = sess[i];
    if (!s || s.status !== 'running') continue;
    if (sessionBelongsToNode(s, name, wins)) out.push(s);
  }
  return out;
}

function nodeState(name) {
  /* Both lists feed this readout — the connection list says the node exists, the
     session snapshot says whether anything is live or has ever been held on it.
     The latter gates the word "offline": the connection list resolves first, and
     painting offline on that alone labelled every fresh host dead before the
     socket's first frame. */
  if (!window._lastConnections || !window._lastSessionsSnapshot) return 'loading';
  var wins = (typeof allShellWins === 'function') ? allShellWins() : [];
  for (var i = 0; i < wins.length; i++) {
    if (wins[i] && wins[i]._placeholder && wins[i]._pendingConnName === name) return 'connecting';
  }
  // One pass over the snapshot: whether any record is ours, and whether one of
  // them is live. A running session outranks a retained one whatever the order.
  var held = false;
  var sessions = window._lastSessionsSnapshot || [];
  for (var j = 0; j < sessions.length; j++) {
    var s = sessions[j];
    if (!s || !sessionBelongsToNode(s, name, wins)) continue;
    if (s.status === 'running') return 'online';
    held = true;
  }
  return held ? 'offline' : 'idle';
}

/** Repaint the node cards from what the page already holds.
 *
 *  "connecting" is the one state that exists only in a window: a placeholder
 *  window for this profile means a dial is open, and it stops being one when the
 *  session arrives or the dial fails. Nothing else changes on those two events
 *  (a successful dial also brings a session frame; a FAILED one brings nothing),
 *  so the transition has to be painted from the window itself or the card keeps
 *  claiming whatever it said before the click.
 *
 *  Coalesced through a timer: a batch of window closes — select-all then delete —
 *  fires this once per closed window, and rebuilding the whole card list once per
 *  id would cost the batch its responsiveness for no visible difference. */
var _nodeRepaintTimer = 0;
function repaintNodeStates() {
  if (_nodeRepaintTimer) return;
  _nodeRepaintTimer = setTimeout(function () {
    _nodeRepaintTimer = 0;
    if (typeof window.renderNodeStates === 'function') window.renderNodeStates();
  }, 0);
}

/** Live session count for a profile, plus whether any of them is in a window. */
function nodeSessionInfo(name) {
  var list = runningSessionsForNode(name);
  var focused = false;
  for (var i = 0; i < list.length; i++) {
    if (typeof getShellWindowBySid === 'function' && getShellWindowBySid(list[i].id)) { focused = true; break; }
  }
  return { count: list.length, focused: focused };
}

/** The protocol label a node shows: what Termcp uses to reach it, which is the
 *  question this column answers. The loopback profile is SSH too — it just dials
 *  this machine over Termcp's own server — so it says so rather than claiming a
 *  protocol of its own. */
function nodeProtocol(c) { return c.kind === 'internal' ? t('nethub.proto.loopback') : 'SSH'; }

/** The address line: user@host:port, or the loopback note for the internal
 *  profile. Never a secret — the profile's password and key stay in the editor. */
function nodeAddress(c) {
  if (c.kind === 'internal') return t('nethub.internal');
  var host = String(c.host || '');
  if (!host) return '';
  var user = String(c.user || '');
  var port = c.port && c.port !== 22 ? ':' + c.port : '';
  return (user ? user + '@' : '') + host + port;
}

function nodeStatusLabel(state) {
  if (state === 'online') return t('nethub.state.online');
  if (state === 'connecting') return t('nethub.state.connecting');
  if (state === 'offline') return t('nethub.state.offline');
  if (state === 'loading') return t('nethub.state.loading');
  return t('nethub.state.idle');
}

/** What the state means when the word alone could still be read as a claim about
 *  the machine. "Not connected" and "disconnected" are different facts — one is
 *  the absence of evidence, the other a connection Termcp held and lost — and the
 *  hint is where that distinction is spelled out, since the node row itself is
 *  small and uppercased. */
function nodeStatusHint(state) {
  if (state === 'idle') return t('nethub.hint.idle');
  if (state === 'offline') return t('nethub.hint.offline');
  return '';
}

/** The rail: the expand key, then one lamp per access node.
 *
 *  A collapsed sidebar still has to answer "which machines can I reach", so the
 *  lamps stay and each one carries the same state the full card shows. No cluster
 *  glyph is rendered here: the trigger directly above already carries it and stays
 *  visible while collapsed, so a second one read as two NetHubs stacked. The count
 *  is capped because the rail is a status strip, not a second list: past the cap
 *  the overflow is one more row with the remainder in its tooltip. */
function renderNodeRail(connections) {
  var rail = document.getElementById('nethub-rail');
  if (!rail) return;
  rail.innerHTML = '';
  /* No glyph of its own at the top: the trigger above the rail already carries
     the cluster icon and stays visible when the body collapses, so a second one
     here read as two NetHubs stacked. The rail starts with the expand key. */
  var expand = document.createElement('button');
  expand.type = 'button';
  expand.className = 'nethub-rail-expand';
  expand.title = t('nethub.expand');
  expand.setAttribute('aria-label', t('nethub.expand'));
  expand.innerHTML = '<svg viewBox="0 0 12 8" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1 2l5 4 5-4"/></svg>';
  expand.addEventListener('click', function (e) { e.stopPropagation(); setNetHubCollapsed(false, true); });
  rail.appendChild(expand);

  var list = (connections || []).slice(0, 12);
  list.forEach(function (c) {
    var state = nodeState(c.name);
    var info = nodeSessionInfo(c.name);
    var hint = nodeStatusHint(state);
    var item = document.createElement('button');
    item.type = 'button';
    item.className = 'nethub-rail-item';
    item.setAttribute('data-conn-name', c.name);
    item.title = c.name + ' · ' + nodeProtocol(c) + ' · ' + nodeStatusLabel(state)
      + (hint ? ' · ' + hint : '')
      + (info.count ? ' · ' + tCount('nethub.sessions.one', 'nethub.sessions.other', { count: info.count }) : '');
    item.setAttribute('aria-label', item.title);
    item.innerHTML =
      '<span class="nethub-rail-status is-' + state + '" aria-hidden="true"></span>' +
      '<span aria-hidden="true">' + escapeHtml(c.name.slice(0, 2).toUpperCase()) + '</span>' +
      (info.focused ? '<span class="nethub-rail-active" aria-hidden="true"></span>' : '');
    /* A lamp names its node and takes you to it: expanding first, so the click
       lands on the card it is about rather than on a rail that cannot show it. */
    item.addEventListener('click', function (e) {
      e.stopPropagation();
      setNetHubCollapsed(false, true);
      focusNodeCard(c.name);
    });
    rail.appendChild(item);
  });
  if ((connections || []).length > list.length) {
    var more = document.createElement('span');
    more.className = 'nethub-rail-glyph';
    more.textContent = '+' + ((connections || []).length - list.length);
    more.title = t('nethub.rail.more', { count: (connections || []).length - list.length });
    rail.appendChild(more);
  }
}

/** Highlight and reveal one node card, after an expand or a rail click. */
function focusNodeCard(name) {
  var grid = document.getElementById('conn-grid');
  if (!grid || !name) return;
  var card = null;
  var cards = grid.querySelectorAll('[data-conn-name]');
  for (var i = 0; i < cards.length; i++) {
    if (cards[i].getAttribute('data-conn-name') === name) { card = cards[i]; break; }
  }
  if (!card) return;
  card.classList.add('node-focus');
  if (typeof card.scrollIntoView === 'function') {
    try { card.scrollIntoView({ block: 'nearest' }); } catch (e) { card.scrollIntoView(false); }
  }
  setTimeout(function () { card.classList.remove('node-focus'); }, 1600);
}

/* ---- Node selection mode --------------------------------------------------
 * One switch re-aims the cards: off, a card is a connect button; on, it is a
 * checkbox and the toolbar's batch actions act on the selection. The set holds
 * profile names rather than DOM state, because the grid rebuilds on every
 * session frame — the checkbox is re-derived from here at each render, the way
 * the session plates derive theirs (sessions.js). */

var _nodeSelectMode = false;
var _nodeSelIds = new Set();

function nodeSelectModeOn() { return _nodeSelectMode; }

/** Profile names that may carry a selection: every node in the list, the
 *  built-in profile included. Excluding it would make a whole card dead under
 *  the pointer with no word why; the actions themselves carry the exceptions —
 *  open works for it, export skips it server-side, delete reports the refusal
 *  per name. */
function selectableNodeNames() {
  var list = window._lastConnections || [];
  var out = [];
  for (var i = 0; i < list.length; i++) {
    if (list[i] && list[i].name) out.push(list[i].name);
  }
  return out;
}

function setNodeSelectMode(on) {
  on = !!on;
  if (_nodeSelectMode === on) return;
  _nodeSelectMode = on;
  if (!on) _nodeSelIds.clear();
  var body = document.getElementById('sec-entries-body');
  if (body) body.classList.toggle('nethub-selecting', on);
  var toggle = document.getElementById('nethub-select-toggle');
  if (toggle) toggle.setAttribute('aria-pressed', String(on));
  // The dropdown belongs to the pressed state; either flip folds it.
  var menu = document.getElementById('nethub-open-menu');
  if (menu) menu.hidden = true;
  var caret = document.getElementById('nethub-open-caret');
  if (caret) caret.setAttribute('aria-expanded', 'false');
  /* The cards do not rebuild on a toggle: selection mode is one class on the
     column (the CSS carries the markers, the quiet buttons, the ticked looks),
     so a flip only retitles them and syncs the toolbar. */
  refreshNodeCardTitles();
  syncNodeCardSelection();
  updateNodeBatchBar();
}

/** Re-apply the ticked look from the selection set. The Set is the source of
 *  truth, so this both paints the ticked cards and — the reason it exists —
 *  strips the class from every card the set no longer holds. Leaving the mode
 *  clears the set; without this the cards keep `.batch-selected`, which is
 *  invisible only while `.nethub-selecting` is off. Toggle back on and those
 *  cards light up again as if ticked, with the count reading 0. */
function syncNodeCardSelection() {
  var grid = document.getElementById('conn-grid');
  if (!grid) return;
  grid.querySelectorAll('.conn-tile[data-conn-name]').forEach(function (tile) {
    var name = tile.getAttribute('data-conn-name');
    tile.classList.toggle('batch-selected', _nodeSelectMode && _nodeSelIds.has(name));
  });
}

/** Retitle the visible cards without rebuilding them: the tooltip is the one
 *  per-card bit that follows the mode ("connect" vs "tick this one"). */
function refreshNodeCardTitles() {
  var grid = document.getElementById('conn-grid');
  if (!grid) return;
  var byName = {};
  (window._lastConnections || []).forEach(function (c) { if (c && c.name) byName[c.name] = c; });
  grid.querySelectorAll('.conn-tile[data-conn-name]').forEach(function (tile) {
    var inner = tile.querySelector('.entry-card-inner');
    var c = byName[tile.getAttribute('data-conn-name')];
    if (!inner || !c) return;
    if (_nodeSelectMode) {
      inner.title = t('nethub.batch.selectOne', { name: c.name });
      return;
    }
    var tipConnect = t('conn.tip.connect');
    if (c.kind === 'internal') inner.title = tipConnect + ' · ' + t('conn.tip.internal');
    else {
      var addr = nodeAddress(c);
      inner.title = addr ? (tipConnect + ' — ' + addr) : tipConnect;
    }
  });
}

/** Sync the toolbar to the current selection: the switch's face becomes the
 *  ticked count (the list icon returns when the selection is spent), the play
 *  split answers disabled while nothing is ticked (its dropdown items with it),
 *  and the select-all key reports whether the entire list is ticked. */
function updateNodeBatchBar() {
  var names = selectableNodeNames();
  var count = 0;
  for (var i = 0; i < names.length; i++) { if (_nodeSelIds.has(names[i])) count++; }
  var showCount = _nodeSelectMode && count > 0;
  var toggle = document.getElementById('nethub-select-toggle');
  if (toggle) {
    var icon = toggle.querySelector('svg');
    if (icon) icon.style.display = showCount ? 'none' : 'block';
    // The label belongs to the toggle, not to the count: they are separate
    // elements, and a surface that carries one without the other would
    // otherwise dereference a null here.
    toggle.setAttribute('aria-label', showCount ? t('batch.count.selected', { count: count }) : t('nethub.batch.select'));
  }
  var countEl = document.getElementById('nethub-toggle-count');
  if (countEl) {
    countEl.hidden = !showCount;
    countEl.textContent = String(count);
  }
  // Keep the control's legacy DOM id for external styles.
  var selAll = document.getElementById('nethub-sel-invert');
  var allSelected = names.length > 0 && count === names.length;
  if (selAll) {
    selAll.disabled = names.length === 0;
    selAll.classList.toggle('all-checked', allSelected);
    var label = allSelected ? t('section.batch.clearSelection') : t('section.batch.selectAll');
    selAll.title = label;
    selAll.setAttribute('aria-label', label);
  }
  var body = document.getElementById('sec-entries-body');
  if (body) body.classList.toggle('has-selection', count > 0);
  var split = document.getElementById('nethub-open-split');
  if (split) split.classList.toggle('is-disabled', count === 0);
  ['nethub-menu-export', 'nethub-menu-delete'].forEach(function (id) {
    var item = document.getElementById(id);
    if (item) item.disabled = count === 0;
  });
}

function toggleNodeSelection(name, tile) {
  var on = !_nodeSelIds.has(name);
  if (on) _nodeSelIds.add(name);
  else _nodeSelIds.delete(name);
  if (tile) tile.classList.toggle('batch-selected', on);
  updateNodeBatchBar();
}

function renderConnGrid(connections, bannerMsg) {
  var grid = document.getElementById('conn-grid');
  if (!grid) return;
  setLoadBanner(document.getElementById('conn-load-banner'), bannerMsg);
  grid.innerHTML = '';
  renderNodeRail(connections);
  (connections || []).sort(function(a, b) {
    if (a.kind === 'internal') return -1;
    if (b.kind === 'internal') return 1;
    return (a.name || '').localeCompare(b.name || '');
  }).forEach(function (c) {
    var tile = document.createElement('div');
    var state = nodeState(c.name);
    var info = nodeSessionInfo(c.name);
    var isInternal = c.kind === 'internal';
    var selected = _nodeSelectMode && _nodeSelIds.has(c.name);
    tile.className = 'conn-tile entry-card node-card' + (info.focused ? ' node-focus' : '') + (selected ? ' batch-selected' : '');
    tile.setAttribute('data-conn-name', c.name);
    tile.setAttribute('data-node-state', state);
    // The internal profile is editable too: its settings (review default, shell)
    // are stored as an override. There is nothing to rename or delete, but there
    // is a setting to change, so the button belongs here as well.
    var editBtn =
      '<button type="button" class="conn-edit-btn" title="Edit profile" data-i18n-title="conn.aria.editProfile" aria-label="Edit profile" data-i18n-aria="conn.aria.editProfile">' + SVG_CONN_EDIT + '</button>';
    var quickBtn = '<button type="button" class="conn-quick-btn" title="Launch options (session name, command)" data-i18n-title="conn.title.launchOptions" aria-label="Launch options" data-i18n-aria="conn.aria.launchOptions">' + SVG_CONN_QUICK + '</button>';
    // A profile that reviews by default says so on the card: which connections
    // are gated is a property of the host, and having to open the editor to
    // find out is how someone launches a production box ungated.
    var apprMark = c.default_approval
      ? '<span class="conn-approval" title="Sessions from this profile start with review mode on" data-i18n-title="conn.approval.mark" aria-label="Reviews by default" data-i18n-aria="conn.approval.markAria">' + SVG_LOCK_CLOSED + '</span>'
      : '';
    var temporaryMark = c.temporary
      ? '<span class="conn-temporary-mark" data-i18n-title="conn.temporary.hint" title="Temporary host" data-i18n="conn.temporary.mark">TEMP</span>'
      : '';
    var addr = nodeAddress(c);
    /* The node's readout, in the order the column is meant to be read: what it
       is (name), how Termcp reaches it (protocol + state), at which address, and
       whether anything is currently running on it. */
    var statusText = nodeStatusLabel(state);
    tile.innerHTML =
      '<div class="entry-card-inner">' +
      '<span class="node-lamp is-' + state + '" aria-hidden="true"></span>' +
      '<div class="entry-card-main">' +
      '<div class="conn-nm-row">' +
      '<span class="conn-nm-cluster">' +
      '<span class="conn-nm" title="Connect" data-i18n-title="conn.title.connect">' + escapeHtml(c.name) + '</span>' +
      '<button type="button" class="sess-copy-btn" title="Copy URL" data-i18n-title="common.copyUrl" aria-label="Copy URL" data-i18n-aria="common.copyUrl">' + SVG_COPY_12 + '</button>' +
      '</span>' +
      quickBtn +
      temporaryMark +
      apprMark +
      editBtn +
      '</div>' +
      '<div class="node-meta">' +
      '<span class="node-proto">' + escapeHtml(nodeProtocol(c)) + '</span>' +
      '<span class="node-sep" aria-hidden="true">·</span>' +
      '<span class="node-status is-' + state + '">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      (addr ? '<div class="node-addr">' + escapeHtml(addr) + '</div>' : '') +
      '<div class="node-sess">' + escapeHtml(info.count
          ? tCount('nethub.sessions.one', 'nethub.sessions.other', { count: info.count })
          : t('nethub.sessions.none')) + '</div>' +
      '</div>' +
      '</div>';
    var inner = tile.querySelector('.entry-card-inner');
    var tipConnect = t('conn.tip.connect');
    if (isInternal) {
      inner.title = tipConnect + ' · ' + t('conn.tip.internal');
    } else {
      inner.title = addr ? (tipConnect + ' — ' + addr) : tipConnect;
    }
    /* The two states that describe the absence of a connection say so on the
       card itself, under the status word: the row is uppercased and small, and
       "offline" read on its own is a claim about a machine this page never
       dialled. "idle" in particular is not a fault state — see nodeState. */
    var statusEl = tile.querySelector('.node-status');
    var stateHint = nodeStatusHint(state);
    if (statusEl && stateHint) statusEl.title = stateHint;
    // Clicking the card body connects directly (most frequent action) — unless
    // selection mode re-aims the card, where a click IS the tick and the
    // connect path stays out of the way entirely.
    inner.addEventListener('click', function (e) {
      if (_nodeSelectMode) {
        toggleNodeSelection(c.name, tile);
        return;
      }
      if (e.target.closest('.conn-edit-btn') || e.target.closest('.sess-copy-btn') || e.target.closest('.conn-quick-btn')) return;
      var b = document.getElementById('conn-load-banner');
      if (b) setLoadBanner(b, '');
      rememberConnBanner(null);
      tile.classList.add('entry-connecting');
      inner.classList.add('entry-connecting');
      /* null position, not the click: NetHub sits at the page's left edge, so a
         pointer-placed window would be clamped against that wall and open under
         the sidebar on a phone (where the panel is still an overlay). */
      startSessionAndOpenShell(c.name, null)
        .catch(function (err) {
          if (err && err.name === 'AbortError') return;
          console.error(err);
          var msg = (err && err.message) ? err.message : String(err);
          rememberConnBanner('banner.conn.failed', { msg: msg });
          if (b) setLoadBanner(b, t('banner.conn.failed', { msg: msg }));
        })
        .finally(function () {
          tile.classList.remove('entry-connecting');
          inner.classList.remove('entry-connecting');
        });
    });
    // The triangle button opens the launch options modal (rename session, custom command).
    var quickBtnEl = tile.querySelector('.conn-quick-btn');
    if (quickBtnEl) {
      quickBtnEl.addEventListener('click', function (e) {
        e.preventDefault();
        e.stopPropagation();
        openStartModal(c.name);
      });
    }
    var entryCopyBtn = tile.querySelector('.sess-copy-btn');
    if (entryCopyBtn) {
      entryCopyBtn.addEventListener('mousedown', function (e) { e.stopPropagation(); });
      entryCopyBtn.addEventListener('click', function (ev) {
        ev.preventDefault();
        ev.stopPropagation();
        copyTextToClipboard(resourceUrlEntry(c.name)).then(function () { showCopyToast(); }).catch(function () { showCopyToast(t('toast.copy.failed')); });
      });
    }
    var editEl = tile.querySelector('.conn-edit-btn');
    if (editEl) {
      editEl.addEventListener('click', function (ev) {
        ev.preventDefault();
        ev.stopPropagation();
        openConnModal(true, c.name, c.kind);
      });
    }
    grid.appendChild(tile);
  });
  if ((connections || []).length === 0 && window._lastConnections) {
    var empty = document.createElement('div');
    empty.className = 'sess-plate-empty';
    empty.textContent = t('nethub.empty');
    grid.appendChild(empty);
  }

  /* The "Add Host" control is not part of this list any more: it is a fixed
     action under the scrolling host list (see #conn-add in conn-form.js), so a
     long list never pushes it out of reach and the list holds hosts only. */
  // Fill the data-i18n* markers baked into the cards above.
  applyI18n(grid);
  /* Names that left the list (deleted here, renamed in another tab) must not
     linger in the selection: the set only holds what the grid can still show. */
  if (window._lastConnections) {
    var present = {};
    (connections || []).forEach(function (c) { if (c && c.name) present[c.name] = true; });
    _nodeSelIds.forEach(function (n) { if (!present[n]) _nodeSelIds.delete(n); });
  }
  updateNodeBatchBar();
}

/*
 * ---- NetHub collapse state ------------------------------------------------
 * Three widths, one manual preference:
 *
 *   wide (>=1200px)   expanded by default
 *   medium (800-1199) collapsed (rail) by default
 *   narrow (<800px)   the panel is the drawer the page always had
 *
 * A manual toggle is stored under one key and wins over the default at every
 * width above the drawer breakpoint — the same localStorage mechanism the
 * section collapse and the language switch already use, so no new state layer.
 * The stored value is read at load, before the first paint of the frame, which
 * is why it lives here rather than in a DOMContentLoaded handler.
 */
var NETHUB_STATE_KEY = 'termcp.nethub';
var NETHUB_WIDE = 1200;
var NETHUB_DRAWER = 800;

function storedNetHubCollapsed() {
  var v = null;
  try { v = localStorage.getItem(NETHUB_STATE_KEY); } catch (e) {}
  if (v === 'collapsed') return true;
  if (v === 'expanded') return false;
  return null;
}

/** True while NetHub is an overlay rather than a layout column. Mirrors the
    stylesheet's breakpoint: below it the panel is fixed and slides, so the
    "collapsed" state means closed, not railed. */
function netHubIsDrawer() {
  return (window.innerWidth || 0) < NETHUB_DRAWER;
}

function netHubDefaultCollapsed() {
  return (window.innerWidth || 0) < NETHUB_WIDE;
}

function setNetHubCollapsed(collapsed, persist) {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('sec-entries-body');
  var trigger = document.getElementById('open-host-drawer');
  if (!frame || !body) return;
  collapsed = !!collapsed;
  var drawer = netHubIsDrawer();
  /* In the drawer scope the body is shown or hidden; above it the frame keeps
     the rail. Both are one boolean, so the sidebar and the overlay cannot
     disagree about being open — and the scrim follows the overlay only. */
  frame.classList.toggle('nethub-collapsed', collapsed && !drawer);
  body.classList.toggle('drawer-open', drawer && !collapsed);
  var scrim = document.querySelector('.drawer-scrim');
  if (scrim) scrim.classList.toggle('drawer-open', drawer && !collapsed);
  /* The overlay blocks the page behind it; a sidebar does not. */
  document.body.style.overflow = drawer && !collapsed ? 'hidden' : '';
  if (trigger) trigger.setAttribute('aria-expanded', String(!collapsed));
  if (persist) {
    try { localStorage.setItem(NETHUB_STATE_KEY, collapsed ? 'collapsed' : 'expanded'); } catch (e) {}
  }
}

/** The user's toggle: the only path that persists a preference. */
function toggleNetHub() {
  setNetHubCollapsed(!netHubCollapsed(), true);
}

function netHubCollapsed() {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('sec-entries-body');
  if (netHubIsDrawer()) return !(body && body.classList.contains('drawer-open'));
  return !!(frame && frame.classList.contains('nethub-collapsed'));
}

function currentNetHubCollapsed() {
  var stored = storedNetHubCollapsed();
  return stored === null ? netHubDefaultCollapsed() : stored;
}

/* A viewport change can move the page across a breakpoint in either direction:
   resizing a desktop window narrow must not leave a fixed panel "open" over the
   page, and growing back must restore the sidebar the user chose. The stored
   preference is re-applied, never rewritten — a resize is not a decision. */
var _netHubResizeTimer = 0;
window.addEventListener('resize', function () {
  if (_netHubResizeTimer) clearTimeout(_netHubResizeTimer);
  _netHubResizeTimer = setTimeout(function () {
    _netHubResizeTimer = 0;
    setNetHubCollapsed(currentNetHubCollapsed(), false);
  }, 120);
});

setNetHubCollapsed(currentNetHubCollapsed(), false);


function loadForwards() {
  return fetch(apiPath('/api/forwards'))
    .then(function (r) { return r.json(); })
    .then(function (j) {
      window._lastForwards = j.forwards || [];
      renderSessionGrid('');
      document.querySelectorAll('.shell-window').forEach(function(w) { if (w._refreshFwList) w._refreshFwList(); });
    })
    .catch(function (e) { window._lastForwards = []; });
}

// Active shell notification rules (registered by the MCP shell_notify tool).
function loadNotifications() {
  return fetch(apiPath('/api/notifications'))
    .then(function (r) { return r.json(); })
    .then(function (j) {
      window._lastNotifications = j.notifications || [];
      document.querySelectorAll('.shell-window').forEach(function(w) { if (w._refreshNtfList) w._refreshNtfList(); });
    })
    .catch(function () { window._lastNotifications = []; });
}

function deleteNotification(ruleId) {
  return fetch(apiPath('/api/notifications/') + encodeURIComponent(ruleId), { method: 'DELETE' })
    .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); });
}



function loadConnections() {
  return fetch(apiPath('/api/connections'))
    .then(function (r) {
      return r.text().then(function (t) {
        if (!r.ok) {
          throw new Error(t || ('HTTP ' + r.status));
        }
        try {
          return JSON.parse(t);
        } catch (e) {
          throw new Error('Server response was not JSON');
        }
      });
    })
    .then(function (j) {
      window._lastConnections = j.connections || [];
      rememberConnBanner(null);
      renderConnGrid(window._lastConnections, '');
    })
    .catch(function (e) {
      console.error(e);
      window._lastConnections = [];
      var msg = (e && e.message) ? e.message : String(e);
      rememberConnBanner('banner.entries.failed', { msg: msg });
      renderConnGrid([], t('banner.entries.failed', { msg: msg }));
    });
}

// ---- connection form helpers ----

/* Language switch: re-render the entry grid from the in-memory snapshot.
   Refetching would flicker, and would blank the list while offline. The banner
   is passed back in so a failure the user is still reading survives the switch
   in the new language. */
onLangChange(function () { renderConnGrid(window._lastConnections || [], connBannerText()); });
