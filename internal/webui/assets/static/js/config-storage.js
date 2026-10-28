/* Configuration-at-rest controls. Main Key is derived from a user password.
   Profile storage stays opt-in, and existing hosts change mode only by selection. */
var _configStorageState = null;
var _configStorageBusy = false;
var _configStorageInitializing = false;
var _configStorageChanging = false;

var _configStorageHeader = document.createElement('div');
_configStorageHeader.id = 'config-storage-header-root';
var _configStorageApiLink = document.querySelector('.app-header-nav .api-nav-link');
_configStorageApiLink.parentNode.insertBefore(_configStorageHeader, _configStorageApiLink);
_configStorageHeader.innerHTML =
  '<button type="button" class="encryption-btn" id="config-storage-open" title="Configuration encryption" data-i18n-title="storage.open" aria-label="Configuration encryption" data-i18n-aria="storage.open">' +
  '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.45" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.2 7V5.2a2.8 2.8 0 015.6 0V7"/></svg>' +
  '<span data-i18n="storage.label">Encryption</span></button>';

var _configStorageRoot = document.createElement('div');
_configStorageRoot.id = 'config-storage-root';
document.body.appendChild(_configStorageRoot);
_configStorageRoot.innerHTML = [
  '<div id="modal-config-storage" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal config-storage-modal"><div class="modal-h">',
  '<h2 data-i18n="storage.title">Connection configuration encryption</h2>',
  '<button type="button" id="config-storage-close" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="config-storage-err" class="err" role="alert" style="display:none"></p>',
  '<form id="config-storage-recovery" class="config-storage-key-form" style="display:none">',
  '<h3 data-i18n="storage.recoverTitle">Unlock encrypted configurations</h3>',
  '<p id="config-storage-recovery-reason" class="conn-check-hint" role="status"></p>',
  '<label for="config-storage-recovery-key" data-i18n="storage.password">Encryption password</label>',
  '<input type="password" id="config-storage-recovery-key" autocomplete="current-password" maxlength="1024" required aria-describedby="config-storage-recovery-hint">',
  '<p id="config-storage-recovery-hint" class="conn-check-hint" data-i18n="storage.recoveryHint">Enter your encryption password. Without an automatic unlock key, enter it again after restart.</p>',
  '<div class="config-storage-form-actions"><button type="submit" class="btn btn-primary" id="config-storage-recover-submit" data-i18n="storage.recover">Unlock</button>',
  '<button type="button" class="btn" id="config-storage-rebind-submit" data-i18n="storage.rebind">Unlock and restore automatic protection</button></div></form>',
  '<div id="config-storage-controls">',
  '<div class="config-storage-toggle"><div><strong data-i18n="storage.toggle">Encrypt new connection profiles</strong>',
  '<p data-i18n="storage.toggleHint">Only newly created profiles use this default. Existing files retain their storage mode.</p></div>',
  '<label class="config-storage-switch" for="config-storage-enabled"><input type="checkbox" id="config-storage-enabled">',
  '<span class="config-storage-switch-track" aria-hidden="true"></span><span data-i18n="storage.enabled">Enabled</span></label></div>',
  '<form id="config-storage-initialize" class="config-storage-key-form" style="display:none">',
  '<h3 data-i18n="storage.initializeTitle">Set encryption password</h3>',
  '<p class="conn-check-hint" id="config-storage-initialize-hint" data-i18n="storage.initializeHint">Main Key is derived from your password. Remember it; existing plaintext files remain unchanged.</p>',
  '<label for="config-storage-initial-password" data-i18n="storage.password">Encryption password</label>',
  '<input type="password" id="config-storage-initial-password" autocomplete="new-password" minlength="8" maxlength="1024" required aria-describedby="config-storage-initialize-hint">',
  '<label for="config-storage-initial-confirmation" data-i18n="storage.confirmPassword">Confirm password</label>',
  '<input type="password" id="config-storage-initial-confirmation" autocomplete="new-password" maxlength="1024" required>',
  '<div class="config-storage-form-actions"><button type="submit" class="btn btn-primary" id="config-storage-initialize-submit" data-i18n="storage.initializeEnable">Set password and enable encryption</button>',
  '<button type="button" class="btn" id="config-storage-initialize-cancel" data-i18n="common.cancel">Cancel</button></div></form>',
  '<p id="config-storage-provider" class="conn-check-hint"></p>',
  '<div id="config-storage-key" class="config-storage-key" style="display:none"><div>',
  '<strong data-i18n="storage.mainKey">Encryption password</strong><p id="config-storage-key-hint" class="conn-check-hint"></p></div>',
  '<button type="button" class="btn" id="config-storage-password-open"></button></div>',
  '<form id="config-storage-password-form" class="config-storage-key-form" style="display:none">',
  '<h3 id="config-storage-password-title"></h3>',
  '<div id="config-storage-current-row"><label for="config-storage-current-password" id="config-storage-current-label"></label>',
  '<input type="password" id="config-storage-current-password" autocomplete="current-password" maxlength="1024"></div>',
  '<div id="config-storage-new-rows"><label for="config-storage-new-password" data-i18n="storage.newPassword">New password</label>',
  '<input type="password" id="config-storage-new-password" autocomplete="new-password" minlength="8" maxlength="1024">',
  '<label for="config-storage-new-confirmation" data-i18n="storage.confirmNewPassword">Confirm new password</label>',
  '<input type="password" id="config-storage-new-confirmation" autocomplete="new-password" maxlength="1024"></div>',
  '<p class="conn-check-hint" data-i18n="storage.changeHint">Changing the password derives a new Main Key and updates protected data keys. Configuration contents stay unchanged.</p>',
  '<div class="config-storage-form-actions"><button type="submit" class="btn btn-primary" id="config-storage-password-submit"></button>',
  '<button type="button" class="btn" id="config-storage-password-cancel" data-i18n="common.cancel">Cancel</button></div></form>',
  '<div class="config-storage-toolbar"><strong data-i18n="storage.hosts">Connection profiles</strong>',
  '<span id="config-storage-selected" class="config-storage-selected"></span>',
  '<button type="button" class="btn config-storage-select-all" id="config-storage-select-all" data-i18n="storage.selectAll">Select all eligible</button></div>',
  '<div id="config-storage-hosts" class="config-storage-hosts" role="list"></div>',
  '<div class="modal-actions config-storage-actions">',
  '<button type="button" class="btn" id="config-storage-encrypt" data-i18n="storage.encryptSelected">Encrypt selected</button>',
  '<button type="button" class="btn" id="config-storage-restore" data-i18n="storage.restoreSelected">Restore selected</button>',
  '</div></div></div></div></div>'
].join('');

function configStorageError(message) {
  var el = document.getElementById('config-storage-err');
  el.textContent = String(message || '');
  el.style.display = message ? 'block' : 'none';
}
function configStorageRequest(path, options) {
  return fetch(apiPath(path), options).then(function (r) {
    return r.text().then(function (text) {
      var parsed = null;
      try { parsed = text ? JSON.parse(text) : null; } catch (ignored) {}
      if (!r.ok) {
        var error = new Error(parsed && parsed.error ? parsed.error : (text || ('HTTP ' + r.status)));
        error.code = parsed && parsed.code;
        throw error;
      }
      if (text && !parsed) throw new Error(t('storage.invalidResponse'));
      return parsed;
    });
  });
}
function configStorageFailure(err) {
  switch (err.code) {
    case 'password_incorrect': return t('storage.passwordIncorrect');
    case 'password_invalid': return t('storage.passwordRules');
    case 'password_confirmation_mismatch': return t('storage.passwordMismatch');
    case 'password_not_configured': return t('storage.legacyPasswordRequired');
    case 'password_required': return t('storage.passwordRequired');
    case 'key_operation_busy': return t('storage.keyBusy');
    case 'key_provider_unavailable': return t('storage.providerUnavailable');
    case 'keyring_authentication_failed':
    case 'config_encryption_locked': return t('storage.providerAuthFailed');
    default: return err.message || String(err);
  }
}
function configStorageModeLabel(mode) {
  var labels = {builtin: t('storage.mode.builtin'), plaintext: t('storage.mode.plaintext'), encrypted: t('storage.mode.encrypted'), temporary: t('storage.mode.temporary')};
  return labels[mode] || mode;
}
function selectedConfigStorageHosts(mode) {
  return Array.prototype.slice.call(document.querySelectorAll('#config-storage-hosts input:checked')).filter(function (input) {
    return !mode || input.getAttribute('data-mode') === mode;
  }).map(function (input) { return input.getAttribute('data-name'); });
}
function updateConfigStorageActions() {
  var state = _configStorageState || {};
  var plain = selectedConfigStorageHosts('plaintext').length;
  var encrypted = selectedConfigStorageHosts('encrypted').length;
  document.getElementById('config-storage-selected').textContent = plain + encrypted ? t('storage.selectedCount', {count: plain + encrypted}) : '';
  document.getElementById('config-storage-encrypt').disabled = _configStorageBusy || !!state.rotation_pending || !state.initialized || !plain;
  document.getElementById('config-storage-restore').disabled = _configStorageBusy || !!state.rotation_pending || !encrypted;
}
function renderConfigStorage() {
  var state = _configStorageState || {initialized: false, encrypt_new: false, hosts: []};
  document.getElementById('config-storage-controls').style.display = state.locked ? 'none' : '';
  document.getElementById('config-storage-recovery').style.display = state.locked ? '' : 'none';
  document.getElementById('config-storage-recovery-reason').textContent = !state.password_configured
    ? t('storage.legacyPasswordRequired') : (state.error_code === 'password_required' ? t('storage.passwordRequired') : t('storage.providerAuthFailed'));
  document.getElementById('config-storage-recovery-key').disabled = _configStorageBusy || !state.password_configured;
  document.getElementById('config-storage-recover-submit').disabled = _configStorageBusy || !state.password_configured;
  document.getElementById('config-storage-rebind-submit').disabled = _configStorageBusy || !state.password_configured;
  var enabled = document.getElementById('config-storage-enabled');
  enabled.checked = !!state.encrypt_new;
  enabled.disabled = _configStorageBusy || !!state.rotation_pending || _configStorageInitializing;
  document.getElementById('config-storage-initialize').style.display = _configStorageInitializing ? '' : 'none';
  document.getElementById('config-storage-provider').textContent = state.legacy_encryption_artifacts
    ? t('storage.provider.legacyArtifacts') : (state.initialized ? t('storage.provider.ready') : t('storage.provider.notInitialized'));
  document.getElementById('config-storage-key').style.display = state.initialized ? '' : 'none';
  document.getElementById('config-storage-key-hint').textContent = state.rotation_pending ? t('storage.rotationPending')
    : (!state.password_configured ? t('storage.legacyPasswordSetup') : (state.automatic_unlock ? t('storage.autoUnlockHint') : t('storage.manualUnlockHint')));
  var title = state.rotation_pending ? t('storage.resumePasswordChange') : (state.password_configured ? t('storage.changePassword') : t('storage.setPassword'));
  document.getElementById('config-storage-password-open').textContent = title;
  document.getElementById('config-storage-password-open').disabled = _configStorageBusy;
  document.getElementById('config-storage-password-title').textContent = title;
  document.getElementById('config-storage-password-submit').textContent = title;
  document.getElementById('config-storage-password-form').style.display = _configStorageChanging ? '' : 'none';
  document.getElementById('config-storage-current-row').style.display = state.password_configured ? '' : 'none';
  document.getElementById('config-storage-current-label').textContent = state.rotation_pending ? t('storage.pendingPassword') : t('storage.currentPassword');
  document.getElementById('config-storage-current-password').required = !!state.password_configured;
  document.getElementById('config-storage-new-rows').style.display = state.rotation_pending ? 'none' : '';
  document.getElementById('config-storage-new-password').required = !state.rotation_pending;
  document.getElementById('config-storage-new-confirmation').required = !state.rotation_pending;
  document.querySelectorAll('#config-storage-initialize input, #config-storage-initialize button, #config-storage-password-form input, #config-storage-password-form button').forEach(function (el) { el.disabled = _configStorageBusy; });
  var box = document.getElementById('config-storage-hosts');
  box.innerHTML = '';
  var eligible = 0;
  (state.hosts || []).forEach(function (host) {
    var convertible = host.mode === 'plaintext' || host.mode === 'encrypted';
    if (convertible) eligible++;
    var row = document.createElement('label');
    row.className = 'config-storage-host';
    row.setAttribute('role', 'listitem');
    row.innerHTML = '<input type="checkbox" data-name="' + escapeHtml(host.name) + '" data-mode="' + escapeHtml(host.mode) + '"' +
      (convertible && !_configStorageBusy && !state.rotation_pending ? '' : ' disabled') + '>' +
      '<span class="config-storage-host-name">' + escapeHtml(host.name) + '</span>' +
      '<span class="config-storage-host-mode" data-mode="' + escapeHtml(host.mode) + '">' + escapeHtml(configStorageModeLabel(host.mode)) + '</span>';
    box.appendChild(row);
  });
  if (!eligible) {
    var empty = document.createElement('div');
    empty.className = 'sess-plate-empty';
    empty.textContent = t('storage.noEligibleHosts');
    box.appendChild(empty);
  }
  document.getElementById('config-storage-select-all').disabled = _configStorageBusy || !!state.rotation_pending || !eligible;
  updateConfigStorageActions();
}
function clearConfigStorageSecrets() {
  _configStorageInitializing = false;
  _configStorageChanging = false;
  ['config-storage-initial-password', 'config-storage-initial-confirmation', 'config-storage-recovery-key', 'config-storage-current-password', 'config-storage-new-password', 'config-storage-new-confirmation'].forEach(function (id) {
    document.getElementById(id).value = '';
  });
}
function setConfigStorageBusy(busy) { _configStorageBusy = busy; renderConfigStorage(); }
function loadConfigStorage() {
  return configStorageRequest('/api/config-storage').then(function (state) {
    _configStorageState = state;
    renderConfigStorage();
  });
}
function openConfigStorage() {
  configStorageError('');
  showModal('modal-config-storage');
  loadConfigStorage().catch(function (err) { configStorageError(configStorageFailure(err)); });
}
function submitConfigStoragePassword(action, body) {
  if (_configStorageBusy) return;
  var oldKeyID = _configStorageState && _configStorageState.main_key_id;
  setConfigStorageBusy(true);
  configStorageError('');
  configStorageRequest('/api/config-storage/' + action, {
    method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'}, body: JSON.stringify(body)
  }).then(function (state) {
    _configStorageState = state;
    clearConfigStorageSecrets();
    if (typeof loadConnections === 'function') loadConnections();
    showCopyToast(action === 'password' ? t('storage.passwordChanged') : (action === 'initialize' ? t('storage.initializeSuccess') : t('storage.recoverySuccess')));
  }).catch(function (err) {
    document.getElementById('config-storage-current-password').value = '';
    document.getElementById('config-storage-recovery-key').value = '';
    return loadConfigStorage().then(function () {
      if (action === 'password' && _configStorageState.rotation_pending) {
        clearConfigStorageSecrets();
        _configStorageChanging = true;
      } else if (_configStorageState.main_key_id !== oldKeyID && !_configStorageState.locked) {
        clearConfigStorageSecrets();
      }
    }).catch(function () {}).then(function () { configStorageError(configStorageFailure(err)); });
  }).finally(function () { body = null; setConfigStorageBusy(false); });
}
function convertSelectedConfigStorage(action) {
  var names = selectedConfigStorageHosts(action === 'encrypt' ? 'plaintext' : 'encrypted');
  if (!names.length) { configStorageError(t('storage.selectAtLeastOne')); return; }
  var title = action === 'encrypt' ? t('storage.encryptTitle') : t('storage.restoreTitle');
  var message = action === 'encrypt'
    ? tCount('storage.encryptConfirm.one', 'storage.encryptConfirm.other', {count: names.length})
    : tCount('storage.restoreConfirm.one', 'storage.restoreConfirm.other', {count: names.length});
  confirmDialog({title: title, message: message, okText: title}).then(function (ok) {
    if (!ok) return;
    clearConfigStorageSecrets();
    setConfigStorageBusy(true);
    configStorageError('');
    configStorageRequest('/api/config-storage/' + action, {
      method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'}, body: JSON.stringify({names: names})
    }).then(function (response) {
      var failed = ((response && response.results) || []).filter(function (r) { return !r.ok; });
      return loadConfigStorage().then(function () {
        if (typeof loadConnections === 'function') loadConnections();
        if (failed.length) throw new Error(failed.map(function (r) { return r.name + ': ' + (r.error || r.code); }).join('; '));
        showCopyToast(tCount('storage.converted.one', 'storage.converted.other', {count: names.length}));
      });
    }).catch(function (err) { configStorageError(configStorageFailure(err)); })
      .finally(function () { setConfigStorageBusy(false); });
  });
}
document.getElementById('config-storage-open').onclick = openConfigStorage;
document.getElementById('config-storage-close').onclick = function () { hideModal('modal-config-storage'); };
document.getElementById('config-storage-enabled').onchange = function () {
  if (this.checked && (!_configStorageState || !_configStorageState.initialized)) {
    _configStorageInitializing = true;
    renderConfigStorage();
    document.getElementById('config-storage-initial-password').focus();
    return;
  }
  var desired = this.checked;
  setConfigStorageBusy(true);
  configStorageRequest('/api/config-storage', {
    method: 'PUT', headers: {'Content-Type': 'application/json; charset=utf-8'}, body: JSON.stringify({encrypt_new: desired})
  }).then(function (state) { _configStorageState = state; })
    .catch(function (err) { configStorageError(configStorageFailure(err)); })
    .finally(function () { setConfigStorageBusy(false); });
};
document.getElementById('config-storage-initialize').onsubmit = function (event) {
  event.preventDefault();
  submitConfigStoragePassword('initialize', {password: document.getElementById('config-storage-initial-password').value, confirmation: document.getElementById('config-storage-initial-confirmation').value});
};
document.getElementById('config-storage-recovery').onsubmit = function (event) {
  event.preventDefault();
  submitConfigStoragePassword('unlock', {password: document.getElementById('config-storage-recovery-key').value});
};
document.getElementById('config-storage-rebind-submit').onclick = function () {
  var password = document.getElementById('config-storage-recovery-key').value;
  if (password) submitConfigStoragePassword('recover', {password: password});
};
document.getElementById('config-storage-password-form').onsubmit = function (event) {
  event.preventDefault();
  var pending = _configStorageState && _configStorageState.rotation_pending;
  submitConfigStoragePassword('password', {
    current_password: document.getElementById('config-storage-current-password').value,
    new_password: pending ? '' : document.getElementById('config-storage-new-password').value,
    confirmation: pending ? '' : document.getElementById('config-storage-new-confirmation').value
  });
};
document.getElementById('config-storage-password-open').onclick = function () {
  _configStorageChanging = !_configStorageChanging;
  configStorageError('');
  renderConfigStorage();
  document.getElementById(_configStorageState.password_configured ? 'config-storage-current-password' : 'config-storage-new-password').focus();
};
document.getElementById('config-storage-initialize-cancel').onclick = document.getElementById('config-storage-password-cancel').onclick = function () {
  clearConfigStorageSecrets(); renderConfigStorage();
};
document.getElementById('config-storage-select-all').onclick = function () {
  document.querySelectorAll('#config-storage-hosts input:not(:disabled)').forEach(function (input) { input.checked = true; });
  updateConfigStorageActions();
};
document.getElementById('config-storage-hosts').onchange = updateConfigStorageActions;
document.getElementById('config-storage-encrypt').onclick = function () { convertSelectedConfigStorage('encrypt'); };
document.getElementById('config-storage-restore').onclick = function () { convertSelectedConfigStorage('restore'); };
new MutationObserver(function () {
  if (document.getElementById('modal-config-storage').classList.contains('hidden')) clearConfigStorageSecrets();
}).observe(document.getElementById('modal-config-storage'), {attributes: true, attributeFilter: ['class']});
window.addEventListener('pagehide', clearConfigStorageSecrets);
onLangChange(renderConfigStorage);
loadConfigStorage().then(function () {
  if (_configStorageState.locked) showModal('modal-config-storage');
}).catch(function () {});
