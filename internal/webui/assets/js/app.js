/* Bootstrap: wire the terminal manager's action hooks, start the WebSocket,
   load initial snapshots, and let the store drive reconciliation + rendering. */
import { REST, WS, store } from './api.js';
import { TM } from './terminal.js';
import { Views } from './views.js';
import { Modals } from './modals.js';
import { toast } from './util.js';

function focusMode() {
  const app = document.getElementById('app');
  const on = !app.classList.contains('focus-mode');
  app.classList.toggle('focus-mode', on);
  app.classList.toggle('inspector-collapsed', on);
  document.getElementById('sidebar').style.display = on ? 'none' : '';
  setTimeout(() => TM.fitActive(), 60);
}

TM.actions = {
  connect: () => {
    const c = store.connections[0];
    if (c) Modals.connect(c.name); else Modals.hostEditor();
  },
  closeSession: async (sid) => {
    const s = store.session(sid);
    const ok = await Modals.confirm({
      title: '关闭会话',
      message: `停止「${(s && (s.name || s.ssh_config)) || sid}」并移入归档？`,
      confirmText: '停止', danger: true,
    });
    if (!ok) return;
    try {
      await REST.terminateSession(sid);
      TM._destroy(sid);
      TM.reconcile();
      toast('已停止并归档', { level: 'success' });
    } catch (e) { toast(e.message, { level: 'error' }); }
  },
  closeChannel: async (sid, shellId) => {
    try { await REST.archiveShell(shellId); TM.removeChannel(shellId); }
    catch (e) { toast(e.message, { level: 'error' }); }
  },
  newChannel: (sid) => Modals.command(sid),
  renameSession: (sid) => Modals.renameSession(sid),
  tool: (sid, tool) => {
    if (tool === 'focus') { focusMode(); return; }
    const map = { forward: 'forwards', files: 'files', review: 'review' };
    if (map[tool]) {
      if (store.activeSessionId !== sid) TM.setActiveSession(sid);
      document.getElementById('app').classList.remove('inspector-collapsed');
      store.inspectorTab = map[tool];
      Views.invalidateInspector();
    }
  },
  toast: (msg) => toast(msg),
};

async function boot() {
  Views.init();
  TM.mount(document.getElementById('workspace'), document.getElementById('sessionTabs'));
  store.subscribe(() => TM.reconcile());

  WS.connect();

  REST.version().then((v) => store.patch({ version: v.version || 'dev' })).catch(() => {});
  REST.connections().then((r) => store.patch({ connections: r.connections || [] })).catch(() => {});
  REST.extensions().then((r) => store.patch({ extensions: r.extensions || [] })).catch(() => {});
  REST.clients().then((r) => store.patch({ clients: r.webui || [], mcpClients: r.mcp || [] })).catch(() => {});
  REST.sessions().then((r) => store.patch({ sessions: r.sessions || [] })).catch(() => {});
}

boot();