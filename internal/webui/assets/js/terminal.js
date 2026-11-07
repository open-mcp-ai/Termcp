/* Terminal core: one window per session, one xterm instance per shell channel.
   I/O rides the single UI WebSocket; retained output is replayed over REST
   (output-range) before the live watch is attached. */
import { REST, WS, store } from './api.js';
import { el, icon, clear, fmtUptime, debounce } from './util.js';

const encoder = new TextEncoder();
const MONO = '"Cascadia Code","JetBrains Mono","SF Mono",Consolas,"Courier New",monospace';
const HISTORY_CHUNK = 512 * 1024;
const HISTORY_CAP = 32 * 1024 * 1024;

function cssVar(n) { return getComputedStyle(document.documentElement).getPropertyValue(n).trim(); }
export function xtermTheme() {
  return {
    background: cssVar('--t-bg'), foreground: cssVar('--t-fg'),
    cursor: cssVar('--t-cursor'), cursorAccent: cssVar('--t-bg'),
    selectionBackground: cssVar('--t-sel'),
    black: cssVar('--t-bg'), red: cssVar('--t-red'), green: cssVar('--t-green'),
    yellow: cssVar('--t-yellow'), blue: cssVar('--t-blue'), magenta: cssVar('--t-purple'),
    cyan: cssVar('--t-cyan'), white: cssVar('--t-fg'),
    brightBlack: cssVar('--t-dim'), brightRed: cssVar('--t-red'), brightGreen: cssVar('--t-green'),
    brightYellow: cssVar('--t-yellow'), brightBlue: cssVar('--t-blue'), brightMagenta: cssVar('--t-purple'),
    brightCyan: cssVar('--t-cyan'), brightWhite: '#ffffff',
  };
}

const wins = new Map();          // session_id -> win
const chanIndex = new Map();     // shell_id -> { win, chan }
let workspace = null;
let tabsHost = null;
let activeSession = null;
let emptyEl = null;

function findChan(shellId) { return chanIndex.get(shellId) || null; }

/* ---- fitting ----------------------------------------------------------- */
function fitChan(chan, syncRemote) {
  const term = chan.term, pane = chan.paneEl;
  if (!term || !pane || !pane.isConnected || pane.clientWidth <= 0 || pane.clientHeight <= 0) return;
  const w = pane.clientWidth - 14, h = pane.clientHeight - 2;
  const opts = term.options || {};
  const fontSize = opts.fontSize || 13;
  let cw = 0, lh = 0;
  const screen = term.element && term.element.querySelector('.xterm-screen');
  if (screen && term.cols > 0 && term.rows > 0) { cw = screen.clientWidth / term.cols; lh = screen.clientHeight / term.rows; }
  if (!(cw > 0) || !(lh > 0)) {
    const span = el('span');
    span.style.cssText = 'position:absolute;visibility:hidden;white-space:pre;font:' + fontSize + 'px ' + (opts.fontFamily || MONO);
    span.textContent = 'M';
    document.body.appendChild(span);
    cw = span.offsetWidth || 8; lh = Math.ceil(fontSize * (opts.lineHeight || 1.2));
    document.body.removeChild(span);
  }
  const cols = Math.max(2, Math.floor(w / cw));
  const rows = Math.max(2, Math.floor(h / lh));
  if (cols !== term.cols || rows !== term.rows) { try { term.resize(cols, rows); } catch (e) {} }
  if (syncRemote && !chan.streamDone) WS.resize(chan.shellId, term.rows, term.cols);
}
const fitActiveSoon = debounce(() => { const c = activeChan(); if (c) fitChan(c, true); }, 120);
function activeWin() { return activeSession ? wins.get(activeSession) : null; }
function activeChan() { const w = activeWin(); return w ? w.channels.get(w.activeShell) : null; }

function nearBottom(term) {
  try { const b = term.buffer.active; return b.viewportY >= b.baseY - 2; } catch (e) { return true; }
}
function stickyScroll(chan) {
  const term = chan.term;
  if (term._stbRaf) return;
  term._stbRaf = requestAnimationFrame(() => {
    term._stbRaf = 0;
    if (nearBottom(term)) { try { term.scrollToBottom(); } catch (e) {} }
    updateScrollFab(chan);
  });
}
function updateScrollFab(chan) {
  const w = chan.win;
  if (w.activeShell !== chan.shellId) return;
  w.fabEl.classList.toggle('show', !nearBottom(chan.term));
}

/* ---- channel status chip ---------------------------------------------- */
function paintStatus(chan, state) {
  chan.status = state;
  if (chan.dotEl) chan.dotEl.className = 'tdot ' + (state === 'running' ? 'running' : state === 'ended' ? 'ended' : '');
}
function touchStatus(chan) {
  if (chan.status === 'ended') return;
  clearTimeout(chan.statusTimer);
  chan.statusTimer = setTimeout(() => { if (chan.status !== 'ended') paintStatus(chan, 'idle'); }, 3000);
}

/* ---- history replay ---------------------------------------------------- */
async function bootstrap(chan) {
  let off = 0, total = -1, shown = 0;
  try {
    while (true) {
      let max = HISTORY_CHUNK;
      if (total >= 0) max = Math.min(max, total - off);
      max = Math.min(max, HISTORY_CAP - shown);
      if (max <= 0) break;
      const j = await REST.outputRange(chan.shellId, 'start=' + off + '&max=' + max);
      const T = Number(j.total); if (isFinite(T) && T >= 0) total = T;
      const d = j.d || ''; const end = Number(j.end);
      if (d) { const b = encoder.encode(d); chan.term.write(b); shown += b.length; }
      const prev = off; off = isFinite(end) ? end : off;
      if (off === prev || total === 0 || off >= total || shown >= HISTORY_CAP) break;
    }
  } catch (e) { /* history best-effort */ }
  try { chan.term.scrollToBottom(); } catch (e) {}
}

/* ---- channel creation -------------------------------------------------- */
function createChannel(win, shellId, index, name, readOnly) {
  const pane = el('div', 'chan-pane');
  win.hostEl.appendChild(pane);
  const term = new Terminal({
    cursorBlink: !readOnly, fontSize: 13, fontFamily: MONO, lineHeight: 1.2,
    letterSpacing: 0, scrollback: 100000, allowTransparency: false,
    theme: xtermTheme(), convertEol: false,
  });
  term.open(pane);

  const tab = el('div', 'wtab');
  const dot = el('span', 'tdot');
  const label = el('span', 'wl', { text: name || ('shell-' + index) });
  tab.append(dot, label);
  if (!readOnly) {
    const x = el('span', 'x', { title: '关闭通道' }); x.appendChild(icon('x'));
    x.addEventListener('click', (e) => { e.stopPropagation(); TM.actions.closeChannel?.(win.id, shellId); });
    tab.appendChild(x);
  }
  tab.addEventListener('click', () => TM.setActiveChannel(win.id, shellId));
  win.tabsEl.insertBefore(tab, win.addTabEl);

  const chan = {
    win, shellId, index, name: name || ('shell-' + index), term, paneEl: pane, tabEl: tab,
    dotEl: dot, streamDone: !!readOnly, readOnly: !!readOnly, userReady: false,
    status: 'idle', statusTimer: 0, lineKind: 0,
  };
  win.channels.set(shellId, chan);
  chanIndex.set(shellId, { win, chan });

  if (!readOnly) {
    const unlock = () => { chan.userReady = true; };
    pane.addEventListener('mousedown', unlock, true);
    pane.addEventListener('keydown', unlock, true);
    pane.addEventListener('paste', unlock, true);
    pane.addEventListener('compositionend', unlock, true);
    term.onData((d) => { if (chan.userReady && !chan.streamDone) WS.input(shellId, d, false); });
    term.onScroll(() => updateScrollFab(chan));
    term.attachCustomKeyEventHandler?.((ev) => {
      if (ev.type === 'keydown' && ev.ctrlKey && !ev.altKey && !ev.metaKey && (ev.key === 'w' || ev.key === 'W')) ev.preventDefault();
      return true;
    });
  }

  bootstrap(chan).then(() => {
    if (!readOnly && !chan.streamDone) WS.watch(shellId, true);
    if (win.activeShell === shellId) { fitChan(chan, true); chan.term.scrollToBottom(); }
  });
  return chan;
}

/* ---- window creation --------------------------------------------------- */
function toolBtn(name, title, tool) {
  const b = el('button', 'wtool', { title }); b.appendChild(icon(name));
  b.addEventListener('click', () => TM.actions.tool?.(win_id_of(b), tool));
  return b;
}
function win_id_of(node) { const w = node.closest('.termwin'); return w && w._sid; }
function buildWindow(info) {
  const winEl = el('div', 'termwin');
  winEl._sid = info.id;
  const bar = el('div', 'win-bar');
  const status = el('span', 'win-status');
  const nameEl = el('span', 'win-name', { text: info.name || info.ssh_config || info.id, title: '双击重命名' });
  nameEl.addEventListener('dblclick', () => TM.actions.renameSession?.(info.id));
  const idEl = el('span', 'win-id', { text: info.id });
  const copy = el('button', 'wtool', { title: '复制定位符' }); copy.appendChild(icon('copy', 'sm'));
  copy.style.width = '24px'; copy.style.height = '24px';
  copy.addEventListener('click', () => { navigator.clipboard?.writeText('termcp://session/' + info.id).catch(() => {}); TM.actions.toast?.('已复制定位符'); });

  const tabsEl = el('div', 'win-tabs');
  const addTabEl = el('button', 'wtab-add', { title: '新建 shell 通道' }); addTabEl.appendChild(icon('plus', 'sm'));
  addTabEl.addEventListener('click', () => TM.actions.newChannel?.(info.id));
  tabsEl.appendChild(addTabEl);

  const tools = el('div', 'win-tools');
  const reviewBtn = toolBtn('shield', '审阅', 'review');
  tools.append(
    toolBtn('network', '端口转发', 'forward'),
    toolBtn('folder', '文件', 'files'),
    reviewBtn,
    el('div', 'win-sep'),
    toolBtn('max', '专注模式', 'focus'),
  );
  const closeBtn = el('button', 'wtool close', { title: '停止并归档' }); closeBtn.appendChild(icon('x'));
  closeBtn.addEventListener('click', () => TM.actions.closeSession?.(info.id));
  tools.appendChild(closeBtn);

  bar.append(status, nameEl, idEl, copy, tabsEl, tools);

  const body = el('div', 'term-body');
  const hostEl = el('div', 'term-host');
  const fab = el('button', 'scroll-fab', { title: '回到最新输出' }); fab.appendChild(icon('arrow-down', 'sm'));
  body.append(hostEl, fab);

  const strip = el('div', 'term-strip');
  const stripShell = el('span', 'it'); stripShell.append(icon('term'), el('span', { text: info.detected_shell || 'shell' }));
  const stripMode = el('span', 'it mono', { text: 'PTY' });
  const sp = el('span', 'sp');
  const stripReview = el('span', 'it'); stripReview.append(icon('shield'), el('span', { text: '审阅' }));
  const stripSize = el('span', 'it mono', { text: '' });
  strip.append(stripShell, el('span', 'it mono', { text: 'UTF-8' }), stripMode, sp, stripReview, stripSize);

  winEl.append(bar, body, strip);
  workspace.appendChild(winEl);

  const win = {
    id: info.id, info, el: winEl, barEl: bar, tabsEl, addTabEl, hostEl, bodyEl: body,
    fabEl: fab, stripEl: strip, stripShell, stripSize, stripReview, statusEl: status, nameEl,
    reviewBtn, channels: new Map(), activeShell: null, dead: false, unread: false, pending: false,
  };
  fab.addEventListener('click', () => { const c = win.channels.get(win.activeShell); if (c) { c.term.scrollToBottom(); updateScrollFab(c); } });
  wins.set(info.id, win);
  updateWinMeta(win);
  return win;
}

function updateWinMeta(win) {
  const s = store.session(win.id) || win.info;
  win.info = s;
  win.nameEl.textContent = s.name || s.ssh_config || s.id;
  win.stripShell.lastChild.textContent = s.detected_shell || 'shell';
  const appr = s.approval_mode && s.approval_mode !== 'off';
  win.stripReview.lastChild.textContent = appr ? '审阅开启' : '审阅关闭';
  win.reviewBtn.classList.toggle('on', !!appr);
}

function markDead(win, s) {
  win.dead = true;
  win.statusEl.classList.add('dead');
  win.channels.forEach((c) => { c.streamDone = true; paintStatus(c, 'ended'); WS.watch(c.shellId, false); });
  if (!win.el.querySelector('.readonly-banner')) {
    const b = el('div', 'readonly-banner'); b.append(icon('archive', 'sm'), el('span', { text: '会话已结束 · 只读转录' + (s && s.exit_reason ? ' · ' + s.exit_reason : '') }));
    win.el.insertBefore(b, win.bodyEl);
  }
}

/* ---- public manager ---------------------------------------------------- */
export const TM = {
  actions: {},
  mount(wsEl, tabsEl) {
    workspace = wsEl; tabsHost = tabsEl;
    emptyEl = el('div', 'empty');
    emptyEl.innerHTML = '<div class="eic"><svg class="ic"><use href="#i-term"/></svg></div>';
    emptyEl.append(el('h2', { text: '终端工作台' }), el('p', { text: '从左侧选择一台主机开始会话，支持本机与 SSH。会话在后台持续运行，可随时切换。' }));
    const b = el('button', 'btn btn-primary'); b.append(icon('plus'), el('span', { text: '连接主机' }));
    b.addEventListener('click', () => TM.actions.connect?.());
    emptyEl.appendChild(b);
    workspace.appendChild(emptyEl);

    WS.on('terminal', (j) => {
      const hit = findChan(j.id); if (!hit || hit.chan.streamDone) return;
      const { win, chan } = hit;
      touchStatus(chan);
      if (chan.status !== 'running' && chan.lineKind === 0) paintStatus(chan, 'running');
      chan.term.write(encoder.encode(j.d || ''));
      stickyScroll(chan);
      if (win.id !== activeSession && !win.unread) { win.unread = true; renderTabs(); }
    });
    WS.on('terminal_done', (j) => {
      const hit = findChan(j.id); if (!hit) return;
      hit.chan.streamDone = true; clearTimeout(hit.chan.statusTimer); paintStatus(hit.chan, 'ended');
      if (!hit.chan.endedPrinted) { hit.chan.endedPrinted = true; try { hit.chan.term.write('\r\n\x1b[90m── 通道已结束 ──\x1b[0m\r\n'); } catch (e) {} }
    });
    WS.on('shell_activity', (j) => {
      const hit = findChan(j.shell_id); if (!hit || hit.chan.streamDone) return;
      const { chan } = hit; touchStatus(chan);
      if (j.submit) { chan.lineKind = 0; paintStatus(chan, 'running'); }
      else { const kind = j.src === 'ai' ? 2 : 1; if (kind > chan.lineKind) chan.lineKind = kind; paintStatus(chan, kind === 2 ? 'ai' : 'typing'); }
    });
    window.addEventListener('resize', fitActiveSoon);
    if (window.ResizeObserver) { new ResizeObserver(fitActiveSoon).observe(workspace); }
    this.reconcile();
  },

  reconcile() {
    if (!workspace) return;
    const running = store.running();
    for (const [id, win] of wins) {
      const s = store.session(id);
      if (!s) { if (!win.pending) this._destroy(id); continue; }
      win.pending = false;
      if (s.status !== 'running' && !win.dead) markDead(win, s);
      if (s.status === 'running') updateWinMeta(win);
    }
    for (const s of running) if (!wins.has(s.id)) this._restore(s);
    if (activeSession && !wins.has(activeSession)) activeSession = null;
    if (!activeSession) { const first = (running[0] && running[0].id) || [...wins.keys()][0] || null; if (first) activeSession = first; }
    this._applyActive();
    renderTabs();
    store.activeSessionId = activeSession;
  },

  async _restore(s) {
    const win = buildWindow(s);
    let shells = [];
    try { const r = await REST.shells(s.id); shells = r.shells || []; } catch (e) {}
    if (!shells.length) shells = [{ id: null, index: 1 }];
    shells.sort((a, b) => (a.index || 0) - (b.index || 0));
    shells.forEach((sh) => { if (sh.id) createChannel(win, sh.id, sh.index || 1, sh.name, false); });
    win.activeShell = win.channels.keys().next().value || null;
    this._applyActive();
  },

  /* open from a fresh POST /api/sessions response */
  openSession(resp, info) {
    let win = wins.get(resp.session_id);
    if (!win) { win = buildWindow({ id: resp.session_id, name: info?.name || resp.ssh_config, ssh_config: resp.ssh_config, detected_shell: info?.detected_shell }); win.pending = true; }
    if (resp.shell_id && !win.channels.has(resp.shell_id)) {
      createChannel(win, resp.shell_id, resp.index || 1, info?.shellName, false);
    }
    win.activeShell = resp.shell_id || win.activeShell;
    this.setActiveSession(resp.session_id);
    return win;
  },

  addChannel(sessionId, resp, name) {
    const win = wins.get(sessionId); if (!win || !resp.shell_id) return;
    if (!win.channels.has(resp.shell_id)) createChannel(win, resp.shell_id, resp.index || (win.channels.size + 1), name, false);
    this.setActiveChannel(sessionId, resp.shell_id);
  },

  removeChannel(shellId) {
    const hit = findChan(shellId); if (!hit) return;
    const { win, chan } = hit;
    WS.watch(shellId, false);
    try { chan.term.dispose(); } catch (e) {}
    chan.paneEl.remove(); chan.tabEl.remove();
    win.channels.delete(shellId); chanIndex.delete(shellId);
    if (win.activeShell === shellId) win.activeShell = win.channels.keys().next().value || null;
    this._applyActive();
  },

  setActiveSession(id) {
    if (!wins.has(id)) return;
    activeSession = id; store.activeSessionId = id;
    const w = wins.get(id); if (w) w.unread = false;
    this._applyActive(); renderTabs();
    store.emit();
    const c = activeChan(); if (c) setTimeout(() => { fitChan(c, true); c.term.focus(); }, 30);
  },
  setActiveChannel(sessionId, shellId) {
    const win = wins.get(sessionId); if (!win || !win.channels.has(shellId)) return;
    win.activeShell = shellId;
    if (activeSession !== sessionId) { this.setActiveSession(sessionId); return; }
    this._applyActive();
    const c = win.channels.get(shellId); setTimeout(() => { fitChan(c, true); c.term.focus(); }, 20);
  },
  _applyActive() {
    wins.forEach((w) => {
      const on = w.id === activeSession;
      w.el.classList.toggle('active', on);
      w.channels.forEach((c) => {
        const co = on && w.activeShell === c.shellId;
        c.paneEl.classList.toggle('active', co);
        c.tabEl.classList.toggle('active', co);
      });
    });
    if (emptyEl) emptyEl.style.display = wins.size === 0 ? 'flex' : 'none';
    const c = activeChan();
    if (c) { fitChan(c, false); updateScrollFab(c); const w = activeWin(); w.stripSize.textContent = c.term.rows + ' 行 · ' + c.term.cols + ' 列'; }
  },

  _destroy(id) {
    const win = wins.get(id); if (!win) return;
    win.channels.forEach((c) => { WS.watch(c.shellId, false); chanIndex.delete(c.shellId); try { c.term.dispose(); } catch (e) {} });
    win.el.remove(); wins.delete(id);
    if (activeSession === id) activeSession = null;
  },

  retheme() { const t = xtermTheme(); wins.forEach((w) => w.channels.forEach((c) => { try { c.term.options.theme = t; } catch (e) {} })); },
  fitActive() { const c = activeChan(); if (c) fitChan(c, true); },
  has(id) { return wins.has(id); },
  count() { return wins.size; },
  setReviewBadge(sessionId, n) {
    const w = wins.get(sessionId); if (!w) return;
    let cnt = w.reviewBtn.querySelector('.cnt');
    if (n > 0) { if (!cnt) { cnt = el('span', 'cnt'); w.reviewBtn.appendChild(cnt); } cnt.textContent = n; }
    else if (cnt) cnt.remove();
  },
};

/* ---- session tabs ------------------------------------------------------ */
function renderTabs() {
  if (!tabsHost) return;
  clear(tabsHost);
  for (const [id, win] of wins) {
    const s = store.session(id) || win.info;
    const tab = el('div', 'stab' + (id === activeSession ? ' active' : ''));
    const dot = el('span', 'sd' + (win.dead ? ' dead' : ''));
    tab.append(dot, el('span', { text: s.name || s.ssh_config || id }));
    if (win.unread) tab.appendChild(el('span', 'unread'));
    const x = el('span', 'sx', { title: '停止并归档' }); x.appendChild(icon('x'));
    x.addEventListener('click', (e) => { e.stopPropagation(); TM.actions.closeSession?.(id); });
    tab.appendChild(x);
    tab.addEventListener('click', () => TM.setActiveSession(id));
    tabsHost.appendChild(tab);
  }
}