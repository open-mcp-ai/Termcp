/* termcp Web UI — the extension panel.
 *
 * The right-hand column is the connections panel's other side. NetHub lists the
 * machines Termcp can dial; this lists the trimmed sessions Termcp keeps warm
 * for whoever is driving it (an agent over MCP, mostly). A row is a template's
 * runtime verdict — up, trying, down with a reason — plus the one handle an up
 * extension is driven through.
 *
 * The panel owns no state of its own. The socket delivers the whole list on
 * every change, because the reconciler broadcasts after each state transition,
 * so renderExtensionGrid is a full repaint with nothing to reconcile against.
 * An empty list keeps the column: it is where a new extension is created from,
 * and a panel that vanishes when it is empty is a panel the author cannot open
 * to author with.
 *
 * Its chrome is the connections panel's, twice over: the same batch row on top
 * (select, invert, open, export, delete) and the same pair of bottom keys
 * (author one, import a set). Where NetHub's controls dial and delete hosts,
 * these open the terminal an extension is served by and delete templates.
 *
 * Authoring goes through the same store the extension MCP tool writes to — the
 * REST routes are that store's other caller — so a template saved here and one
 * saved by an agent are indistinguishable, and the two surfaces cannot drift
 * apart on what a valid template is. The dialog has two pages: Status is what a
 * card click opens (the verdict, the handles, the failure history, and the
 * actions that follow from them), Edit is the template itself, as fields or as
 * the TOML file the store holds.
 *
 * Loaded as a plain synchronous <script> like every other module: the
 * declarations land in the shared global scope, and the socket's frame handler
 * is what calls into it.
 */

// The last frame delivered, kept only so a language switch can repaint the
// cards from the same rows rather than from a cache of its own.
var _extRows = null;

// Reconciler statuses, in the order the panel lists them: what can be used
// first, what is being brought up, what needs attention, what was put away.
/* Order is the panel's reading order, and it is a claim about what the reader
   should look at first: something running, then something one ask away, then
   something trying, then something down, then something parked. */
var EXT_STATUS_ORDER = ['available', 'idle', 'probing', 'unavailable', 'disabled'];

// The lamp class is keyed on the status, so a status the panel has never seen
// falls back to the down lamp rather than to no dot at all.
var EXT_LAMP_CLASSES = {
  available: 'is-online',
  /* On demand and nothing asked: not broken and not running, so neither the
     online lamp (which would claim a process) nor the offline one (which would
     claim a fault). The faint lamp is the honest one, and the word beside it
     says why. */
  idle: 'is-disabled',
  probing: 'is-connecting',
  unavailable: 'is-offline',
  disabled: 'is-disabled'
};

// A clockwise arrow, read as "try again": the one per-card key, for the
// repair an author is most often looking at a down extension to make.
// A trash can, read as "remove this extension": the card is where an author
// looks when a template is no longer wanted.
var SVG_EXT_RETRY = '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M14 8a6 6 0 11-1.9-4.4"/><path d="M14 1.6V4.6H11"/></svg>';

function extLampClass(status) {
  return EXT_LAMP_CLASSES[status] || 'is-offline';
}

/* The catalog keys are spelt out in the switches rather than concatenated
   (`t('ext.status.' + status)`) so a reader grepping for a string finds the
   panel, and so a status the panel has never seen falls back to the word for
   "unavailable" instead of a raw key on screen. */
function extStatusLabel(status) {
  switch (status) {
    case 'available': return t('ext.status.available');
    case 'idle': return t('ext.status.idle');
    case 'probing': return t('ext.status.probing');
    case 'disabled': return t('ext.status.disabled');
    default: return t('ext.status.unavailable');
  }
}

// The forward modal already names the three directions; reusing those strings
// keeps one vocabulary for one concept, and an unknown kind still has a label.
function extForwardLabel(kind) {
  var key = 'modal.forward.dir.' + kind;
  var s = t(key);
  return (s === key) ? (kind || '') : s;
}

/* Sort by what the reader is here for, then by name: an extension that is up is
   above one that is trying, which is above one that is down, and a disabled one
   is out of the way at the bottom. */
function extRowOrder(a, b) {
  var ai = EXT_STATUS_ORDER.indexOf(a.status);
  var bi = EXT_STATUS_ORDER.indexOf(b.status);
  if (ai < 0) ai = EXT_STATUS_ORDER.length;
  if (bi < 0) bi = EXT_STATUS_ORDER.length;
  if (ai !== bi) return ai - bi;
  return String(a.name || '').localeCompare(String(b.name || ''));
}

/** Every extension the panel lists, in the order it lists them: the set the
 *  batch keys act on, read from the last frame rather than from the DOM. */
function selectableExtensionNames() {
  return (_extRows || []).slice().sort(extRowOrder).map(function (e) { return e.name; })
    .filter(function (n) { return !!n; });
}

function extRowByName(name) {
  var rows = _extRows || [];
  for (var i = 0; i < rows.length; i++) {
    if (rows[i] && rows[i].name === name) return rows[i];
  }
  return null;
}

function renderExtensionGrid(rows) {
  _extRows = rows || [];
  var grid = document.getElementById('ext-grid');
  if (!grid) return;
  var list = _extRows.slice().sort(extRowOrder);
  /* The column is permanent in the markup (.ext-on is on the frame from the
     start), so an empty list is laid out exactly like a full one and the empty
     state is where the first extension is created from — a panel that
     disappeared when empty was a panel the author could not open to author
     with. */
  grid.innerHTML = '';
  /* THE PLATE'S WORDS ARE ITS EMPTY STATE, so they go the moment a card exists —
     the same rule the archive applies to its own sentence. The element stays in
     the DOM (it is part of the plate) and is toggled, never re-created: a node
     that came and went would be another thing the next render has to know about. */
  var inscription = document.getElementById('ext-inscription');
  if (inscription) inscription.hidden = list.length > 0;
  if (list.length === 0) {
    grid.appendChild(renderExtensionEmpty());
  } else {
    list.forEach(function (ext) {
      grid.appendChild(renderExtensionCard(ext));
    });
  }
  /* Selection is derived from the set, never read back from the DOM, and it is
     pruned against what is still listed: a template that disappeared while the
     batch was being picked must not stay ticked, or the count lies and a delete
     is sent for something already gone. The prune rides on rows having arrived —
     an empty list would drop a tick made before the first frame. */
  _extBatch.syncCards(!!_extRows && _extRows.length > 0);
  _extBatch.update();
  /* The count above the grid updates with the list: whether empty or populated,
     the readout is permanent. */
  paintExtensionStat();
  /* The cards are live: a frame that arrives while one is open is what it was
     opened to watch, so every open card repaints from the row that just changed. */
  extRepaintWindows();
}

/* The empty state carries NO copy any more, and that is deliberate.
 *
 * The panel's words are printed on the plate itself (see #ext-inscription in
 * index.html): a watermark under the grid that is there at zero extensions and at
 * fifty. Copy that lives in the empty state can only exist while the list is empty,
 * which is what made the column one height before the first card and another after —
 * and a slogan that appears and vanishes with the data reads as a rendering glitch
 * rather than as the surface's own inscription.
 *
 * So this function is down to keeping the flow's shape: the grid keeps its place, and
 * the inscription behind it never moves. Nothing is added, so nothing changes height.
 */
function renderExtensionEmpty() {
  var slot = document.createElement('div');
  slot.className = 'ext-empty';
  return slot;
}

/* The column's count, painted on the header trigger.
 *
 * It used to be a line of its own above the cards, labelled "Extensions:" — under a
 * header that already said "Extensions", which is the same word twice, one row apart.
 * On the header the number belongs to the title instead, and the body is left to the
 * cards. The archive's own count rides its header the same way.
 *
 * It does NOT hide at zero, and that is the difference from the archive: an empty
 * archive has nothing to say, while "no extensions yet" is exactly what an author
 * needs to see before creating the first one.
 *
 * The capacity bar beside it is pure CSS: `html[data-theme="cyberpunk"]` lays it out
 * and every other theme leaves it `display: none`. The theme also renames the column
 * to 义体容量 through the catalog, so the same number reads as a capacity there — no
 * branch here knows which theme is active.
 */
function paintExtensionStat() {
  var badge = document.getElementById('ext-count');
  if (badge) badge.textContent = String((_extRows || []).length);
}

function renderExtensionCard(ext) {
  var tile = document.createElement('div');
  var lamp = extLampClass(ext.status);
  /* AN ON-DEMAND EXTENSION IS STARTED BY ASKING, AND THIS KEY IS THE ASK. It is
     the same key for a down extension because it means the same thing to a human
     either way — bring it up now — and only its tooltip differs: "retry" is a
     repair, "start" is a first use. */
  var retryable = ext.status === 'unavailable' || ext.status === 'probing' || ext.status === 'idle';
  var retryTitle = ext.status === 'idle' ? t('ext.use.title') : t('ext.retry.title');
  var selected = _extBatch.has(ext.name);
  tile.className = 'conn-tile entry-card ext-card' + (selected ? ' batch-selected' : '');
  tile.setAttribute('data-ext-name', ext.name || '');
  tile.setAttribute('data-ext-status', ext.status || '');
  var name = ext.name || '';
  var retryBtn = retryable
    ? '<button type="button" class="ext-retry-btn" title="' + escapeHtml(retryTitle) +
      '" aria-label="' + escapeHtml(retryTitle) + '">' + SVG_EXT_RETRY + '</button>'
    : '';
  /* The gear opens the configuration, the way the host cards' own gear does: the
     card is the readout, and the one thing a human reaches for by hand is filed
     behind the key that means "settings". */
  var gearBtn = '<button type="button" class="conn-edit-btn" title="' + escapeHtml(t('conn.aria.editProfile')) +
    '" aria-label="' + escapeHtml(t('conn.aria.editProfile')) + '">' + SVG_CONN_EDIT + '</button>';
  var meta =
    '<span class="node-status ' + lamp + '">' + escapeHtml(extStatusLabel(ext.status)) + '</span>' +
    (ext.needs && ext.needs.length
      ? '<span class="node-sep" aria-hidden="true">·</span>' +
        '<span class="ext-needs">' + escapeHtml(t('ext.card.needs', { names: ext.needs.join(', ') })) + '</span>'
      : '');
  /* COUNTS, NOT CONTENT. A template that names eight hosts with three tunnels
     each would push every other card off the panel; what a reader needs at this
     level is "how much is behind this one", and the window behind the card is
     where the list itself lives. */
  var hostCount = ext.host_count !== undefined ? ext.host_count :
    (ext.targets && ext.targets.length ? ext.targets.length : (ext.target ? 1 : 0));
  var fwdCount = ext.forward_count !== undefined ? ext.forward_count : (ext.forwards || []).length;
  if (ext.forward_count === undefined) {
    (ext.targets || []).forEach(function (row) { fwdCount += (row.forwards || []).length; });
  }
  var scope = '';
  if (hostCount > 0) {
    scope += '<span class="ext-count">' +
      escapeHtml(tCount('ext.card.hosts.one', 'ext.card.hosts.other', { count: hostCount })) + '</span>';
  }
  if (fwdCount > 0) {
    if (scope) scope += '<span class="node-sep" aria-hidden="true">·</span>';
    scope += '<span class="ext-count">' +
      escapeHtml(tCount('ext.card.forwards.one', 'ext.card.forwards.other', { count: fwdCount })) + '</span>';
  }
  if (scope) scope = '<div class="node-addr">' + scope + '</div>';
  // One line, not the history: the verdict is what the panel is for, and the
  // card's own window serves the rest.
  var fail = '';
  if (ext.failure && ext.failure.reason) {
    fail = '<div class="ext-fail" title="' + escapeHtml(t('ext.fail.title', { stage: ext.failure.stage || '' })) + '">' +
      escapeHtml(ext.failure.reason) + '</div>';
  }
  tile.innerHTML =
    '<div class="entry-card-inner">' +
    /* The tick target. A real checkbox, the same choice the archive's rows make and
       for the same reason: the card body already means "open", so the tick needs an
       element of its own rather than a mode that reinterprets the body. */
    '<input type="checkbox" class="ext-card-tick" tabindex="-1"' + (selected ? ' checked' : '') +
      ' title="' + escapeHtml(t('nethub.batch.selectOne', { name: name })) + '"' +
      ' aria-label="' + escapeHtml(t('nethub.batch.selectOne', { name: name })) + '">' +
    '<span class="node-lamp ' + lamp + '" aria-hidden="true"></span>' +
    '<div class="entry-card-main">' +
    '<div class="conn-nm-row">' +
    '<span class="conn-nm-cluster">' +
    '<span class="conn-nm">' + escapeHtml(name) + '</span>' +
    '</span>' +
    retryBtn +
    gearBtn +
    '</div>' +
    '<div class="node-meta">' + meta + '</div>' +
    scope +
    fail +
    '</div>' +
    '</div>';
  var inner = tile.querySelector('.entry-card-inner');
  if (inner) {
    /* The card OPENS. It used to branch on the component's mode flag, which this
       column always answers true to — it runs alwaysOn — so every click ticked and
       the card could only ever be selected, never opened. That branch was written
       when this plate had a selection MODE; when the mode went away the check became
       a constant and the open path below it became dead code.
     *
     * So the tick has its own target, exactly as the archive's rows do: a checkbox
     * on the card. Two targets, two meanings, no mode to consult. */
    inner.title = t('ext.tip.status');
    inner.addEventListener('click', function (e) {
      if (e.target.closest('.ext-retry-btn, .conn-edit-btn, .ext-card-tick')) return;
      /* A status card is a readout, not a terminal the click is aiming at: it opens
         centred, in the cascade, wherever the pointer happened to be. */
      openExtensionWindow(name, null);
    });
  }
  var tick = tile.querySelector('.ext-card-tick');
  if (tick) tick.addEventListener('click', function (e) {
    e.stopPropagation();
    _extBatch.toggleSelection(name, tile);
  });
  var btn = tile.querySelector('.ext-retry-btn');
  if (btn) {
    btn.addEventListener('click', function (e) {
      e.stopPropagation();
      retryExtension(name, btn);
    });
  }
  var gear = tile.querySelector('.conn-edit-btn');
  if (gear) {
    gear.addEventListener('click', function (e) {
      e.stopPropagation();
      openExtensionDialog(name);
    });
  }
  return tile;
}

/* Ask the reconciler to try again now. The human's nudge is the same call the
   agent's extension(action=retry) makes, so the two cannot drift apart in what
   they are allowed to do. The card stays pressed until the next frame lands;
   the repaint is the receipt. */
function retryExtension(name, btn) {
  if (!name) return;
  if (btn) btn.disabled = true;
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name) + '/retry', { method: 'POST' })
    .then(function (r) {
      if (!r.ok) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          throw new Error(j.error || r.status);
        });
      }
    })
    .catch(function (err) {
      if (btn) btn.disabled = false;
      showCopyToast(t('ext.toast.retryFailed', { msg: String((err && err.message) || err) }));
    });
}

/* ---- Batch selection ---
 * The extension plate keeps the SAME selection engine as the hosts column
 * (batch-bar.js) but wears it differently: flat keys on the header bar instead of
 * the hosts column's mode-switched row.
 *
 * The difference is not styling. A host card's click CONNECTS, so the column
 * needs a switch that turns that click into a tick — hence the big list-checks
 * key. An extension card's click opens a status window, which is not something a
 * batch needs, so there is no mode to enter: every card is tickable, the keys
 * that act on a selection simply appear when there is one, and `alwaysOn` tells
 * the shared component to skip the switch and keep the mode on.
 *
 * The verbs the shared component calls are open / export / delete; only export
 * and delete are surfaced as keys here. Opening several status windows at once
 * was a key nobody reached for — the cards are opened one at a time, by clicking
 * them.
 */
var _extBatch = createBatchBar({
  bodyId: 'sec-exts-body',
  /* The keys live on the plate's HEADER bar, so the selecting class has to sit on
     the plate that contains both the bar and the cards. */
  selectingHostId: 'ext-plate',
  alwaysOn: true,
  /* No toggle, no split key, no menu: this column's controls are flat. */
  toggleId: null,
  countId: null,
  invertId: 'ext-sel-invert',
  splitId: null,
  mainId: null,
  caretId: null,
  menuId: null,
  exportId: 'ext-batch-export',
  deleteId: 'ext-batch-delete',
  actionIds: ['ext-batch-export', 'ext-batch-delete'],
  gridId: 'ext-grid',
  cardSelector: '.conn-tile[data-ext-name]',
  nameAttr: 'data-ext-name',
  selectingClass: 'ext-selecting',
  selectKey: 'nethub.batch.select',
  names: selectableExtensionNames,
  render: function () { renderExtensionGrid(_extRows || []); },
  tipFor: function () { return t('ext.tip.status'); },
  onExport: function (names) { exportSelectedExtensions(names); },
  onDelete: function (names) { deleteSelectedExtensions(names); },
  /* The flat bar's own dressing: the two action keys exist only while something
     is ticked. The component paints them disabled in that state (an always-live
     key that does nothing reads as broken), but this bar has no switch to sit a
     disabled key beside, so they are hidden outright and the bar stays two keys
     wide when idle. Hidden, not disabled, for the same reason the hosts column's
     trash is: the ticked cards are already the signal that a batch exists. */
  onUpdate: function (n) {
    ['ext-batch-export', 'ext-batch-delete'].forEach(function (id) {
      var b = document.getElementById(id);
      if (b) b.hidden = n === 0;
    });
    /* The card's checkbox is its visible state, and CSS cannot set `checked`, so it
       is written here from the component's set — the same source the card's
       .batch-selected class comes from. Painted on every update rather than at the
       end of a render, because a rebuild happens when the LIST changes and a tick
       happens when the SELECTION does: painting in the renderer left invert
       showing a count over visibly clean boxes, which is the bug the archive had
       for exactly the same reason. */
    var grid = document.getElementById('ext-grid');
    if (!grid) return;
    grid.querySelectorAll('.ext-card').forEach(function (card) {
      var box = card.querySelector('.ext-card-tick');
      if (!box) return;
      var key = card.getAttribute('data-ext-name');
      box.checked = !!(key && _extBatch.has(key));
    });
  }
});

/* Open the ticked extensions: one card per extension, the way a host batch opens
   one session per ticked host. No key calls this any more — the flat bar has no
   "open the selection" verb — but it is kept because it is the correct meaning of
   that verb if the key ever returns, and because openExtensionWindow is what the
   per-card click path uses. */
function openSelectedExtensions(names) {
  if (!names || !names.length) return;
  names.forEach(function (name) { openExtensionWindow(name, null); });
  // The ticks are spent: clear the selection, keep the mode on for the next batch.
  _extBatch.clear();
  _extBatch.repaint();
}

/* Delete the ticked templates. Deleting a template closes the connection kept
   warm for it once nothing else is served by that connection, which is what the
   dialog says: the author is removing both the recipe and what is running from
   it. */
function deleteSelectedExtensions(names) {
  if (!names || !names.length) return;
  var message = tCount('ext.batch.deleteMsg.one', 'ext.batch.deleteMsg.other', { count: names.length });
  confirmDialog({
    title: t('ext.batch.deleteTitle'),
    message: message,
    okText: t('batch.del.ok', { count: names.length }),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    /* One request for every name (comma-separated path, like the session and
       profile routes): the server reports per-name outcomes and a name that is
       already gone counts as cleared here. */
    deleteResourcesBatch('extensions', names).then(function (results) {
      var failed = [];
      (results || []).forEach(function (r) {
        if (r.ok || r.code === 'extension_not_found') {
          /* A removed template takes its own card with it, exactly as the single
             delete does: a window left behind says "this extension is gone" over
             a readout of something the author just removed. */
          _extBatch.deselect(r.id);
          var win = extWinFor(r.id);
          if (win) closeShellWindow(win);
        } else {
          failed.push(r.id + ': ' + (r.error || r.code || 'failed'));
        }
      });
      if (failed.length) showCopyToast(t('toast.delete.failed', { msg: failed.join('; ') }));
      if (_extDialogName && names.indexOf(_extDialogName) >= 0) closeExtensionDialog();
    }).catch(function (err) {
      showCopyToast(t('toast.delete.failed', { msg: String((err && err.message) || err) }));
    });
  });
}

/* The TOML download. The backend composes the document — the selection, or every
   stored template when the export-all key is used — so the browser stays a pipe
   for bytes. */
function downloadExtensionsToml(names) {
  var path = apiPath('/api/extensions/batch');
  if (names && names.length) path += '?names=' + names.map(encodeURIComponent).join(',');
  return downloadTOML(path, 'termcp-extensions.toml');
}

function exportSelectedExtensions(names) {
  if (!names || !names.length) return;
  downloadExtensionsToml(names).catch(function (e) {
    showCopyToast(t('conn.batch.exportFailed', { msg: String((e && e.message) || e) }));
  });
}

/* The socket frame is the panel's usual painter, but it only carries the key
   when a reconciler is running — a Termcp started without one sends nothing,
   and the column is in the markup whether or not anything answers. This fills
   that gap once: it runs after the socket connects, paints nothing the frame
   has not already painted (_extRows stays null until someone does), and a
   failure still lands as an empty column rather than as an unrendered one. */
function loadExtensionsOnce() {
  if (_extRows) return;
  fetch(apiPath('/api/extensions'))
    .then(function (r) { return r.json(); })
    .then(function (j) { renderExtensionGrid(j.extensions || []); })
    .catch(function () { renderExtensionGrid([]); });
}

onLangChange(function () {
  if (_extRows) renderExtensionGrid(_extRows);
  extRepaintWindows();
});

/* ---- One card per extension: a window, and the configuration behind it --------
 * A card click opens the extension's card — its status — as a window, the kind a
 * session opens: several are open at once, they drag, tile and minimise together,
 * and the batch row's ▶ opens one for every ticked extension. The readout is the
 * reconciler's verdict, the handles an agent drives it through, which other
 * extensions share that connection, the tunnels it bound, the next retry, and the
 * failure history.
 *
 * The template itself lives behind the card's gear, in a modal — editing is one
 * thing at a time, and one TOML document is what it edits (removal included,
 * because removing a template is a template-level act). A create uses the same
 * modal with its name editable and a documented default seeded into the box.
 *
 * The window is a session window with the terminal taken out; the connecting
 * placeholder is the precedent. The chrome, drag, z-order, tile and touch
 * behaviour come from those classes, the body is ours, and it deliberately has no
 * session behind it: no _parentSid (a session frame must not close it), no
 * _placeholder (the Sessions plate must not grow a card for it), no _channels.
 * _sid carries the extension's name — which is what the tab bar labels it with —
 * and _extName is the identity every session lookup excludes it by (see
 * getShellWindowBySid): a name and a session id are both [A-Za-z0-9_-]{1,64}, so
 * _sid alone cannot tell the two kinds of window apart.
 */
var _extDialogName = null;   // extension the configuration modal is showing; null = a new one
var _extDetail = {};         // name -> failure history, for the open windows

function extModalEl(id) { return document.getElementById(id); }

/* The open card of one extension, found by _extName — the identity only these
 * windows carry. Looking it up by _sid was a collision waiting to happen: _sid
 * holds the extension's NAME, and a name is the author's to choose under the same
 * [A-Za-z0-9_-]{1,64} rule a session id is a valid 12-hex string under, so an
 * extension named after a copied id would have resolved to that session's terminal
 * window (and a delete would have closed it). One open card per extension. */
function extWinFor(name) {
  var wins = allShellWins();
  for (var i = 0; i < wins.length; i++) {
    if (wins[i]._extName === name) return wins[i];
  }
  return null;
}

/* openExtensionWindow opens (or raises) the card of one extension. pos is where
 * the click was, or null for the centred cascade a batch uses. */
function openExtensionWindow(name, pos) {
  var ex = extWinFor(name);
  if (ex) { focusShellWindow(ex); return ex; }
  var container = shellWindowsEl();
  if (!container) return null;

  shellWindowCount += 1;
  var win = document.createElement('div');
  /* ext-window is the card's own class: the shared chrome comes from .shell-window,
     and the size rule (as tall as what it says) hangs off this name. */
  win.className = 'shell-window ext-window';
  win.id = 'shell-window-' + shellWindowCount;
  win._sid = name;
  win._extName = name;
  win._connName = '';
  win._streamDone = true;   // there is no session behind this to watch
  win.setAttribute('data-ext-name', name);
  win.innerHTML =
    '<div class="shell-window-header">' +
      '<div class="shell-window-title-cluster">' +
        /* The plate's glyph, in the same slot a terminal window puts its monitor.
           Without it this card carried no icon at all, so the dock fell back to the
           monitor drawing and an extension window was indistinguishable from a
           session window in the one list that holds both. `shell-header-icon` is the
           hook terminal-switcher.js turns into the window's TICK; keeping the class
           identical is what gives extension cards the same tick target as sessions
           rather than a second mechanism. */
        '<span class="shell-header-icon ext-window-icon"><svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><rect x="1.5" y="1.5" width="8" height="8" rx="1.5"/><rect x="6.5" y="6.5" width="8" height="8" rx="1.5"/><path d="M4 14.5v1.5M14.5 4H16"/></svg></span>' +
        '<span class="shell-link-state"></span>' +
        '<h3 class="shell-window-title"><span class="sw-conn">' + escapeHtml(name) + '</span></h3>' +
      '</div>' +
      '<div class="shell-window-header-btns">' +
        '<button type="button" class="conn-edit-btn ext-win-config" title="Edit profile" data-i18n-title="conn.aria.editProfile" aria-label="Edit profile" data-i18n-aria="conn.aria.editProfile">' + SVG_CONN_EDIT + '</button>' +
        SHELL_WINDOW_MAX_HTML +
        SHELL_WINDOW_MIN_HTML_CARD +
        '<button type="button" class="close-btn" title="Close" data-i18n-title="common.close"><svg viewBox="0 0 12 12" width="12" height="12"><path fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" d="M2 2l8 8M10 2L2 10"/></svg></button>' +
      '</div>' +
    '</div>' +
    '<div class="shell-window-content"><div class="ext-win-body">' +
      '<div class="ext-status-head">' +
        '<span class="ext-status-word"></span>' +
        '<span class="ext-status-meta"></span>' +
      '</div>' +
      '<div class="ext-status-grid"></div>' +
      '<div class="ext-status-fails"></div>' +
      '<div class="modal-actions">' +
        '<button type="button" class="btn ext-win-toggle"></button>' +
        '<button type="button" class="btn btn-primary ext-win-retry" title="Retry now" data-i18n-title="ext.retry.title"><span data-i18n="ext.retry.title">Retry now</span></button>' +
      '</div>' +
    '</div>' +
    SHELL_RESIZE_HANDLES_HTML +
    '</div>';

  applyI18n(win);
  var header = win.querySelector('.shell-window-header');
  positionShellWindowFromClick(win, pos);
  container.appendChild(win);
  /* Placement ran on the session default (640x480) and a card is as big as what it
     says, so neither placement dimension is kept. BOTH overrides are desktop-only:
     on touch the window is .win-fullscreen, whose rule owns 100vw x 100dvh, and an
     inline width/height beats a media query — writing either here would shrink a
     full-screen card to its content (see the note on that rule in terminal.css). */
  if (!isMobileViewport()) {
    win.style.width = 'max-content';
    win.style.height = 'auto';
    /* The box has to be in the document and measured before it can be re-centred on
       the placement's centre lines. */
    recentreShellWindowForSize(win);
  }
  /* The card stays on those centre lines while its own numbers arrive — the failure
     history is fetched after the window exists. Grabbing the header or a grip hands
     the geometry over for good: a card the reader has moved (or sized) is theirs. */
  [header].concat(Array.prototype.slice.call(win.querySelectorAll('.shell-window-resize-handle')))
    .forEach(function (el) {
      if (el) el.addEventListener('pointerdown', function () { win._extTaken = true; }, true);
    });
  bindShellWindowMouseToFront(win);
  bindShellWindowMinButton(win, win.querySelector('.shell-window-min-btn'));
  bindShellWindowMaxButton(win);
  setupShellWindowDrag(win, header);
  setupShellWindowResize(win);
  setupMobileSessionSwitcher(win);
  bringShellWindowToFront(win);
  if (_layoutMode === 'tile') { tileWindow(win); openPaneWorkspace(); }
  else { _layoutMaybeAutoTile(); }
  if (_winsHidden) showAllWindows();

  /* This window's close is its own: there is no session to archive, so it must not
     reach the close-session dialog the shared binding offers. */
  var closeBtn = win.querySelector('.close-btn');
  if (closeBtn) closeBtn.addEventListener('click', function (e) { e.stopPropagation(); closeShellWindow(win); });
  var cfg = win.querySelector('.ext-win-config');
  if (cfg) cfg.addEventListener('click', function (e) { e.stopPropagation(); openExtensionDialog(name); });
  var toggle = win.querySelector('.ext-win-toggle');
  if (toggle) toggle.addEventListener('click', function () {
    var row = extRowByName(name);
    setExtensionEnabled(name, !(row && row.enabled), toggle);
  });
  var retry = win.querySelector('.ext-win-retry');
  if (retry) retry.addEventListener('click', function () { retryExtension(name, retry); });

  paintExtensionWindow(win, name);
  loadExtensionHistory(name);
  return win;
}

/* paintExtensionWindow is the card's readout, painted from the frame the panel
 * already has: a status transition repaints every open card, the same way it
 * repaints the grid behind them. */
function paintExtensionWindow(win, name) {
  var row = extRowByName(name);
  var body = win && win.querySelector('.ext-win-body');
  if (!body) return;
  if (!row) {
    body.innerHTML = '<p class="ext-empty-text">' + escapeHtml(t('ext.status.gone')) + '</p>';
    return;
  }
  /* The dot: is-live for a working extension, and the neutral base style for
     everything else — there is no is-idle rule, and the panel's own word sits
     right beside it. The shared helper's fallback tip is session vocabulary
     ("Running"), which would be a lie on an extension that is down, so the tip
     is this panel's own label instead. */
  setWindowLinkState(win, row.status === 'available' ? 'is-live' : 'is-idle');
  var dot = win.querySelector('.shell-link-state');
  if (dot) { dot.title = extStatusLabel(row.status); dot.setAttribute('aria-label', extStatusLabel(row.status)); }
  var word = win.querySelector('.ext-status-word');
  if (word) word.textContent = extStatusLabel(row.status);
  var meta = win.querySelector('.ext-status-meta');
  if (meta) {
    meta.textContent = [row.target, row.enabled ? '' : t('ext.status.parked')]
      .filter(function (x) { return !!x; }).join(' · ');
  }
  /* Per-host rows, only when there is more than one host. A single-target
     extension already says everything above; repeating it as a row of one is
     noise, and the counts on the card are what told the reader to look here. */
  var hostRows = '';
  if ((row.targets || []).length > 1) {
    hostRows = (row.targets || []).map(function (h) {
      var ports = (h.forwards || []).filter(function (f) { return !!f.listen_addr; })
        .map(function (f) { return f.listen_addr; }).join(', ');
      return extStatusRow(h.target, [extStatusLabel(h.status), ports].filter(function (x) { return !!x; }).join(' · '), false);
    }).join('');
  }

  var grid = win.querySelector('.ext-status-grid');
  if (grid) {
    var rows = hostRows;
    if (row.description) rows += extStatusRow(t('ext.modal.description'), row.description, false);
    /* A url template's endpoint is what it talks to, and the one field that says
       which of the two mcp shapes this is; the kind label alone cannot. */
    if (row.url) rows += extStatusRow(t('ext.status.url'), row.url, false);
    rows += extStatusRow(t('ext.status.session'), row.session_id, true);
    rows += extStatusRow(t('ext.status.shell'), row.shell_id, true);
    (row.forwards || []).forEach(function (f) {
      rows += extStatusRow(extForwardLabel(f.kind),
        [f.listen_addr, f.target_addr].filter(function (x) { return !!x; }).join(' → '), false);
    });
    // A multi-target template carries its tunnels per host, not at the top.
    if (!(row.forwards || []).length && (row.targets || []).length < 2) {
      (row.targets || []).forEach(function (t) {
        (t.forwards || []).forEach(function (f) {
          rows += extStatusRow(t.target + ' · ' + extForwardLabel(f.kind),
            [f.listen_addr, f.target_addr].filter(function (x) { return !!x; }).join(' → '), false);
        });
      });
    }
    if (row.next_retry_ms) {
      var secs = Math.max(0, Math.round((row.next_retry_ms - Date.now()) / 1000));
      rows += extStatusRow(t('ext.status.nextRetry'), t('ext.status.inSeconds', { count: secs }), false);
    }
    var shared = extSharingNote(row);
    if (shared) rows = '<p class="conn-check-hint">' + escapeHtml(shared) + '</p>' + rows;
    grid.innerHTML = rows;
    grid.querySelectorAll('.ext-copy-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        copyTextToClipboard(btn.getAttribute('data-copy')).then(function () {
          showCopyToast();
        }).catch(function () {
          showCopyToast(t('toast.copy.failed'));
        });
      });
    });
  }

  var fails = win.querySelector('.ext-status-fails');
  if (fails) {
    var history = _extDetail[name] || [];
    if (!history.length) {
      fails.innerHTML = '';
    } else {
      fails.innerHTML = '<div class="ext-status-sub">' + escapeHtml(t('ext.status.failures')) + '</div>' +
        history.slice().reverse().map(function (f) {
          return '<div class="ext-fail-item"><span class="ext-fail-stage">' + escapeHtml(f.stage || '') + '</span>' +
            '<span class="ext-fail-time">' + escapeHtml(fmtTime(f.at)) + '</span>' +
            '<span class="ext-fail-reason">' + escapeHtml(f.reason || '') + '</span></div>';
        }).join('');
    }
  }

  var retryBtn = win.querySelector('.ext-win-retry');
  if (retryBtn) retryBtn.disabled = !(row.status === 'unavailable' || row.status === 'probing' || row.status === 'idle');
  var toggleBtn = win.querySelector('.ext-win-toggle');
  if (toggleBtn) toggleBtn.textContent = row.enabled ? t('ext.status.disable') : t('ext.status.enable');
  /* A taller readout is still the card the click asked for, in the middle. */
  if (!win._extTaken) recentreShellWindowForSize(win);
}

/* loadExtensionHistory reads the failure history the frame does not carry. The
 * card is already up — the row it was opened from is the verdict — so a failed
 * read costs the history, not the window. */
function loadExtensionHistory(name) {
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name))
    .then(function (r) {
      if (!r.ok) throw new Error(r.status);
      return r.json();
    })
    .then(function (j) {
      _extDetail[name] = j.failures || [];
      var win = extWinFor(name);
      if (win) paintExtensionWindow(win, name);
    })
    .catch(function () {});
}

/* Every open card repaints from the frame, and from a language switch. */
function extRepaintWindows() {
  allShellWins().forEach(function (w) {
    if (w._extName) paintExtensionWindow(w, w._extName);
  });
}

/* openExtensionDialog is the configuration, and only that: a create (name null,
 * the name editable, a documented default in the box) or an edit of a stored
 * template, which is the TOML file the store holds. */
function openExtensionDialog(name) {
  var modal = extModalEl('modal-ext');
  if (!modal) return;
  _extDialogName = name || null;
  showExtError('');
  var title = extModalEl('modal-ext-title');
  if (title) title.textContent = name || t('ext.modal.title');
  var nameEl = extModalEl('ext-f-name');
  if (nameEl) {
    nameEl.readOnly = !!name;
    nameEl.value = name || '';
  }
  /* Removal is a stored template's business: a create has nothing to remove. */
  var delBtn = extModalEl('modal-ext-delete');
  if (delBtn) delBtn.style.display = name ? '' : 'none';
  modal.classList.remove('hidden');
  if (name) {
    loadExtensionTemplate(name);
  } else {
    paintExtensionHoles(null, null);
    var area = extModalEl('ext-f-toml');
    if (area) area.value = extTomlSeed();
    if (nameEl) nameEl.focus();
  }
}

function closeExtensionDialog() {
  var modal = extModalEl('modal-ext');
  if (modal) modal.classList.add('hidden');
  _extDialogName = null;
  showExtError('');
}

function showExtError(msg) {
  var err = extModalEl('modal-ext-err');
  if (!err) return;
  err.textContent = msg || '';
  err.style.display = msg ? 'block' : 'none';
}

/* ---- The card's readout ----------------------------------------------------- */

/* A labelled row of the card. The value is monospace because most of them are
   identifiers an author copies into a tool call. */
function extStatusRow(label, value, copyable) {
  if (!value) return '';
  var copy = copyable
    ? '<button type="button" class="ext-copy-btn" data-copy="' + escapeHtml(value) + '" title="' +
      escapeHtml(t('ext.status.copy')) + '" aria-label="' + escapeHtml(t('ext.status.copy')) + '">' + SVG_COPY_12 + '</button>'
    : '';
  return '<div class="ext-status-row"><span class="ext-status-label">' + escapeHtml(label) + '</span>' +
    '<span class="ext-status-value">' + escapeHtml(value) + '</span>' + copy + '</div>';
}

/* Other extensions served by the same connection. The session is shared per host,
   and saying so is what explains why an agent's shell_id is not the session the
   listing names. */
function extSharingNote(row) {
  if (!row.session_id) return '';
  var names = (_extRows || []).filter(function (e) {
    return e && e.session_id === row.session_id && e.name !== row.name;
  }).map(function (e) { return e.name; });
  if (!names.length) return '';
  return t('ext.status.shared', { names: names.join(', ') });
}

/* ---- The configuration page ------------------------------------------------- */

/* TWO THINGS, AND NEITHER IS A FILE NAME.
 *
 * The template is the document: what the extension IS, with holes where this
 * machine has to answer. The values are this installation's answers, one per hole.
 * Which file either lands in is the store's business — a person filling in a
 * hostname has no reason to learn that it went into config.local.toml, and an
 * agent that had to know would be one more thing to get wrong.
 *
 * A template with unfilled holes is a normal, finished-looking state: the card
 * shows what it still needs, and this form is where it gets it. Nothing here
 * deletes a value for being wrong; a value that does not render yet is kept and
 * the reason is shown, because the value is the only copy of what was typed. */
var _extHoles = [];

function extHoleInputId(name) { return 'ext-hole-' + String(name).replace(/[^A-Za-z0-9_-]/g, '_'); }

/* paintExtensionHoles draws one row per hole the template asks for: what it is,
   what this installation answered, and a box the right size for its type. */
function paintExtensionHoles(name, holes) {
  _extHoles = holes || [];
  var box = extModalEl('ext-holes');
  if (!box) {
    // Built here rather than in index.html, which is at its size budget: the
    // dialog's own document is the textarea, and this is the part that goes under it.
    box = document.createElement('div');
    box.id = 'ext-holes';
    box.className = 'ext-holes';
    var anchor = extModalEl('ext-f-toml');
    if (!anchor || !anchor.parentNode) return;
    anchor.parentNode.insertBefore(box, anchor.nextSibling);
  }
  if (!name || !_extHoles.length) {
    box.innerHTML = '';
    box.hidden = true;
    return;
  }
  box.hidden = false;
  var head = '<div class="ext-holes-head">' + escapeHtml(t('ext.holes.title')) + '</div>' +
    '<p class="conn-check-hint">' + escapeHtml(t('ext.holes.hint')) + '</p>';
  box.innerHTML = head + _extHoles.map(function (h) {
    var id = extHoleInputId(h.name);
    var label = '<label for="' + id + '"><code>{{' + escapeHtml(h.name) + '}}</code>' +
      (h.help ? ' — ' + escapeHtml(h.help) : '') + '</label>';
    var input;
    if (h.type === 'boolean') {
      input = '<input id="' + id + '" type="checkbox" data-hole="' + escapeHtml(h.name) +
        '" data-hole-type="boolean"' + (h.value === true ? ' checked' : '') + '>';
    } else if (h.type === 'array' || h.type === 'table') {
      input = '<textarea id="' + id + '" rows="4" spellcheck="false" data-hole="' + escapeHtml(h.name) +
        '" data-hole-type="' + h.type + '">' +
        escapeHtml(h.value === undefined ? '' : JSON.stringify(h.value)) + '</textarea>';
    } else if (h.type === 'integer' || h.type === 'number') {
      input = '<input id="' + id + '" type="number" step="' + (h.type === 'integer' ? '1' : 'any') +
        '" data-hole="' + escapeHtml(h.name) + '" data-hole-type="' + h.type + '" value="' +
        escapeHtml(h.value === undefined ? '' : String(h.value)) + '">';
    } else {
      input = '<input id="' + id + '" type="text" spellcheck="false" data-hole="' + escapeHtml(h.name) +
        '" data-hole-type="string" value="' + escapeHtml(h.value === undefined ? '' : String(h.value)) + '">';
    }
    return '<div class="ext-hole' + (h.set ? '' : ' is-empty') + '">' + label + input + '</div>';
  }).join('');
  box.querySelectorAll('[data-hole]').forEach(function (el) {
    el.addEventListener('change', function () { saveExtensionValue(name, el); });
  });
}

/* readHoleInput turns what the box holds into the TOML value the template expects.
   A box that cannot be parsed is not saved and not cleared: the row keeps what was
   typed and says what is wrong with it. */
function readHoleInput(el) {
  var kind = el.getAttribute('data-hole-type');
  if (kind === 'boolean') return el.checked;
  var raw = String(el.value || '').trim();
  if (raw === '') return null;   // null removes the value, which is "unset" again
  if (kind === 'integer') {
    var i = Number(raw);
    if (!Number.isInteger(i)) throw new Error(t('ext.holes.badInteger'));
    return i;
  }
  if (kind === 'number') {
    var n = Number(raw);
    if (!isFinite(n)) throw new Error(t('ext.holes.badNumber'));
    return n;
  }
  if (kind === 'array' || kind === 'table') {
    try { return JSON.parse(raw); } catch (e) { throw new Error(t('ext.holes.badJSON')); }
  }
  return raw;
}

function saveExtensionValue(name, el) {
  var hole = el.getAttribute('data-hole');
  var value;
  try {
    value = readHoleInput(el);
  } catch (err) {
    showExtError((err && err.message) || String(err));
    return;
  }
  showExtError('');
  var body = {};
  body[hole] = value;
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name), {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ values: body })
  }).then(function (r) {
    /* A 400 STILL STORED THE VALUE. The store keeps what was typed and reports why
       the extension cannot start yet; the message is the second half of that, and
       the first half is why the box does not clear itself. */
    return r.text().then(function (txt) { return { ok: r.ok, text: txt }; });
  }).then(function (res) {
    if (!res.ok) {
      showExtError(t('ext.holes.notApplied') + ' ' + res.text);
      return;
    }
    /* Repainted from the answer, so "filled in" and "still needed" are the
       server's reading rather than a guess from what was typed. */
    var doc = {};
    try { doc = JSON.parse(res.text) || {}; } catch (e) { doc = {}; }
    paintExtensionHoles(name, doc.holes || _extHoles);
    showExtError('');
  }).catch(function (err) {
    showExtError(t('ext.modal.saveFailed') + ' ' + ((err && err.message) || err));
  });
}

/* loadExtensionTemplate reads the document with its holes, then the holes with
   what this installation has already answered. Two reads because they are two
   facts: the document is what an author wrote, the holes are what is still open. */
function loadExtensionTemplate(name) {
  _extHoles = [];
  var area = extModalEl('ext-f-toml');
  if (area) area.value = '';
  paintExtensionHoles(name, null);
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name) + '?format=toml')
    .then(function (r) {
      if (!r.ok) throw new Error(r.status);
      return r.text();
    })
    .then(function (text) {
      if (_extDialogName !== name) return;
      if (area) area.value = text;
      return fetch(apiPath('/api/extensions/') + encodeURIComponent(name))
        .then(function (r) { return r.ok ? r.json() : {}; })
        .then(function (doc) {
          if (_extDialogName !== name) return;
          paintExtensionHoles(name, doc.holes || []);
        });
    })
    .catch(function () {
      showExtError(t('ext.modal.loadFailed'));
    });
}

/* The document a create starts from. A hole is written as a quoted placeholder —
   `command = "{{command}}"` — because a bare `{{command}}` is not TOML and a
   package has to parse before anybody has filled anything in. The [params] block
   is what tells a reader (and the panel) what the hole is for. */
function extTomlSeed() {
  return [
    '# What this extension IS. Holes are written as "{{name}}" and declared under',
    '# [params.<name>]; Termcp asks this installation for them and keeps the answers',
    '# in config.local.toml, which is never part of an export.',
    'description = "what this extension is for"',
    'target = "internal"',
    'mode = "pipe"',
    'command = "{{command}}"',
    '',
    '# A read-only window onto remote files instead of a program: paths = ["/srv/docs/"]',
    '# An HTTP MCP endpoint instead of a program: url = "https://mcp.example.com/mcp"',
    '',
    '[params.command]',
    'type = "string"',
    'help = "The program that serves MCP over the pipe (its arguments are `args`)"',
    ''
  ].join('\n');
}

/* extSaveTemplate writes the document. The values are NOT part of this call: they
   are saved one at a time, as they are typed, and replacing the template must not
   disturb them — that separation is the whole reason a package can be upgraded. */
function extSaveTemplate() {
  var name = _extDialogName;
  var forCreate = !name;
  var area = extModalEl('ext-f-toml');
  var body = area ? area.value : '';
  if (!body.trim()) {
    showExtError(t('ext.modal.tomlEmpty'));
    return;
  }
  var query = '';
  if (forCreate) {
    var nameEl = extModalEl('ext-f-name');
    var newName = nameEl ? nameEl.value.trim() : '';
    if (!newName) {
      showExtError(t('ext.modal.nameRequired'));
      return;
    }
    query = '?name=' + encodeURIComponent(newName);
  }
  var saveBtn = extModalEl('modal-ext-save');
  if (saveBtn) saveBtn.disabled = true;
  fetch(forCreate ? apiPath('/api/extensions') + query
                  : apiPath('/api/extensions/') + encodeURIComponent(name), {
    method: forCreate ? 'POST' : 'PUT',
    headers: { 'Content-Type': 'application/toml; charset=utf-8' },
    body: body
  }).then(function (r) {
    if (saveBtn) saveBtn.disabled = false;
    if (!r.ok) {
      return r.text().then(function (txt) { throw new Error(txt || r.status); });
    }
    if (forCreate) {
      /* THE CREATE BECOMES AN EDIT, in place: the document is stored, and what it
         asks for is now a fact this form can show. Closing here would leave the
         holes to be discovered on the card, which is where a reader looks at a
         result rather than where it finishes an install. */
      var created = String((extModalEl('ext-f-name') || {}).value || '').trim();
      _extDialogName = created;
      var nameInput = extModalEl('ext-f-name');
      if (nameInput) nameInput.readOnly = true;
      var delBtn = extModalEl('modal-ext-delete');
      if (delBtn) delBtn.style.display = '';
      var title = extModalEl('modal-ext-title');
      if (title) title.textContent = created;
      loadExtensionTemplate(created);
      return;
    }
    /* An edit closes: the next frame repaints the card, and the card is where the
       result of a change to the document is read. */
    closeExtensionDialog();
  }).catch(function (err) {
    if (saveBtn) saveBtn.disabled = false;
    showExtError(t('ext.modal.saveFailed') + ' ' + ((err && err.message) || err));
  });
}

/* Park or wake a template. The reconciler does the rest: a parked extension's
 * channel and tunnels are closed, an enabled one is brought up again. */
function setExtensionEnabled(name, enabled, btn) {
  if (!name) return;
  if (btn) btn.disabled = true;
  fetch(apiPath('/api/extensions/') + encodeURIComponent(name), {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled: enabled })
  }).then(function (r) {
    if (btn) btn.disabled = false;
    if (!r.ok) return r.text().then(function (txt) { throw new Error(txt || r.status); });
  }).catch(function (err) {
    if (btn) btn.disabled = false;
    showCopyToast(t('ext.toast.saveFailed', { msg: String((err && err.message) || err) }));
  });
}

/* Delete is a confirm, because the template is a file but the extension is a
 * connection the reconciler was holding warm — the author is removing both. */
function deleteOneExtension(name, win) {
  if (!name) return;
  confirmDialog({
    title: t('ext.delete.title'),
    message: t('ext.delete.confirm', { name: name })
  }).then(function (ok) {
    if (!ok) return;
    fetch(apiPath('/api/extensions/') + encodeURIComponent(name), { method: 'DELETE' })
      .then(function (r) {
        if (!r.ok && r.status !== 204) {
          return r.text().then(function (txt) { throw new Error(txt || r.status); });
        }
        if (win) closeShellWindow(win);
        closeExtensionDialog();
      })
      .catch(function (err) {
        showCopyToast(t('toast.delete.failed', { msg: String((err && err.message) || err) }));
      });
  });
}

/* ---- The add split key -------------------------------------------------------
 * The hosts column's split key, used for the same reason: one key whose wide half
 * runs the common verb and whose caret holds the rarer one. Here the face creates
 * a blank extension and the menu imports a file — authoring one is what people do
 * most, importing a set is the occasional door.
 *
 * The menu is a SIBLING of the plate, not a child of it, because the plate carries
 * the shared clip-path and a clip cuts descendants too — the same trap the hosts
 * column documents. Escape closes the menu first and does nothing else; the
 * column has no mode to leave. */
function closeExtAddMenu() {
  var m = document.getElementById('ext-add-menu');
  var c = document.getElementById('ext-add-caret');
  if (m) m.hidden = true;
  if (c) c.setAttribute('aria-expanded', 'false');
}

(function () {
  var caret = document.getElementById('ext-add-caret');
  var menu = document.getElementById('ext-add-menu');
  if (!caret || !menu) return;
  caret.addEventListener('click', function (e) {
    e.preventDefault();
    e.stopPropagation();
    menu.hidden = !menu.hidden;
    caret.setAttribute('aria-expanded', String(!menu.hidden));
  });
  menu.addEventListener('click', function (e) { e.stopPropagation(); });
  document.addEventListener('click', function (e) {
    if (menu.hidden) return;
    if (e.target && e.target.closest &&
        (e.target.closest('#ext-add-menu') || e.target.closest('#ext-add-caret'))) return;
    closeExtAddMenu();
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && !menu.hidden) closeExtAddMenu();
  });
})();

/* ---- Import ----------------------------------------------------------------
 * The dialog and the readout are the connection import's, over the extension
 * document: one file, no overwrites, and the names that had to move are listed
 * because that is the only thing the author has to be told about. */
(function () {
  /* Import lives in the add key's dropdown now: creating one and importing a set
     are the two ways an extension enters the list, so they are one control with
     two doors rather than two keys competing for the bar's width. */
  var open = document.getElementById('ext-menu-import');
  if (!open) return;
  open.addEventListener('click', function (e) {
    e.stopPropagation();
    closeExtAddMenu();
    var btn = document.getElementById('ext-import-run');
    if (btn) btn.disabled = false;
    var file = document.getElementById('ext-import-file');
    if (file) file.value = '';
    var err = document.getElementById('ext-import-err');
    if (err) { err.style.display = 'none'; err.textContent = ''; }
    var out = document.getElementById('ext-import-result');
    if (out) { out.style.display = 'none'; out.textContent = ''; }
    showModal('modal-ext-import');
  });
  var close = document.getElementById('ext-import-close');
  if (close) close.addEventListener('click', function () { hideModal('modal-ext-import'); });
  var run = document.getElementById('ext-import-run');
  if (run) run.addEventListener('click', function () {
    var input = document.getElementById('ext-import-file');
    var err = document.getElementById('ext-import-err');
    var out = document.getElementById('ext-import-result');
    if (err) err.style.display = 'none';
    if (out) out.style.display = 'none';
    var file = input && input.files && input.files[0];
    if (!file) {
      if (err) { err.textContent = t('conn.batch.selectFile'); err.style.display = 'block'; }
      return;
    }
    var btn = this;
    btn.disabled = true;
    // The file is opaque to the UI; the backend parses and validates its TOML.
    fetch(apiPath('/api/extensions/batch'), {
      method: 'POST', headers: { 'Content-Type': 'application/toml' }, body: file
    }).then(function (r) {
      if (!r.ok) return r.text().then(function (message) { throw new Error(message || String(r.status)); });
      return r.json();
    }).then(function (result) {
      if (result.renamed && result.renamed.length) {
        var lines = [t('ext.batch.imported', { count: result.imported })];
        result.renamed.forEach(function (entry) {
          lines.push(t('conn.batch.renamed', { from: entry.from, to: entry.to }));
        });
        if (out) {
          out.textContent = lines.join('\n');
          out.style.display = 'block';
        }
        if (input) input.value = '';
      } else {
        hideModal('modal-ext-import');
        showCopyToast(t('ext.batch.imported', { count: result.imported }));
      }
    }).catch(function (e) {
      if (err) { err.textContent = String((e && e.message) || e); err.style.display = 'block'; }
    }).finally(function () { btn.disabled = false; });
  });
})();
/* ---- Panel wiring ---------------------------------------------------------- */
(function () {
  var add = document.getElementById('ext-add');
  if (add) {
    /* A real <button> now (it is the wide half of a split key), so Enter and Space
       already click it — a keydown shim here would fire the dialog twice. */
    add.addEventListener('click', function () { openExtensionDialog(null); });
  }

  var modal = extModalEl('modal-ext');
  if (!modal) return;
  extModalEl('modal-ext-close').addEventListener('click', closeExtensionDialog);
  extModalEl('modal-ext-cancel').addEventListener('click', closeExtensionDialog);
  extModalEl('modal-ext-save').addEventListener('click', extSaveTemplate);
  var modalDelete = extModalEl('modal-ext-delete');
  if (modalDelete) modalDelete.addEventListener('click', function () {
    var name = _extDialogName;
    if (!name) return;
    deleteOneExtension(name, extWinFor(name));
  });
  modal.addEventListener('click', function (e) {
    if (e.target === modal) closeExtensionDialog();
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (!modal.classList.contains('hidden')) {
      e.stopImmediatePropagation();
      closeExtensionDialog();
    }
  }, true);
})();
