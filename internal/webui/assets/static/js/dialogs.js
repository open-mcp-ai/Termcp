function showModal(id) { document.getElementById(id).classList.remove('hidden'); }
function hideModal(id) { document.getElementById(id).classList.add('hidden'); }

/**
 * Only guard page leave when terminal windows are actually open.
 * Sessions keep running on the server even when windows are closed.
 */
function pageHasLiveState() {
  var openWins = (typeof allShellWins === 'function' ? allShellWins() : []).length;
  if (openWins > 0) return openWins + ' open terminal window' + (openWins > 1 ? 's' : '');
  return '';
}

window.addEventListener('beforeunload', function (e) {
  var live = pageHasLiveState();
  if (!live) return;
  e.preventDefault();
  // Modern browsers always show a generic dialog regardless of text; legacy
  // browsers render returnValue verbatim.
  e.returnValue = 'termcp still has ' + live + '. Leaving closes the terminal view — sessions keep running on the server.';
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
  title.textContent = opts.title || 'Confirm';
  msg.textContent = opts.message || '';
  okBtn.textContent = opts.okText || 'OK';
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
    var editBtn = c.kind === 'internal' ? '' :
      '<button type="button" class="conn-edit-btn" title="Edit profile" aria-label="Edit profile">' + SVG_CONN_EDIT + '</button>';
    var quickBtn = '<button type="button" class="conn-quick-btn" title="Launch options (session name, command)" aria-label="Launch options">' + SVG_CONN_QUICK + '</button>';
    tile.innerHTML =
      '<div class="entry-card-inner">' +
      '<div class="conn-tile-stack">' +
      '<div class="icon-wrap">' + icon + '</div>' +
      '</div>' +
      '<div class="entry-card-main">' +
      '<div class="conn-nm-row">' +
      '<span class="conn-nm-cluster">' +
      '<span class="conn-nm" title="Connect">' + escapeHtml(c.name) + '</span>' +
      '<button type="button" class="sess-copy-btn" title="Copy URL" aria-label="Copy URL">' + SVG_COPY_12 + '</button>' +
      '</span>' +
      quickBtn +
      editBtn +
      '</div>' +
      '</div>' +
      '</div>';
    var inner = tile.querySelector('.entry-card-inner');
    var tipConnect = 'Click to connect directly; ▶ for options (session name, command)';
    if (c.kind === 'internal') {
      inner.title = tipConnect + ' (built-in loopback; not editable)';
    } else {
      var hostLine = ((c.user || '') + '@' + (c.host || '') + (c.port && c.port !== 22 ? ':' + c.port : '')).trim();
      inner.title = hostLine ? (tipConnect + ' — ' + hostLine) : tipConnect;
    }
    // Clicking the card body connects directly (most frequent action).
    inner.addEventListener('click', function (e) {
      if (e.target.closest('.conn-edit-btn') || e.target.closest('.sess-copy-btn') || e.target.closest('.conn-quick-btn')) return;
      var b = document.getElementById('conn-load-banner');
      if (b) setLoadBanner(b, '');
      tile.classList.add('entry-connecting');
      inner.classList.add('entry-connecting');
      startSessionAndOpenShell(c.name, e)
        .catch(function (err) {
          if (err && err.name === 'AbortError') return;
          console.error(err);
          var msg = (err && err.message) ? err.message : String(err);
          if (b) setLoadBanner(b, 'Connection failed: ' + msg);
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
        copyTextToClipboard(resourceUrlEntry(c.name)).then(function () { showCopyToast(); }).catch(function () { showCopyToast('Copy failed'); });
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

  /* "Add connection" as a trailing card rather than a header button: it belongs
     with the list it extends, and the header keeps only the section toggle. The
     card is the plus alone — the label said nothing the glyph does not, and on a
     phone it turned the row into a wide band with one word in it. The accessible
     name still carries the meaning. */
  var addCard = document.createElement('div');
  addCard.className = 'conn-tile entry-card entry-card-add';
  addCard.setAttribute('role', 'button');
  addCard.setAttribute('tabindex', '0');
  addCard.setAttribute('aria-label', 'Add connection');
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
}


function loadForwards() {
  return fetch('/api/forwards')
    .then(function (r) { return r.json(); })
    .then(function (j) {
      window._lastForwards = j.forwards || [];
      renderSessionGrid(window._lastSessionsSnapshot || [], '');
      refreshFwList();
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
      renderConnGrid(j.connections || [], '');
    })
    .catch(function (e) {
      console.error(e);
      var msg = (e && e.message) ? e.message : String(e);
      renderConnGrid([], 'Failed to load entries: ' + msg + '. Ensure termcp is running and you use the correct port. You can still add a profile with +.');
    });
}

// ---- connection form helpers ----
