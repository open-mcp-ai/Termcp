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

function renderConnGrid(connections, bannerMsg) {
  var grid = document.getElementById('conn-grid');
  if (!grid) return;
  setLoadBanner(document.getElementById('conn-load-banner'), bannerMsg);
  grid.innerHTML = '';
  (connections || []).sort(function(a, b) {
    if (a.kind === 'internal') return -1;
    if (b.kind === 'internal') return 1;
    return (a.name || '').localeCompare(b.name || '');
  }).forEach(function (c) {
    var tile = document.createElement('div');
    tile.className = 'conn-tile entry-card';
    tile.setAttribute('data-conn-name', c.name);
    var icon = c.kind === 'internal' ? '🔁' : '🌐';
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
    tile.innerHTML =
      '<div class="entry-card-inner">' +
      '<div class="conn-tile-stack">' +
      '<div class="icon-wrap">' + icon + '</div>' +
      '</div>' +
      '<div class="entry-card-main">' +
      '<div class="conn-nm-row">' +
      '<span class="conn-nm-cluster">' +
      '<span class="conn-nm" title="Connect" data-i18n-title="conn.title.connect">' + escapeHtml(c.name) + '</span>' +
      '<button type="button" class="sess-copy-btn" title="Copy URL" data-i18n-title="common.copyUrl" aria-label="Copy URL" data-i18n-aria="common.copyUrl">' + SVG_COPY_12 + '</button>' +
      '</span>' +
      quickBtn +
      apprMark +
      editBtn +
      '</div>' +
      '</div>' +
      '</div>';
    var inner = tile.querySelector('.entry-card-inner');
    var tipConnect = t('conn.tip.connect');
    if (c.kind === 'internal') {
      inner.title = tipConnect + ' · ' + t('conn.tip.internal');
    } else {
      var hostLine = ((c.user || '') + '@' + (c.host || '') + (c.port && c.port !== 22 ? ':' + c.port : '')).trim();
      inner.title = hostLine ? (tipConnect + ' — ' + hostLine) : tipConnect;
    }
    // Clicking the card body connects directly (most frequent action).
    inner.addEventListener('click', function (e) {
      if (e.target.closest('.conn-edit-btn') || e.target.closest('.sess-copy-btn') || e.target.closest('.conn-quick-btn')) return;
      var b = document.getElementById('conn-load-banner');
      if (b) setLoadBanner(b, '');
      rememberConnBanner(null);
      tile.classList.add('entry-connecting');
      inner.classList.add('entry-connecting');
      startSessionAndOpenShell(c.name, e)
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
        openStartModal(c.name, e);
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

  /* "Add Host" as a trailing card rather than a header button: it belongs
     with the list it extends, and the header keeps only the section toggle. The
     card is the plus alone — the label said nothing the glyph does not, and on a
     phone it turned the row into a wide band with one word in it. The accessible
     name still carries the meaning. */
  var addCard = document.createElement('div');
  addCard.className = 'conn-tile entry-card entry-card-add';
  addCard.setAttribute('role', 'button');
  addCard.setAttribute('tabindex', '0');
  addCard.setAttribute('aria-label', t('conn.aria.add'));
  addCard.innerHTML =
    '<div class="entry-card-inner">' +
    '<div class="conn-tile-stack">' +
    '<div class="icon-wrap"><svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2.25" stroke-linecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg></div>' +
    '</div>' +
    '</div>';
  function openAdd() { openConnModal(false, ''); }
  addCard.addEventListener('click', openAdd);
  addCard.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openAdd(); }
  });
  grid.appendChild(addCard);
  // Fill the data-i18n* markers baked into the cards above.
  applyI18n(grid);
}


function loadForwards() {
  return fetch('/api/forwards')
    .then(function (r) { return r.json(); })
    .then(function (j) {
      window._lastForwards = j.forwards || [];
      renderSessionGrid(window._lastSessionsSnapshot || [], '');
      document.querySelectorAll('.shell-window').forEach(function(w) { if (w._refreshFwList) w._refreshFwList(); });
    })
    .catch(function (e) { window._lastForwards = []; });
}

// Active shell notification rules (registered by the MCP shell_notify tool).
function loadNotifications() {
  return fetch('/api/notifications')
    .then(function (r) { return r.json(); })
    .then(function (j) {
      window._lastNotifications = j.notifications || [];
      document.querySelectorAll('.shell-window').forEach(function(w) { if (w._refreshNtfList) w._refreshNtfList(); });
    })
    .catch(function () { window._lastNotifications = []; });
}

function deleteNotification(ruleId) {
  return fetch('/api/notifications/' + encodeURIComponent(ruleId), { method: 'DELETE' })
    .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); });
}



function loadConnections() {
  return fetch('/api/connections')
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
