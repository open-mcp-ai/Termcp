/* Configuration-at-rest controls. Plaintext remains the default and no host is
   converted until it is selected here. This dialog owns the global "encrypt new"
   preference and per-host explicit conversions. */
var _configStorageState = null;
var _configStorageBusy = false;

var _configStorageHeader = document.createElement('div');
_configStorageHeader.id = 'config-storage-header-root';
var _configStorageApiLink = document.querySelector('.app-header-nav .api-nav-link');
_configStorageApiLink.parentNode.insertBefore(_configStorageHeader, _configStorageApiLink);
_configStorageHeader.innerHTML =
  '<button type="button" class="encryption-btn" id="config-storage-open" title="Configuration encryption" data-i18n-title="storage.open" aria-label="Configuration encryption" data-i18n-aria="storage.open">' +
    '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.45" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.2 7V5.2a2.8 2.8 0 015.6 0V7"/><path d="M8 10v1.5"/></svg>' +
    '<span data-i18n="storage.label">Encryption</span>' +
  '</button>';

var _configStorageRoot = document.createElement('div');
_configStorageRoot.id = 'config-storage-root';
document.body.appendChild(_configStorageRoot);
_configStorageRoot.innerHTML =
  '<div id="modal-config-storage" class="modal-backdrop hidden" role="dialog" aria-modal="true">' +
    '<div class="modal config-storage-modal">' +
      '<div class="modal-h">' +
        '<h2 data-i18n="storage.title">Connection configuration encryption</h2>' +
        '<button type="button" id="config-storage-close" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg></button>' +
      '</div>' +
      '<div class="modal-body">' +
        '<p id="config-storage-err" class="err" role="alert" style="display:none"></p>' +
        '<div class="config-storage-toggle">' +
          '<div><strong data-i18n="storage.toggle">Encrypt new connection profiles</strong>' +
            '<p data-i18n="storage.toggleHint">Only profiles created after this is enabled are encrypted. Existing plaintext files stay where they are until you select them below.</p></div>' +
          '<label class="config-storage-switch" for="config-storage-enabled"><input type="checkbox" id="config-storage-enabled">' +
            '<span class="config-storage-switch-track" aria-hidden="true"></span><span data-i18n="storage.enabled">Enabled</span></label>' +
        '</div>' +
        '<p id="config-storage-provider" class="conn-check-hint"></p>' +
        '<div class="config-storage-toolbar"><strong data-i18n="storage.hosts">Connection profiles</strong>' +
          '<span id="config-storage-selected" class="config-storage-selected"></span>' +
          '<button type="button" class="btn config-storage-select-all" id="config-storage-select-all" data-i18n="storage.selectAll">Select all eligible</button>' +
        '</div>' +
        '<div id="config-storage-hosts" class="config-storage-hosts" role="list"></div>' +
        '<div class="modal-actions config-storage-actions">' +
          '<button type="button" class="btn" id="config-storage-encrypt"><svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.2 7V5.2a2.8 2.8 0 015.6 0V7"/></svg><span data-i18n="storage.encryptSelected">Encrypt selected</span></button>' +
          '<button type="button" class="btn" id="config-storage-restore"><svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3.2 7.2A5.2 5.2 0 111.8 10"/><path d="M1.5 5.5v3.2h3.2"/></svg><span data-i18n="storage.restoreSelected">Restore selected</span></button>' +
        '</div>' +
      '</div>' +
    '</div>' +
  '</div>';

function configStorageError(message) {
  var el = document.getElementById('config-storage-err');
  if (!el) return;
  el.textContent = String(message || '');
  el.style.display = message ? 'block' : 'none';
}

function configStorageRequest(path, options) {
  return fetch(apiPath(path), options).then(function (r) {
    return r.text().then(function (text) {
      if (!r.ok) throw new Error(text || ('HTTP ' + r.status));
      if (!text) return null;
      try {
        return JSON.parse(text);
      } catch (e) {
        throw new Error(t('storage.invalidResponse'));
      }
    });
  });
}

function configStorageModeLabel(mode) {
  var labels = {
    builtin: t('storage.mode.builtin'),
    plaintext: t('storage.mode.plaintext'),
    encrypted: t('storage.mode.encrypted'),
    temporary: t('storage.mode.temporary')
  };
  return labels[mode] || mode;
}

function selectedConfigStorageHosts(mode) {
  return Array.prototype.slice.call(
    document.querySelectorAll('#config-storage-hosts input[type="checkbox"]:checked')
  ).filter(function (input) {
    return !mode || input.getAttribute('data-mode') === mode;
  }).map(function (input) {
    return input.getAttribute('data-name');
  });
}

function updateConfigStorageActions() {
  var plain = selectedConfigStorageHosts('plaintext').length;
  var encrypted = selectedConfigStorageHosts('encrypted').length;
  var result = plain + encrypted;
  var selected = document.getElementById('config-storage-selected');
  if (selected) selected.textContent = result ? t('storage.selectedCount', { count: result }) : '';
  var encrypt = document.getElementById('config-storage-encrypt');
  var restore = document.getElementById('config-storage-restore');
  if (encrypt) encrypt.disabled = _configStorageBusy || plain === 0;
  if (restore) restore.disabled = _configStorageBusy || encrypted === 0;
}

function renderConfigStorage() {
  var state = _configStorageState || { initialized: false, encrypt_new: false, hosts: [] };
  var enabled = document.getElementById('config-storage-enabled');
  enabled.checked = !!state.encrypt_new;
  enabled.disabled = _configStorageBusy;
  document.getElementById('config-storage-provider').textContent =
    state.legacy_encryption_artifacts ? t('storage.provider.legacyArtifacts') :
      (state.initialized ? t('storage.provider.ready') : t('storage.provider.notInitialized'));

  var box = document.getElementById('config-storage-hosts');
  box.innerHTML = '';
  var eligible = 0;
  (state.hosts || []).forEach(function (host) {
    var convertible = host.mode === 'plaintext' || host.mode === 'encrypted';
    if (convertible) eligible++;
    var row = document.createElement('label');
    row.className = 'config-storage-host';
    row.setAttribute('role', 'listitem');
    row.innerHTML =
      '<input type="checkbox" data-name="' + escapeHtml(host.name) + '" data-mode="' + escapeHtml(host.mode) + '"' +
        (convertible && !_configStorageBusy ? '' : ' disabled') + '>' +
      '<span class="config-storage-host-name">' + escapeHtml(host.name) + '</span>' +
      '<span class="config-storage-host-mode" data-mode="' + escapeHtml(host.mode) + '">' +
        escapeHtml(configStorageModeLabel(host.mode)) + '</span>';
    box.appendChild(row);
  });
  if (!eligible) {
    var empty = document.createElement('div');
    empty.className = 'sess-plate-empty';
    empty.textContent = t('storage.noEligibleHosts');
    box.appendChild(empty);
  }
  var selectAll = document.getElementById('config-storage-select-all');
  if (selectAll) selectAll.disabled = _configStorageBusy || eligible === 0;
  updateConfigStorageActions();
  applyI18n(box);
}

function loadConfigStorage() {
  configStorageError('');
  return configStorageRequest('/api/config-storage').then(function (state) {
    _configStorageState = state;
    renderConfigStorage();
  }).catch(function (err) {
    configStorageError(err.message || err);
  });
}

function setConfigStorageBusy(busy) {
  _configStorageBusy = busy;
  renderConfigStorage();
}

function openConfigStorage() {
  configStorageError('');
  showModal('modal-config-storage');
  loadConfigStorage();
}

function convertSelectedConfigStorage(action) {
  var mode = action === 'encrypt' ? 'plaintext' : 'encrypted';
  var names = selectedConfigStorageHosts(mode);
  if (!names.length) {
    configStorageError(t('storage.selectAtLeastOne'));
    return;
  }
  var title = action === 'encrypt' ? t('storage.encryptTitle') : t('storage.restoreTitle');
  var message = action === 'encrypt'
    ? tCount('storage.encryptConfirm.one', 'storage.encryptConfirm.other', { count: names.length })
    : tCount('storage.restoreConfirm.one', 'storage.restoreConfirm.other', { count: names.length });
  confirmDialog({ title: title, message: message, okText: title }).then(function (ok) {
    if (!ok) return;
    setConfigStorageBusy(true);
    configStorageError('');
    configStorageRequest('/api/config-storage/' + action, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json; charset=utf-8' },
      body: JSON.stringify({ names: names })
    }).then(function (response) {
      var results = (response && response.results) || [];
      var failed = results.filter(function (r) { return !r.ok; });
      return loadConfigStorage().then(function () {
        if (typeof loadConnections === 'function') loadConnections();
        if (failed.length) {
          throw new Error(failed.map(function (r) {
            return r.name + ': ' + (r.error || r.code || 'failed');
          }).join('; '));
        }
        showCopyToast(tCount('storage.converted.one', 'storage.converted.other', {
          count: results.filter(function (r) { return r.ok; }).length
        }));
      });
    }).catch(function (err) {
      configStorageError(err.message || err);
    }).finally(function () {
      setConfigStorageBusy(false);
    });
  });
}

document.getElementById('config-storage-open').onclick = openConfigStorage;
document.getElementById('config-storage-close').onclick = function () {
  hideModal('modal-config-storage');
};
document.getElementById('config-storage-enabled').onchange = function () {
  var desired = this.checked;
  setConfigStorageBusy(true);
  configStorageError('');
  configStorageRequest('/api/config-storage', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
    body: JSON.stringify({ encrypt_new: desired })
  }).then(function (state) {
    _configStorageState = state;
    if (typeof loadConnections === 'function') loadConnections();
  }).catch(function (err) {
    this.checked = !desired;
    configStorageError(err.message || err);
  }.bind(this)).finally(function () {
    setConfigStorageBusy(false);
  });
};
document.getElementById('config-storage-hosts').addEventListener('change', function (e) {
  if (e.target && e.target.matches('input[type="checkbox"]')) updateConfigStorageActions();
});
document.getElementById('config-storage-select-all').onclick = function () {
  document.querySelectorAll(
    '#config-storage-hosts input[type="checkbox"]:not(:disabled):not(:checked)'
  ).forEach(function (input) { input.checked = true; });
  updateConfigStorageActions();
};
document.getElementById('config-storage-encrypt').onclick = function () {
  convertSelectedConfigStorage('encrypt');
};
document.getElementById('config-storage-restore').onclick = function () {
  convertSelectedConfigStorage('restore');
};
onLangChange(function () { renderConfigStorage(); });
