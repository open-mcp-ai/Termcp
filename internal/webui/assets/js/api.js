/* Data layer: REST client, WebSocket (/api/ui/ws) and a tiny observable store.
   The UI is served at /newui2/ but the API lives at the origin root, so every
   request uses a root-absolute path — no base-path derivation needed. */

const enc = encodeURIComponent;

async function req(path, opts = {}) {
  const res = await fetch(path, { credentials: 'same-origin', ...opts });
  if (!res.ok) {
    let detail = '';
    try { const j = await res.json(); detail = j.error || j.message || ''; } catch (e) {}
    const err = new Error(detail || ('HTTP ' + res.status));
    err.status = res.status;
    throw err;
  }
  return res;
}
async function jget(path) { return (await req(path)).json(); }
async function jsend(path, method, body) {
  const res = await req(path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : {},
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204) return null;
  const txt = await res.text();
  return txt ? JSON.parse(txt) : null;
}

export const REST = {
  version: () => jget('/api/version'),
  clients: () => jget('/api/clients'),
  activity: (params = {}) => {
    const qs = new URLSearchParams(params).toString();
    return jget('/api/activity' + (qs ? '?' + qs : ''));
  },
  // connections
  connections: () => jget('/api/connections'),
  connectionToml: async (name) => (await req('/api/connections/' + enc(name))).text(),
  saveConnection: (name, toml, temporary) =>
    req('/api/connections/' + enc(name) + (temporary ? '?temporary=true' : ''), {
      method: 'PUT', headers: { 'Content-Type': 'application/toml' }, body: toml,
    }),
  deleteConnection: (name) => req('/api/connections/' + enc(name), { method: 'DELETE' }),
  testConnection: async (toml) => (await req('/api/connections/test', {
    method: 'POST', headers: { 'Content-Type': 'application/toml' }, body: toml,
  })).json(),
  importConnections: (toml) => req('/api/connections/batch', {
    method: 'POST', headers: { 'Content-Type': 'application/toml' }, body: toml,
  }),
  // sessions
  sessions: () => jget('/api/sessions'),
  createSession: (body) => jsend('/api/sessions', 'POST', body),
  session: (id) => jget('/api/sessions/' + enc(id)),
  renameSession: (id, name) => jsend('/api/sessions/' + enc(id), 'PATCH', { name }),
  terminateSession: (id) => req('/api/sessions/' + enc(id) + '/terminate', { method: 'POST' }),
  deleteSession: (id) => req('/api/sessions/' + enc(id), { method: 'DELETE' }),
  // shells
  shells: (id) => jget('/api/sessions/' + enc(id) + '/shells'),
  createShell: (id, body) => jsend('/api/sessions/' + enc(id) + '/shells', 'POST', body),
  archiveShell: (shellId) => req('/api/shells/' + enc(shellId) + '/archive', { method: 'POST' }),
  deleteShell: (shellId) => req('/api/shells/' + enc(shellId), { method: 'DELETE' }),
  outputRange: (shellId, qs) => jget('/api/shells/' + enc(shellId) + '/output-range?' + qs),
  // forwards
  forwards: (sessionId) => jget('/api/sessions/' + enc(sessionId) + '/forwards'),
  allForwards: () => jget('/api/forwards'),
  addForward: (sessionId, body) => jsend('/api/sessions/' + enc(sessionId) + '/forwards', 'POST', body),
  deleteForward: (id) => req('/api/forwards/' + enc(id), { method: 'DELETE' }),
  // files
  files: (sessionId, path) => jget('/api/sessions/' + enc(sessionId) + '/files?path=' + enc(path)),
  fileDownloadUrl: (sessionId, path) => '/api/sessions/' + enc(sessionId) + '/files/download?path=' + enc(path),
  fileUpload: (sessionId, path, file, offset = 0) => {
    const headers = {};
    if (offset > 0) headers['Content-Range'] = `bytes ${offset}-/${file.size}`;
    return req('/api/sessions/' + enc(sessionId) + '/files/upload?path=' + enc(path) + '&offset=' + offset, {
      method: 'POST', headers, body: file,
    });
  },
  fileDelete: (sessionId, path) => req('/api/sessions/' + enc(sessionId) + '/files?path=' + enc(path), { method: 'DELETE' }),
  fileRename: (sessionId, from, to) => req('/api/sessions/' + enc(sessionId) + '/files?from=' + enc(from) + '&to=' + enc(to), { method: 'PUT' }),
  fileMkdir: (sessionId, path) => req('/api/sessions/' + enc(sessionId) + '/files/dir?path=' + enc(path), { method: 'POST' }),
  // approval
  approval: (sessionId) => jget('/api/sessions/' + enc(sessionId) + '/approval'),
  setApproval: (sessionId, body) => jsend('/api/sessions/' + enc(sessionId) + '/approval', 'PATCH', body),
  approvals: (sessionId) => jget('/api/approvals' + (sessionId ? '?session_id=' + enc(sessionId) : '')),
  approve: (id) => jsend('/api/approvals/' + enc(id) + '/approve', 'POST', {}),
  reject: (id, reason) => jsend('/api/approvals/' + enc(id) + '/reject', 'POST', reason ? { reason } : {}),
  // extensions
  extensions: () => jget('/api/extensions'),
  retryExtension: (name) => jsend('/api/extensions/' + enc(name) + '/retry', 'POST', {}),
};

/* ---- observable store -------------------------------------------------- */
export const store = {
  sessions: [],
  connections: [],
  extensions: [],
  clients: [],
  mcpClients: [],
  version: '',
  activeSessionId: null,
  inspectorTab: 'details',
  linkUp: false,
  _subs: new Set(),
  subscribe(fn) { this._subs.add(fn); return () => this._subs.delete(fn); },
  emit() { this._subs.forEach((fn) => { try { fn(); } catch (e) { console.error(e); } }); },
  patch(p) { Object.assign(this, p); this.emit(); },
  session(id) { return this.sessions.find((s) => s.id === id) || null; },
  running() { return this.sessions.filter((s) => s.status === 'running'); },
  archived() { return this.sessions.filter((s) => s.status !== 'running'); },
  hostSessions(name) { return this.sessions.filter((s) => s.ssh_config === name); },
  hostRunning(name) { return this.hostSessions(name).filter((s) => s.status === 'running'); },
  connection(name) { return this.connections.find((c) => c.name === name) || null; },
};

/* ---- WebSocket --------------------------------------------------------- */
function wsUrl() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  return proto + '://' + location.host + '/api/ui/ws';
}

export const WS = {
  ws: null,
  backoff: 1000,
  _handlers: {},
  _pendingWatch: new Set(),
  on(type, fn) { (this._handlers[type] || (this._handlers[type] = [])).push(fn); },
  _dispatch(type, payload) { (this._handlers[type] || []).forEach((fn) => { try { fn(payload); } catch (e) { console.error(e); } }); },
  connect() {
    if (this.ws) { try { this.ws.close(); } catch (e) {} }
    const ws = new WebSocket(wsUrl());
    this.ws = ws;
    ws.onopen = () => {
      this.backoff = 1000;
      store.patch({ linkUp: true });
      this._pendingWatch.forEach((id) => this.watch(id, true));
      this._pendingWatch.clear();
      this._dispatch('open', {});
    };
    ws.onmessage = (ev) => {
      let j; try { j = JSON.parse(ev.data); } catch (e) { return; }
      if (j.type === 'sessions') {
        store.patch({
          sessions: j.sessions || [],
          connections: j.connections || store.connections,
          extensions: j.extensions || store.extensions,
          clients: j.clients || store.clients,
          mcpClients: j.mcp_clients || store.mcpClients,
        });
      }
      this._dispatch(j.type, j);
    };
    ws.onclose = () => {
      if (this.ws !== ws) return;
      this.ws = null;
      store.patch({ linkUp: false });
      const delay = this.backoff;
      this.backoff = Math.min(30000, Math.max(2000, Math.floor(delay * 1.5)));
      setTimeout(() => this.connect(), delay);
    };
    ws.onerror = () => {};
  },
  send(obj) { if (this.ws && this.ws.readyState === 1) this.ws.send(JSON.stringify(obj)); },
  watch(shellId, add) {
    if (!shellId) return;
    if (this.ws && this.ws.readyState === 1) this.send({ type: add ? 'watch_add' : 'watch_remove', id: shellId });
    else if (add) this._pendingWatch.add(shellId);
  },
  input(shellId, d, nl = false) { this.send({ type: 'input', id: shellId, d, nl }); },
  resize(shellId, rows, cols) { this.send({ type: 'resize', id: shellId, rows, cols }); },
};

export const enc$ = enc;