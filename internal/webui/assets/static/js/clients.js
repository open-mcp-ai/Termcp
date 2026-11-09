/**
 * Connected clients — who is attached to this instance (issue #99).
 *
 * Two kinds of caller, reported side by side and never merged:
 *
 *   MCP    an agent driving sessions. It announced a name during initialize
 *          ("claude-ai 0.1.0"), and it may have claimed a namespace, which is
 *          what tells you *which* agent it is when several share this instance.
 *   Web UI a browser tab holding this page's socket, with the address it came
 *          from and how many sessions it is streaming.
 *
 * They are different transports with different lifetimes, so they get different
 * rows rather than one shape with a kind discriminator and half its fields empty.
 *
 * The data is pushed, not polled: both lists ride the session frame, which the
 * server already sends when a session, a client or a viewer changes. This module
 * therefore only renders what it is handed.
 */

/** Render one row. Built from textContent, never innerHTML: a client name and a
 *  user agent are both attacker-controlled strings. */
function clientRow(kind, title, meta, detail) {
  var row = document.createElement('div');
  row.className = 'clients-row';
  row.setAttribute('role', 'listitem');

  var badge = document.createElement('span');
  badge.className = 'clients-badge clients-badge-' + kind;
  badge.textContent = kind === 'mcp' ? 'MCP' : 'WEB';
  row.appendChild(badge);

  var body = document.createElement('div');
  body.className = 'clients-body';
  var name = document.createElement('div');
  name.className = 'clients-name';
  name.textContent = title;
  body.appendChild(name);

  if (meta) {
    var metaEl = document.createElement('div');
    metaEl.className = 'clients-meta';
    metaEl.textContent = meta;
    body.appendChild(metaEl);
  }
  if (detail) {
    var detailEl = document.createElement('div');
    detailEl.className = 'clients-detail';
    detailEl.textContent = detail;
    body.appendChild(detailEl);
  }
  row.appendChild(body);
  return row;
}

/** A client that never said its name is still a client: the spec makes clientInfo
 *  optional, so the fallback names the fact rather than inventing an identity. */
function mcpClientTitle(c) {
  if (c && c.agent_name) return String(c.agent_name);
  var name = c && c.name ? String(c.name) : t('clients.unnamed');
  var version = c && c.version ? ' ' + String(c.version) : '';
  return name + version;
}

function mcpTransportLabel(transport) {
  if (transport === 'streamable_http') return t('clients.transport.streamableHttp');
  if (transport === 'sse') return t('clients.transport.sse');
  return transport ? String(transport) : '';
}

function shortMCPConnectionID(value) {
  var text = String(value || '');
  return text.length > 12 ? text.slice(0, 12) + '…' : text;
}

/** The client name is metadata once the server has a verified Agent identity;
 *  keeping both lets a reader distinguish two bridges of the same Agent. */
function mcpClientMeta(c) {
  var parts = [];
  var name = c && c.name ? String(c.name) : '';
  var version = c && c.version ? String(c.version) : '';
  if (name) parts.push(version ? name + ' ' + version : name);
  var transport = mcpTransportLabel(c && c.transport);
  if (transport) parts.push(transport);
  if (c && c.client_mode === 'stdio-bridge') parts.push(t('clients.stdioBridge'));
  if (c && c.connected_at) parts.push(t('clients.since', { time: shortClock(c.connected_at) }));
  return parts.join(' · ');
}

/** Token and transport session are independent identifiers: the Agent owns the
 *  former, while one Agent can hold several live protocol sessions. */
function mcpClientDetail(c) {
  var parts = [];
  if (c && c.credential_id) parts.push(t('clients.token', { id: shortMCPConnectionID(c.credential_id) }));
  if (c && c.session_id) parts.push(t('clients.session', { id: shortMCPConnectionID(c.session_id) }));
  if (c && !c.agent_name && c.namespace) parts.push(t('clients.namespace', { ns: String(c.namespace) }));
  return parts.join(' · ');
}

/** A viewer's browser, shortened to something that fits a narrow panel. The
 *  user agent is the only thing distinguishing two tabs from one machine. */
function shortUserAgent(ua) {
  var text = String(ua || '');
  // Chrome/Edge/Safari's own token is the useful part; the compatibility noise
  // in front of it is the same for every browser.
  var m = /(Firefox|Edg|Chrome|Safari|curl|wget)\/[\d.]+/.exec(text);
  if (m) return m[0];
  return text.length > 40 ? text.slice(0, 40) + '…' : text;
}

/** RFC 3339 in, "HH:MM" out, in the reader's own zone — a connection timestamp is
 *  read as "when", and the date is implied by it being a live connection. */
function shortClock(rfc3339) {
  var d = new Date(rfc3339);
  if (isNaN(d.getTime())) return String(rfc3339);
  return d.toLocaleTimeString(_termcpLang === 'en' ? 'en' : _termcpLang, { hour: '2-digit', minute: '2-digit' });
}

/** The last lists handed to renderClients, kept so the panel can be painted on
 *  the moment it is OPENED rather than on the next frame.
 *
 *  Frames are change-driven: the server broadcasts when a session, a client or a
 *  tab changes, which on a quiet page can be minutes apart. A panel that only
 *  ever painted from a frame was therefore blank from page load until something
 *  else happened — the list was already in hand and got thrown away, and all a
 *  reader saw was the "nothing connected yet" hint sitting under an empty box. */
var _clientsLast = null;

/** Paint the panel from the session frame's client lists.
 *
 *  Cheap and side-effect-free while the panel is closed: the frame arrives many
 *  times per second while a command streams, and rebuilding a hidden DOM list
 *  each time is pure waste. The lists are remembered instead, so opening the
 *  panel draws immediately (see the toggle wiring at the end of this file). */
var _clientsSignature = '';
function renderClients(mcpClients, viewers) {
  var body = document.getElementById('clients-body');
  if (!body) return;
  mcpClients = Array.isArray(mcpClients) ? mcpClients : [];
  viewers = Array.isArray(viewers) ? viewers : [];
  _clientsLast = [mcpClients, viewers];

  var menu = document.getElementById('clients-menu');
  if (menu && !menu.open) return;
  paintClients(mcpClients, viewers);
}

/** Draw the panel from one pair of lists. Split from renderClients so the open
 *  event can repaint without a fresh frame to carry it. */
function paintClients(mcpClients, viewers) {
  var body = document.getElementById('clients-body');
  if (!body) return;

  /* There is no count badge: how many clients are attached is what the panel
     says when it is opened, and a number in the header that is almost always 0
     or 1 is one more thing to read for nothing. The current tab is still listed
     and marked, so the list is explainable rather than a mystery extra row. */
  var selfCount = 0;
  viewers.forEach(function (v) { if (v && v.self) selfCount++; });

  /* Skip the rebuild when nothing changed. A frame arrives per output batch; with
     a terminal streaming that is thousands of identical repaints of a list that
     changes once a minute. The signature is the rendered content, not the input:
     two frames that would draw the same panel are the same frame here. */
  var signature = JSON.stringify([mcpClients, viewers]);
  if (signature === _clientsSignature) return;
  _clientsSignature = signature;

  body.textContent = '';
  mcpClients.forEach(function (c) {
    body.appendChild(clientRow('mcp', mcpClientTitle(c), mcpClientMeta(c), mcpClientDetail(c)));
  });
  viewers.forEach(function (v) {
    var meta = shortUserAgent(v && v.user_agent);
    if (v && typeof v.watching === 'number' && v.watching > 0) {
      meta += (meta ? ' · ' : '') + t('clients.watching', { count: v.watching });
    }
    // This tab says so, so a list with one row is explainable rather than a
    // mystery extra client.
    var title = v && v.addr ? String(v.addr) : '';
    body.appendChild(clientRow('web', title, meta, v && v.self ? t('clients.thisTab') : ''));
  });
  /* The empty state is still about the OTHER clients: your own tab is always
     the one row the list has, so a panel that only hid the hint on a non-empty
     body would look populated while nothing is actually attached. */
  var others = mcpClients.length + viewers.length - selfCount;
  var empty = document.getElementById('clients-empty');
  if (empty) empty.hidden = others > 0;
}

/* Paint on open.
 *
 *  Without this the panel is drawn only when a frame happens to arrive WHILE it
 *  is open, and frames are change-driven: opening the panel on a quiet page left
 *  the list empty and the "nothing connected yet" hint showing, which reads as
 *  "nobody is attached" even though this very tab is. The lists are already in
 *  hand from the last frame, so the fix is to paint from them rather than to
 *  wait for the next one.
 *
 *  A `toggle` listener rather than an onclick on the summary: the element is a
 *  <details>, so it can also be opened by the keyboard, by find-in-page, or by
 *  the browser restoring its state, and all of those fire toggle. */
(function wireClientsPanelOpen() {
  function wire() {
    var menu = document.getElementById('clients-menu');
    if (!menu || menu._termcpClientsWired) return;
    menu._termcpClientsWired = true;
    menu.addEventListener('toggle', function () {
      if (!menu.open) return;
      /* One header menu at a time — see closeHeaderMenus in i18n.js. */
      if (typeof closeHeaderMenus === 'function') closeHeaderMenus('clients-menu');
      if (!_clientsLast) return;
      paintClients(_clientsLast[0], _clientsLast[1]);
    });
    /* A header menu closes when the user moves to another one.
     *
     *  The theme switch and the language menu already did this; this panel did
     *  not, and that is the whole reason one menu could cover another: all three
     *  are children of the header and share one rung of the ladder
     *  (--z-dropdown), so paint order among them is DOM order — and this panel is
     *  the LAST of the three in the header, so it won over every menu opened
     *  after it. The reader saw the trigger they had just pressed render its menu
     *  underneath this one.
     *
     *  Closing the old menu is the fix rather than ranking the new one higher:
     *  two header menus open at once is a state worth nothing (they do not talk
     *  to each other, and the second click is always "I meant that one"), so
     *  removing it leaves nothing for a z-index to arbitrate. It also keeps the
     *  ladder as it is — no rung is added for a condition that no longer
     *  happens. */
    document.addEventListener('click', function (e) {
      if (menu.open && !menu.contains(e.target)) menu.open = false;
    });
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', wire);
  } else {
    wire();
  }
})();
