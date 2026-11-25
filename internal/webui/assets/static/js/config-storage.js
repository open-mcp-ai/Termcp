/* Configuration at rest: the keychain, and the two valves it controls.
   Two independent switches, and the panel shows them as two:

     key inserted / removed    main material in memory (persisted as `plugged`)
     host activated / inactive the second layer: a passphrase-derived key for a key
                               that has one, an authenticated boolean for one that
                               does not

   There is no "default key" and nothing is preselected: which key seals a new host
   is that host's own choice, so this panel is a list of independent keys. Converting
   and activating belong to the host column's batch bar — the same ticked set that
   dials, exports and deletes — so a selection is never repeated inside this modal. */
var _configStorageState = null;
var _configStorageKeys = [];
var _configStorageBusy = false;
/* What the key list is filtered by. Kept at module scope so a repaint (a key
   inserted, a request failing) does not silently clear what was typed. */
var _keyFilter = '';
/* Which keys are ticked. Selection is how a keychain is handled in bulk, so it
   is module state that survives a repaint and a busy pass. */
var _keySelected = {};
/* Which key the detail view is showing, and the words for it. They live at module
   scope because the busy pass repaints the view and must not lose them. */
/* The form's state: which key it is, which mode, and the words it has fetched.
   Module scope because a busy pass repaints the form and must not lose them. */
var _keyFormMode = 'new';
var _keyFormID = '';
var _keyFormWords = '';
var _keyFormRevealed = false;
/* The words panel's verbs are icons. Three words of text beside a heading that
   already says "recovery phrase" is three labels for one panel; each keeps its word
   as a tooltip and an aria-label, which is where a label belongs when the glyph is
   unambiguous at 15px. */
var KEY_GEAR_EYE = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1.6 8S4 4.2 8 4.2 14.4 8 14.4 8 12 11.8 8 11.8 1.6 8 1.6 8z"/><circle cx="8" cy="8" r="2" fill="none"/></svg>';
var KEY_GEAR_EYE_SHUT = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1.6 8S4 4.2 8 4.2 14.4 8 14.4 8 12 11.8 8 11.8 1.6 8 1.6 8z"/><circle cx="8" cy="8" r="2" fill="none"/><path d="M2.6 13.4L13.4 2.6"/></svg>';
var KEY_GEAR_COPY = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="5.6" y="5.6" width="7.8" height="7.8" rx="1.5"/><path d="M10.4 5.6V3.9A1.4 1.4 0 009 2.5H3.9A1.4 1.4 0 002.5 3.9V9a1.4 1.4 0 001.4 1.4h1.7"/></svg>';
var KEY_GEAR_DOWNLOAD = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 2v8M5 7.2L8 10.2l3-3M3 12.5h10"/></svg>';
/* One plug/unplug request at a time. Separate from _configStorageBusy on purpose:
   that flag repaints the whole panel disabled, which a toggle must not do. */
var _keyActionInFlight = false;
/* What the rows currently show, so an identical payload does not repaint them. */
var _configStorageKeysFingerprint = '';

var _configStorageRoot = document.createElement('div');
_configStorageRoot.id = 'config-storage-root';
document.body.appendChild(_configStorageRoot);
_configStorageRoot.innerHTML = [
  '<div id="modal-config-storage" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:560px"><div class="modal-h cs-title-bar">',
  '<h2 data-i18n="storage.title">Keychain</h2>',
  /* The tick sits beside the name it applies to, and it has no word of its own:
     a tri-state box IS the verb (tick all, untick all, or a dash for "some"),
     and the tooltip names it for anyone who wants the word. */
  '<input type="checkbox" class="cs-tick-all" id="config-storage-key-select-all" title="Invert" data-i18n-title="storage.keySelectAll">',
  '<span id="config-storage-status" class="cs-title-note"></span>',
  '<span class="cs-list-tools">',
  /* Delete first, and red: it is the one action on this row that cannot be undone,
     so it is the one that should never be mistaken for the other three. It is
     disabled until something is ticked, because deleting nothing is not an
     action and a live button that does nothing is a lie. */
  '<button type="button" class="cs-tool cs-tool-danger" id="config-storage-key-sel-delete" data-i18n-title="storage.keyRemove" title="Delete" disabled><svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 4.5h11M6 4.5V3h4v1.5M4 4.5l.7 9h6.6l.7-9"/></svg></button>',
  '<button type="button" class="cs-tool" id="config-storage-key-backup" data-i18n-title="storage.keyExportAction" title="Export keys" disabled><svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 2v8M5 7.2L8 10.2l3-3M3 12.5h10"/></svg></button>',
  '<button type="button" class="cs-tool" id="config-storage-key-import" data-i18n-title="storage.keyImportAction" title="Import keys"><svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 13V5M5 8.2L8 5.2l3 3M3 3.5h10"/></svg></button>',
  '<button type="button" class="cs-tool is-primary" id="config-storage-key-add" data-i18n-title="storage.keyAdd" title="Add key"><svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true"><path d="M8 3.5v9M3.5 8h9"/></svg></button>',
  '</span>',
  '<button type="button" id="config-storage-close" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="config-storage-err" class="err" style="display:none"></p>',
  '<div id="config-storage-keys-view">',
  /* A filter, not a search: the list is already here, and past a dozen keys the
     question is "where is the one I mean", which typing part of its name answers.
     It is the only thing left in this row: the tick and the keychain's actions
     moved up to the title bar, where the title already was. */
  '<div class="cs-list-head">',
  '<input type="search" class="cs-filter" id="config-storage-key-filter" autocomplete="off" spellcheck="false" data-i18n-placeholder="storage.keyFilter">',
  '</div>',
  /* A keychain nothing can open is not an empty keychain, so it does not get the
     empty state — it gets this, in the list's own place. The way out lives here
     rather than in the toolbar: a destructive, once-in-a-lifetime action has no
     business sitting beside "Add key", and this is how the products that own this
     problem do it — the failure takes over the view and carries its own exit
     (Keychain Access' "Reset to Defaults", Bitwarden's undecryptable-vault state,
     KeePassXC's corrupt-database dialog). */
  '<div class="cs-damaged" id="config-storage-key-damaged" style="display:none">',
  '<p class="cs-damaged-h" data-i18n="storage.keyDamagedTitle">This keychain cannot be opened</p>',
  '<p class="cs-damaged-b" id="config-storage-key-damaged-why"></p>',
  '<div class="cs-damaged-actions">',
  '<button type="button" class="btn btn-danger" id="config-storage-key-discard" data-i18n="storage.keyDiscard">Delete the unreadable keychain</button>',
  '</div></div>',
  '<ul class="cs-keys" id="config-storage-key-list"></ul>',
  '</div>',
  '</div></div></div>',
  /* ---- Modal: Key Detail Sheet --------------------------------------------
     Opened by clicking a row. Name editing, public key copy, words folded by
     default, rotation, and PER-KEY HOST ACTIONS: receiving a host on THIS key,
     or exporting a host sealed by this key. */
  '<div id="modal-key-form" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:520px"><div class="modal-h">',
  '<h2 id="key-form-title"></h2>',
  '<button type="button" id="key-form-x" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="key-form-err" class="err" style="display:none"></p>',
  '<div class="cs-name-row"><label for="key-form-name" data-i18n="storage.keyNameLabel">Name</label>',
  '<input type="text" id="key-form-name" maxlength="64" spellcheck="false" autocomplete="off"></div>',
  /* One form, two states. `new` asks for the words (typed or generated) and an
     optional passphrase; `edit` shows what the key already is. Reusing the form
     is not an aesthetic choice: both states are "the fields of a key", and two
     copies drifted the moment one gained a rule (name required). */
  '<div class="cs-form-block" id="key-form-words-input-block">',
  '<div class="cs-form-head"><p class="cs-form-h" data-i18n="storage.keyWordsTitle">Recovery phrase</p>',
  '<button type="button" class="btn" id="key-form-generate" data-i18n="storage.keyGenerate">Generate</button></div>',
  '<textarea id="key-form-words-input" rows="3" spellcheck="false" autocomplete="off" autocapitalize="off" placeholder="word word word …"></textarea>',
  '</div>',
  '<div class="cs-form-block" id="key-form-pass-block" style="display:none">',
  '<div class="cs-name-row"><label for="key-form-pass" data-i18n="storage.keyPassLabel">Passphrase</label>',
  '<input type="password" id="key-form-pass" maxlength="256" spellcheck="false" autocomplete="new-password"></div>',
  '</div>',
  /* One line of fact, and it is about THIS key: how many hosts it seals. What a
     key does after a restart, and what changing its passphrase would cost, are
     answers to questions asked once — they belong in the docs, not above a form
     that is opened every time someone renames a key. */
  '<p class="cs-fact-line" id="key-form-hosts-summary"></p>',
  '<div class="cs-form-block" id="key-form-pub-block">',
  '<div class="cs-form-head"><p class="cs-form-h" data-i18n="storage.keyPublicTitle">Public key</p>',
  '<div class="cs-form-actions">',
  '<button type="button" class="cs-tool" id="key-form-pub-copy" title="Copy the address" data-i18n-title="storage.keyAddressCopy" aria-label="Copy the address" data-i18n-aria="storage.keyAddressCopy">' + KEY_GEAR_COPY + '</button>',
    /* Two glyphs, one shape: importing a host is the mirror of copying the address, so
     it wears the same box. Its word lives in the tooltip — `data-i18n` on the button
     would set `textContent` and delete the glyph. */
  '<button type="button" class="cs-tool" id="key-form-receive" title="Import host ciphertext" data-i18n-title="storage.keyImportHostCiphertext" aria-label="Import host ciphertext" data-i18n-aria="storage.keyImportHostCiphertext"><svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 2.5v7M5.2 7L8 9.8 10.8 7M3 13h10"/></svg></button>',
  '</div></div>',
  '<div class="cs-pub" id="key-form-pub" role="button" tabindex="0" data-i18n-title="storage.keyAddressCopy"><code id="key-form-pub-value"></code></div>',
  '<p class="conn-field-hint" data-i18n="share.mineHint">Give this to whoever is sending you a host: they encrypt it and send the text back. It is not a secret.</p>',
  '</div>',
  /* The words block: the verbs sit on the heading row, because they act on the
     panel below them and a second row of buttons would be a second thing to read
     under a title that already says what this is. */
  '<div class="cs-form-block" id="key-form-words-block">',
  '<div class="cs-form-head"><p class="cs-form-h" data-i18n="storage.keyWordsTitle">Recovery phrase</p>',
  '<div class="cs-form-actions">',
  '<button type="button" class="cs-tool" id="key-form-reveal" title="Show" data-i18n-title="storage.keyReveal" aria-label="Show" data-i18n-aria="storage.keyReveal">' + KEY_GEAR_EYE + '</button>',
  '<button type="button" class="cs-tool" id="key-form-copy" title="Copy" data-i18n-title="share.copy" aria-label="Copy" data-i18n-aria="share.copy">' + KEY_GEAR_COPY + '</button>',
  '<button type="button" class="cs-tool" id="key-form-export" title="Export" data-i18n-title="storage.keyExportFile" aria-label="Export" data-i18n-aria="storage.keyExportFile">' + KEY_GEAR_DOWNLOAD + '</button>',
  '</div></div>',
  '<ol class="cs-phrase" id="key-form-phrase"></ol>',
  '</div>',
  /* The footer holds the key's own fate: what replaces it, what destroys it, then
     a divider and the ordinary dialog verbs. Two destructive buttons side by side
     are a pair; one stacked over the other is an accident. */
  '<div class="modal-actions cs-footer">',
  '<button type="button" class="btn" id="key-form-passphrase" data-i18n="storage.keyPassChangeAction">Change passphrase</button>',
  '<button type="button" class="btn btn-danger" id="key-form-rotate" data-i18n="storage.keyRotate">Rotate</button>',
  '<button type="button" class="btn btn-danger" id="key-form-delete" data-i18n="storage.keyRemove">Delete</button>',
  '<span class="cs-detail-spacer"></span>',
  '<button type="button" class="btn" id="key-form-cancel" data-i18n="common.cancel">Cancel</button>',
  '<button type="button" class="btn btn-primary" id="key-form-primary" data-i18n="storage.keySaveName">Save name</button>',
  '</div>',
  '</div></div></div>',

  /* ---- Modal: Import Keys (global tool) -------------------------------------
     Imports KEY words from a backup file or pasted text. */
  '<div id="modal-key-import" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:480px"><div class="modal-h">',
  '<h2 data-i18n="storage.keyImportKeysTitle">Import keys</h2>',
  '<button type="button" id="key-import-x" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="key-import-err" class="err" style="display:none"></p>',
  '<textarea id="key-import-text" rows="5" spellcheck="false" autocomplete="off" placeholder="name / word word word …"></textarea>',
  /* The picker shares the footer with the verbs: it is how the box gets filled, not
     a decision, so it sits on the left of the row the decisions end. */
  '<div class="modal-actions cs-actions-split">',
  '<button type="button" class="btn" id="key-import-file" data-i18n="storage.keyImportFile">Choose a backup file</button>',
  '<input type="file" id="key-import-file-input" class="cs-file-input" accept=".txt,text/plain">',
  '<button type="button" class="btn cs-actions-push" id="key-import-cancel" data-i18n="common.cancel">Cancel</button>',
  '<button type="button" class="btn btn-primary" id="key-import-go" data-i18n="storage.keyImportKeysDo">Import</button>',
  '</div></div></div></div>',

/* Which key seals the selection. A host's key is a property of that host, so the
     server refuses to guess when the keychain holds more than one — which meant the
     lock on a host card did nothing but show an error. This is that missing
     question, asked once for the whole selection because the batch IS one action. */
  '<div id="modal-pick-key" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:440px"><div class="modal-h"><h2 id="pick-key-title"></h2>',
  '<button type="button" id="pick-key-x" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="pick-key-err" class="err" style="display:none"></p>',
  '<div id="pick-key-list" class="cs-pick"></div>',
  '<div class="modal-actions"><button type="button" class="btn btn-primary" id="pick-key-ok" data-i18n="storage.keyPickOk">Use this key</button></div>',
  '</div></div></div>',
  /* One box, one secret, no storage: the only thing this dialog can do is hand a
     passphrase back to the caller for the length of one request. */
  '<div id="modal-secret" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:420px"><div class="modal-h"><h2 id="secret-title"></h2>',
  '<button type="button" id="secret-x" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p class="conn-field-hint" id="secret-message"></p>',
  '<div class="conn-field"><label for="secret-input" id="secret-label" data-i18n="storage.keyPassLabel">Passphrase</label>',
  '<input type="password" id="secret-input" maxlength="256" spellcheck="false" autocomplete="new-password"></div>',
  /* Shown only when the caller asks for two answers — changing a passphrase needs the
     old one and the new one, and two separate prompts would be two chances to lose
     what was typed in the first. */
  '<div class="conn-field" id="secret-second-field" style="display:none"><label for="secret-input-2" id="secret-label-2" data-i18n="storage.keyPassNewLabel">New passphrase</label>',
  '<input type="password" id="secret-input-2" maxlength="256" spellcheck="false" autocomplete="new-password"></div>',
  '<div class="modal-actions"><button type="button" class="btn" id="secret-cancel" data-i18n="common.cancel">Cancel</button>',
  '<button type="button" class="btn btn-primary" id="secret-ok" data-i18n="common.done">Done</button></div>',
  '</div></div></div>',
  /* One dialog for the passphrase side of activating: a key per box, never a host
     per box, because a passphrase belongs to a KEY (§8.5). */
  '<div id="modal-activate" class="modal-backdrop hidden" role="dialog" aria-modal="true">',
  '<div class="modal" style="max-width:440px"><div class="modal-h"><h2 id="modal-activate-title"></h2>',
  '<button type="button" id="modal-activate-close" class="modal-x" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" d="M2 2l8 8M10 2L2 10"/></svg></button>',
  '</div><div class="modal-body">',
  '<p id="modal-activate-err" class="err" style="display:none"></p>',
  '<p class="conn-field-hint" id="modal-activate-hint"></p>',
  '<div id="modal-activate-fields"></div>',
  '<div class="modal-actions"><button type="button" class="btn" id="modal-activate-cancel" data-i18n="common.cancel">Cancel</button>',
  '<button type="button" class="btn btn-primary" id="modal-activate-ok" data-i18n="common.done">Done</button></div>',
  '</div></div></div>',
].join('');

/* The panel is built and appended AFTER i18n.js has already run its first pass
   over the document. Its data-i18n attributes would therefore sit unapplied
   until some later language switch, which is exactly how a dialog ends up half
   translated. Translate the new subtree as soon as it exists. */
if (typeof applyI18n === 'function') applyI18n(_configStorageRoot);

/* The host column's key sits with the other add keys, built here rather than in
   index.html because that shell has a markup budget (TestIndexHTMLIsThin) and
   this module owns when the control exists at all. The batch menu gains the four
   per-host actions next to export and delete: they act on the same ticked set. */
document.getElementById('conn-import-open').insertAdjacentHTML('beforebegin',
  '<button type="button" class="drawer-add drawer-import" id="config-storage-open" title="Keychain" data-i18n-title="storage.open" aria-label="Keychain" data-i18n-aria="storage.open">' +
  '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="4" cy="4" r="2.1"/><path d="M5.6 5.4l6.2 6.2"/><path d="M11.8 11.6v2.2h-2.2"/><path d="M9.2 9l-1.6 1.6"/></svg></button>');
document.getElementById('nethub-menu-delete').insertAdjacentHTML('beforebegin',
  '<button type="button" class="nethub-menu-item" id="nethub-menu-encrypt" role="menuitem" title="Encrypt selected hosts" data-i18n-title="storage.encryptTitle">' +
  '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.2 7V5.2a2.8 2.8 0 015.6 0V7"/></svg>' +
  '<span data-i18n="storage.encryptSelected">Encrypt selected</span></button>' +
  '<button type="button" class="nethub-menu-item" id="nethub-menu-restore" role="menuitem" title="Restore selected hosts" data-i18n-title="storage.restoreTitle">' +
  '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5"/><path d="M5.2 10.5h5.6"/></svg>' +
  '<span data-i18n="storage.restoreSelected">Restore selected</span></button>' +
  '<button type="button" class="nethub-menu-item" id="nethub-menu-activate" role="menuitem" title="Activate selected hosts" data-i18n-title="storage.activateTitle">' +
  '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="8" r="5.2"/><path d="M8 5.4v5.2M5.4 8h5.2"/></svg>' +
  '<span data-i18n="storage.activateSelected">Activate selected</span></button>' +
  '<button type="button" class="nethub-menu-item" id="nethub-menu-deactivate" role="menuitem" title="Deactivate selected hosts" data-i18n-title="storage.deactivateTitle">' +
  '<svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="8" r="5.2"/><path d="M5.4 8h5.2"/></svg>' +
  '<span data-i18n="storage.deactivateSelected">Deactivate selected</span></button>');

function configStorageError(message) {
  var el = document.getElementById('config-storage-err');
  if (!el) return;
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
        error.detail = parsed && parsed.detail;
        throw error;
      }
      if (text && !parsed) throw new Error(t('storage.invalidResponse'));
      return parsed;
    });
  });
}

/* One place that turns a server failure into the user's language. Shared by the
   panel and by every surface that can meet a host whose key or activation is
   missing, because "the same condition answered in two languages" is how a UI
   starts feeling half-translated. */
function describeStorageError(err) {
  if (err && err.code === 'config_encryption_orphaned') return t('storage.hostOrphaned');
  if (err && err.code === 'key_locked') return t('storage.hostLocked');
  if (err && err.code === 'host_not_activated') return t('storage.hostNotActivated');
  return configStorageFailure(err);
}

function configStorageFailure(err) {
  switch (err && err.code) {
    case 'key_in_use': return t('storage.keyInUse') + (err && err.detail ? ' ' + err.detail : '');
    case 'key_backup_required': return t('storage.keyBackupRequired');
    case 'key_unknown': return t('storage.keyUnknown');
    case 'key_required': return t('storage.keyRequiredPick');
    case 'key_not_found': return t('storage.keyGone');
    case 'key_locked': return t('storage.keyNotPluggedHint');
    case 'key_mnemonic_invalid': return t('storage.keyMnemonicInvalid');
    case 'key_last_wrapper': return t('storage.keyLast');
    case 'key_provider_unavailable': return t('storage.keyUnavailable');
    case 'keyring_missing': return t('storage.keyringDamaged');
    case 'keyring_authentication_failed': return t('storage.keyringRejected');
    case 'config_initialization_required': return t('storage.notInitialized');
    case 'host_not_activated': return t('storage.hostNotActivated');
    case 'passphrase_required': return t('storage.passphraseRequired');
    case 'passphrase_incorrect': return t('storage.passphraseIncorrect');
    case 'forbidden': return t('storage.humanOnly');
    /* The server speaks English, and a Chinese panel answering in English is the
       same defect as a missing translation. Known codes are mapped above; anything
       else gets a localised sentence plus the code, and the raw text goes to the
       console where a developer can still read it. */
    default:
      if (err && err.message) console.warn('config-storage:', err.message);
      return err && err.code ? t('storage.failedWithCode', {code: err.code}) : t('storage.failed');
  }
}

/* The entry key is the only place encryption is visible while the panel is
   closed, so it has to answer one question at a glance: would encrypted hosts
   work right now? Four answers:

     unknown     nothing fetched yet — say nothing rather than something wrong
     orphaned    no key, yet ciphertext is sitting on disk that nothing can read
     off         no key has ever been set
     unplugged   keys exist, none of them is inserted
     on          a key is inserted, encrypted hosts can be activated and read */
function configStorageView(state) {
  if (!state) return 'unknown';
  var hosts = state.hosts || [];
  var encryptedHosts = 0;
  for (var i = 0; i < hosts.length; i++) {
    if (hosts[i].mode === 'encrypted') encryptedHosts++;
  }
  var keys = state.keys || 0;
  var plugged = state.keys_plugged || 0;
  if (!keys) return encryptedHosts ? 'orphaned' : 'off';
  return plugged ? 'on' : 'unplugged';
}

function configStorageBadgeTitle(view) {
  switch (view) {
    case 'on': return t('storage.badgeOn');
    case 'unplugged': return t('storage.badgeUnplugged');
    case 'orphaned': return t('storage.badgeOrphaned');
    case 'off': return t('storage.badgeOff');
    default: return t('storage.badgeUnknown');
  }
}

/* The single writer of that button. Every state change goes through here, so the
   glyph cannot be left saying "on" after the last key was removed. */
function paintConfigStorageBadge() {
  var key = document.getElementById('config-storage-open');
  if (!key) return;
  var view = configStorageView(_configStorageState);
  key.classList.toggle('is-configured', view === 'on');
  key.classList.toggle('is-locked', view === 'unplugged');
  key.classList.toggle('is-orphaned', view === 'orphaned');
  key.classList.toggle('is-unknown', view === 'unknown');
  var title = configStorageBadgeTitle(view);
  key.title = title;
  key.setAttribute('aria-label', title);
}

/* Frames arrive from the server's config-storage broadcast, so a key inserted in
   another tab — or by an MCP client — repaints this one without a reload. */
function applyConfigStorageSnapshot(state) {
  if (!state) return;
  _configStorageState = state;
  paintConfigStorageBadge();
  var modal = document.getElementById('modal-config-storage');
  if (modal && !modal.classList.contains('hidden')) renderConfigStorage();
}

/* A key is known by its name. The short id stays beside it because it is the one
   thing that cannot be mistyped for another key, but it is never the headline —
   "f69cba19" tells nobody which machine this is. */
function configStorageKeyLabel(key) {
  if (key && key.label) return key.label;
  return t('storage.keyUnnamed');
}

function configStorageKeyShortID(key) {
  return String((key && key.id) || '').slice(0, 8);
}

/* What this key is and what it would cost to lose: one line, in the order a person
   asks those questions. */
function configStorageKeyFacts(key) {
  var parts = [];
  parts.push(key.hosts ? t('storage.keyHosts', {count: String(key.hosts)}) : t('storage.keyHostsNone'));
  if (key.passphrase) parts.push(t('storage.keyHasPassphrase'));
  return parts.join(' · ');
}

function renderMainKeys() {
  var list = document.getElementById('config-storage-key-list');
  if (!list) return;
  list.innerHTML = '';
  /* A keychain that cannot be opened is not an empty one: "no keys yet" beside a
     failure notice is two answers to the same question. The damaged block says it. */
  var stated = _configStorageState || {};
  if (!_configStorageKeys.length && stated.error_code) return;
  if (!_configStorageKeys.length) {
    var empty = document.createElement('li');
    empty.className = 'cs-key-empty';
    var head = document.createElement('p');
    head.className = 'cs-key-empty-h';
    head.textContent = t('storage.keysEmpty');
    empty.appendChild(head);
    var body = document.createElement('p');
    body.className = 'cs-key-empty-b';
    body.textContent = t('storage.keysEmptyWhat');
    empty.appendChild(body);
    /* No buttons here: the title bar above carries both entrances and is never
       folded, so repeating them would be the same two actions twice in one dialog. */
    list.appendChild(empty);
    return;
  }
  var needle = String(_keyFilter || '').trim().toLowerCase();
  var shown = 0;
  _configStorageKeys.forEach(function (key) {
    if (needle && String(configStorageKeyLabel(key)).toLowerCase().indexOf(needle) < 0 &&
        String(key.id || '').toLowerCase().indexOf(needle) < 0) {
      return;
    }
    list.appendChild(configStorageKeyRow(key));
    shown++;
  });
  if (!shown) {
    var none = document.createElement('li');
    none.className = 'cs-key-empty';
    none.textContent = t('storage.keyFilterNone', {needle: _keyFilter});
    list.appendChild(none);
  }
  syncConfigStorageSelection();
}

/* A row is one key in the list. Everything that acts WITHOUT looking at the key's
 * contents is here — insert, remove, copy the address, delete — because those are
 * the things you do to a keychain you already know. Editing, which opens the key
 * itself, is the name.
 */
/* One key, one table row. The row is a line, not a card: a keychain is a list of
 * things you scan and compare, and the two things that matter while scanning are
 * its name and whether it is inserted. Everything else — the address, the words,
 * the rotation — is behind the row, in the sheet the row opens.
 */
function configStorageKeyRow(key) {
  var row = document.createElement('li');
  row.className = 'cs-key-row' + (key.plugged ? ' is-loaded' : '');
  row.setAttribute('data-key-id', key.id);
  row.tabIndex = 0;
  if (_keySelected[key.id]) row.classList.add('is-selected');
  /* Clicking a row SELECTS it; opening the sheet is the eye button's job. One
     function flips the tick, so the row, the checkbox and the keyboard cannot
     disagree about what a click means. */
  function toggle() {
    var on = !_keySelected[key.id];
    if (on) _keySelected[key.id] = true; else delete _keySelected[key.id];
    box.checked = on;
    row.classList.toggle('is-selected', on);
    syncConfigStorageSelection();
  }
  row.onclick = toggle;
  row.onkeydown = function (e) { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } };

  var box = document.createElement('input');
  box.type = 'checkbox';
  box.className = 'cs-key-tick';
  box.checked = !!_keySelected[key.id];
  box.setAttribute('aria-label', configStorageKeyLabel(key));
  box.onclick = function (e) { e.stopPropagation(); };
  box.onchange = function () {
    if (box.checked) _keySelected[key.id] = true; else delete _keySelected[key.id];
    row.classList.toggle('is-selected', box.checked);
    syncConfigStorageSelection();
  };
  row.appendChild(box);

  var name = document.createElement('span');
  name.className = 'cs-key-name-text' + (key.label ? '' : ' is-unnamed');
  name.textContent = configStorageKeyLabel(key);
  row.appendChild(name);

  var badge = document.createElement('span');
  badge.className = 'cs-key-badge ' + (key.plugged ? 'is-active' : 'is-inactive');
  badge.textContent = key.plugged ? t('storage.keyBadgeInserted') : t('storage.keyBadgeRemoved');
  row.appendChild(badge);

  /* The row's controls, together: what you can do to this key without opening it.
     Two verbs, so two buttons in one group — a menu holding a single item is a
     click that exists only to hide the thing it holds. */
  var actions = document.createElement('span');
  actions.className = 'cs-key-actions';

  /* Insert / remove: a switch read in two directions, so one button whose state
     is visible at rest. */
  var plug = document.createElement('button');
  plug.type = 'button';
  plug.className = 'cs-key-power' + (key.plugged ? ' is-on' : '');
  plug.setAttribute('aria-pressed', key.plugged ? 'true' : 'false');
  plug.title = key.plugged ? t('storage.keyUnplug') : t('storage.keyPlug');
  plug.setAttribute('aria-label', plug.title);
  plug.disabled = _configStorageBusy;
  /* A key, not a power symbol: what this button moves is the key itself, and a
     power glyph at 15px reads as the word "On". */
  plug.innerHTML = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="5.2" cy="10.8" r="2.6"/><path d="M7 9l5.2-5.2M10.4 5.6l1.6 1.6M12 4l1.2 1.2"/></svg>';
  plug.onclick = function (e) {
    e.stopPropagation();
    configStorageKeyAction(key.id, key.plugged ? 'unplug' : 'plug', key);
  };
  actions.appendChild(plug);

  var remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'cs-key-remove';
  remove.title = t('storage.keyRemove');
  remove.setAttribute('aria-label', t('storage.keyRemove'));
  remove.disabled = _configStorageBusy;
  remove.innerHTML = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 4.5h11M6 4.5V3h4v1.5M4 4.5l.7 9h6.6l.7-9"/></svg>';
  remove.onclick = function (e) {
    e.stopPropagation();
    configStorageKeyAction(key.id, 'remove', key);
  };
  actions.appendChild(remove);

  /* Detail LAST, at the row's trailing edge: the two buttons before it act on the
     key, this one opens it, and a chevron at the end of a row is the shape people
     already read as "there is more behind this". An eye was the wrong glyph — it
     means "reveal a secret", which is a different verb on a different screen. */
  var view = document.createElement('button');
  view.type = 'button';
  view.className = 'cs-key-view';
  view.title = t('storage.keyView');
  view.setAttribute('aria-label', t('storage.keyView'));
  view.innerHTML = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6.2 3.5L10.7 8l-4.5 4.5"/></svg>';
  view.onclick = function (e) {
    e.stopPropagation();
    openKeyDetail(key.id);
  };
  actions.appendChild(view);

  row.appendChild(actions);
  return row;
}

function configStorageKeyButton(label, action, key, className) {
  var button = document.createElement('button');
  button.type = 'button';
  button.className = className || 'btn';
  button.textContent = label;
  button.disabled = _configStorageBusy;
  button.onclick = function () { configStorageKeyAction(key.id, action, key); };
  return button;
}

/* ---- creating a key (in-page form) --------------------------------------- */

/* Creating happens in the page, not on a second screen: a key is a name, the
   words it is made of, and an optional passphrase. The words can be typed in
   (someone restoring a key) or generated, so both are first class. */
function openKeyCreate() {
  _keyFormMode = 'new';
  _keyFormID = '';
  _keyFormWords = '';
  _keyFormRevealed = false;
  keyFormError('');
  document.getElementById('key-form-title').textContent = t('storage.keyNewTitle');
  document.getElementById('key-form-name').value = '';
  document.getElementById('key-form-words-input').value = '';
  document.getElementById('key-form-pass').value = '';
  document.getElementById('key-form-primary').textContent = t('storage.keyCreate');
  document.getElementById('key-form-cancel').textContent = t('common.cancel');
  document.getElementById('key-form-delete').style.display = 'none';
  document.getElementById('key-form-rotate').style.display = 'none';
  document.getElementById('key-form-passphrase').style.display = 'none';
  document.getElementById('key-form-generate').style.display = '';
  document.getElementById('key-form-words-input-block').style.display = '';
  document.getElementById('key-form-pass-block').style.display = '';
  document.getElementById('key-form-pub-block').style.display = 'none';
  document.getElementById('key-form-words-block').style.display = 'none';
  /* The hosts line describes a key that exists; a create form has none yet, and the
     dialog is a reused singleton, so it would otherwise show the count of whatever
     key was opened last. */
  var facts = document.getElementById('key-form-hosts-summary');
  if (facts) facts.textContent = '';
  showModal('modal-key-form');
  try { document.getElementById('key-form-name').focus(); } catch (e) {}
}

function closeKeyCreate() {
  hideModal('modal-key-form');
  _keyFormMode = 'new';
  _keyFormID = '';
  _keyFormWords = '';
  _keyFormRevealed = false;
  keyFormError('');
}

function keyCreateError(msg) { keyFormError(msg); }

function keyCreateGenerateWords() {
  keyFormError('');
  var btn = document.getElementById('key-form-generate');
  if (btn) btn.disabled = true;
  configStorageRequest('/api/keychain/new-mnemonic', {method: 'POST'}).then(function (body) {
    var words = (body && body.mnemonic) || '';
    if (!words) throw new Error(t('storage.invalidResponse'));
    document.getElementById('key-form-words-input').value = words;
  }).catch(function (err) {
    keyFormError(configStorageFailure(err));
  }).finally(function () {
    if (btn) btn.disabled = false;
  });
}

/* `new` creates; `edit` renames. Both are "save this key's fields", which is why
   they share one function and one button. */
function keyCreateSubmit() {
  var name = String(document.getElementById('key-form-name').value || '').trim();
  if (!name) {
    keyFormError(t('storage.keyNameRequired'));
    try { document.getElementById('key-form-name').focus(); } catch (e) {}
    return;
  }
  if (_keyFormMode !== 'new') { keyFormSubmit(); return; }
  var words = String(document.getElementById('key-form-words-input').value || '').trim();
  if (!words) {
    keyFormError(t('storage.keyWordsRequired'));
    try { document.getElementById('key-form-words-input').focus(); } catch (e) {}
    return;
  }
  var pass = String(document.getElementById('key-form-pass').value || '');
  keyFormBusy(true);
  keyFormError('');
  configStorageRequest('/api/keychain', {
    method: 'POST',
    headers: {'Content-Type': 'application/json; charset=utf-8'},
    body: JSON.stringify({label: name, secret: words, passphrase: pass})
  }).then(function (payload) {
    configStorageApplyPayload(payload);
    hideModal('modal-key-form');
    _keyFormMode = 'new';
    showCopyToast(t('storage.keyCreatedToast'));
    if (typeof loadConnections === 'function') loadConnections();
  }).catch(function (err) {
    keyFormError(configStorageFailure(err));
  }).finally(function () { keyFormBusy(false); });
}

/* ---- key detail sheet (clicking the item) ------------------------------- */

/* One screen per key, opened by clicking its row. Everything about one key is
   here — its name, its address, its words, its deletion — because that is the
   question a row raises. */
function openKeyDetail(keyID) {
  _keyFormMode = 'edit';
  _keyFormID = keyID || '';
  _keyFormWords = '';
  _keyFormRevealed = false;
  keyFormError('');
  var key = configStorageDetailKey(keyID);
  if (!key) return;

  document.getElementById('key-form-title').textContent = configStorageKeyLabel(key);
  document.getElementById('key-form-name').value = key.label || '';
  document.getElementById('key-form-primary').textContent = t('storage.keySave');
  document.getElementById('key-form-cancel').textContent = t('common.close');
  document.getElementById('key-form-delete').style.display = '';
  document.getElementById('key-form-rotate').style.display = '';
  /* Offered only where it means something: a key without a passphrase gets one
     here, and a key with one gets a different one. */
  document.getElementById('key-form-passphrase').style.display = '';
  /* The fields that only make sense while creating a key. */
  document.getElementById('key-form-generate').style.display = 'none';
  document.getElementById('key-form-words-input-block').style.display = 'none';
  document.getElementById('key-form-pass-block').style.display = 'none';
  document.getElementById('key-form-pub-block').style.display = '';
  document.getElementById('key-form-words-block').style.display = '';
  paintKeyFormAddress(key.public_key);

  var sumEl = document.getElementById('key-form-hosts-summary');
  if (sumEl) {
    sumEl.textContent = key.hosts
      ? t('storage.keyHostsSummary', {count: String(key.hosts)})
      : t('storage.keyHostsSummaryNone');
  }
  renderKeyFormWords();
  showModal('modal-key-form');
}

function closeKeyForm() {
  hideModal('modal-key-form');
  _keyFormMode = 'new';
  _keyFormID = '';
  _keyFormWords = '';
  _keyFormRevealed = false;
}

/* ---- selection ----------------------------------------------------------- */

function configStorageSelectedKeyIDs() {
  var out = [];
  _configStorageKeys.forEach(function (key) { if (_keySelected[key.id]) out.push(key.id); });
  return out;
}

/* The header checkbox answers for the whole visible list: a filter that hides
   keys must not let "select all" quietly mean "all 40, including the ones you
   cannot see". */
function syncConfigStorageSelection() {
  var all = document.getElementById('config-storage-key-select-all');
  if (!all) return;
  var needle = String(_keyFilter || '').trim().toLowerCase();
  var visible = _configStorageKeys.filter(function (key) {
    if (!needle) return true;
    return String(configStorageKeyLabel(key)).toLowerCase().indexOf(needle) >= 0 ||
      String(key.id || '').toLowerCase().indexOf(needle) >= 0;
  });
  var ticked = 0;
  visible.forEach(function (key) { if (_keySelected[key.id]) ticked++; });
  all.checked = visible.length > 0 && ticked === visible.length;
  all.indeterminate = ticked > 0 && ticked < visible.length;
  all.disabled = _configStorageBusy || visible.length === 0;
  var count = configStorageSelectedKeyIDs().length;
  /* Both selection verbs are armed by the same fact, so they are armed here,
     once, next to the count that states it. */
  var selDelete = document.getElementById('config-storage-key-sel-delete');
  if (selDelete) selDelete.disabled = _configStorageBusy || count === 0;
  renderConfigStorageTitleNote(count);
  /* Export means "export THESE": nothing ticked, nothing to write, so the button
     is disabled rather than silently switching scope to every key. */
  var exp = document.getElementById('config-storage-key-backup');
  if (exp) {
    exp.title = count ? t('storage.keyExportSelected', {count: String(count)}) : t('storage.keyExportAction');
    exp.disabled = count === 0;
    exp.classList.toggle('is-active', count > 0);
  }
}

function configStorageDetailKey(keyID) {
  var found = null;
  _configStorageKeys.forEach(function (k) { if (k.id === keyID) found = k; });
  return found;
}

function keyFormError(message) {
  var el = document.getElementById('key-form-err');
  el.textContent = String(message || '');
  el.style.display = message ? 'block' : 'none';
}

/* The 24 words as a numbered grid. In `new` they are the only copy that will ever
   exist, so they are open; in `edit` they are a secret that happens to be ours, so
   they stay folded until someone asks — a phrase on screen is readable by whoever
   is behind you, and this is the only screen it is ever on. */
function renderKeyFormWords() {
  var grid = document.getElementById('key-form-phrase');
  grid.innerHTML = '';
  var words = String(_keyFormWords || '').trim().split(/\s+/).filter(Boolean);
  var reveal = document.getElementById('key-form-reveal');
  var shown = _keyFormRevealed && words.length > 0;
  if (reveal) {
    reveal.innerHTML = _keyFormRevealed ? KEY_GEAR_EYE_SHUT : KEY_GEAR_EYE;
    var label = _keyFormRevealed ? t('storage.keyHide') : t('storage.keyReveal');
    reveal.title = label;
    reveal.setAttribute('aria-label', label);
  }
  ['key-form-copy', 'key-form-export'].forEach(function (id) {
    var btn = document.getElementById(id);
    if (btn) btn.disabled = !shown;
  });
  /* Folded is BLURRED, not absent: 24 placeholder cells hold the same four-column
     grid, so the panel keeps its height and toggling never resizes the dialog — a
     modal that jumps under the pointer is a modal you lose your place in. The real
     words are only put in the DOM once someone asks for them. */
  var cells = shown ? words : new Array(24).fill('\u2022\u2022\u2022\u2022\u2022\u2022');
  cells.forEach(function (word) {
    var li = document.createElement('li');
    var w = document.createElement('span');
    w.className = 'cs-phrase-word';
    w.textContent = word;
    li.appendChild(w);
    grid.appendChild(li);
  });
  grid.classList.toggle('is-folded', !shown);
}

function configStorageKeyAction(id, action, key) {
  if (_configStorageBusy || _keyActionInFlight) return;
  var path = '/api/keychain/' + encodeURIComponent(id);
  var options = {method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'}};
  switch (action) {
    case 'remove':
      options.method = 'DELETE';
      /* Deleting is destruction, not removal: the words on paper become the only
         copy, so the confirmation is about the backup, not about the key. The name
         goes in the sentence because "this key" is ambiguous the moment there are
         two, and the count goes in because losing a key is only ever about what it
         strands. */
      confirmDialog({
        title: t('storage.keyRemoveTitle', {name: configStorageKeyLabel(key)}),
        message: key.hosts
          ? t('storage.keyRemoveSealed', {name: configStorageKeyLabel(key), count: String(key.hosts)})
          : t('storage.keyRemoveConfirm'),
        okText: t('storage.keyRemove')
      }).then(function (ok) {
        if (!ok) return;
        options.body = JSON.stringify({backup_confirmed: true});
        configStorageRunKeyAction(path, options, action);
        closeKeyForm();
      });
      return;
    case 'plug':
      path += '/plug';
      break;
    case 'unplug':
      path += '/unplug';
      break;
  }
  configStorageRunKeyAction(path, options, action);
}

function configStorageRunKeyAction(path, options, action) {
  /* `_configStorageBusy` repaints the panel with every control disabled, and it is
     for a DESTRUCTIVE action: it stops a second click landing on something that
     cannot be undone. Plugging is a toggle — idempotent, one request, two clicks are
     two toggles — and routing it through that flag made one click repaint the list
     five times (measured), which is the flash this panel used to do. It also left
     the button disabled afterwards: the rows were painted while the flag was up, and
     clearing the flag does not repaint them.
     So the toggle holds its own one-at-a-time guard and paints exactly once, when
     the answer arrives. */
  var blocking = action === 'remove';
  if (blocking) setConfigStorageBusy(true);
  else _keyActionInFlight = true;
  configStorageRequest(path, options).then(function (payload) {
    configStorageApplyPayload(payload);
    if (typeof loadConnections === 'function') loadConnections();
  }).catch(function (err) {
    /* Insert/remove is a switch, not a workflow: if the kernel refused, toast it
       briefly instead of staining the top of the dialog with red text. */
    showCopyToast(configStorageFailure(err));
  }).finally(function () {
    if (blocking) setConfigStorageBusy(false);
    else _keyActionInFlight = false;
  });
}

/* The words are shown in the panel's own area rather than a toast that scrolls
   away: writing them down is the only way the key survives this machine. */
function configStorageApplyPayload(payload) {
  if (!payload || !payload.keys) return;
  /* The same list arrives twice: once as this response, once as the server's
     websocket broadcast (which is what makes another tab agree). Repainting for the
     copy that says nothing new is a second teardown of the list for one click — the
     flicker. The fingerprint is the cheapest way to say "identical". */
  var fingerprint = JSON.stringify(payload.keys);
  var unchanged = fingerprint === _configStorageKeysFingerprint;
  _configStorageKeys = payload.keys;
  _configStorageKeysFingerprint = fingerprint;
  if (unchanged) return;

  /* One repaint: `renderConfigStorage` already renders the rows. */
  renderConfigStorage();
  syncConfigStorageSelection();
}

function loadConfigStorage() {
  return configStorageRequest('/api/config-storage').then(function (state) {
    _configStorageState = state;
    _configStorageFetchedAt = Date.now();
    paintConfigStorageBadge();
    paintMaskToggle();
    /* At startup the list may already be on screen, rendered while this state was
       still unknown — which painted real addresses under a "hidden" eye. The mask
       is now locally decided, so announcing the load is enough to make the two
       agree; nothing needs refetching. */
    if (typeof notifyMaskChange === 'function') notifyMaskChange();
  });
}

/* The list is its own read: it is available with nothing inserted, and it must not
   make the status call any slower. */
function loadConfigStorageKeys() {
  return configStorageRequest('/api/keychain').then(function (payload) {
    configStorageApplyPayload(payload);
  }).catch(function () { /* the badge already reports a broken store */ });
}

/* The title's caption: activation progress and the current selection.
 *
 * Activation belongs to HOSTS, so that number counts encrypted hosts and how many
 * of them are activated; the selection belongs to KEYS, which is what the ticks
 * are. Two numbers, two clauses, joined where both exist — the panel has room for
 * a caption, not for two sentences.
 */
function renderConfigStorageTitleNote(selected) {
  var status = document.getElementById('config-storage-status');
  if (!status) return;
  var state = _configStorageState || {};
  var keys = _configStorageKeys;
  var parts = [];
  if (!keys.length) {
    /* Something is wrong with the file itself, and saying "encryption is off"
       would invite adding a key over it. */
    parts.push(state.error_code ? t('storage.statusDamaged', {code: state.error_code}) : t('storage.statusOff'));
  } else {
    var activated = 0;
    (state.hosts || []).forEach(function (host) {
      if (host.mode === 'encrypted' && host.activated) activated++;
    });
    /* Three counters, in the order the states nest: the ticks are keys out of all
       keys, the load is keys whose words are in memory, and the activation is hosts,
       which have no denominator worth printing — every plaintext host is trivially
       "not activated" and counting it would make the number look like a problem.
       The load is shown even at zero: unlike a plaintext host, a key that is not
       loaded is the reason nothing sealed by it can be read. */
    var loaded = 0;
    keys.forEach(function (key) { if (key.plugged) loaded++; });
    parts.push(t('storage.titleSelected', {count: String(selected || 0), total: String(keys.length)}));
    parts.push(t('storage.titleLoaded', {count: String(loaded)}));
    if (activated) {
      parts.push(t('storage.titleActivated', {count: String(activated)}));
    }
  }
  status.textContent = parts.join(' · ');
}

function renderConfigStorage() {
  var state = _configStorageState || {};
  var keys = _configStorageKeys;
  renderConfigStorageTitleNote(configStorageSelectedKeyIDs().length);
  /* Re-render the rows so a busy flag disables them: the flag is the only source
     of `disabled`, which is what keeps a failed request from latching them. */
  renderMainKeys();
  var damaged = !keys.length && !!state.error_code;
  /* The header row is only the filter now — the tick and the panel's actions moved
     up to the title bar — so it appears with the filter and folds with it. Folding
     it when the keychain is empty is what it always did; what it must never do
     again is fold the only way to add a key, which is why the entrances live in the
     empty state itself. */
  var head = document.querySelector('.cs-list-head');
  if (head) head.style.display = keys.length > 4 && !damaged ? '' : 'none';
  var damagedBlock = document.getElementById('config-storage-key-damaged');
  if (damagedBlock) damagedBlock.style.display = damaged ? '' : 'none';
  var why = document.getElementById('config-storage-key-damaged-why');
  if (why) why.textContent = damaged ? t('storage.keyDamagedWhy', {code: state.error_code}) : '';
  var list = document.getElementById('config-storage-key-list');
  if (list) list.style.display = damaged ? 'none' : '';
  var filter = document.getElementById('config-storage-key-filter');
  if (filter) {
    /* Below a handful of keys the box is furniture: it answers a question nobody
       has yet, and it takes the row the list needs. A damaged keychain has nothing
       to filter either. */
    filter.style.display = keys.length > 4 && !damaged ? '' : 'none';
    if (filter.value !== _keyFilter) filter.value = _keyFilter;
  }
  var discard = document.getElementById('config-storage-key-discard');
  if (discard) discard.disabled = _configStorageBusy;
  var add = document.getElementById('config-storage-key-add');
  /* A damaged keychain with nothing sealed by it is a leftover, not a loss: adding
     a key replaces it, and the server decides (it refuses when profiles are still
     sealed). Disabling the button here would leave the panel with no action at all,
     which is how a robustness problem turns into a dead feature. */
  if (add) {
    add.disabled = _configStorageBusy || keys.length >= 32;
    add.title = state.error_code ? t('storage.keyDamagedReplace') : '';
  }
  paintConfigStorageBadge();
  paintMaskToggle();
  syncConfigStorageActions();
}

function setConfigStorageBusy(busy) {
  _configStorageBusy = busy;
  renderConfigStorage();
}

function openConfigStorage() {
  configStorageError('');
  showModal('modal-config-storage');
  loadConfigStorage().then(function () { return loadConfigStorageKeys(); })
    .catch(function (err) { configStorageError(configStorageFailure(err)); });
}

/* The batch menu's actions answer to the selection AND to the key: with no key
   inserted there is nothing to encrypt with, and a key that opens a panel the menu
   cannot explain reads as a broken action. */
function configStorageSelectedNames() {
  if (typeof _nodeBatch !== 'undefined' && _nodeBatch) return _nodeBatch.selected();
  return [];
}

function syncConfigStorageActions() {
  var encryptKey = document.getElementById('nethub-menu-encrypt');
  var restoreKey = document.getElementById('nethub-menu-restore');
  var activateKey = document.getElementById('nethub-menu-activate');
  var deactivateKey = document.getElementById('nethub-menu-deactivate');
  if (!encryptKey || !restoreKey || !activateKey || !deactivateKey) return;
  var state = _configStorageState || {};
  var byName = {};
  (window._lastConnections || []).forEach(function (c) { if (c && c.name) byName[c.name] = c; });
  var plain = 0, sealed = 0, inactive = 0, active = 0;
  (state.hosts || []).forEach(function (host) {
    if (!byName[host.name]) return;
    if (host.mode === 'plaintext') plain++;
    if (host.mode === 'encrypted') {
      sealed++;
      if (host.activated) active++; else inactive++;
    }
  });
  var selected = configStorageSelectedNames();
  var selectedPlain = 0, selectedInactive = 0, selectedActive = 0;
  selected.forEach(function (name) {
    var host = null;
    (state.hosts || []).forEach(function (candidate) { if (candidate.name === name) host = candidate; });
    if (!host) return;
    if (host.mode === 'plaintext') selectedPlain++;
    if (host.mode === 'encrypted') {
      if (host.activated) selectedActive++; else selectedInactive++;
    }
  });
  var keyReady = !!state.keys_plugged;
  /* Sealed hosts are selectable here too: encrypting one means "move it to the
     key I pick", and the same key answers already_encrypted without writing.
     Refusing the click would hide the only way to re-key a host — the panel has
     no per-host key switcher, and the plaintext route would pass through disk. */
  encryptKey.disabled = !selected.length || !keyReady;
  restoreKey.disabled = !selectedActive;
  activateKey.disabled = !selectedInactive;
  deactivateKey.disabled = !selectedActive;
  encryptKey.title = keyReady ? t('storage.encryptTitle') : t('storage.initializeFirst');
  /* Sending is about exactly one host, so this item follows its own rule rather
     than the batch bar's "one or more": with two ticked there is no single host to
     send, and the dialog could only say so after the click. */
  var shareKey = document.getElementById('nethub-menu-share');
  if (shareKey) shareKey.disabled = selected.length !== 1;
}

/* Neither action is a button of its own: they are the two answers to the same
   question on the ticked set, and only the one that fits the current state is
   clickable. */
/* Asks for a secret and resolves with it, "" for "none", or null when cancelled.
   Kept separate from the confirm dialog because a confirm has no field, and from
   the activate dialog because that one is about hosts, not about one key. */
function configStoragePromptSecret(opts) {
  opts = opts || {};
  document.getElementById('secret-title').textContent = opts.title || '';
  document.getElementById('secret-message').textContent = opts.message || '';
  document.getElementById('secret-ok').textContent = opts.okText || t('common.done');
  var input = document.getElementById('secret-input');
  input.value = '';
  /* A one-field prompt resolves to a string (the passphrase), a two-field one to an
     object — the caller knows which it asked for. */
  var second = document.getElementById('secret-second-field');
  var input2 = document.getElementById('secret-input-2');
  var twoFields = !!opts.second;
  if (second) second.style.display = twoFields ? '' : 'none';
  if (input2) input2.value = '';
  if (twoFields) {
    document.getElementById('secret-label').textContent = opts.firstLabel || t('storage.keyPassLabel');
    document.getElementById('secret-label-2').textContent = opts.second.label || t('storage.keyPassNewLabel');
  }
  return new Promise(function (resolve) {
    var ok = document.getElementById('secret-ok');
    var cancel = document.getElementById('secret-cancel');
    var close = function (value) {
      hideModal('modal-secret');
      ok.onclick = null;
      cancel.onclick = null;
      document.getElementById('secret-x').onclick = null;
      input.removeEventListener('keydown', onKey);
      resolve(value);
    };
    var onKey = function (e) { if (e.key === 'Enter') { e.preventDefault(); ok.click(); } };
    ok.onclick = function () {
      if (!twoFields) { close(String(input.value || '')); return; }
      close({first: String(input.value || ''), second: String(input2.value || '')});
    };
    cancel.onclick = function () { close(null); };
    document.getElementById('secret-x').onclick = function () { close(null); };
    input.addEventListener('keydown', onKey);
    showModal('modal-secret');
    try { input.focus(); } catch (e) {}
  });
}

/* ---- picking the key that seals a selection ----------------------------- */

/* Returns a Promise for the chosen key id, or null when the person backed out.
   With exactly one usable key there is nothing to choose, so the question is not
   asked — ceremony for a decision that has one answer is how a dialog teaches
   people to dismiss dialogs. */
function configStoragePickKey(names) {
  var keys = (_configStorageKeys || []).filter(function (k) { return k.plugged; });
  if (keys.length === 1) return Promise.resolve(keys[0].id);
  if (!keys.length) return Promise.resolve(null);

  var list = document.getElementById('pick-key-list');
  list.innerHTML = '';
  keys.forEach(function (key, index) {
    var label = document.createElement('label');
    label.className = 'cs-pick-row';
    var radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = 'pick-key';
    radio.value = key.id;
    if (index === 0) radio.checked = true;
    label.appendChild(radio);
    var text = document.createElement('span');
    text.className = 'cs-pick-text';
    var name = document.createElement('span');
    name.className = 'cs-pick-name';
    name.textContent = configStorageKeyLabel(key);
    text.appendChild(name);
    var facts = document.createElement('span');
    facts.className = 'cs-pick-facts';
    facts.textContent = configStorageKeyFacts(key);
    text.appendChild(facts);
    label.appendChild(text);
    list.appendChild(label);
  });
  document.getElementById('pick-key-title').textContent = t('storage.keyPickTitle');
  document.getElementById('pick-key-err').style.display = 'none';

  return new Promise(function (resolve) {
    var ok = document.getElementById('pick-key-ok');
    var close = function (value) {
      hideModal('modal-pick-key');
      ok.onclick = null;
      document.getElementById('pick-key-x').onclick = null;
      resolve(value);
    };
    ok.onclick = function () {
      var chosen = list.querySelector('input[name="pick-key"]:checked');
      close(chosen ? chosen.value : null);
    };
    document.getElementById('pick-key-x').onclick = function () { close(null); };
    showModal('modal-pick-key');
  });
}

function configStorageConvert(action, names) {
  names = names || [];
  var state = _configStorageState || {};
  if (typeof _nodeBatch !== 'undefined' && _nodeBatch) _nodeBatch.closeMenu();
  if (!state.keys) {
    openConfigStorage();
    return;
  }
  if (!names.length) { showCopyToast(t('storage.selectAtLeastOne')); return; }
  if ((action === 'encrypt' || action === 'activate') && !state.keys_plugged) {
    showCopyToast(t('storage.keyNotPluggedHint'));
    openConfigStorage();
    return;
  }
  if (action === 'activate') {
    configStorageOpenActivate(names);
    return;
  }
  if (action === 'deactivate') {
    var titleOff = t('storage.deactivateTitle');
    confirmDialog({title: titleOff, message: tCount('storage.deactivateConfirm.one', 'storage.deactivateConfirm.other', {count: names.length}), okText: titleOff}).then(function (ok) {
      if (!ok) return;
      configStoragePostBatch('/api/config-storage/deactivate', {names: names}, function () {
        showCopyToast(tCount('storage.deactivated.one', 'storage.deactivated.other', {count: names.length}));
      });
    });
    return;
  }
  if (action === 'encrypt') {
    /* The key comes first: it decides what the confirmation is even about. */
    configStoragePickKey(names).then(function (keyID) {
      if (!keyID) return;
      var key = configStorageDetailKey(keyID);
      /* A key that carries a passphrase needs it NOW: the second layer is what
         seals the new ciphertext, and the server refuses the batch without it
         instead of writing something the person could not open again. */
      var ask = key && key.passphrase
        ? configStoragePromptSecret({
            title: t('storage.encryptPassTitle'),
            message: t('storage.encryptPassMessage'),
            okText: t('storage.encryptTitle')
          })
        : Promise.resolve('');
      return ask.then(function (passphrase) {
        if (passphrase === null) return;
        var titleOn = t('storage.encryptTitle');
        var messageOn = tCount('storage.encryptConfirm.one', 'storage.encryptConfirm.other', {count: names.length}) +
          ' ' + t('storage.encryptWithKey', {key: configStorageKeyLabel(key)});
        confirmDialog({title: titleOn, message: messageOn, okText: titleOn}).then(function (ok) {
          if (!ok) return;
          var body = {names: names, key: keyID};
          if (passphrase) body.passphrase = passphrase;
          configStoragePostBatch('/api/config-storage/encrypt', body, function (response) {
            var failed = ((response && response.results) || []).filter(function (r) { return !r.ok; });
            if (failed.length) throw new Error(failed.map(function (r) { return r.name + ': ' + (r.error || r.code); }).join('; '));
            showCopyToast(tCount('storage.converted.one', 'storage.converted.other', {count: names.length}));
          });
        });
      });
    });
    return;
  }
  var title = t('storage.restoreTitle');
  var message = tCount('storage.restoreConfirm.one', 'storage.restoreConfirm.other', {count: names.length});
  confirmDialog({title: title, message: message, okText: title}).then(function (ok) {
    if (!ok) return;
    configStoragePostBatch('/api/config-storage/' + action, {names: names}, function (response) {
      var failed = ((response && response.results) || []).filter(function (r) { return !r.ok; });
      if (failed.length) throw new Error(failed.map(function (r) { return r.name + ': ' + (r.error || r.code); }).join('; '));
      showCopyToast(tCount('storage.converted.one', 'storage.converted.other', {count: names.length}));
    });
  });
}

function configStoragePostBatch(path, body, onDone) {
  setConfigStorageBusy(true);
  configStorageError('');
  configStorageRequest(path, {
    method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'}, body: JSON.stringify(body)
  }).then(function (response) {
    return loadConfigStorage().then(function () { return loadConfigStorageKeys(); }).then(function () {
      if (typeof loadConnections === 'function') loadConnections();
      if (typeof _nodeBatch !== 'undefined' && _nodeBatch) _nodeBatch.update();
      if (onDone) onDone(response);
    });
  }).catch(function (err) { configStorageError(configStorageFailure(err)); })
    .finally(function () { setConfigStorageBusy(false); });
}

/* Activation groups the selection by KEY, because the passphrase belongs to the
   key (§8.5): 5 hosts under 2 passphrase-bearing keys means 2 boxes, not 5. */
/* A key is named by the name the person gave it; the short id is the fallback for
   a key that has none, and it is the same fallback the keychain list uses. */
function keyDisplayName(keyID) {
  var found = null;
  _configStorageKeys.forEach(function (k) { if (k.id === keyID) found = k; });
  return found && found.label ? found.label : String(keyID || '').slice(0, 8);
}

function configStorageActivateGroups(names) {
  var hosts = (_configStorageState && _configStorageState.hosts) || [];
  var byName = {};
  hosts.forEach(function (host) { byName[host.name] = host; });
  var groups = [];
  var index = {};
  names.forEach(function (name) {
    var host = byName[name];
    if (!host || host.mode !== 'encrypted') return;
    var keyId = host.key_id || '';
    if (!index[keyId]) {
      index[keyId] = {keyId: keyId, passphrase: !!host.passphrase, names: []};
      groups.push(index[keyId]);
    }
    index[keyId].names.push(name);
  });
  return groups;
}

function configStorageOpenActivate(names) {
  var groups = configStorageActivateGroups(names);
  if (!groups.length) { showCopyToast(t('storage.selectAtLeastOne')); return; }
  var fields = document.getElementById('modal-activate-fields');
  fields.innerHTML = '';
  var needed = 0;
  groups.forEach(function (group) {
    if (!group.passphrase) return;
    needed++;
    var field = document.createElement('div');
    field.className = 'conn-field';
    var label = document.createElement('label');
    label.textContent = t('storage.passphraseForKey', {key: keyDisplayName(group.keyId), count: String(group.names.length)});
    var input = document.createElement('input');
    input.type = 'password';
    input.autocomplete = 'off';
    input.setAttribute('data-activate-key', group.keyId);
    field.appendChild(label);
    field.appendChild(input);
    fields.appendChild(field);
  });
  document.getElementById('modal-activate-hint').textContent = needed
    ? t('storage.activateHint', {count: String(names.length)})
    : t('storage.activateNoPassphrase', {count: String(names.length)});
  document.getElementById('modal-activate-title').textContent = t('storage.activateTitle');
  document.getElementById('modal-activate-err').style.display = 'none';
  var ok = document.getElementById('modal-activate-ok');
  ok.onclick = function () { configStorageSubmitActivate(names); };
  showModal('modal-activate');
}

function configStorageSubmitActivate(names) {
  var passphrases = {};
  var inputs = document.querySelectorAll('#modal-activate-fields [data-activate-key]');
  for (var i = 0; i < inputs.length; i++) {
    if (!inputs[i].value) continue;
    passphrases[inputs[i].getAttribute('data-activate-key')] = inputs[i].value;
  }
  hideModal('modal-activate');
  configStoragePostBatch('/api/config-storage/activate', {names: names, passphrases: passphrases}, function () {
    showCopyToast(tCount('storage.activated.one', 'storage.activated.other', {count: names.length}));
  });
}

/* A stale badge is worse than no badge: it answers "encrypted hosts work" from an
   answer that expired when the daemon restarted. Cheap refreshes ride the moments
   the page can actually have changed under us. */
var _configStorageFetchedAt = 0;
function refreshConfigStorageState(force) {
  var now = Date.now();
  if (!force && now - _configStorageFetchedAt < 30000) return;
  _configStorageFetchedAt = now;
  loadConfigStorage().catch(function () {});
}
document.addEventListener('visibilitychange', function () {
  if (!document.hidden) refreshConfigStorageState(false);
});
window.addEventListener('focus', function () { refreshConfigStorageState(false); });

document.getElementById('config-storage-open').onclick = openConfigStorage;
document.getElementById('config-storage-close').onclick = function () { hideModal('modal-config-storage'); };
document.getElementById('modal-activate-close').onclick = function () { hideModal('modal-activate'); };
document.getElementById('modal-activate-cancel').onclick = function () { hideModal('modal-activate'); };
document.getElementById('nethub-menu-encrypt').onclick = function () { configStorageConvert('encrypt', configStorageSelectedNames()); };
document.getElementById('nethub-menu-restore').onclick = function () { configStorageConvert('restore', configStorageSelectedNames()); };
document.getElementById('nethub-menu-activate').onclick = function () { configStorageConvert('activate', configStorageSelectedNames()); };
document.getElementById('nethub-menu-deactivate').onclick = function () { configStorageConvert('deactivate', configStorageSelectedNames()); };
document.getElementById('config-storage-key-add').onclick = openKeyCreate;
/* Importing is its own icon rather than a menu under the add button: a dropdown
   with one item is a click that only exists to hide the thing it holds. */
document.getElementById('config-storage-key-import').onclick = openKeyImport;
/* Receiving a shared host starts from this key's address: the sender sealed it to
   that address, so the paste has nothing to choose and share.js owns the dialog. */
document.getElementById('key-form-receive').onclick = function () {
  if (typeof shareOpen === 'function') shareOpen('import');
};

/* ---- the shared key form (new + edit) ----------------------------------- */

document.getElementById('key-form-generate').onclick = keyCreateGenerateWords;
['key-form-name', 'key-form-words-input', 'key-form-pass'].forEach(function (id) {
  var el = document.getElementById(id);
  if (el) el.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && e.target.tagName !== 'TEXTAREA') {
      e.preventDefault();
      keyCreateSubmit();
    }
  });
});

/* ---- importing KEYS (whole keychain) ------------------------------------ */

function openKeyImport() {
  keyImportError('');
  document.getElementById('key-import-text').value = '';
  showModal('modal-key-import');
  try { document.getElementById('key-import-text').focus(); } catch (e) {}
}

function keyImportError(msg) {
  var el = document.getElementById('key-import-err');
  if (!el) return;
  el.textContent = String(msg || '');
  el.style.display = msg ? 'block' : 'none';
}

/* A backup file is "name, then its words" blocks; pasted text is often just the
   words. Both are accepted: a line of 24+ lowercase words is a phrase, and a line
   above it that is not a phrase is that phrase's name. */
function configStorageParseKeyBackup(text) {
  var lines = String(text || '').split(/\r?\n/);
  var entries = [];
  var pendingName = '';
  lines.forEach(function (raw) {
    var line = raw.trim();
    if (!line) { pendingName = ''; return; }
    var words = line.split(/\s+/).filter(Boolean);
    var looksLikePhrase = words.length >= 12 && words.every(function (w) { return /^[a-z]+$/.test(w); });
    if (looksLikePhrase) {
      entries.push({label: pendingName, secret: words.join(' ')});
      pendingName = '';
      return;
    }
    if (words.length === 1 || words.length < 12) { pendingName = line; return; }
    pendingName = '';
  });
  return entries;
}

function keyImportSubmit() {
  var text = String(document.getElementById('key-import-text').value || '');
  var entries = configStorageParseKeyBackup(text);
  if (!entries.length) { keyImportError(t('storage.keyImportNoneFound')); return; }
  var go = document.getElementById('key-import-go');
  if (go) go.disabled = true;
  keyImportError('');
  var chain = Promise.resolve();
  var failed = 0;
  entries.forEach(function (entry) {
    chain = chain.then(function () {
      return configStorageRequest('/api/keychain', {
        method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'},
        body: JSON.stringify({label: entry.label || '', secret: entry.secret})
      }).then(function (payload) {
        configStorageApplyPayload(payload);
      }).catch(function () { failed++; });
    });
  });
  chain.then(function () {
    hideModal('modal-key-import');
    return loadConfigStorageKeys().then(function () {
      if (failed) showCopyToast(t('storage.keyImportPartial', {count: String(failed)}));
      else showCopyToast(t('storage.keyImportedKeys', {count: String(entries.length)}));
    });
  }).catch(function (err) { keyImportError(configStorageFailure(err)); })
    .finally(function () { if (go) go.disabled = false; });
}

/* The button is the picker: a visible file input cannot be styled to match the
   other verbs, and one hidden input driven by a real button is how the rest of this
   dialog already works. The text goes into the box instead of importing directly,
   so what is about to become keys is visible before it is. */
document.getElementById('key-import-file').onclick = function () {
  var input = document.getElementById('key-import-file-input');
  if (input) input.click();
};
document.getElementById('key-import-file-input').onchange = function () {
  var input = this;
  var file = input.files && input.files[0];
  if (!file) return;
  var reader = new FileReader();
  reader.onload = function () {
    document.getElementById('key-import-text').value = String(reader.result || '');
    keyImportError('');
  };
  reader.onerror = function () { keyImportError(t('storage.keyImportFileFailed')); };
  reader.readAsText(file);
  /* Cleared so choosing the SAME file twice fires change again. */
  input.value = '';
};
document.getElementById('key-import-x').onclick = function () { hideModal('modal-key-import'); };
document.getElementById('key-import-cancel').onclick = function () { hideModal('modal-key-import'); };
document.getElementById('key-import-go').onclick = keyImportSubmit;


document.getElementById('config-storage-key-filter').addEventListener('input', function (e) {
  _keyFilter = String(e.target.value || '');
  renderMainKeys();
  syncConfigStorageSelection();
});
/* Deleting a SET. Each key is still one request and one guard (words written down,
   seals nothing, is not the index owner), so a batch reports what it could not do
   instead of pretending a single transaction. */
document.getElementById('config-storage-key-sel-delete').onclick = function () {
  var ids = configStorageSelectedKeyIDs();
  if (!ids.length) return;
  var keys = ids.map(configStorageDetailKey).filter(Boolean);
  var hosts = keys.reduce(function (sum, k) { return sum + (k.hosts || 0); }, 0);
  confirmDialog({
    title: tCount('storage.keyRemoveManyTitle.one', 'storage.keyRemoveManyTitle.other', {count: ids.length}),
    message: hosts
      ? tCount('storage.keyRemoveManySealed.one', 'storage.keyRemoveManySealed.other', {count: hosts})
      : t('storage.keyRemoveManyConfirm'),
    okText: t('storage.keyRemove'),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    setConfigStorageBusy(true);
    var failed = 0;
    var chain = Promise.resolve();
    ids.forEach(function (id) {
      chain = chain.then(function () {
        return configStorageRequest('/api/keychain/' + encodeURIComponent(id), {
          method: 'DELETE',
          headers: {'Content-Type': 'application/json; charset=utf-8'},
          body: JSON.stringify({backup_confirmed: true})
        }).then(function (payload) {
          configStorageApplyPayload(payload);
          delete _keySelected[id];
        }).catch(function () { failed++; });
      });
    });
    chain.then(function () { return loadConfigStorageKeys(); })
      .then(function () {
        renderMainKeys();
        syncConfigStorageSelection();
        if (failed) showCopyToast(t('storage.keyRemovePartial', {count: String(failed)}));
        else showCopyToast(tCount('storage.keyRemoved.one', 'storage.keyRemoved.other', {count: String(ids.length - failed)}));
      })
      .finally(function () { setConfigStorageBusy(false); });
  });
};

/* The header box INVERTS the visible selection. "Select all" was the wrong verb:
 * with two of twelve ticked, clicking it to complete the set also meant there was
 * no way to get to the other ten, and the tri-state box was already describing the
 * current selection rather than an action. Invert has one meaning in every state.
 *
 * Mutation check: restore "checked sets everything to true" and ticking a partial
 * selection leaves the same partial selection.
 */
document.getElementById('config-storage-key-select-all').onchange = function () {
  var needle = String(_keyFilter || '').trim().toLowerCase();
  _configStorageKeys.forEach(function (key) {
    if (needle && String(configStorageKeyLabel(key)).toLowerCase().indexOf(needle) < 0 &&
        String(key.id || '').toLowerCase().indexOf(needle) < 0) return;
    if (_keySelected[key.id]) delete _keySelected[key.id]; else _keySelected[key.id] = true;
  });
  renderMainKeys();
  syncConfigStorageSelection();
};

/* Backing up every key at once. Each phrase needs its key plugged in — the words
   only exist while it is — so this walks the plugged ones and says what it skipped
   instead of writing a file that silently loses half the keychain. */
/* Exporting is for KEYS: it writes down the words of what is ticked (or all
   plugged keys when nothing is ticked). Missing words are stated in the toast
   instead of written as an empty row. */
document.getElementById('config-storage-key-backup').onclick = function () {
  /* The button is disabled with nothing ticked, so this always has a scope: the
     ticked keys. No "nothing selected means everything" fallback. */
  var selectedIDs = configStorageSelectedKeyIDs();
  var pool = _configStorageKeys.filter(function (k) { return selectedIDs.indexOf(k.id) >= 0; });
  var plugged = pool.filter(function (k) { return k.plugged; });
  var missing = pool.length - plugged.length;
  if (!plugged.length) { showCopyToast(t('storage.keyBackupNone')); return; }
  setConfigStorageBusy(true);
  Promise.all(plugged.map(function (key) {
    return configStorageRequest('/api/keychain/' + encodeURIComponent(key.id) + '/mnemonic')
      .then(function (body) { return {name: configStorageKeyLabel(key), words: (body && body.mnemonic) || ''}; })
      .catch(function () { return {name: configStorageKeyLabel(key), words: ''}; });
  })).then(function (entries) {
    var lines = entries.map(function (entry) {
      return entry.name + String.fromCharCode(10) + (entry.words || t('storage.keyBackupUnreadable')) + String.fromCharCode(10);
    });
    downloadTextFile(lines.join(String.fromCharCode(10)), 'termcp-keys.txt');
    if (missing) showCopyToast(t('storage.keyBackupPartial', {count: String(missing)}));
  }).finally(function () { setConfigStorageBusy(false); });
};

/* One place that turns text into a saved file, because there are now two callers
   and the second one would otherwise be a copy of the first. */
function downloadTextFile(text, filename) {
  var blob = new Blob([text], {type: 'text/plain;charset=utf-8'});
  var url = URL.createObjectURL(blob);
  var a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
}
/* Discarding is destructive and irreversible, so it asks first: the file is the
   only thing that could ever open hosts sealed by it, and although the server
   refuses in that case, a confirm is what makes the click mean what it says. */
document.getElementById('config-storage-key-discard').onclick = function () {
  /* The same confirmation every other destructive action uses. A native confirm()
     here read as a browser warning rather than as part of this panel. */
  confirmDialog({
    title: t('storage.keyDiscard'),
    message: t('storage.keyDiscardConfirm'),
    okText: t('storage.keyDiscard'),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    setConfigStorageBusy(true);
    configStorageRequest('/api/config-storage/discard-keychain', {method: 'POST'}).then(function (payload) {
      configStorageApplyPayload(payload);
      return loadConfigStorageKeys();
    }).then(function () {
      setConfigStorageBusy(false);
    }).catch(function (err) {
      setConfigStorageBusy(false);
      configStorageError(configStorageFailure(err));
    });
  });
};

/* ---- key form wiring ---------------------------------------------------- */

document.getElementById('key-form-x').onclick = closeKeyForm;
document.getElementById('key-form-cancel').onclick = closeKeyForm;
document.getElementById('key-form-primary').onclick = keyCreateSubmit;

/* An address is derived from a Main Key, so a released key has none — and an empty
   dashed box is indistinguishable from a broken one. The box says why. */
function paintKeyFormAddress(publicKey) {
  document.getElementById('key-form-pub-value').textContent =
    publicKey || t('storage.keyAddressNotLoaded');
  document.getElementById('key-form-pub').classList.toggle('is-empty', !publicKey);
  var copy = document.getElementById('key-form-pub-copy');
  if (copy) copy.disabled = !publicKey;
}

/* The address is a string people copy, so both the box and the button beside it copy
   it. Empty means the box is a sentence, not an address. */
function copyKeyFormAddress() {
  var box = document.getElementById('key-form-pub');
  if (!box || box.classList.contains('is-empty')) return;
  var value = document.getElementById('key-form-pub-value').textContent;
  if (!value) return;
  copyTextToClipboard(value);
  showCopyToast(t('share.copied'));
}
document.getElementById('key-form-pub').onclick = copyKeyFormAddress;
document.getElementById('key-form-pub-copy').onclick = copyKeyFormAddress;

/* Revealing fetches the words the first time and remembers them; folding back is
   free, because the point of folding is the screen, not the memory. */
document.getElementById('key-form-reveal').onclick = function () {
  if (_keyFormRevealed) {
    _keyFormRevealed = false;
    renderKeyFormWords();
    return;
  }
  if (_keyFormWords) {
    _keyFormRevealed = true;
    renderKeyFormWords();
    return;
  }
  if (!_keyFormID) return;
  keyFormError('');
  configStorageRequest('/api/keychain/' + encodeURIComponent(_keyFormID) + '/mnemonic').then(function (body) {
    _keyFormWords = (body && body.mnemonic) || '';
    _keyFormRevealed = true;
    renderKeyFormWords();
  }).catch(function (err) { keyFormError(configStorageFailure(err)); });
};

document.getElementById('key-form-copy').onclick = function () {
  if (!_keyFormWords) return;
  copyTextToClipboard(_keyFormWords);
  showCopyToast(t('storage.keyWordsCopied'));
};

/* Export writes a file instead of showing a toast: the words have to survive the
   browser closing, and a clipboard is not a backup. */
document.getElementById('key-form-export').onclick = function () {
  if (!_keyFormWords) return;
  var key = configStorageDetailKey(_keyFormID);
  var name = (key && key.label ? key.label : 'termcp-key').replace(/[\/:*?"<>|]+/g, '-');
  var blob = new Blob([_keyFormWords + String.fromCharCode(10)], {type: 'text/plain;charset=utf-8'});
  var url = URL.createObjectURL(blob);
  var a = document.createElement('a');
  a.href = url;
  a.download = name + '.txt';
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
};

document.getElementById('key-form-rotate').onclick = configStorageRotateKey;
document.getElementById('key-form-passphrase').onclick = configStorageChangePassphrase;

/* Changing the passphrase is not rotating the key: the words stay, the id stays and
   the address stays, and every host sealed by this key is re-sealed in place. The
   old passphrase is asked for because the hosts cannot be opened without it, and a
   wrong one is refused before anything is written. */
function configStorageChangePassphrase() {
  var key = configStorageDetailKey(_keyFormID);
  if (!key || _configStorageBusy) return;
  configStoragePromptSecret({
    title: t('storage.keyPassChangeTitle', {name: configStorageKeyLabel(key)}),
    message: key.passphrase
      ? t('storage.keyPassChangeMessage', {count: String(key.hosts)})
      : t('storage.keyPassAddMessage', {count: String(key.hosts)}),
    okText: t('storage.keyPassChangeAction'),
    firstLabel: t('storage.keyPassOldLabel'),
    second: {label: t('storage.keyPassNewLabel')}
  }).then(function (values) {
    if (!values) return;
    keyFormBusy(true);
    keyFormError('');
    configStorageRequest('/api/keychain/' + encodeURIComponent(key.id) + '/passphrase', {
      method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'},
      body: JSON.stringify({old: values.first, new: values.second})
    }).then(function (payload) {
      configStorageApplyPayload(payload);
      closeKeyForm();
      showCopyToast(t('storage.keyPassChanged'));
    }).catch(function (err) { keyFormError(configStorageFailure(err)); })
      .finally(function () { keyFormBusy(false); });
  });
}
document.getElementById('key-form-delete').onclick = function () {
  var key = configStorageDetailKey(_keyFormID);
  if (key) configStorageKeyAction(key.id, 'remove', key);
};

/* Enter in the name box means the form's primary action: it is the only field on
   the new state, so there is nothing else the keystroke could mean. */
document.getElementById('key-form-name').addEventListener('keydown', function (e) {
  if (e.key !== 'Enter') return;
  e.preventDefault();
  keyFormSubmit();
});

/* The form's one verb. In `new` it makes a key and shows the words once; in `edit`
   it saves the name, which is the only editable field — everything else on that
   form is an action of its own. */
function keyFormSubmit() {
  var name = String(document.getElementById('key-form-name').value || '').trim();
  if (!name) {
    keyFormError(t('storage.keyNameRequired'));
    try { document.getElementById('key-form-name').focus(); } catch (e) {}
    return;
  }
  if (!_keyFormID) return;
  var key = configStorageDetailKey(_keyFormID);
  if (key && name === (key.label || '')) { closeKeyForm(); return; }
  keyFormBusy(true);
  configStorageRequest('/api/keychain/' + encodeURIComponent(_keyFormID) + '/rename', {
    method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'},
    body: JSON.stringify({label: name})
  }).then(function (payload) {
    configStorageApplyPayload(payload);
    closeKeyForm();
    showCopyToast(t('storage.keyRenamed'));
  }).catch(function (err) { keyFormError(configStorageFailure(err)); })
    .finally(function () { keyFormBusy(false); });
}

function keyFormBusy(busy) {
  ['key-form-primary', 'key-form-cancel', 'key-form-delete', 'key-form-rotate', 'key-form-passphrase'].forEach(function (id) {
    var el = document.getElementById(id);
    if (el) el.disabled = busy;
  });
}

/* Rotation runs from the form because that is where the words are. The view
   follows the new key: rotation ends with the old one deleted. */
function configStorageRotateKey() {
  var key = configStorageDetailKey(_keyFormID);
  if (!key || _configStorageBusy) return;
  confirmDialog({
    title: t('storage.keyRotateTitle', {name: configStorageKeyLabel(key)}),
    message: t('storage.keyRotateConfirm'),
    okText: t('storage.keyRotate')
  }).then(function (ok) {
    if (!ok) return;
    if (key.passphrase) {
      /* Rotation makes a new key, and a key's second layer belongs to the key. An
         empty box here means "the new key has no passphrase", which is a decision
         the person makes rather than one that happens to them. */
      configStoragePromptSecret({
        title: t('storage.keyRotatePassTitle', {name: configStorageKeyLabel(key)}),
        message: t('storage.keyRotatePassMessage'),
        okText: t('storage.keyRotate')
      }).then(function (passphrase) {
        if (passphrase === null) return;
        rotateWith(passphrase);
      });
      return;
    }
    rotateWith('');
  });

  function rotateWith(passphrase) {
    keyFormBusy(true);
    keyFormError('');
    configStorageRequest('/api/keychain/' + encodeURIComponent(key.id) + '/rotate', {
      method: 'POST', headers: {'Content-Type': 'application/json; charset=utf-8'},
      body: JSON.stringify({passphrase: passphrase})
    })
      .then(function (payload) {
        configStorageApplyPayload(payload);
        var created = (payload.keys || []).filter(function (k) { return k.label === key.label; });
        if (created.length) {
          _keyFormID = created[0].id;
          paintKeyFormAddress(created[0].public_key);
        }
        _keyFormWords = (payload && payload.mnemonic) || '';
        _keyFormRevealed = true;
        renderKeyFormWords();
        showCopyToast(t('storage.keyRotated'));
        if (typeof loadConnections === 'function') loadConnections();
      })
      .catch(function (err) { keyFormError(configStorageFailure(err)); })
      .finally(function () { keyFormBusy(false); });
  }
}

/* ---- The masking eye -------------------------------------------------------
   One toggle, in the host column's own bar. `aria-pressed` IS the state: it drives
   the glyph swap in CSS and is what a screen reader announces, so there is no
   second copy of the truth to drift. The eye shows what the list looks like right
   now — closed eye means addresses are hidden — because that is the question a
   person has when glancing at it. */
function maskToggleButton() {
  return document.getElementById('conn-mask-toggle');
}

/* The eye is built here rather than written into index.html: that file has a size
   budget because markup creeping into it is how the page became one 6000-line
   document, and this control belongs to the list this module already owns. It is
   inserted into the host column's add row on load. */
function installMaskToggle() {
  /* Immediately right of "Add host", where the question it answers comes up: it
     is a view of the list being added to, not a sibling of import. */
  var add = document.getElementById('conn-add');
  if (!add || maskToggleButton()) return;
  var btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'drawer-add drawer-mask';
  btn.id = 'conn-mask-toggle';
  btn.setAttribute('aria-pressed', 'true');
  btn.innerHTML =
    '<svg class="eye-open" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1 8s2.5-5 7-5 7 5 7 5-2.5 5-7 5-7-5-7-5z"/><circle cx="8" cy="8" r="2.5"/></svg>' +
    /* Same eye, struck through. The two glyphs share the outline on purpose: at
       16px a redrawn shape reads as a different icon, and this one has to read as
       "the same eye, closed". */
    '<svg class="eye-closed" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1 8s2.5-5 7-5 7 5 7 5-2.5 5-7 5-7-5-7-5z"/><circle cx="8" cy="8" r="2.5"/><path d="M2.5 2.5l11 11"/></svg>';
  btn.onclick = toggleMask;
  add.insertAdjacentElement('afterend', btn);
  paintMaskToggle();
}

function paintMaskToggle() {
  var btn = maskToggleButton();
  if (!btn) return;
  var masked = connMasked();
  /* aria-pressed describes the button, and this button is "show the address and
     the user name". Pressed therefore means SHOWING, which is the inverse of
     `masked` — the eye is open exactly when the information is on screen. Getting
     this backwards is how an icon starts lying while every value under it is
     still correct. */
  btn.setAttribute('aria-pressed', masked ? 'false' : 'true');
  /* Both strings appear literally so the i18n scanner can see them; a computed key
     is invisible to it and the key gets reported as an orphan. */
  var label = masked ? t('storage.maskToggleOn') : t('storage.maskToggleOff');
  btn.title = label;
  btn.setAttribute('aria-label', label);
}

function toggleMask() {
  var btn = maskToggleButton();
  if (!btn || btn.disabled) return;
  var wanted = !connMasked();
  btn.disabled = true;
  configStorageRequest('/api/config-storage/mask', {
    method: 'PUT', headers: {'Content-Type': 'application/json; charset=utf-8'},
    body: JSON.stringify({masked: wanted})
  }).then(function () {
    _configStorageState = _configStorageState || {};
    _configStorageState.masked = wanted;
    paintMaskToggle();
    /* The rows are rebuilt from the snapshot already in memory: the mask is a
       display decision, and the data behind it does not change. */
    notifyMaskChange();
  }).catch(function (err) {
    /* Masking is a display switch; a failure must not leave the eye claiming a
       state the server did not accept. */
    configStorageError(configStorageFailure(err));
  }).finally(function () { btn.disabled = false; });
}

installMaskToggle();
/* The lock on a card does what it shows: an open one encrypts that host, a
   closed one restores it. Delegated rather than bound per card because the grid
   repaints on every connection refresh. In selection mode the click still belongs
   to the batch bar — a tick is what a card press means there. */
/* Why this card cannot be reached, in the person's language. The card already
   says "locked"; a click is the natural place to say which lock and what opens it,
   because that is the moment the question is being asked. */
document.getElementById('conn-grid').addEventListener('click', function (event) {
  var reason = event.target.closest('[data-lock-reason]');
  if (!reason) return;
  event.stopPropagation();
  var kind = reason.getAttribute('data-lock-reason');
  showCopyToast(kind === 'not_activated' ? t('storage.cardInactiveHint') : t('storage.cardLockedHint'));
});
document.getElementById('conn-grid').addEventListener('click', function (event) {
  var mark = event.target.closest('.conn-storage-mark');
  if (!mark) return;
  if (typeof _nodeBatch !== 'undefined' && _nodeBatch && _nodeBatch.isOn()) return;
  event.stopPropagation();
  configStorageConvert(mark.getAttribute('data-storage') === 'encrypted' ? 'restore' : 'encrypt',
    [mark.getAttribute('data-conn-name')]);
});
new MutationObserver(function () {
  if (document.getElementById('modal-config-storage').classList.contains('hidden')) {
    closeKeyForm();
    configStorageError('');
  }
}).observe(document.getElementById('modal-config-storage'), {attributes: true, attributeFilter: ['class']});
onLangChange(function () { if (_configStorageState) renderConfigStorage(); });

loadConfigStorage().then(function () { return loadConfigStorageKeys(); }).catch(function () {});
