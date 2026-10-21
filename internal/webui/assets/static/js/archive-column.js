/* termcp Web UI — the archive column.
 *
 * The right-hand column holds the sessions that are OVER, and it wears the
 * extension panel's frame: a trigger key, a body that folds away, and a rail of
 * lamps while it is folded. The frame is this page's vocabulary for "a column
 * you put away" — and the archive is exactly that kind of column: read, not
 * driven, consulted rather than worked in.
 *
 * The two columns swapped contents, which is why the collapse machinery lives
 * here rather than in extensions.js: what used to be the extension column's
 * state is now this column's, and the extension plate in the centre has no
 * fold of its own (it is the page's main region).
 *
 * The rail is a LAMP PER ARCHIVED SESSION, the same readout the extension
 * column used to carry: a folded column still has to answer "is there anything
 * in here", and a bare glyph cannot say whether the archive is empty.
 */
var ARCHIVE_STATE_KEY = 'termcp.archive';

function storedArchiveCollapsed() {
  var v = null;
  try { v = localStorage.getItem(ARCHIVE_STATE_KEY); } catch (e) {}
  if (v === 'collapsed') return true;
  if (v === 'expanded') return false;
  return null;
}

/* Mirrors the stylesheet's breakpoint. Below it the body is fixed and slides, so
   the state means closed rather than railed — and the frame's collapsed class is
   kept off it, or the third grid track would still be a rail on a page that has
   no grid columns at all. */
function archiveIsDrawer() {
  return (window.innerWidth || 0) < 800;
}

/* The column starts FOLDED at every width, and that is the point of the archive:
   these sessions are over. Nothing in here is urgent, nothing can be typed into,
   and the work it would take pixels from is the extensions beside it — so it
   waits behind a rail that says how much is waiting. Opening it is a deliberate
   act, and the choice is remembered once made. */
function archiveDefaultCollapsed() {
  return true;
}

function setArchiveCollapsed(collapsed, persist) {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('extpanel-body');
  var trigger = document.getElementById('extpanel-toggle');
  if (!frame || !body) return;
  collapsed = !!collapsed;
  var drawer = archiveIsDrawer();
  /* The two drawers share one scrim and one page, so the one being opened puts
     the other away. The call is guarded by "am I opening" rather than fired on
     every state write, and closing never calls across, so the pair cannot
     recurse into each other. */
  if (drawer && !collapsed && !netHubCollapsed()) setNetHubCollapsed(true, false);
  frame.classList.toggle('ext-collapsed', collapsed && !drawer);
  body.classList.toggle('drawer-open', drawer && !collapsed);
  /* The scrim and the page lock follow BOTH drawers (paintDrawerScrim in
     dialogs.js), so folding this column never drops the overlay the connections
     drawer is still depending on. */
  if (typeof paintDrawerScrim === 'function') paintDrawerScrim();
  if (trigger) trigger.setAttribute('aria-expanded', String(!collapsed));
  paintArchiveTrigger();
  /* The drawer scope never writes the preference: below the breakpoint the
     column is an overlay, and dismissing an overlay is not a statement about how
     the desktop column should sit. */
  if (persist && !drawer) {
    try { localStorage.setItem(ARCHIVE_STATE_KEY, collapsed ? 'collapsed' : 'expanded'); } catch (e) {}
  }
}

function archiveCollapsed() {
  var frame = document.querySelector('.workspace-frame');
  var body = document.getElementById('extpanel-body');
  if (archiveIsDrawer()) return !(body && body.classList.contains('drawer-open'));
  return !!(frame && frame.classList.contains('ext-collapsed'));
}

function currentArchiveCollapsed() {
  var stored = storedArchiveCollapsed();
  return stored === null ? archiveDefaultCollapsed() : stored;
}

/* A drawer is an overlay over the page, and an overlay does not open itself.
   "Expanded" stored on a wide screen means the column is out; carried below the
   breakpoint it would mean a drawer over a page that cannot scroll behind it,
   with a scrim nobody asked for. */
function initialArchiveCollapsed() {
  return archiveIsDrawer() ? true : currentArchiveCollapsed();
}

function toggleArchive() {
  setArchiveCollapsed(!archiveCollapsed(), true);
}

/* The drawer close key and the drawer's own x both reach for this, so the column
   has one door from the inside as well as out. */
window.termcpToggleArchive = function (open) {
  setArchiveCollapsed(open === false, true);
};

/* The folded rail's readout: how many sessions are archived.
 *
 *  It is a NUMBER, not a lamp per session. A row of lamps was the extension
 *  column's readout for a good reason — an extension up or down is a state you
 *  act on — but every archived session is in the same state (over), so lamps
 *  would be decoration that always shows the same colour. What the rail has to
 *  answer is "is there anything in here, and how much", which is a count.
 */
function paintArchiveRail() {
  var el = document.getElementById('extpanel-count');
  if (!el) return;
  var grid = document.getElementById('archive-grid');
  var n = grid ? grid.querySelectorAll('.archive-row').length : 0;
  el.textContent = n > 99 ? '99+' : String(n);
  el.hidden = n === 0;
  paintArchiveTrigger(n);
}

/** The trigger's own label.
 *
 *  One key, two states, and it names the move rather than the object: open, it
 *  offers to fold; folded, to open, and the count rides along when there is one.
 *  That is the same vocabulary the chevron speaks, so the tooltip and the glyph
 *  cannot disagree about which way the column is about to go. */
function paintArchiveTrigger(n) {
  var btn = document.getElementById('extpanel-toggle');
  if (!btn) return;
  var open = !!document.querySelector('.workspace-frame:not(.ext-collapsed)');
  var count = n === undefined ? 0 : n;
  btn.title = open
    ? t('archive.fold')
    : (count ? t('archive.count', { count: count }) : t('archive.expand'));
  btn.setAttribute('aria-label', btn.title);
}

/* The cards are repainted from socket frames by sessions.js, so the rail follows
   the grid rather than keeping a second copy of the archive list. */
(function () {
  var grid = document.getElementById('archive-grid');
  if (grid && typeof MutationObserver !== 'undefined') {
    new MutationObserver(paintArchiveRail).observe(grid, { childList: true });
  }
  paintArchiveRail();
})();

/* A viewport change can move the page across a breakpoint in either direction,
   and the stored preference is re-applied, never rewritten — a resize is not a
   decision. Only a crossing is acted on: a resize that stays on one side of the
   breakpoint changes the column's geometry but not its state, and re-applying
   would close a drawer the user is in the middle of using. */
var _archiveWasDrawer = archiveIsDrawer();
var _archiveResizeTimer = 0;
window.addEventListener('resize', function () {
  if (_archiveResizeTimer) clearTimeout(_archiveResizeTimer);
  _archiveResizeTimer = setTimeout(function () {
    _archiveResizeTimer = 0;
    var drawer = archiveIsDrawer();
    if (drawer === _archiveWasDrawer) return;
    var wasDrawer = _archiveWasDrawer;
    _archiveWasDrawer = drawer;
    if (drawer) {
      setArchiveCollapsed(initialArchiveCollapsed(), false);
      return;
    }
    var panel = document.getElementById('extpanel-body');
    var wasOpen = wasDrawer && panel && panel.classList.contains('drawer-open');
    setArchiveCollapsed(wasOpen ? false : currentArchiveCollapsed(), false);
  }, 120);
});

(function () {
  var trigger = document.getElementById('extpanel-toggle');
  if (!trigger) return;
  /* CAPTURE, because this handler must run before anything the trigger contains.
     The invert key briefly nested inside this trigger and had to be exempted here;
     it now lives on the toolbar, so the trigger contains only its own glyphs and the
     capture handler can claim every press unconditionally again. */
  trigger.addEventListener('click', function (e) {
    e.stopImmediatePropagation();
    e.preventDefault();
    toggleArchive();
  }, true);
  trigger.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.stopImmediatePropagation();
    e.preventDefault();
    toggleArchive();
  }, true);

  /* The scrim and Escape close the overlay only. On desktop the scrim is not in
     the layout and the body is a column, so neither can fold a rail the user
     chose — the same guard NetHub's own handlers carry. */
  var scrim = document.querySelector('.drawer-scrim');
  if (scrim) scrim.addEventListener('click', function () { setArchiveCollapsed(true, false); });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (!archiveIsDrawer()) return;
    var body = document.getElementById('extpanel-body');
    if (!body || !body.classList.contains('drawer-open')) return;
    e.stopImmediatePropagation();
    setArchiveCollapsed(true, false);
  }, true);
})();

setArchiveCollapsed(initialArchiveCollapsed(), false);
