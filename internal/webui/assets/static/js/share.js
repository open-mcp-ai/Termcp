/* Sharing a host with another person, through whatever chat they already use.
 *
 * The sender never hands over a key: the profile is sealed to the recipient's
 * public key, so the blob is useless to anyone else, including the chat server.
 * The recipient's side is one paste: the profile lands in their list, validated
 * like any other paste and named after what the sender called it.
 *
 * The dialog owns both directions because they are one thought — "move a host to
 * another person". Sending opens from the batch menu on the ticked host; receiving
 * opens from the keychain toolbar, since a received host needs no host to start
 * from and lands on whichever key its address named. */
var _shareMode = 'export';   /* 'export' | 'import' */
var _shareBusy = false;

var _shareRoot = document.createElement('div');
_shareRoot.id = 'share-root';
document.body.appendChild(_shareRoot);
_shareRoot.innerHTML = [
  '<div id="modal-share" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:520px"><div class="modal-h">',
  '<h2 id="share-title" data-i18n="share.titleExport">Share a host</h2>',
  '<button type="button" id="share-close" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="share-err" class="err" style="display:none"></p>',
  /* Both directions need a second party's text, and both texts are long: one
     textarea serves them, labelled by whichever direction is open. */
  '<div class="conn-field"><label for="share-input" id="share-input-label" data-i18n="share.recipientLabel">Their public key</label>',
  '<textarea id="share-input" rows="4" spellcheck="false" autocomplete="off"></textarea></div>',
  '<div id="share-out-row" style="display:none">',
  '<p class="conn-field-hint" data-i18n="share.outHint">Send this text to the other person. Only their key can open it.</p>',
  '<textarea id="share-out" rows="5" readonly spellcheck="false"></textarea>',
  '<div class="modal-actions"><button type="button" class="btn btn-primary" id="share-out-copy" data-i18n="share.copyBlob">Copy share text</button></div>',
  '</div>',
  '<div class="modal-actions" id="share-actions">',
  '<button type="button" class="btn" id="share-cancel" data-i18n="common.cancel">Cancel</button>',
  '<button type="button" class="btn btn-primary" id="share-go" data-i18n="share.create">Create share text</button>',
  '</div>',
  '</div></div></div>',
].join('');
if (typeof applyI18n === 'function') applyI18n(_shareRoot);

/* The batch menu owns "act on the ticked host", so SENDING one sits beside encrypt
   and restore. Receiving is not a host action at all — the address is the thing
   handed out — so it lives in the keychain panel's toolbar, where the keys are. */
document.getElementById('nethub-menu-delete').insertAdjacentHTML('beforebegin',
  /* `data-i18n` on the button would make a language switch write `textContent` over
   the glyph beside the words; the label rides the tooltip and the words live in the
   span. Same rule as the keychain's own icon buttons. */
  '<button type="button" class="nethub-menu-item" id="nethub-menu-share" role="menuitem" data-i18n-title="share.menuShare">' +
  '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="3.5" r="2"/><circle cx="4" cy="8" r="2"/><circle cx="12" cy="12.5" r="2"/><path d="M6 7l4-2.4M6 9l4 2.4"/></svg>' +
  '<span data-i18n="share.shareSelected">Share selected host</span></button>');
function shareError(message) {
  var el = document.getElementById('share-err');
  el.textContent = String(message || '');
  el.style.display = message ? 'block' : 'none';
}

function shareRequest(path, body) {
  return fetch(apiPath(path), {
    method: body === undefined ? 'GET' : 'POST',
    headers: {'Content-Type': 'application/json; charset=utf-8'},
    body: body === undefined ? undefined : JSON.stringify(body)
  }).then(function (r) {
    return r.text().then(function (text) {
      var parsed = null;
      try { parsed = text ? JSON.parse(text) : null; } catch (ignored) {}
      if (!r.ok) {
        var error = new Error(parsed && parsed.error ? parsed.error : (text || ('HTTP ' + r.status)));
        error.code = parsed && parsed.code;
        throw error;
      }
      return parsed || {};
    });
  });
}

function shareFailure(err) {
  switch (err && err.code) {
    case 'share_recipient_invalid': return t('share.badRecipient');
    case 'share_recipient_required': return t('share.recipientRequired');
    case 'share_blob_invalid': return t('share.badBlob');
    case 'share_wrong_key': return t('share.wrongKey');
    case 'key_locked': return t('share.locked');
    case 'config_initialization_required': return t('share.notInitialized');
    default:
      if (err && err.message) console.warn('share:', err.message);
      return err && err.code ? t('storage.failedWithCode', {code: err.code}) : t('storage.failed');
  }
}

function shareSetMode(mode) {
  _shareMode = mode;
  var exporting = mode === 'export';
  document.getElementById('share-title').textContent = exporting ? t('share.titleExport') : t('share.titleImport');
  document.getElementById('share-input-label').textContent = exporting ? t('share.recipientLabel') : t('share.blobLabel');
  document.getElementById('share-go').textContent = exporting ? t('share.create') : t('share.import');
  /* An address belongs to a KEY, and this dialog is about one HOST: which key
     protects it is already decided, and the address for it lives in that key's own
     view. */
  document.getElementById('share-out-row').style.display = 'none';
  document.getElementById('share-actions').style.display = '';
  document.getElementById('share-input').value = '';
  document.getElementById('share-input').placeholder = exporting ? '' : 'termcp-host-v1:';
  shareError('');
}

function shareOpen(mode) {
  shareSetMode(mode);
  document.getElementById('share-input').disabled = _shareBusy;
  showModal('modal-share');
  try { document.getElementById('share-input').focus(); } catch (e) {}
}

function shareCreateBlob() {
  var names = (typeof configStorageSelectedNames === 'function') ? configStorageSelectedNames() : [];
  if (names.length !== 1) {
    shareError(t('share.selectOne'));
    return;
  }
  var recipient = document.getElementById('share-input').value.trim();
  if (!recipient) {
    shareError(t('share.recipientRequired'));
    return;
  }
  _shareBusy = true;
  document.getElementById('share-go').disabled = true;
  shareError('');
  shareRequest('/api/share/export', {name: names[0], recipient: recipient})
    .then(function (body) {
      document.getElementById('share-out').value = body.blob || '';
      document.getElementById('share-out-row').style.display = '';
      document.getElementById('share-actions').style.display = 'none';
    })
    .catch(function (err) { shareError(shareFailure(err)); })
    .finally(function () { _shareBusy = false; document.getElementById('share-go').disabled = false; });
}

function shareImport() {
  var blob = document.getElementById('share-input').value.trim();
  if (!blob) {
    shareError(t('share.blobRequired'));
    return;
  }
  _shareBusy = true;
  document.getElementById('share-go').disabled = true;
  shareError('');
  shareRequest('/api/share/import', {blob: blob})
    .then(function (body) {
      hideModal('modal-share');
      /* The imported host is sealed to whichever key its address named, so both
         the host list and the keychain panel are stale: refresh both, or the
         panel reports a host count that no longer matches. */
      if (typeof loadConfigStorage === 'function') loadConfigStorage();
      if (typeof loadConfigStorageKeys === 'function') loadConfigStorageKeys();
      if (typeof loadConnections === 'function') loadConnections();
      showCopyToast(t('share.imported', {name: body.name}));
    })
    .catch(function (err) { shareError(shareFailure(err)); })
    .finally(function () { _shareBusy = false; document.getElementById('share-go').disabled = false; });
}

document.getElementById('share-go').onclick = function () {
  if (_shareBusy) return;
  if (_shareMode === 'export') shareCreateBlob();
  else shareImport();
};
document.getElementById('share-cancel').onclick = function () { hideModal('modal-share'); };
document.getElementById('share-close').onclick = function () { hideModal('modal-share'); };
document.getElementById('share-out-copy').onclick = function () {
  copyTextToClipboard(document.getElementById('share-out').value);
  showCopyToast(t('share.copiedBlob'));
};

var _shareMenu = document.getElementById('nethub-menu-share');
if (_shareMenu) _shareMenu.onclick = function () { shareOpen('export'); };
