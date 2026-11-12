/* Configuration-at-rest controls. Main Key is derived from a user password.
   Profile storage stays opt-in: this panel owns the key, while converting a
   profile belongs to the host column's own batch bar — the same ticked set that
   dials, exports and deletes, so the encryption keys sit where the selection is
   rather than repeating the host list inside a modal.

   The panel shows exactly one state at a time, and each state shows the one form
   that state has: a lock that is not set up asks for a password, a locked lock
   asks for the password it already has, and a configured key shows its setting.
   A checkbox that swaps itself for a password form would be a control whose
   meaning changes under the pointer, and it made the first thing a new user saw
   a switch they had to reason about rather than the password they came to set. */
var _configStorageState = null;
var _configStorageBusy = false;
var _configStorageChanging = false;

var _configStorageRoot = document.createElement('div');
_configStorageRoot.id = 'config-storage-root';
document.body.appendChild(_configStorageRoot);
_configStorageRoot.innerHTML = [
  '<div id="modal-config-storage" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:440px"><div class="modal-h">',
  '<h2 data-i18n="storage.title">Connection encryption</h2>',
  '<button type="button" id="config-storage-close" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="config-storage-err" class="err" style="display:none"></p>',
  '<p id="config-storage-status" class="conn-field-hint"></p>',
  '<form id="config-storage-setup" style="display:none">',
  '<div class="conn-field"><label class="conn-check"><input type="radio" name="config-storage-mode" value="machine" id="config-storage-mode-machine" checked><span data-i18n="storage.modeMachine">Bind to this machine (unusable elsewhere)</span></label>',
  '<div class="conn-check-hint" id="config-storage-mode-machine-hint" data-i18n="storage.modeMachineHint">The key is held by this machine\u2019s credential store. Restarts need no password; copied data cannot be read anywhere else.</div></div>',
  '<div class="conn-field"><label class="conn-check"><input type="radio" name="config-storage-mode" value="password" id="config-storage-mode-password"><span data-i18n="storage.modePassword">Protect with a password (portable)</span></label>',
  '<div class="conn-check-hint" data-i18n="storage.modePasswordHint">Enter this password once on any other machine to unlock the profiles.</div></div>',
  '<div id="config-storage-setup-password-fields" style="display:none">',
  '<div class="conn-field"><label for="config-storage-initial-password" data-i18n="storage.password">Encryption password</label>',
  _configStoragePasswordInput('config-storage-initial-password', 'new-password', 8) + '</div>',
  '<div class="conn-field"><label for="config-storage-initial-confirmation" data-i18n="storage.confirmPassword">Confirm password</label>',
  _configStoragePasswordInput('config-storage-initial-confirmation', 'new-password', 8) + '</div>',
  '</div>',
  '<div class="modal-actions"><button type="submit" class="btn btn-primary" id="config-storage-setup-submit" data-i18n="storage.createKey">Create key</button></div>',
  '</form>',
  '<form id="config-storage-unlock" style="display:none">',
  '<div class="conn-field"><label for="config-storage-unlock-key" data-i18n="storage.password">Encryption password</label>',
  _configStoragePasswordInput('config-storage-unlock-key', 'current-password') + '</div>',
  '<div class="conn-field" id="config-storage-unlock-bind-row" style="display:none"><label class="conn-check"><input type="checkbox" id="config-storage-unlock-bind"><span data-i18n="storage.unlockBind">Also bind to this machine (restarts won\u2019t ask)</span></label></div>',
  '<div class="modal-actions"><button type="submit" class="btn btn-primary" data-i18n="storage.unlock">Unlock</button>',
  '<button type="button" class="btn btn-danger" id="config-storage-destroy-locked" data-i18n="storage.destroy">Delete key</button></div>',
  '</form>',
  '<div id="config-storage-on" style="display:none">',
  '<div class="modal-actions"><button type="button" class="btn" id="config-storage-lock" data-i18n="storage.lock">Lock</button>',
  '<button type="button" class="btn" id="config-storage-change-open" data-i18n="storage.changePassword">Change password</button>',
  '<button type="button" class="btn btn-danger" id="config-storage-destroy" data-i18n="storage.destroy">Delete key</button></div>',
  '<form id="config-storage-change" style="display:none">',
  '<div class="conn-field"><label for="config-storage-current-password" id="config-storage-current-label" data-i18n="storage.currentPassword">Current password</label>',
  _configStoragePasswordInput('config-storage-current-password', 'current-password') + '</div>',
  '<div class="conn-field" id="config-storage-new-rows"><label for="config-storage-new-password" data-i18n="storage.newPassword">New password</label>',
  _configStoragePasswordInput('config-storage-new-password', 'new-password', 8) +
  '<label for="config-storage-new-confirmation" data-i18n="storage.confirmNewPassword">Confirm new password</label>',
  _configStoragePasswordInput('config-storage-new-confirmation', 'new-password', 8) + '</div>',
  '<p class="conn-field-hint" data-i18n="storage.changeHint">Changing the password derives a new Main Key and rewraps the protected data keys. Configuration contents stay unchanged.</p>',
  '<div class="modal-actions"><button type="button" class="btn" id="config-storage-change-cancel" data-i18n="common.cancel">Cancel</button>',
  '<button type="submit" class="btn btn-primary" id="config-storage-change-submit" data-i18n="storage.changePassword">Change password</button></div>',
  '</form>',
  '</div>',
  '</div></div></div>',
].join('');

/* The eye belongs to the field, not to the panel: same markup the connection
   editor uses, so the two read as one product and the show/hide gesture is
   already known. */
function _configStoragePasswordInput(id, autocomplete, minlength) {
  return '<div class="psw-wrap"><input type="password" id="' + id + '" class="psw-input" autocomplete="' + autocomplete + '"' +
    (minlength ? ' minlength="' + minlength + '"' : '') + ' maxlength="1024">' +
    '<button type="button" class="psw-eye" title="Show password" data-i18n-title="modal.conn.showPassword" aria-label="Show password" data-i18n-aria="modal.conn.showPassword">' +
    '<svg class="eye-open" viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"><path d="M1 8s2.5-5 7-5 7 5 7 5-2.5 5-7 5-7-5-7-5z"/><circle cx="8" cy="8" r="2.5"/></svg>' +
    '<svg class="eye-closed" viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round" style="display:none"><path d="M6 3.5A6.4 6.4 0 018 3.2C12.5 3.2 15 8 15 8s-1 2-2.8 3.8M10 10a3 3 0 01-4-4M3.5 11.5l-2 2M3 8c0 0 2.5-5 7-5 1 0 2 .4 3 1M14.5 1l-13 13"/></svg>' +
    '</button></div>';
}

/* The host column's key sits with the other add keys, built here rather than in
   index.html because that shell has a markup budget (TestIndexHTMLIsThin) and
   this module owns when the control exists at all. The batch menu gains the two
   conversions next to export and delete: they act on the same ticked set. */
document.getElementById('conn-import-open').insertAdjacentHTML('beforebegin',
  '<button type="button" class="drawer-add drawer-import" id="config-storage-open" title="Connection configuration encryption" data-i18n-title="storage.open" aria-label="Connection configuration encryption" data-i18n-aria="storage.open">' +
  '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 014.9-.6"/></svg></button>');
document.getElementById('nethub-menu-delete').insertAdjacentHTML('beforebegin',
  '<button type="button" class="nethub-menu-item" id="nethub-menu-encrypt" role="menuitem" title="Encrypt selected hosts" data-i18n-title="storage.encryptTitle">' +
  '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.2 7V5.2a2.8 2.8 0 015.6 0V7"/></svg>' +
  '<span data-i18n="storage.encryptSelected">Encrypt selected</span></button>' +
  '<button type="button" class="nethub-menu-item" id="nethub-menu-restore" role="menuitem" title="Restore selected hosts" data-i18n-title="storage.restoreTitle">' +
  '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.2 7V7a2.8 2.8 0 012.8-2.8" opacity="0"/><path d="M10.8 7V5.2a2.8 2.8 0 00-5.6 0" opacity="0"/><path d="M5.2 10.5h5.6"/></svg>' +
  '<span data-i18n="storage.restoreSelected">Restore selected</span></button>');

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
function renderConfigStorage() {
  var state = _configStorageState || {};
  var locked = !!state.locked;
  var configured = !!state.initialized;
  document.getElementById('config-storage-status').textContent = locked
    ? t('storage.statusLocked')
    : (configured ? t('storage.statusOn') : t('storage.statusOff'));
  document.getElementById('config-storage-unlock').style.display = locked ? '' : 'none';
  document.getElementById('config-storage-setup').style.display = !locked && !configured ? '' : 'none';
  document.getElementById('config-storage-on').style.display = !locked && configured ? '' : 'none';
  /* Busy first, from the flag alone: `el.disabled || busy` latched forever — a
     failed submit left every field of the form disabled, so the user could not
     correct a rejected password and Cancel was dead with it. Every rule below
     then narrows that answer by state, and the next render starts over. */
  ['config-storage-unlock', 'config-storage-setup', 'config-storage-change'].forEach(function (id) {
    document.getElementById(id).querySelectorAll('input, button').forEach(function (el) { el.disabled = _configStorageBusy; });
  });
  document.getElementById('config-storage-unlock-key').disabled = _configStorageBusy || !state.password_configured;
  /* Pre-flight the machine option: if this OS has no credential store,
     disable the radio and force password protection. The user sees WHY up
     front, instead of submitting an empty password and receiving 503. */
  var machineRadio = document.getElementById('config-storage-mode-machine');
  var sysAvail = !!state.system_available;
  machineRadio.disabled = _configStorageBusy || !sysAvail;
  if (!sysAvail && machineRadio.checked) {
    document.getElementById('config-storage-mode-password').checked = true;
    document.getElementById('config-storage-setup-password-fields').style.display = '';
  }
  /* Unlock: only show the "also bind to this machine" checkbox when a machine
     credential store is actually available on this OS. */
  document.getElementById('config-storage-unlock-bind-row').style.display = sysAvail ? '' : 'none';
  document.getElementById('config-storage-mode-machine-hint').textContent =
    sysAvail ? t('storage.modeMachineHint') : t('storage.noSystemStore');
  var showChange = _configStorageChanging || !!state.rotation_pending;
  document.getElementById('config-storage-change').style.display = showChange ? '' : 'none';
  document.getElementById('config-storage-change-open').style.display = showChange ? 'none' : '';
  document.getElementById('config-storage-change-open').disabled = _configStorageBusy;
  document.getElementById('config-storage-current-label').textContent = state.rotation_pending ? t('storage.pendingPassword') : t('storage.currentPassword');
  document.getElementById('config-storage-current-password').required = true;
  document.getElementById('config-storage-current-password').disabled = _configStorageBusy || !state.password_configured;
  document.getElementById('config-storage-new-rows').style.display = state.rotation_pending ? 'none' : '';
  document.getElementById('config-storage-new-password').required = !state.rotation_pending;
  document.getElementById('config-storage-new-confirmation').required = !state.rotation_pending;
  document.getElementById('config-storage-change-submit').textContent = state.rotation_pending ? t('storage.resumePasswordChange') : t('storage.changePassword');
  /* The add-row entry key shows the state of the vault: open while no password
     is configured, closed once a key protects the store. */
  var openKey = document.getElementById('config-storage-open');
  if (openKey) {
    openKey.innerHTML = (state.initialized && state.password_configured) ? SVG_LOCK_CLOSED : SVG_LOCK_OPEN;
    openKey.classList.toggle('is-configured', !!(state.initialized && state.password_configured));
    openKey.classList.toggle('is-locked', !!state.locked);
    openKey.title = state.locked ? t('storage.clickToUnlock') : (state.initialized ? t('storage.title') : t('storage.statusOff'));
  }
  syncConfigStorageActions();
}
/* The batch menu's two encryption keys answer to the selection AND to the key:
   with no password there is nothing to encrypt with, and a key that opens a
   password panel the menu cannot explain reads as a broken action. */
function configStorageSelectedNames() {
  if (typeof _nodeBatch !== 'undefined' && _nodeBatch) return _nodeBatch.selected();
  return [];
}
function syncConfigStorageActions() {
  var encryptKey = document.getElementById('nethub-menu-encrypt');
  var restoreKey = document.getElementById('nethub-menu-restore');
  if (!encryptKey || !restoreKey) return;
  var state = _configStorageState || {};
  var byName = {};
  (window._lastConnections || []).forEach(function (c) { if (c && c.name) byName[c.name] = c; });
  var plain = 0, sealed = 0;
  configStorageSelectedNames().forEach(function (name) {
    var c = byName[name];
    if (!c) return;
    if (c.storage === 'plaintext') plain++;
    else if (c.storage === 'encrypted') sealed++;
  });
  var busy = !!state.rotation_pending;
  encryptKey.disabled = !plain || busy || !state.initialized;
  restoreKey.disabled = !sealed || busy;
  encryptKey.title = state.initialized ? t('storage.encryptTitle') : t('storage.initializeFirst');
}
function clearConfigStorageSecrets() {
  _configStorageChanging = false;
  var setupFields = document.getElementById('config-storage-setup-password-fields');
  if (setupFields) {
    var isPass = (document.getElementById('config-storage-mode-password') || {}).checked;
    setupFields.style.display = isPass ? '' : 'none';
  }
  ['config-storage-initial-password', 'config-storage-initial-confirmation', 'config-storage-unlock-key',
   'config-storage-current-password', 'config-storage-new-password', 'config-storage-new-confirmation'].forEach(function (id) {
    var input = document.getElementById(id);
    input.value = '';
    input.type = 'password';
    var eye = input.parentNode.querySelector('.psw-eye');
    if (eye) { eye.querySelector('.eye-open').style.display = ''; eye.querySelector('.eye-closed').style.display = 'none'; }
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
    document.getElementById('config-storage-unlock-key').value = '';
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
/* Converting a profile runs from the host column's batch bar: the ticked set is
   already the user's answer to "which hosts", so asking again in a modal table
   was a second list to keep in step with the first. */
function configStorageConvert(action, names) {
  names = names || [];
  var state = _configStorageState || {};
  if (typeof _nodeBatch !== 'undefined' && _nodeBatch) _nodeBatch.closeMenu();
  if (!state.initialized) {
    openConfigStorage();
    configStorageError(t('storage.initializeFirst'));
    return;
  }
  if (!names.length) { showCopyToast(t('storage.selectAtLeastOne')); return; }
  var title = action === 'encrypt' ? t('storage.encryptTitle') : t('storage.restoreTitle');
  var message = action === 'encrypt'
    ? tCount('storage.encryptConfirm.one', 'storage.encryptConfirm.other', {count: names.length})
    : tCount('storage.restoreConfirm.one', 'storage.restoreConfirm.other', {count: names.length});
  confirmDialog({title: title, message: message, okText: title}).then(function (ok) {
    if (!ok) return;
    setConfigStorageBusy(true);
    configStorageError('');
    configStorageRequest('/api/config-storage/' + action, {
      method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'}, body: JSON.stringify({names: names})
    }).then(function (response) {
      var failed = ((response && response.results) || []).filter(function (r) { return !r.ok; });
      return loadConfigStorage().then(function () {
        if (typeof loadConnections === 'function') loadConnections();
        if (typeof _nodeBatch !== 'undefined' && _nodeBatch) _nodeBatch.update();
        if (failed.length) throw new Error(failed.map(function (r) { return r.name + ': ' + (r.error || r.code); }).join('; '));
        showCopyToast(tCount('storage.converted.one', 'storage.converted.other', {count: names.length}));
      });
    }).catch(function (err) { configStorageError(configStorageFailure(err)); })
      .finally(function () { setConfigStorageBusy(false); });
  });
}
document.getElementById('config-storage-open').onclick = openConfigStorage;
document.getElementById('config-storage-close').onclick = function () { hideModal('modal-config-storage'); };
document.getElementById('nethub-menu-encrypt').onclick = function () { configStorageConvert('encrypt', configStorageSelectedNames()); };
document.getElementById('nethub-menu-restore').onclick = function () { configStorageConvert('restore', configStorageSelectedNames()); };
/* The lock on a card does what it shows: an open lock encrypts that host, a
   closed one restores it. Delegated rather than bound per card because the grid
   repaints on every connection refresh. In selection mode the click still belongs
   to the batch bar — a tick is what a card press means there. */
document.getElementById('conn-grid').addEventListener('click', function (event) {
  var mark = event.target.closest('.conn-storage-mark');
  if (!mark) return;
  if (typeof _nodeBatch !== 'undefined' && _nodeBatch && _nodeBatch.isOn()) return;
  event.stopPropagation();
  configStorageConvert(mark.getAttribute('data-storage') === 'encrypted' ? 'restore' : 'encrypt',
    [mark.getAttribute('data-conn-name')]);
});
document.querySelectorAll('#modal-config-storage .psw-eye').forEach(function (btn) { btn.onclick = function () { _toggleEye(this); }; });
/* Radio: selecting 'password' reveals the inputs; 'machine' folds them. */
document.querySelectorAll('input[name="config-storage-mode"]').forEach(function (radio) {
  radio.onchange = function () {
    var isPass = this.value === 'password' && this.checked;
    document.getElementById('config-storage-setup-password-fields').style.display = isPass ? '' : 'none';
    document.getElementById('config-storage-initial-password').required = isPass;
    document.getElementById('config-storage-initial-confirmation').required = isPass;
    if (isPass) document.getElementById('config-storage-initial-password').focus();
  };
});
/* 生成密钥: 'machine' submits empty password; 'password' submits the fields. */
document.getElementById('config-storage-setup').onsubmit = function (event) {
  event.preventDefault();
  var mode = (document.querySelector('input[name="config-storage-mode"]:checked') || {}).value;
  if (mode === 'machine') {
    submitConfigStoragePassword('initialize', {password: '', confirmation: ''});
  } else {
    submitConfigStoragePassword('initialize', {
      password: document.getElementById('config-storage-initial-password').value,
      confirmation: document.getElementById('config-storage-initial-confirmation').value
    });
  }
};
/* 解锁: credential entered; optionally bind to this machine so future starts are quiet. */
document.getElementById('config-storage-unlock').onsubmit = function (event) {
  event.preventDefault();
  var bind = document.getElementById('config-storage-unlock-bind').checked;
  var action = bind ? 'recover' : 'unlock';
  submitConfigStoragePassword(action, {password: document.getElementById('config-storage-unlock-key').value});
};
/* 上锁: one click, drops the key from memory. Encrypted hosts stop being readable. */
document.getElementById('config-storage-lock').onclick = function () {
  if (_configStorageBusy) return;
  setConfigStorageBusy(true);
  configStorageError('');
  configStorageRequest('/api/config-storage/lock', {method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'}, body: '{}'})
    .then(function (state) {
      _configStorageState = state;
      clearConfigStorageSecrets();
      if (typeof loadConnections === 'function') loadConnections();
      showCopyToast(t('storage.locked'));
    })
    .catch(function (err) { configStorageError(configStorageFailure(err)); })
    .finally(function () { setConfigStorageBusy(false); });
};
/* 删除密钥: requires confirmation. Key is deleted, encrypted profiles stay on disk. */
function promptDestroyKey() {
  if (_configStorageBusy) return;
  confirmDialog({
    title: t('storage.destroyTitle'),
    message: t('storage.destroyConfirm'),
    okText: t('storage.destroy'),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    setConfigStorageBusy(true);
    configStorageError('');
    configStorageRequest('/api/config-storage/destroy', {method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'}, body: '{}'})
      .then(function (state) {
        _configStorageState = state;
        clearConfigStorageSecrets();
        if (typeof loadConnections === 'function') loadConnections();
        showCopyToast(t('storage.destroyed'));
      })
      .catch(function (err) { configStorageError(configStorageFailure(err)); })
      .finally(function () { setConfigStorageBusy(false); });
  });
}
document.getElementById('config-storage-destroy').onclick = promptDestroyKey;
document.getElementById('config-storage-destroy-locked').onclick = promptDestroyKey;
document.getElementById('config-storage-change').onsubmit = function (event) {
  event.preventDefault();
  var pending = _configStorageState && _configStorageState.rotation_pending;
  submitConfigStoragePassword('password', {
    current_password: document.getElementById('config-storage-current-password').value,
    new_password: pending ? '' : document.getElementById('config-storage-new-password').value,
    confirmation: pending ? '' : document.getElementById('config-storage-new-confirmation').value
  });
};
/* Turning it off is one action. Every encrypted profile goes back to plaintext
   first, then the keyring and catalog are removed server-side — the user is never
   asked to delete a key file to make the button work. */
document.getElementById('config-storage-change-open').onclick = function () {
  _configStorageChanging = true;
  configStorageError('');
  renderConfigStorage();
  document.getElementById('config-storage-current-password').focus();
};
document.getElementById('config-storage-change-cancel').onclick = function () { clearConfigStorageSecrets(); renderConfigStorage(); };
new MutationObserver(function () {
  if (document.getElementById('modal-config-storage').classList.contains('hidden')) clearConfigStorageSecrets();
}).observe(document.getElementById('modal-config-storage'), {attributes: true, attributeFilter: ['class']});
window.addEventListener('pagehide', clearConfigStorageSecrets);
onLangChange(function () { if (_configStorageState) renderConfigStorage(); });
loadConfigStorage().then(function () {
  if (_configStorageState.locked) showModal('modal-config-storage');
}).catch(function () {});