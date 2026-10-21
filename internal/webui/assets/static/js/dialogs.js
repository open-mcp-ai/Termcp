function showModal(id) { document.getElementById(id).classList.remove('hidden'); }
function hideModal(id) { document.getElementById(id).classList.add('hidden'); }

/**
 * Only guard page leave when terminal windows are actually open.
 * Sessions keep running on the server even when windows are closed.
 *
 * "Actually open" means a window with a session or a dial behind it: the guard's
 * claim is about sessions surviving on the server, and an extension's status card
 * (no _parentSid, no _placeholder) has neither a terminal nor anything that keeps
 * running — counting it would warn about a session the reader never started.
 */
function pageHasLiveState() {
  var openWins = (typeof allShellWins === 'function' ? allShellWins() : [])
    .filter(function (w) { return !!(w._parentSid || w._placeholder); }).length;
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
    /* A subsystem's own connection is not this node's session: an extension dials
       the profile for its own reasons, and counting it here would put a session on
       a card whose count the user cannot act on — the session has no card of its
       own to act from (see isHiddenSession). */
    if (typeof isHiddenSession === 'function' && isHiddenSession(s)) continue;
    if (sessionBelongsToNode(s, name, wins)) out.push(s);
  }
  return out;
}

/** Repaint the node cards from what the page already holds.
 *
 *  A dial in flight lives only in its placeholder window, and a session arriving
 *  or a window opening is not always a socket frame, so the session count is
 *  painted from the windows themselves — otherwise a card keeps claiming what it
 *  said before the click.
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

/** The rail: the expand key, then one key per access node.
 *
 *  A collapsed sidebar still has to answer "which machines can I reach", so each
 *  node keeps a key here, carrying its name and, in the tooltip, how Termcp
 *  reaches it and how many sessions are running on it. No cluster glyph is
 *  rendered here: the trigger directly above already carries it and stays visible
 *  while collapsed, so a second one read as two NetHubs stacked. The count is
 *  capped because the rail is a strip, not a second list: past the cap the
 *  overflow is one more row with the remainder in its tooltip. */
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
    var info = nodeSessionInfo(c.name);
    var item = document.createElement('button');
    item.type = 'button';
    item.className = 'nethub-rail-item';
    item.setAttribute('data-conn-name', c.name);
    item.title = c.name + ' · ' + nodeProtocol(c)
      + (info.count ? ' · ' + tCount('nethub.sessions.one', 'nethub.sessions.other', { count: info.count }) : '');
    item.setAttribute('aria-label', item.title);
    item.innerHTML =
      '<span aria-hidden="true">' + escapeHtml(c.name.slice(0, 2).toUpperCase()) + '</span>' +
      (info.focused ? '<span class="nethub-rail-active" aria-hidden="true"></span>' : '');
    /* The initials name their node and take you to it: expanding first, so the
       click lands on the card it is about rather than on a rail that cannot show it. */
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

/* ---- Host card selection ---
 * One instance of the shared component (batch-bar.js): this column's ids, its
 * list, its verb (dial the selection) and its two menu actions. What used to be
 * a set of helpers here and a copy in the extension column now has one home —
 * the copy had already drifted once. */
var _nodeBatch = createBatchBar({
  bodyId: 'sec-entries-body',
  toggleId: 'nethub-select-toggle',
  countId: 'nethub-toggle-count',
  invertId: 'nethub-sel-invert',
  splitId: 'nethub-open-split',
  mainId: 'nethub-batch-open',
  caretId: 'nethub-open-caret',
  menuId: 'nethub-open-menu',
  exportId: 'nethub-menu-export',
  deleteId: 'nethub-menu-delete',
  actionIds: ['nethub-menu-export', 'nethub-menu-delete'],
  gridId: 'conn-grid',
  cardSelector: '.conn-tile[data-conn-name]',
  nameAttr: 'data-conn-name',
  selectingClass: 'nethub-selecting',
  selectKey: 'nethub.batch.select',
  names: function () {
    return (window._lastConnections || []).map(function (c) { return c && c.name; });
  },
  render: function () { renderConnGrid(window._lastConnections || [], connBannerText()); },
  /* The card's own tooltip, away from selection mode: what this column is for. */
  tipFor: function (name) {
    var byName = {};
    (window._lastConnections || []).forEach(function (c) { if (c && c.name) byName[c.name] = c; });
    var c = byName[name];
    if (!c) return '';
    var tipConnect = t('conn.tip.connect');
    if (c.kind === 'internal') return tipConnect + ' · ' + t('conn.tip.internal');
    var addr = nodeAddress(c);
    return addr ? (tipConnect + ' — ' + addr) : tipConnect;
  },
  onOpen: function (names) { openSelectedNodes(names); },
  onExport: function (names) { exportSelectedNodes(names); },
  onDelete: function (names) { deleteSelectedNodes(names); }
});

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
    var info = nodeSessionInfo(c.name);
    var isInternal = c.kind === 'internal';
    var selected = _nodeBatch.has(c.name);
    tile.className = 'conn-tile entry-card node-card' + (info.focused ? ' node-focus' : '') + (selected ? ' batch-selected' : '');
    tile.setAttribute('data-conn-name', c.name);
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
    /* The node's readout, in the order the column is meant to be read: what it is
       (name), how Termcp reaches it (protocol), at which address, and whether
       anything is running on it. Deliberately no reachability verdict: that would
       be derived from the session list, which keeps archives and holds sessions no
       card shows, so it would call a host dead for a history someone chose to keep
       — and "is this machine up" is not what this column is asked. */
    tile.innerHTML =
      '<div class="entry-card-inner">' +
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
    // Clicking the card body connects directly (most frequent action) — unless
    // selection mode re-aims the card, where a click IS the tick and the
    // connect path stays out of the way entirely.
    inner.addEventListener('click', function (e) {
      if (_nodeBatch.isOn()) {
        _nodeBatch.toggleSelection(c.name, tile);
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
     linger in the selection: the set only holds what the grid can still show.
     The prune rides on the list having arrived — an empty one would drop a tick
     made against a list the page has not seen yet. */
  _nodeBatch.syncCards(!!window._lastConnections);
  _nodeBatch.update();
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
 * Only a toggle above the drawer breakpoint writes it: below it the panel is an
 * overlay, and the overlay's open and close are the page's business rather than
 * the sidebar's preference. The stored value is read at load, before the first
 * paint of the frame, which is why it lives here rather than in a
 * DOMContentLoaded handler.
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

/* The scrim and the page lock belong to the page, not to whichever drawer last
   wrote them: both drawers share one of each, so they must be open when EITHER
   drawer is open. Each writer used to set them from its own state alone, and on
   a narrow load that raced — dialogs.js applied a stored "expanded" preference
   and opened this drawer, then extensions.js applied its own stored "collapsed"
   and cleared the scrim this drawer was still using, leaving the drawer over a
   page it could not lock or dismiss. Reading both bodies makes the shared
   surfaces a function of the pair, so a drawer closing never takes them away
   from a drawer that is still open. */
function paintDrawerScrim() {
  var anyOpen = !!(document.querySelector('#sec-entries-body.drawer-open') ||
                   document.querySelector('#extpanel-body.drawer-open'));
  var scrim = document.querySelector('.drawer-scrim');
  if (scrim) scrim.classList.toggle('drawer-open', anyOpen);
  document.body.style.overflow = anyOpen ? 'hidden' : '';
}

/* A breakpoint crossing is a change of geometry, not of state, and the drawers'
   slide belongs to the gesture that opens them. The closed pane is nevertheless
   off-screen whether or not its transition finishes: visibility is what carries
   it out of the page (see the drawer rules in workspace.css), so nothing here
   has to win a race with the compositor. */

function setNetHubCollapsed(collapsed, persist) {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('sec-entries-body');
  var trigger = document.getElementById('open-host-drawer');
  if (!frame || !body) return;
  collapsed = !!collapsed;
  var drawer = netHubIsDrawer();
  /* The archive drawer shares this scrim and this page; opening this one puts it
     away. archive-column.js loads after this file, so the guard is a typeof
     rather than a call — at load time the function is not there yet, and the
     cross-call only matters once a click can happen. The pair cannot recurse:
     only an opening state calls across, and closing never does.

     This used to name the EXTENSION column's functions, which went with the
     swap: the folding column holds the archive now. The typeof guard meant the
     stale names failed silently, so two drawers could sit open at once — the
     one case the guard was written to make safe is the same one that hides a
     rename, which is why the test pins the names this file actually calls. */
  if (drawer && !collapsed && typeof archiveCollapsed === 'function' && !archiveCollapsed()) {
    setArchiveCollapsed(true, false);
  }
  /* In the drawer scope the body is shown or hidden; above it the frame keeps
     the rail. Both are one boolean, so the sidebar and the overlay cannot
     disagree about being open — and the scrim follows the overlay only. */
  frame.classList.toggle('nethub-collapsed', collapsed && !drawer);
  body.classList.toggle('drawer-open', drawer && !collapsed);
  /* The scrim and the page lock follow BOTH drawers (paintDrawerScrim), so
     closing this one leaves them to the extension drawer if it is open. */
  paintDrawerScrim();
  if (trigger) trigger.setAttribute('aria-expanded', String(!collapsed));
  /* The drawer scope never writes the preference: below the breakpoint the
     panel is an overlay, and dismissing an overlay is not a statement about how
     the desktop sidebar should sit. Writing it here would rail a column the
     wide screen later arrives at — the sidebar found collapsed for a decision
     made about a different shape of the same page. The overlay's state lives in
     the page, and the sidebar keeps its own. */
  if (persist && !drawer) {
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

/* A drawer is an overlay, and an overlay does not open itself: "expanded" was
   stored on a wide screen where it means the sidebar is out, and carried below
   the breakpoint it means a drawer over a page that cannot scroll behind it.
   The stored value still decides the desktop sidebar — this is the state the
   sidebar arrives in, not a rewrite of the preference. */
function initialNetHubCollapsed() {
  return netHubIsDrawer() ? true : currentNetHubCollapsed();
}

/* A viewport change can move the page across a breakpoint in either direction.
   Only a crossing is acted on: a resize that stays on one side of the
   breakpoint changes the sidebar's geometry but not its state, and re-applying
   would close a drawer the user is in the middle of using. Crossing into drawer
   mode closes the panel rather than carrying an expanded sidebar over the page;
   crossing back out keeps a drawer the user had open as the expanded column it
   becomes, rather than re-deriving it from storage — a rotation into a width
   whose default is the rail would otherwise put away a panel the user was
   reading. The stored preference is re-applied, never rewritten — a resize is
   not a decision. */
var _netHubWasDrawer = netHubIsDrawer();
var _netHubResizeTimer = 0;
window.addEventListener('resize', function () {
  if (_netHubResizeTimer) clearTimeout(_netHubResizeTimer);
  _netHubResizeTimer = setTimeout(function () {
    _netHubResizeTimer = 0;
    var drawer = netHubIsDrawer();
    if (drawer === _netHubWasDrawer) return;
    var wasDrawer = _netHubWasDrawer;
    _netHubWasDrawer = drawer;
    if (drawer) {
      setNetHubCollapsed(initialNetHubCollapsed(), false);
      return;
    }
    var body = document.getElementById('sec-entries-body');
    var wasOpen = wasDrawer && body && body.classList.contains('drawer-open');
    setNetHubCollapsed(wasOpen ? false : currentNetHubCollapsed(), false);
  }, 120);
});

setNetHubCollapsed(initialNetHubCollapsed(), false);


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
