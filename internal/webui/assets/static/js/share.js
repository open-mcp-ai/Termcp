/* Exporting a host from here, and receiving a host somebody else exported.
 *
 * What leaves the machine is always the same thing — the profile's own TOML — so
 * "export" and "share" were never two features, only two answers to one question:
 * who may open what is leaving? The dialog asks that once, and the answer decides
 * the transport:
 *
 *   plain     a TOML file; anyone who opens it reads the credentials
 *   password  the same file sealed with configcrypt (TERMCP-ENC 1); it comes back
 *             through the import dialog with the passphrase typed in
 *   key       sealed to the recipient's public key with an ephemeral X25519
 *             exchange; it leaves as one line of text for whatever chat they
 *             already use, and comes back through the keychain's receive button
 *
 * The two file-shaped answers are the export route with or without its password
 * header; the text-shaped one is the hostshare route. Three answers, three existing
 * routes, one dialog.
 *
 * The receiving half is in the same dialog because it is the same thought read
 * backwards: the text arrives in a chat window, so the person pastes it here, and
 * the profile lands on whichever key its address named. A received profile is
 * untrusted input — a blob proves only that its author had this store's public key,
 * which anyone can obtain — so it is validated like any other paste and named after
 * what the sender called it. */
var _shareMode = 'export';   /* 'export' | 'import' */
var _shareBusy = false;

var _shareRoot = document.createElement('div');
_shareRoot.id = 'share-root';
document.body.appendChild(_shareRoot);
_shareRoot.innerHTML = [
  '<div id="modal-share" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:520px"><div class="modal-h">',
  '<h2 id="share-title" data-i18n="share.titleExport">Export or share a host</h2>',
  '<button type="button" id="share-close" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="share-err" class="err" style="display:none"></p>',
  /* Sending: what is ticked, and who may open it. A <select> rather than a row of
     radios — there is no radio styling in the sheet, and three options already
     read as one choice. */
  '<div id="share-export-fields">',
  '<p class="conn-field-hint" id="share-count"></p>',
  '<div class="conn-field"><label for="share-protect" data-i18n="share.protect">Who can open it</label>',
  '<select id="share-protect">',
  '<option value="plain" data-i18n="share.protectPlain">Anyone — a plain TOML file</option>',
  '<option value="password" data-i18n="share.protectPassword">Whoever I give a passphrase to</option>',
  '<option value="key" data-i18n="share.protectKey">Only their keychain</option>',
  '</select>',
  '<p class="conn-check-hint" id="share-protect-hint"></p></div>',
  '<div id="share-pass-row" style="display:none">',
  '<div class="conn-field"><label for="share-pass" data-i18n="share.passLabel">Passphrase</label>',
  '<input type="password" id="share-pass" autocomplete="new-password"></div>',
  /* Twice, because the file cannot be checked here: encryption succeeds whatever is
     typed, and a mistyped passphrase is discovered on the other machine, where the
     only fix is to go back and export again. */
  '<div class="conn-field"><label for="share-pass2" data-i18n="share.passConfirmLabel">Type it again</label>',
  '<input type="password" id="share-pass2" autocomplete="new-password"></div>',
  '</div>',
  '</div>',
  /* Both directions need a second party's text, and both texts are long: one
     textarea serves them, labelled by whichever direction is open. */
  '<div class="conn-field" id="share-input-wrap" style="display:none">',
  '<label for="share-input" id="share-input-label" data-i18n="share.recipientLabel">Their public key</label>',
  '<textarea id="share-input" rows="4" spellcheck="false" autocomplete="off"></textarea></div>',
  '<div id="share-out-row" style="display:none">',
  '<p class="conn-field-hint" data-i18n="share.outHint">Send this text to the other person. Only their key can open it.</p>',
  '<textarea id="share-out" rows="5" readonly spellcheck="false"></textarea>',
  '<div class="modal-actions"><button type="button" class="btn btn-primary" id="share-out-copy" data-i18n="share.copyBlob">Copy share text</button></div>',
  '</div>',
  '<div class="modal-actions" id="share-actions">',
  '<button type="button" class="btn" id="share-cancel" data-i18n="common.cancel">Cancel</button>',
  /* No data-i18n here: the label follows the protection, and a language switch
     repaints it through sharePaint instead of resetting it to one mode's words. */
  '<button type="button" class="btn btn-primary" id="share-go">Download</button>',
  '</div>',
  '</div></div></div>',
].join('');
if (typeof applyI18n === 'function') applyI18n(_shareRoot);

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

/* The ticked hosts. Read live rather than captured at open: the dialog is modal, so
   the ticks cannot move under it, and a copy would be one more thing to keep in
   step with the batch bar. */
function shareSelected() {
  return (typeof configStorageSelectedNames === 'function') ? configStorageSelectedNames() : [];
}

function shareProtection() {
  var sel = document.getElementById('share-protect');
  return sel ? sel.value : 'plain';
}

/* Paint the mode-dependent half of the dialog: which fields exist, what each verb
   says, and the sentence that explains the choice. Called on open, on a protection
   change, and after a language switch — it deliberately does not touch the input
   text or the produced blob, which shareReset and shareCreate own. */
function sharePaint() {
  var importing = _shareMode === 'import';
  var prot = importing ? 'plain' : shareProtection();
  var names = importing ? [] : shareSelected();

  document.getElementById('share-title').textContent = importing ? t('share.titleImport') : t('share.titleExport');
  document.getElementById('share-export-fields').style.display = importing ? 'none' : '';
  document.getElementById('share-count').textContent = names.length
    ? tCount('share.selected.one', 'share.selected.other', { count: names.length })
    : t('share.selectOne');

  document.getElementById('share-pass-row').style.display = (!importing && prot === 'password') ? '' : 'none';
  var wantsText = importing || prot === 'key';
  document.getElementById('share-input-wrap').style.display = wantsText ? '' : 'none';
  document.getElementById('share-input-label').textContent = importing ? t('share.blobLabel') : t('share.recipientLabel');
  document.getElementById('share-input').placeholder = importing ? 'termcp-host-v1:' : '';

  var hint;
  switch (prot) {
    case 'password': hint = t('share.hintPassword'); break;
    case 'key': hint = t('share.hintKey'); break;
    default: hint = t('share.hintPlain');
  }
  document.getElementById('share-protect-hint').textContent = importing ? '' : hint;

  var go = document.getElementById('share-go');
  if (importing) go.textContent = t('share.import');
  else if (prot === 'key') go.textContent = t('share.create');
  else if (prot === 'password') go.textContent = t('share.downloadSealed');
  else go.textContent = t('share.download');
}

/* Everything a previous visit left behind: the typed text, the error, and a share
   blob that belongs to the host it was made from, not to the one about to be. */
function shareReset() {
  document.getElementById('share-input').value = '';
  document.getElementById('share-out').value = '';
  document.getElementById('share-out-row').style.display = 'none';
  document.getElementById('share-actions').style.display = '';
  document.getElementById('share-pass').value = '';
  document.getElementById('share-pass2').value = '';
  document.getElementById('share-protect').value = 'plain';
  document.getElementById('share-go').disabled = _shareBusy;
  shareError('');
}

function shareOpen(mode) {
  _shareMode = mode === 'import' ? 'import' : 'export';
  shareReset();
  sharePaint();
  showModal('modal-share');
  try {
    var focus = _shareMode === 'import' ? document.getElementById('share-input') : document.getElementById('share-protect');
    focus.focus();
  } catch (e) {}
}

/* The public-key half: seal ONE profile to the recipient and hand back the text.
   The single-host rule is hostshare's, not a UI preference — the profile name is
   bound into the envelope so a blob cannot be relabelled, which makes one blob one
   host. */
function shareCreateBlob(names) {
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

/* The file half: one route, plain or sealed. The passphrase is typed twice before
   the request, because nothing on this side can tell a typo from a passphrase. */
function shareDownload(names) {
  var password = '';
  if (shareProtection() === 'password') {
    password = String(document.getElementById('share-pass').value || '');
    if (!password) { shareError(t('share.passRequired')); return; }
    if (password !== String(document.getElementById('share-pass2').value || '')) {
      shareError(t('share.passMismatch'));
      return;
    }
  }
  _shareBusy = true;
  document.getElementById('share-go').disabled = true;
  shareError('');
  downloadConnectionsToml(names, password)
    .then(function () {
      hideModal('modal-share');
      showCopyToast(t('share.exported', { count: names.length }));
    })
    .catch(function (err) { shareError(t('conn.batch.exportFailed', { msg: String(err.message || err) })); })
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

document.getElementById('share-protect').onchange = function () { sharePaint(); };
document.getElementById('share-go').onclick = function () {
  if (_shareBusy) return;
  if (_shareMode === 'import') { shareImport(); return; }
  var names = shareSelected();
  if (!names.length) { shareError(t('share.selectOne')); return; }
  /* The built-in profile is not a profile that can leave: the server refuses it on
     both routes, so the refusal belongs here, before a request that can only
     partly succeed. */
  if (selectionHasInternal(names)) { showCopyToast(t('nethub.batch.exportInternal')); return; }
  if (shareProtection() === 'key') {
    if (names.length !== 1) { shareError(t('share.selectOne')); return; }
    shareCreateBlob(names);
    return;
  }
  shareDownload(names);
};
document.getElementById('share-cancel').onclick = function () { hideModal('modal-share'); };
document.getElementById('share-close').onclick = function () { hideModal('modal-share'); };
document.getElementById('share-out-copy').onclick = function () {
  copyTextToClipboard(document.getElementById('share-out').value);
  showCopyToast(t('share.copiedBlob'));
};

/* A language switch has to repaint the verb and the hint, which are chosen from the
   current protection rather than marked in the markup. */
if (typeof onLangChange === 'function') onLangChange(sharePaint);
