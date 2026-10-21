/* termcp Web UI — the batch bar, one implementation for every column that has one.
 *
 * The connections column and the extension column carry the same row of keys over
 * their cards: a switch that enters selection mode — its face becoming the ticked
 * count — an invert key, and a split key whose wide half runs the column's own
 * verb (dial the selection, open the selection's cards) while its caret unfolds
 * export and delete. The two columns differ only in what they list and what that
 * verb means, so they share this component instead of a copy each: the copy
 * drifted the first time one side was edited — the extension column shipped the
 * count without hiding the glyph under it, so a 34px square held both, and the
 * two toolbars no longer behaved alike.
 *
 * A caller supplies the DOM ids, how to list what is selectable, how to repaint
 * its own cards, and the three actions. The component owns the state (the mode and
 * the ticked set), paints the bar, and keeps the selection and the cards in step.
 *
 * Loaded as a plain synchronous <script> like every other module; it must come
 * before the first caller's own script (see index.html).
 */
function createBatchBar(cfg) {
  /* `alwaysOn` starts the column in select mode with no way out of it, because
   * that column has no mode to be in: its keys are flat controls on the header
   * bar, and a card click there ticks rather than opens. Everything downstream —
   * the ticked set, the count, the invert key, the menu's disabled state — is the
   * same code either way; only the switch that enters the mode is gone. */
  var state = { mode: !!cfg.alwaysOn, ids: new Set() };

  function el(id) { return id ? document.getElementById(id) : null; }
  function body() { return el(cfg.bodyId); }
  /* Where the selecting class goes.
   *
   * It used to be the body, because the toolbar lived inside it. A column whose
   * controls sit in its HEADER (the extension plate puts the batch keys on the
   * title bar, beside the fold trigger) needs the class on an ancestor that
   * contains both, or the CSS that reveals the keys would never match. cfg names
   * that host; every other column keeps the body and its current behaviour. */
  function selectingHost() { return el(cfg.selectingHostId) || body(); }
  function names() { return (cfg.names() || []).filter(function (n) { return !!n; }); }
  function count() {
    var n = 0;
    names().forEach(function (name) { if (state.ids.has(name)) n++; });
    return n;
  }
  /* What the column's verb acts on: the ticked names, in the order the grid shows
     them (name order), and nothing at all while the mode is off. */
  function selected() {
    if (!state.mode) return [];
    return names().filter(function (name) { return state.ids.has(name); })
      .sort(function (a, b) { return a.localeCompare(b); });
  }

  /* The switch's face: the count while there is one, the list glyph otherwise.
     Hiding the glyph is the step a copy is most likely to miss, and without it
     the two share a 34px square. */
  function paintToggle(n) {
    /* The count is painted BEFORE the early return, and that is the whole bug this
       structure exists to prevent. It used to sit after `if (!key) return`, so any
       caller with no mode switch — the dock and the archive both run alwaysOn with
       `toggleId: null` — never reached it. Their count element kept whatever the
       markup shipped ("0") and stayed `hidden`, so ticking a tab updated the
       selection and left the readout saying zero forever. A number that never
       changes is worse than no number: it actively contradicts the list beside it.
     *
     * The count is not a property of the switch. It is the size of the selection,
     * and every caller that wants to show one passes a countId. */
    var countEl = el(cfg.countId);
    if (countEl) {
      countEl.hidden = !(state.mode && n > 0);
      countEl.textContent = String(n);
    }
    var key = el(cfg.toggleId);
    if (!key) return;
    var showCount = state.mode && n > 0;
    var icon = key.querySelector('svg');
    if (icon) icon.style.display = showCount ? 'none' : 'block';
    key.setAttribute('aria-pressed', String(state.mode));
    key.setAttribute('aria-label', showCount
      ? t('batch.count.selected', { count: n })
      : t(cfg.selectKey));
  }

  /* The invert key flips EVERY row, and its face reports the state it would leave
     behind when the list is uniform.
   *
   * It used to be select-all / clear-all: press it with nothing ticked and the whole
   * list lit up, press it again and the list went dark. That is a toggle of one
   * boolean dressed as a list operation — it cannot express "these four, not those
   * two", which is the thing a partial selection is FOR. Inverting per row does:
   * whatever is ticked becomes unticked and vice versa, so a hand-picked subset can
   * be flipped in one press instead of being thrown away.
   *
   * The label still names the SHORTCUT, not the outcome, because "invert" has no
   * colloquial face: when everything is already ticked it reads as the clear it
   * would perform, and otherwise as the select-all it looks like. Anything partial
   * is just "invert". */
  function paintInvert(n) {
    var invert = el(cfg.invertId);
    if (!invert) return;
    var all = names();
    var allSelected = all.length > 0 && n === all.length;
    var noneSelected = n === 0;
    invert.disabled = all.length === 0;
    invert.classList.toggle('all-checked', allSelected);
    var label;
    if (allSelected) label = t('section.batch.clearSelection');
    else if (noneSelected) label = t('section.batch.selectAll');
    else label = t('section.batch.invert');
    invert.title = label;
    invert.setAttribute('aria-label', label);
  }

  /* The verb and the menu answer disabled while nothing is ticked: an always-live
     key that does nothing reads as broken. */
  function paintActions(n) {
    var split = el(cfg.splitId);
    if (split) split.classList.toggle('is-disabled', n === 0);
    (cfg.actionIds || []).forEach(function (id) {
      var item = el(id);
      if (item) item.disabled = n === 0;
    });
  }

  function update() {
    var n = count();
    paintToggle(n);
    paintInvert(n);
    paintActions(n);
    var host = selectingHost();
    if (host) host.classList.toggle('has-selection', n > 0);
    /* The caller's own pass, for the parts of a column's dressing the component
       cannot know: a flat bar hides its action keys outright (there is no switch
       to sit a disabled one beside), while a mode-switched row keeps them visible
       and disabled. Called LAST so the caller sees the painted state, not the one
       it is about to replace. */
    if (cfg.onUpdate) cfg.onUpdate(n);
  }

  /* Paint the ticked look from the set, which is the source of truth: this both
     marks the ticked cards and strips the class from every card the set no longer
     holds.
     prune drops ids the list has lost (a card deleted under the selection) and is
     the caller's to decide: a list that has not arrived yet is empty, and pruning
     against it would quietly throw away a tick someone just made. */
  function syncCards(prune) {
    if (prune) {
      var live = names();
      state.ids.forEach(function (name) { if (live.indexOf(name) < 0) state.ids.delete(name); });
    }
    var grid = el(cfg.gridId);
    if (!grid) return;
    grid.querySelectorAll(cfg.cardSelector).forEach(function (tile) {
      var name = tile.getAttribute(cfg.nameAttr);
      tile.classList.toggle('batch-selected', state.mode && state.ids.has(name));
    });
  }

  /* The per-card tooltip is the one bit of a card that follows the mode. */
  function refreshTitles() {
    var grid = el(cfg.gridId);
    if (!grid || !cfg.tipFor) return;
    grid.querySelectorAll(cfg.cardSelector).forEach(function (tile) {
      var inner = tile.querySelector('.entry-card-inner');
      if (!inner) return;
      var name = tile.getAttribute(cfg.nameAttr);
      inner.title = state.mode ? t('nethub.batch.selectOne', { name: name }) : cfg.tipFor(name);
    });
  }

  function repaint() { if (cfg.render) cfg.render(); }

  function setMode(on) {
    if (cfg.alwaysOn) on = true;   /* there is no off state to switch to */
    state.mode = !!on;
    if (!state.mode) state.ids.clear();
    var host = selectingHost();
    if (host) host.classList.toggle(cfg.selectingClass, state.mode);
    if (!state.mode) closeMenu();
    syncCards();
    refreshTitles();
    update();
  }

  /* Ticking is a change to the SET, and the card's class is derived from it.
   *
   * The optional `tile` used to be the only thing that painted: passing it wrote the
   * class on that one element and skipping it wrote nothing, so a caller that ticked
   * by name left the set and the view disagreeing. The window header's monitor glyph
   * does exactly that — it knows the window, not which tab in the dock represents it
   * — and ticking from there changed the count while every tab stayed unmarked.
   *
   * A tile is now only a hint for the immediate repaint; the authoritative pass comes
   * from syncCards, which finds the cards by the configured selector. That keeps one
   * writer for the class and makes ticking by name work, which is what a second view
   * of the same control needs. */
  function toggleSelection(name, tile) {
    if (!name) return;
    if (state.ids.has(name)) state.ids.delete(name);
    else state.ids.add(name);
    if (tile) tile.classList.toggle('batch-selected', state.mode && state.ids.has(name));
    syncCards();
    update();
  }

  /* The caret's dropdown, and the two ways out of it: a click anywhere else, and
     Escape — which folds the menu first and the mode second, so one key walks back
     one step at a time instead of dropping both. */
  function menuOpen() { var m = el(cfg.menuId); return !!(m && !m.hidden); }
  function closeMenu() {
    var was = menuOpen();
    var m = el(cfg.menuId);
    var caret = el(cfg.caretId);
    if (m) m.hidden = true;
    if (caret) caret.setAttribute('aria-expanded', 'false');
    return was;
  }
  function toggleMenu() {
    var m = el(cfg.menuId);
    if (!m) return;
    m.hidden = !m.hidden;
    var caret = el(cfg.caretId);
    if (caret) caret.setAttribute('aria-expanded', String(!m.hidden));
  }

  (function wire() {
    var key = el(cfg.toggleId);
    if (key) key.addEventListener('click', function (e) { e.stopPropagation(); setMode(!state.mode); });

    var invert = el(cfg.invertId);
    if (invert) {
      /* Invert flips each name INDEPENDENTLY. It was a select-all/clear-all toggle:
         one boolean dressed as a list operation, which cannot express "these four,
         not those two" — the only reason to pick a subset by hand.
         stopPropagation matters more than usual now that this key may be nested
         inside the column's own trigger: without it, inverting would also fold the
         archive shut. */
      var doInvert = function (e) {
        e.stopPropagation();
        names().forEach(function (name) {
          if (state.ids.has(name)) state.ids.delete(name);
          else state.ids.add(name);
        });
        syncCards();
        update();
      };
      /* A real <button> in every caller, so the keyboard is already handled: the
         browser turns Enter and Space into a click. It was a <span role="button">
         while the archive nested it inside the column's trigger (a button inside a
         button is invalid HTML), and that is gone — re-adding a `keydown` handler
         here would make one press invert TWICE, once directly and once through the
         synthesized click. */
      invert.addEventListener('click', doInvert);
    }

    var caret = el(cfg.caretId);
    if (caret) caret.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      toggleMenu();
    });
    var menu = el(cfg.menuId);
    if (menu) menu.addEventListener('click', function (e) { e.stopPropagation(); });

    var main = el(cfg.mainId);
    if (main) main.addEventListener('click', function (e) {
      e.stopPropagation();
      if (cfg.onOpen) cfg.onOpen(selected());
    });
    var exportKey = el(cfg.exportId);
    if (exportKey) exportKey.addEventListener('click', function () {
      closeMenu();
      if (cfg.onExport) cfg.onExport(selected());
    });
    var deleteKey = el(cfg.deleteId);
    if (deleteKey) deleteKey.addEventListener('click', function () {
      closeMenu();
      if (cfg.onDelete) cfg.onDelete(selected());
    });

    document.addEventListener('click', function (e) {
      if (!menuOpen()) return;
      if (e.target && e.target.closest &&
          (e.target.closest('#' + cfg.menuId) || e.target.closest('#' + cfg.caretId))) return;
      closeMenu();
    });

    /* Escape walks back one step — the menu first, the mode second — and only for
       the column the key belongs to: two panels both listen, and folding the other
       column's mode because this one had a menu open is exactly the cross-talk a
       shared component should not introduce. "Belongs to" is the panel holding the
       focus, or nowhere in particular (a body with no focus): a key pressed while
       the user is working in the other column is that column's. */
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape') return;
      if (!state.mode && !menuOpen()) return;
      var t = e.target;
      /* Tested against the selecting host, not the body: a column whose keys live
         in its header would otherwise ignore Escape typed while one of those keys
         has focus, and the mode would only be escapable from inside the list. */
      var b = selectingHost();
      var loose = !t || t === document || t === document.body || t === document.documentElement;
      if (b && t && !loose && !b.contains(t)) return;
      e.stopImmediatePropagation();
      if (closeMenu()) return;
      setMode(false);
    }, true);
  })();

  update();
  return {
    isOn: function () { return state.mode; },
    count: count,
    has: function (name) { return state.mode && state.ids.has(name); },
    setMode: setMode,
    toggleSelection: toggleSelection,
    selected: selected,
    /* `deselect` and `clear` repaint what they changed. Leaving that to the caller
       made the set and the DOM two things to keep in step by hand, and one of the
       four call sites forgot: the selection was gone from the set while the cards
       stayed ticked, so the bar read "nothing selected" over a list of six
       ticked rows. A mutator that leaves its own view stale is a trap, not an
       optimisation. */
    deselect: function (name) { state.ids.delete(name); syncCards(); update(); },
    clear: function () { state.ids.clear(); syncCards(); update(); },
    update: update,
    syncCards: syncCards,
    refreshTitles: refreshTitles,
    repaint: repaint,
    closeMenu: closeMenu,
    menuOpen: menuOpen,
  };
}
