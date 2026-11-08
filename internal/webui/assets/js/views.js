/* Rendering: sidebar, inspector (details/forwards/files/review), statusbar, topbar. */
import { REST, WS, store } from './api.js';
import { TM } from './terminal.js';
import { Modals } from './modals.js';
import { el, icon, clear, esc, fmtBytes, fmtUptime, fmtTime, ago, toast, debounce } from './util.js';

const $ = (id) => document.getElementById(id);
let hostFilter = '';
let inspKey = '';
let filesState = {};

/* ---- dropdown menu ----------------------------------------------------- */
let menuEl = null;
function closeMenu() { if (menuEl) { menuEl.remove(); menuEl = null; } }
function showMenu(x, y, items) {
  closeMenu();
  const m = el('div', 'menu');
  items.forEach((it) => {
    if (it.div) { m.appendChild(el('div', 'menu-div')); return; }
    const b = el('button', it.danger ? 'danger' : '');
    if (it.icon) b.appendChild(icon(it.icon, 'sm'));
    b.appendChild(el('span', { text: it.label }));
    b.addEventListener('click', () => { closeMenu(); it.onClick?.(); });
    m.appendChild(b);
  });
  document.body.appendChild(m);
  const r = m.getBoundingClientRect();
  m.style.left = Math.min(x, window.innerWidth - r.width - 8) + 'px';
  m.style.top = Math.min(y, window.innerHeight - r.height - 8) + 'px';
  menuEl = m;
  setTimeout(() => document.addEventListener('mousedown', function h(e) { if (menuEl && !menuEl.contains(e.target)) { closeMenu(); document.removeEventListener('mousedown', h); } }), 0);
}

/* ---- sidebar ----------------------------------------------------------- */
function buildSidebarSkeleton() {
  const sb = $('sidebar'); clear(sb);
  const sec = el('div', 'side-sec');
  const head = el('div', 'side-head');
  head.append(el('span', 'side-title', { text: '主机' }), el('span', 'side-count', { id: 'hostCount', text: '0' }), el('span', 'sp'));
  const addB = el('button', 'mini-btn', { title: '添加主机' }); addB.appendChild(icon('plus'));
  addB.addEventListener('click', () => Modals.hostEditor());
  head.appendChild(addB);
  const filter = el('div', 'filter'); filter.appendChild(icon('search'));
  const fi = el('input', { placeholder: '筛选主机…', autocomplete: 'off' });
  fi.addEventListener('input', () => { hostFilter = fi.value.trim().toLowerCase(); renderHosts(); });
  filter.appendChild(fi);
  const hostList = el('div', { id: 'hostList' });
  const add = el('div', 'side-add');
  const ghost = el('button', 'btn-ghost'); ghost.append(icon('plus', 'sm'), el('span', { text: '添加主机' }));
  ghost.addEventListener('click', () => Modals.hostEditor());
  const imp = el('button', 'btn-sq', { title: '导入配置' }); imp.appendChild(icon('upload', 'sm'));
  imp.addEventListener('click', importHosts);
  add.append(ghost, imp);
  sec.append(head, filter, hostList, add);

  const div = el('div', 'side-div');
  const esec = el('div', 'side-sec');
  const ehead = el('div', 'side-head');
  ehead.append(el('span', 'side-title', { text: '扩展' }), el('span', 'side-count', { id: 'extCount', text: '0' }), el('span', 'sp'));
  const extList = el('div', { id: 'extList' });
  esec.append(ehead, extList);

  const foot = el('div', 'side-foot');
  const arc = el('div', 'foot-row', { id: 'archiveRow' }); arc.append(icon('archive'), el('span', { text: '归档' }), el('span', 'n', { id: 'archiveCount', text: '0' }));
  arc.addEventListener('click', () => openArchive());
  const set = el('div', 'foot-row'); set.append(icon('settings'), el('span', { text: '设置' }));
  set.addEventListener('click', () => toast('设置：主题与语言可在顶栏切换', { level: 'info' }));
  const ver = el('div', 'version'); ver.append(el('span', 'd'), el('span', { id: 'sideVersion', text: '…' }));
  foot.append(arc, set, ver);
  sb.append(sec, div, esec, foot);
}

function renderHosts() {
  const list = $('hostList'); if (!list) return;
  clear(list);
  const conns = store.connections.filter((c) => !hostFilter || c.name.toLowerCase().includes(hostFilter) || (c.host || '').toLowerCase().includes(hostFilter));
  $('hostCount').textContent = store.connections.length;
  if (!conns.length) { list.appendChild(el('div', 'side-empty', { text: '暂无主机' })); return; }
  conns.forEach((c) => {
    const running = store.hostRunning(c.name);
    const total = store.hostSessions(c.name);
    const active = store.activeSessionId && store.session(store.activeSessionId)?.ssh_config === c.name;
    const card = el('div', 'host' + (active ? ' active' : ''));
    const ic = el('div', 'host-ic'); ic.appendChild(icon(c.kind === 'internal' ? 'laptop' : 'server'));
    ic.appendChild(el('span', 'lamp ' + (running.length ? 'on' : 'off')));
    const meta = el('div', 'host-meta');
    const nm = el('div', 'host-name', { text: c.name });
    if (total.length) nm.appendChild(el('span', 'hc', { text: total.length + ' 会话' }));
    const sub = el('div', 'host-sub');
    const tag = el('span', 'tag', { text: c.kind === 'internal' ? '本机' : 'SSH' });
    sub.append(tag, el('span', { text: c.kind === 'internal' ? (running.length ? ' · 运行中' : ' · 空闲') : ' · ' + (c.user ? c.user + '@' : '') + (c.host || '') }));
    meta.append(nm, sub);
    const more = el('button', 'host-menu', { title: '更多' }); more.appendChild(icon('more', 'sm'));
    more.addEventListener('click', (e) => {
      e.stopPropagation();
      const r = more.getBoundingClientRect();
      showMenu(r.left, r.bottom + 4, [
        { label: '连接', icon: 'play', onClick: () => Modals.connect(c.name) },
        { label: '编辑配置', icon: 'edit', onClick: () => Modals.hostEditor(c.name) },
        { label: '复制 URL', icon: 'copy', onClick: () => { navigator.clipboard?.writeText('termcp://entry/' + c.name).catch(() => {}); toast('已复制'); } },
        { div: true },
        { label: '删除', icon: 'trash', danger: true, onClick: async () => {
          const ok = await Modals.confirm({ title: '删除主机', message: `删除主机「${c.name}」？`, confirmText: '删除', danger: true });
          if (!ok) return;
          try { await REST.deleteConnection(c.name); toast('已删除', { level: 'success' }); const r2 = await REST.connections(); store.patch({ connections: r2.connections || [] }); }
          catch (e) { toast(e.message, { level: 'error' }); }
        } },
      ]);
    });
    card.append(ic, meta, more);
    card.addEventListener('click', () => Modals.connect(c.name));
    list.appendChild(card);
  });
}

function renderExtensions() {
  const list = $('extList'); if (!list) return;
  clear(list);
  const exts = store.extensions || [];
  $('extCount').textContent = exts.length;
  if (!exts.length) { list.appendChild(el('div', 'side-empty', { text: '暂无扩展' })); return; }
  const statusMap = { available: ['运行中', 'green'], idle: ['空闲', 'yellow'], probing: ['探测中', 'yellow'], unavailable: ['不可用', 'red'], disabled: ['已禁用', 'gray'] };
  exts.forEach((x) => {
    const row = el('div', 'ext');
    const ic = el('div', 'ext-ic'); ic.appendChild(icon('box'));
    const meta = el('div', 'ext-meta');
    meta.append(el('div', 'ext-name', { text: x.name }), el('div', 'ext-sub', { text: x.description || x.target || '' }));
    const [label, color] = statusMap[x.status] || [x.status || '未知', 'gray'];
    const pill = el('span', 'pill ' + color, { text: label });
    row.append(ic, meta, pill);
    row.addEventListener('click', () => {
      const r = row.getBoundingClientRect();
      showMenu(r.right - 150, r.bottom + 4, [
        { label: '重试', icon: 'refresh', onClick: async () => { try { await REST.retryExtension(x.name); toast('已触发重试', { level: 'success' }); } catch (e) { toast(e.message, { level: 'error' }); } } },
      ]);
    });
    list.appendChild(row);
  });
}

async function importHosts() {
  const inp = el('input', { type: 'file', accept: '.toml,text/plain' });
  inp.addEventListener('change', async () => {
    const file = inp.files[0]; if (!file) return;
    try { const text = await file.text(); await REST.importConnections(text); toast('已导入', { level: 'success' }); const r = await REST.connections(); store.patch({ connections: r.connections || [] }); }
    catch (e) { toast(e.message, { level: 'error' }); }
  });
  inp.click();
}

function openArchive() {
  const archived = store.archived();
  const body = el('div');
  if (!archived.length) body.appendChild(el('div', 'files-empty', { text: '暂无归档会话' }));
  archived.forEach((s) => {
    const row = el('div', 'arow');
    const ic = el('div', 'aic'); ic.appendChild(icon(s.ssh_endpoint === 'internal' ? 'laptop' : 'server'));
    const meta = el('div', 'am'); meta.append(el('div', 'an', { text: s.name || s.ssh_config || s.id }), el('div', 'as', { text: (s.ssh_config || '') + ' · ' + (s.exit_reason || s.status) }));
    row.append(ic, meta, el('span', 'at', { text: ago(s.updated_at) }));
    row.addEventListener('click', () => Modals.transcript(s));
    body.appendChild(row);
  });
  const m = Modals.openModal({ title: '归档', sub: archived.length + ' 个会话', wide: true, body, footer: (() => { const f = document.createDocumentFragment(); f.append(el('span', 'sp')); const b = el('button', 'btn btn-secondary', { text: '关闭' }); b.addEventListener('click', () => m.close()); f.append(b); return f; })() });
}

/* ---- inspector --------------------------------------------------------- */
function renderInspector() {
  const sid = store.activeSessionId;
  const tab = store.inspectorTab || 'details';
  const key = (sid || '') + ':' + tab;
  const insp = $('inspector');
  if (key !== inspKey) { inspKey = key; buildInspector(insp, sid, tab); }
  else updateInspectorDynamic(sid);
}

function buildInspector(insp, sid, tab) {
  clear(insp);
  const head = el('div', 'insp-head');
  head.append(el('span', 'insp-title', { text: '会话详情' }), el('span', 'sp'));
  const collapse = el('button', 'mini-btn', { title: '收起' }); collapse.appendChild(icon('chev-right'));
  collapse.addEventListener('click', () => { $('app').classList.add('inspector-collapsed'); TM.fitActive(); });
  head.appendChild(collapse);
  insp.appendChild(head);

  const s = sid ? store.session(sid) : null;
  if (!s) {
    const e = el('div', 'insp-empty'); e.append(icon('term'), el('div', { text: '尚未选择会话' }), el('div', { text: '打开主机或选择终端后，可在这里查看实时状态。' }));
    insp.appendChild(e); return;
  }
  const scroll = el('div', 'insp-scroll');
  const idr = el('div', 'insp-id');
  idr.append(el('span', 'lamp2' + (s.status !== 'running' ? ' dead' : '')), el('span', 'nm', { text: s.name || s.ssh_config || s.id }),
    el('span', 'pill ' + (s.status === 'running' ? 'green' : 'gray'), { text: s.status === 'running' ? '已连接' : '已结束' }));
  const tabs = el('div', 'insp-tabs');
  [['details', '详情'], ['forwards', '转发'], ['files', '文件'], ['review', '审阅']].forEach(([k, label]) => {
    const t = el('div', 'itab' + (tab === k ? ' active' : ''), { text: label });
    if (k === 'review') t.appendChild(el('span', 'b', { id: 'inspReviewBadge', hidden: true, text: '0' }));
    t.addEventListener('click', () => { store.inspectorTab = k; inspKey = ''; renderInspector(); });
    tabs.appendChild(t);
  });
  const body = el('div', 'insp-body'); body.id = 'inspBody';
  scroll.append(idr, tabs, body);
  insp.appendChild(scroll);
  if (tab === 'details') buildDetails(body, s);
  else if (tab === 'forwards') buildForwards(body, s);
  else if (tab === 'files') buildFiles(body, s);
  else if (tab === 'review') buildReview(body, s);
}

function card(title, iconName, n) {
  const c = el('div', 'card');
  const h = el('div', 'card-h'); h.append(icon(iconName), el('span', { text: title }));
  if (n != null) h.appendChild(el('span', 'n', { text: String(n) }));
  c.appendChild(h);
  return c;
}
function kv(k, vNode, mono) {
  const row = el('div', 'kv');
  row.append(el('span', 'k', { text: k }));
  const v = el('span', 'v' + (mono ? ' mono' : ''));
  if (typeof vNode === 'string') v.textContent = vNode; else v.append(vNode);
  row.appendChild(v);
  return row;
}

function buildDetails(body, s) {
  const runtime = card('运行状态', 'activity');
  const upV = el('span', { 'data-uptime': s.created_at, text: fmtUptime(s.created_at) });
  const apprSwitch = el('span', 'switch' + (s.approval_mode && s.approval_mode !== 'off' ? ' on' : ''));
  apprSwitch.addEventListener('click', async () => {
    const on = !apprSwitch.classList.contains('on');
    try { await REST.setApproval(s.id, { enabled: on }); apprSwitch.classList.toggle('on', on); toast(on ? '已开启审阅' : '已关闭审阅', { level: 'success' }); }
    catch (e) { toast(e.message, { level: 'error' }); }
  });
  runtime.append(
    kv('连接类型', s.ssh_endpoint === 'internal' ? '本机' : 'SSH'),
    kv('运行时间', upV, true),
    kv('默认 Shell', s.detected_shell || '—', true),
    kv('审阅 AI 写入', apprSwitch),
  );
  body.appendChild(runtime);

  const fwCard = card('端口转发', 'network', 0); fwCard.id = 'detFw';
  const fwBody = el('div'); fwBody.appendChild(el('div', 'files-empty', { text: '加载中…' })); fwCard.appendChild(fwBody);
  body.appendChild(fwCard);

  const actCard = card('实时动态', 'zap'); actCard.id = 'detAct';
  const actBody = el('div', 'feed'); actBody.appendChild(el('div', 'files-empty', { text: '加载中…' })); actCard.appendChild(actBody);
  body.appendChild(actCard);

  const cliCard = card('已连接客户端', 'users'); cliCard.id = 'detCli';
  cliCard.appendChild(buildClients());
  body.appendChild(cliCard);

  loadDetailsData(s.id);
}

function buildClients() {
  const wrap = el('div', 'clients');
  const all = [...(store.mcpClients || []), ...(store.clients || [])];
  const stack = el('div', 'avstack');
  const colors = ['#d97757', '#5b5bd6', '#0ea5e9', '#16a34a', '#d97706'];
  all.slice(0, 4).forEach((c, i) => {
    const name = c.name || c.user_agent || 'C';
    stack.appendChild(el('span', 'av', { style: 'background:' + colors[i % colors.length], text: name.slice(0, 2).toUpperCase() }));
  });
  if (all.length > 4) stack.appendChild(el('span', 'av', { style: 'background:var(--bg-elev-2);color:var(--text-2)', text: '+' + (all.length - 4) }));
  if (!all.length) stack.appendChild(el('span', 'av', { style: 'background:var(--bg-elev-2);color:var(--text-3)', text: '0' }));
  wrap.append(stack, el('span', 'ct', { html: '<b>' + all.length + '</b> 个在线' }), el('span', 'on', { html: '<span class="d"></span>实时' }));
  return wrap;
}

async function loadDetailsData(sid) {
  const key = inspKey;
  try {
    const [fw, act] = await Promise.all([
      REST.forwards(sid).catch(() => ({ forwards: [] })),
      REST.activity({ session_id: sid, limit: 6 }).catch(() => ({ activity: [] })),
    ]);
    if (key !== inspKey) return;
    const fwCard = $('detFw');
    if (fwCard) {
      const list = fw.forwards || [];
      fwCard.querySelector('.n').textContent = list.length;
      const b = fwCard.lastChild; clear(b);
      if (!list.length) b.appendChild(el('div', 'files-empty', { text: '暂无活动转发' }));
      list.slice(0, 4).forEach((f) => b.appendChild(fwRow(f, false)));
    }
    const actCard = $('detAct');
    if (actCard) {
      const b = actCard.lastChild; clear(b);
      const list = act.activity || [];
      if (!list.length) b.appendChild(el('div', 'files-empty', { text: '暂无输入动态' }));
      list.forEach((a) => b.appendChild(feedItem(a)));
    }
  } catch (e) {}
}

function feedItem(a) {
  const src = a.source || 'system';
  const it = el('div', 'feed-it');
  const ic = el('div', 'feed-ic ' + (src === 'ai' ? 'ai' : src === 'human' ? 'human' : src === 'approval' ? 'appr' : 'system'));
  ic.appendChild(icon(src === 'ai' ? 'edit' : src === 'human' ? 'term' : 'check'));
  const tx = el('div', 'feed-tx');
  tx.append(el('div', 'feed-t', { text: a.kind || a.verdict || '活动' }));
  const sub = el('div', 'feed-s');
  sub.append(el('span', 'src ' + (src === 'ai' ? 'ai' : src === 'human' ? 'human' : 'system'), { text: src === 'ai' ? 'AI' : src === 'human' ? '人工' : '系统' }), el('span', { text: ' · ' + ago(a.unix_ms || a.time) }));
  tx.appendChild(sub);
  it.append(ic, tx);
  return it;
}

function fwRow(f, withDelete) {
  const row = el('div', 'fw');
  const listen = f.listen_addr || ((f.local_host || '') + ':' + (f.local_port || ''));
  const target = f.target_addr || ((f.remote_host || '') + ':' + (f.remote_port || ''));
  row.append(el('span', 'fd'), el('span', 'fp', { text: listen }),
    el('span', 'fa', { text: '→ ' + target }),
    el('span', 'ft', { text: f.direction || '' }));
  if (withDelete) {
    const d = el('button', 'fdel', { title: '删除转发' }); d.appendChild(icon('trash', 'xs'));
    d.addEventListener('click', async () => {
      const ok = await Modals.confirm({ title: '删除转发', message: '删除该端口转发？', confirmText: '删除', danger: true });
      if (!ok) return;
      try { await REST.deleteForward(f.forward_id); toast('已删除', { level: 'success' }); inspKey = ''; renderInspector(); }
      catch (e) { toast(e.message, { level: 'error' }); }
    });
    row.appendChild(d);
  }
  return row;
}

function buildForwards(body, s) {
  const top = el('div'); top.style.cssText = 'display:flex;align-items:center;gap:8px;margin-bottom:4px';
  top.append(el('span', { text: '端口转发', style: 'font-size:12.5px;font-weight:700' }), el('span', 'sp', { style: 'flex:1' }));
  const add = el('button', 'btn btn-primary btn-sm'); add.append(icon('plus', 'sm'), el('span', { text: '添加' }));
  add.addEventListener('click', () => { Modals.forward(s.id); setTimeout(() => { inspKey = ''; renderInspector(); }, 800); });
  const refresh = el('button', 'btn-sq', { title: '刷新', style: 'width:29px;height:29px' }); refresh.appendChild(icon('refresh', 'sm'));
  refresh.addEventListener('click', () => { inspKey = ''; renderInspector(); });
  top.append(refresh, add);
  body.appendChild(top);
  const list = el('div', 'card'); list.appendChild(el('div', 'files-empty', { text: '加载中…' }));
  body.appendChild(list);
  REST.forwards(s.id).then((r) => {
    clear(list);
    const fs = r.forwards || [];
    if (!fs.length) list.appendChild(el('div', 'files-empty', { text: '暂无转发，点击「添加」创建' }));
    fs.forEach((f) => list.appendChild(fwRow(f, true)));
  }).catch((e) => { clear(list); list.appendChild(el('div', 'files-empty', { text: e.message })); });
}

/* ---- files (SFTP) ------------------------------------------------------ */
function buildFiles(body, s) {
  if (s.status !== 'running') { body.appendChild(el('div', 'files-empty', { text: '会话已结束，文件浏览不可用' })); return; }
  const st = filesState[s.id] || (filesState[s.id] = { path: '.' });
  const wrap = el('div', 'files');
  const bar = el('div', 'files-bar');
  const up = el('button', 'btn-sq', { title: '上级目录', style: 'width:32px;height:32px' }); up.appendChild(icon('arrow-up', 'sm'));
  const pathWrap = el('div', 'files-path'); pathWrap.appendChild(icon('folder', 'sm'));
  const pathI = el('input', { value: st.path, placeholder: '/' });
  pathI.addEventListener('keydown', (e) => { if (e.key === 'Enter') { st.path = pathI.value.trim(); load(); } });
  pathWrap.appendChild(pathI);
  const refresh = el('button', 'btn-sq', { title: '刷新', style: 'width:32px;height:32px' }); refresh.appendChild(icon('refresh', 'sm'));
  const upload = el('button', 'btn-sq', { title: '上传文件', style: 'width:32px;height:32px' }); upload.appendChild(icon('upload', 'sm'));
  const mkdir = el('button', 'btn-sq', { title: '新建目录', style: 'width:32px;height:32px' }); mkdir.appendChild(icon('plus', 'sm'));
  bar.append(up, pathWrap, refresh, upload, mkdir);
  const list = el('div', 'files-list');
  wrap.append(bar, list);
  body.appendChild(wrap);

  async function load() {
    pathI.value = st.path;
    clear(list); list.appendChild(el('div', 'files-empty', { text: '加载中…' }));
    try {
      const r = await REST.files(s.id, st.path);
      clear(list);
      const children = r.children || [];
      if (st.path && st.path !== '.') {
        const parent = el('div', 'frow dir'); parent.append(icon('folder', 'fic'), el('span', 'fn', { text: '..' }));
        parent.addEventListener('click', () => { st.path = parentPath(st.path); load(); });
        list.appendChild(parent);
      }
      if (!children.length && st.path === '.') list.appendChild(el('div', 'files-empty', { text: '空目录' }));
      children.sort((a, b) => (b.is_dir - a.is_dir) || a.name.localeCompare(b.name));
      children.forEach((ch) => {
        const full = joinPath(st.path, ch.name);
        const row = el('div', 'frow' + (ch.is_dir ? ' dir' : ''));
        row.append(icon(ch.is_dir ? 'folder' : 'file', 'fic'), el('span', 'fn', { text: ch.name }), el('span', 'fs', { text: ch.is_dir ? '' : fmtBytes(ch.size) }));
        const acts = el('div', 'fa');
        if (!ch.is_dir) {
          const dl = el('button', { title: '下载' }); dl.appendChild(icon('download', 'xs'));
          dl.addEventListener('click', (e) => { e.stopPropagation(); window.open(REST.fileDownloadUrl(s.id, full), '_blank'); });
          acts.appendChild(dl);
        }
        const ren = el('button', { title: '重命名' }); ren.appendChild(icon('edit', 'xs'));
        ren.addEventListener('click', (e) => { e.stopPropagation(); renameFile(s.id, full, ch.name, load); });
        const del = el('button', 'del', { title: '删除' }); del.appendChild(icon('trash', 'xs'));
        del.addEventListener('click', (e) => { e.stopPropagation(); deleteFile(s.id, full, ch.name, load); });
        acts.append(ren, del);
        row.appendChild(acts);
        row.addEventListener('click', () => { if (ch.is_dir) { st.path = full; load(); } });
        list.appendChild(row);
      });
    } catch (e) { clear(list); list.appendChild(el('div', 'files-empty', { text: e.message })); }
  }
  up.addEventListener('click', () => { st.path = parentPath(st.path); load(); });
  refresh.addEventListener('click', load);
  mkdir.addEventListener('click', () => newDir(s.id, st.path, load));
  upload.addEventListener('click', () => {
    const inp = el('input', { type: 'file' });
    inp.addEventListener('change', async () => {
      const file = inp.files[0]; if (!file) return;
      const target = joinPath(st.path, file.name);
      toast('上传中 ' + file.name + '…', { level: 'info', duration: 2000 });
      try { await REST.fileUpload(s.id, target, file); toast('已上传 ' + file.name, { level: 'success' }); load(); }
      catch (e) { toast(e.message, { level: 'error' }); }
    });
    inp.click();
  });
  load();
}
function joinPath(base, name) { if (!base || base === '.') return './' + name; return base.replace(/\/+$/, '') + '/' + name; }
function parentPath(p) { if (!p || p === '.') return '.'; const i = p.lastIndexOf('/'); if (i <= 0) return '.'; const r = p.slice(0, i); return (r === '.' || r === '') ? '.' : r; }
async function renameFile(sid, full, oldName, reload) {
  const body = el('div'); const inp = textInputFull(oldName); body.appendChild(fieldFull('新名称', inp));
  const m = Modals.openModal({ title: '重命名', body, footer: footOk(() => doIt()) });
  async function doIt() { const n = inp.value.trim(); if (!n || n === oldName) { m.close(); return; } try { await REST.fileRename(sid, full, joinPath(parentPath(full), n)); m.close(); toast('已重命名', { level: 'success' }); reload(); } catch (e) { toast(e.message, { level: 'error' }); } }
}
async function deleteFile(sid, full, name, reload) {
  const ok = await Modals.confirm({ title: '删除', message: `删除「${name}」？此操作不可撤销。`, confirmText: '删除', danger: true });
  if (!ok) return;
  try { await REST.fileDelete(sid, full); toast('已删除', { level: 'success' }); reload(); } catch (e) { toast(e.message, { level: 'error' }); }
}
async function newDir(sid, base, reload) {
  const body = el('div'); const inp = textInputFull(''); body.appendChild(fieldFull('目录名称', inp));
  const m = Modals.openModal({ title: '新建目录', body, footer: footOk(() => doIt()) });
  async function doIt() { const n = inp.value.trim(); if (!n) { m.close(); return; } try { await REST.fileMkdir(sid, joinPath(base, n)); m.close(); toast('已创建', { level: 'success' }); reload(); } catch (e) { toast(e.message, { level: 'error' }); } }
}
function textInputFull(v) { const i = el('input', 'input', { value: v, autocomplete: 'off' }); setTimeout(() => i.focus(), 50); return i; }
function fieldFull(label, control) { const f = el('div', 'field'); f.append(el('label', { text: label }), control); return f; }
function footOk(onOk) { const f = document.createDocumentFragment(); f.append(el('span', 'sp')); const b = el('button', 'btn btn-primary', { text: '确定' }); b.addEventListener('click', onOk); f.append(b); return f; }

/* ---- review ------------------------------------------------------------ */
function buildReview(body, s) {
  const toggle = el('div', 'appr-toggle');
  const tx = el('div', 'tx'); tx.append(el('div', 't1', { text: '审阅 AI 写入' }), el('div', 't2', { text: '开启后，AI 的每次输入都需人工批准' }));
  const sw = el('span', 'switch' + (s.approval_mode && s.approval_mode !== 'off' ? ' on' : ''));
  sw.addEventListener('click', async () => {
    const on = !sw.classList.contains('on');
    try { await REST.setApproval(s.id, { enabled: on }); sw.classList.toggle('on', on); toast(on ? '已开启审阅' : '已关闭审阅', { level: 'success' }); }
    catch (e) { toast(e.message, { level: 'error' }); }
  });
  toggle.append(tx, sw);
  body.appendChild(toggle);
  const list = el('div'); list.appendChild(el('div', 'files-empty', { text: '加载中…' }));
  body.appendChild(list);
  REST.approvals(s.id).then((r) => {
    clear(list);
    const pending = (r.approvals || []).filter((a) => a.request && a.request.state === 'pending');
    if (!pending.length) { list.appendChild(el('div', 'files-empty', { text: '暂无待审阅请求' })); return; }
    pending.forEach((a) => {
      const req = a.request;
      const c = el('div', 'appr-card');
      const h = el('div', 'appr-h'); h.append(el('span', 'src ' + (req.source === 'mcp' ? 'ai' : 'human'), { text: req.source === 'mcp' ? 'AI' : 'REST' }), el('span', { text: req.summary || '输入请求' }), el('span', 'at', { text: ago(req.created_at) }));
      const cmd = el('div', 'appr-cmd', { text: req.text || (req.keys || []).join(' ') });
      const f = el('div', 'appr-f');
      const rej = el('button', 'btn btn-danger btn-sm'); rej.append(icon('x', 'sm'), el('span', { text: '拒绝' }));
      const acc = el('button', 'btn btn-primary btn-sm'); acc.append(icon('check', 'sm'), el('span', { text: '批准' }));
      rej.addEventListener('click', async () => { try { await REST.reject(req.id); toast('已拒绝', { level: 'success' }); inspKey = ''; renderInspector(); } catch (e) { toast(e.message, { level: 'error' }); } });
      acc.addEventListener('click', async () => { try { await REST.approve(req.id); toast('已批准', { level: 'success' }); inspKey = ''; renderInspector(); } catch (e) { toast(e.message, { level: 'error' }); } });
      f.append(rej, acc); c.append(h, cmd, f); list.appendChild(c);
    });
  }).catch((e) => { clear(list); list.appendChild(el('div', 'files-empty', { text: e.message })); });
}

function updateInspectorDynamic(sid) {
  if (!sid) return;
  const s = store.session(sid); if (!s) return;
  const idr = document.querySelector('.insp-id');
  if (idr) {
    const nm = idr.querySelector('.nm'); if (nm) nm.textContent = s.name || s.ssh_config || s.id;
    const pill = idr.querySelector('.pill'); if (pill) { pill.className = 'pill ' + (s.status === 'running' ? 'green' : 'gray'); pill.textContent = s.status === 'running' ? '已连接' : '已结束'; }
    idr.querySelector('.lamp2')?.classList.toggle('dead', s.status !== 'running');
  }
}

/* ---- statusbar / topbar ------------------------------------------------ */
function renderStatusbar() {
  const sb = $('statusbar'); clear(sb);
  const link = el('span', 'sb-it'); link.append(el('span', 'd' + (store.linkUp ? '' : ' down')), el('span', { text: store.linkUp ? '已连接' : '重连中…' }));
  const running = store.running().length;
  const clients = (store.clients || []).length + (store.mcpClients || []).length;
  sb.append(link, el('span', 'sb-it', { html: '<b>' + running + '</b> 个会话' }), el('span', 'sb-it', { html: '<b>' + clients + '</b> 个客户端' }), el('span', 'sb-sp'));
  const focus = el('button', 'sb-btn', { title: '专注模式' }); focus.appendChild(icon('max'));
  focus.addEventListener('click', toggleFocus);
  const expand = el('button', 'sb-btn', { title: '展开检查器' }); expand.appendChild(icon('chev-left'));
  expand.addEventListener('click', () => { $('app').classList.remove('inspector-collapsed'); TM.fitActive(); });
  sb.append(focus, expand, el('span', 'sb-it mono', { style: 'font-family:var(--font-mono);font-size:10.5px', text: store.version || '' }));
}
function toggleFocus() {
  const app = $('app');
  const on = !app.classList.contains('focus-mode');
  app.classList.toggle('focus-mode', on);
  app.classList.toggle('inspector-collapsed', on);
  $('sidebar').style.display = on ? 'none' : '';
  setTimeout(() => TM.fitActive(), 60);
}

function updateTopbar() {
  $('versionBadge').textContent = store.version || 'dev';
  const sv = $('sideVersion'); if (sv) sv.textContent = (store.version || 'dev') + ' · ' + (store.linkUp ? '在线' : '离线');
  const theme = document.documentElement.getAttribute('data-theme');
  $('themeIcon').innerHTML = theme === 'dark' ? '<use href="#i-sun"/>' : '<use href="#i-moon"/>';
}

async function refreshApprovalCount() {
  try {
    const r = await REST.approvals();
    const n = (r.approvals || []).filter((a) => a.request && a.request.state === 'pending').length;
    const badge = $('approvalCount');
    badge.hidden = n === 0; badge.textContent = n;
    document.querySelectorAll('.termwin').forEach(() => {});
    const sids = new Set((r.approvals || []).map((a) => a.request && a.request.session_id));
    sids.forEach((sid) => { if (sid) TM.setReviewBadge(sid, (r.approvals || []).filter((a) => a.request && a.request.session_id === sid && a.request.state === 'pending').length); });
    const ib = $('inspReviewBadge'); if (ib) { const sn = store.activeSessionId ? (r.approvals || []).filter((a) => a.request && a.request.session_id === store.activeSessionId && a.request.state === 'pending').length : 0; ib.hidden = sn === 0; ib.textContent = sn; }
  } catch (e) {}
}

/* ---- public ------------------------------------------------------------ */
export const Views = {
  init() {
    buildSidebarSkeleton();
    store.subscribe(() => { renderHosts(); renderExtensions(); renderStatusbar(); updateTopbar(); renderInspector(); $('archiveCount').textContent = store.archived().length; });
    setInterval(() => { document.querySelectorAll('[data-uptime]').forEach((n) => { n.textContent = fmtUptime(Number(n.getAttribute('data-uptime'))); }); }, 1000);
    setInterval(() => { if (store.inspectorTab === 'details' && store.activeSessionId) loadDetailsData(store.activeSessionId); refreshApprovalCount(); }, 8000);
    $('themeToggle').addEventListener('click', () => {
      const cur = document.documentElement.getAttribute('data-theme');
      const next = cur === 'dark' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', next);
      try { localStorage.setItem('termcp2.theme', next); } catch (e) {}
      updateTopbar(); TM.retheme();
    });
    $('approvalBtn').addEventListener('click', () => Modals.approvalQueue(store.activeSessionId));
    $('searchInput').addEventListener('input', (e) => { hostFilter = e.target.value.trim().toLowerCase(); renderHosts(); });
    document.addEventListener('keydown', (e) => { if (e.key === '/' && !/INPUT|TEXTAREA/.test(document.activeElement.tagName) && !document.activeElement.isContentEditable) { e.preventDefault(); $('searchInput').focus(); } });
    WS.on('ui_notify', (j) => toast(j.message || '', { level: j.level === 'warn' ? 'warn' : j.level === 'error' ? 'error' : 'info', title: j.title, duration: (j.duration_seconds || 5) * 1000, onClick: j.session_id ? () => TM.setActiveSession(j.session_id) : undefined }));
    WS.on('approval', (j) => { refreshApprovalCount(); if (j.request && j.request.state === 'pending') toast(j.message || '新的审阅请求', { level: 'warn', title: j.title || '审阅', onClick: j.session_id ? () => { TM.setActiveSession(j.session_id); store.inspectorTab = 'review'; inspKey = ''; renderInspector(); } : undefined }); });
    WS.on('open', () => { refreshApprovalCount(); });
    renderHosts(); renderExtensions(); renderStatusbar(); updateTopbar(); renderInspector();
    refreshApprovalCount();
  },
  refreshApprovalCount,
  invalidateInspector() { inspKey = ''; renderInspector(); },
};