/* Dialogs: a tiny modal stack plus the concrete forms. */
import { REST, store } from './api.js';
import { TM, xtermTheme } from './terminal.js';
import { el, icon, clear, esc, toast, fmtDate } from './util.js';

const stack = [];
const root = () => document.getElementById('modalRoot');

function openModal({ title, sub, wide, body, footer, onClose }) {
  const r = root(); r.classList.add('open');
  const overlay = el('div', 'modal-overlay');
  const modal = el('div', 'modal' + (wide ? ' wide' : ''));
  const h = el('div', 'modal-h');
  h.appendChild(el('h2', { text: title }));
  if (sub) h.appendChild(el('span', 'sub', { text: sub }));
  h.appendChild(el('span', 'sp'));
  const x = el('button', 'modal-x', { title: '关闭' }); x.appendChild(icon('x')); h.appendChild(x);
  const b = el('div', 'modal-b'); if (body) b.append(body);
  modal.append(h, b);
  if (footer) { const f = el('div', 'modal-f'); f.append(footer); modal.append(f); }
  overlay.appendChild(modal); r.appendChild(overlay);
  const entry = { overlay, close };
  function close() {
    overlay.remove();
    const i = stack.indexOf(entry); if (i >= 0) stack.splice(i, 1);
    if (!stack.length) r.classList.remove('open');
    if (onClose) onClose();
  }
  x.addEventListener('click', close);
  overlay.addEventListener('mousedown', (e) => { if (e.target === overlay) close(); });
  stack.push(entry);
  return { close, body: b, modal };
}
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && stack.length) stack[stack.length - 1].close(); });

function field(labelText, control, hint) {
  const f = el('div', 'field');
  f.appendChild(el('label', { text: labelText }));
  f.append(control);
  if (hint) f.appendChild(el('span', 'hint', { text: hint }));
  return f;
}
function textInput(value, placeholder, type = 'text') {
  const i = el('input', 'input', { type, placeholder: placeholder || '', autocomplete: 'off' });
  if (value != null) i.value = value;
  return i;
}
function select(options, value) {
  const s = el('select', 'select');
  options.forEach(([v, l]) => { const o = el('option', { value: v, text: l }); if (v === value) o.selected = true; s.appendChild(o); });
  return s;
}
function btn(text, cls, iconName) {
  const b = el('button', 'btn ' + cls);
  if (iconName) b.appendChild(icon(iconName));
  b.appendChild(el('span', { text }));
  return b;
}
function footerWith(...nodes) { const f = document.createDocumentFragment(); nodes.forEach((n) => f.append(n)); return f; }

function confirm({ title, message, confirmText = '确认', danger = false }) {
  return new Promise((resolve) => {
    const body = el('p', { text: message }); body.style.cssText = 'font-size:13px;color:var(--text-2);line-height:1.7';
    const cancel = btn('取消', 'btn-secondary');
    const ok = btn(confirmText, danger ? 'btn-danger' : 'btn-primary');
    const m = openModal({ title, body, footer: footerWith(el('span', 'sp'), cancel, ok), onClose: () => resolve(false) });
    cancel.addEventListener('click', () => m.close());
    ok.addEventListener('click', () => { resolve(true); m.close(); });
  });
}

/* ---- connect / start session ------------------------------------------ */
function connect(connName) {
  const conn = store.connection(connName);
  const nameI = textInput(connName, '会话名称');
  const cmdI = textInput('', '留空 = 登录 shell；或 bash、powershell…');
  const modeS = select([['pty', 'pty（交互终端）'], ['pipe', 'pipe（管道）']], conn?.default_mode || 'pty');
  const body = el('div');
  body.append(field('会话名称（可选）', nameI), field('命令（可选）', cmdI), field('模式', modeS));
  const cancel = btn('取消', 'btn-secondary');
  const go = btn('连接', 'btn-primary', 'play');
  const m = openModal({ title: '连接', sub: connName, body, footer: footerWith(el('span', 'sp'), cancel, go) });
  cancel.addEventListener('click', () => m.close());
  async function submit() {
    go.disabled = true;
    try {
      const payload = { ssh_config: connName, mode: modeS.value, rows: 24, cols: 80, name: nameI.value.trim() || connName };
      if (cmdI.value.trim()) payload.command = cmdI.value.trim();
      const resp = await REST.createSession(payload);
      TM.openSession(resp, { name: payload.name, detected_shell: conn?.default_shell });
      m.close();
      toast('已连接 ' + connName, { level: 'success' });
    } catch (e) { toast(e.message || '连接失败', { level: 'error' }); go.disabled = false; }
  }
  go.addEventListener('click', submit);
  nameI.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });
  setTimeout(() => nameI.focus(), 50);
}

/* ---- host editor ------------------------------------------------------- */
function tomlStr(s) { return '"' + String(s).replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"'; }
function buildToml(f) {
  const L = ['kind = ' + tomlStr(f.kind || 'remote')];
  if (f.description) L.push('description = ' + tomlStr(f.description));
  if ((f.kind || 'remote') === 'remote') {
    L.push('host = ' + tomlStr(f.host || ''));
    L.push('port = ' + (parseInt(f.port) || 22));
    L.push('user = ' + tomlStr(f.user || ''));
    if (f.password) L.push('password = ' + tomlStr(f.password));
    if (f.private_key) L.push('private_key = ' + tomlStr(f.private_key));
    if (f.key_passphrase) L.push('key_passphrase = ' + tomlStr(f.key_passphrase));
    if (f.trust_unknown_host) L.push('trust_unknown_host = true');
    if (f.proxy) L.push('proxy = ' + tomlStr(f.proxy));
  }
  if (f.default_shell) L.push('default_shell = ' + tomlStr(f.default_shell));
  if (f.default_approval) L.push('default_approval = true');
  return L.join('\n') + '\n';
}
function parseToml(t) {
  const f = {};
  String(t).split(/\r?\n/).forEach((line) => {
    const m = line.match(/^\s*([A-Za-z_]+)\s*=\s*(.+?)\s*$/);
    if (!m) return;
    let v = m[2];
    if (v.startsWith('"')) { try { f[m[1]] = JSON.parse(v); } catch (e) { f[m[1]] = v.slice(1, -1); } }
    else if (v === 'true') f[m[1]] = true;
    else if (v === 'false') f[m[1]] = false;
    else if (/^\d+$/.test(v)) f[m[1]] = parseInt(v);
  });
  return f;
}

async function hostEditor(existingName) {
  let f = { kind: 'remote', port: 22, trust_unknown_host: true };
  if (existingName) {
    try { f = Object.assign(f, parseToml(await REST.connectionToml(existingName))); } catch (e) {}
  }
  const nameI = textInput(existingName || '', '例如 prod-web'); if (existingName) nameI.disabled = true;
  const kindS = select([['remote', 'SSH 远程主机'], ['internal', '本机回环']], f.kind || 'remote');
  const hostI = textInput(f.host, '192.168.1.10');
  const portI = textInput(f.port || 22, '22', 'number');
  const userI = textInput(f.user, 'root');
  const passI = textInput(f.password, '密码（可选）', 'password');
  const keyI = el('textarea', 'textarea', { placeholder: '私钥内容（可选，与密码二选一）', rows: 3 }); keyI.value = f.private_key || '';
  const passphrI = textInput(f.key_passphrase, '私钥口令（可选）', 'password');
  const shellI = textInput(f.default_shell, '默认 shell（可选，如 bash）');
  const proxyI = textInput(f.proxy, 'socks5://user:pass@host:port（可选）');
  const trustC = el('input', { type: 'checkbox' }); trustC.checked = !!f.trust_unknown_host;
  const apprC = el('input', { type: 'checkbox' }); apprC.checked = !!f.default_approval;

  const remoteFields = el('div');
  remoteFields.append(
    field('主机地址', hostI),
    (() => { const r = el('div', 'field-row'); r.append(field('端口', portI), field('用户名', userI)); return r; })(),
    field('密码', passI),
    field('私钥', keyI),
    field('私钥口令', passphrI),
    field('代理', proxyI),
    (() => { const c = el('label', 'check'); c.append(trustC, el('span', { text: '信任未知主机密钥' })); return c; })(),
  );
  function syncKind() { remoteFields.style.display = kindS.value === 'remote' ? '' : 'none'; }
  kindS.addEventListener('change', syncKind); syncKind();

  const body = el('div');
  body.append(field('名称', nameI), field('类型', kindS), remoteFields, field('默认 Shell', shellI),
    (() => { const c = el('label', 'check'); c.append(apprC, el('span', { text: '默认审阅每次 AI 写入' })); return c; })());

  const cancel = btn('取消', 'btn-secondary');
  const test = btn('测试', 'btn-secondary', 'zap');
  const save = btn('保存', 'btn-primary', 'check');
  const foot = footerWith(existingName ? btn('删除', 'btn-danger', 'trash') : el('span'), el('span', 'sp'), test, cancel, save);
  const m = openModal({ title: existingName ? '编辑主机' : '添加主机', sub: existingName || '', wide: true, body, footer: foot });
  cancel.addEventListener('click', () => m.close());

  function collect() {
    return {
      kind: kindS.value, host: hostI.value.trim(), port: portI.value, user: userI.value.trim(),
      password: passI.value, private_key: keyI.value.trim(), key_passphrase: passphrI.value,
      default_shell: shellI.value.trim(), proxy: proxyI.value.trim(),
      trust_unknown_host: trustC.checked, default_approval: apprC.checked,
    };
  }
  test.addEventListener('click', async () => {
    test.disabled = true;
    try { const r = await REST.testConnection(buildToml(collect())); toast(r.ok ? `连接成功 · ${r.duration_ms}ms` : (r.error || '连接失败'), { level: r.ok ? 'success' : 'error' }); }
    catch (e) { toast(e.message, { level: 'error' }); }
    test.disabled = false;
  });
  save.addEventListener('click', async () => {
    const name = nameI.value.trim();
    if (!name) { toast('请填写名称', { level: 'warn' }); return; }
    save.disabled = true;
    try { await REST.saveConnection(name, buildToml(collect()), false); m.close(); toast('已保存 ' + name, { level: 'success' }); refreshConnections(); }
    catch (e) { toast(e.message, { level: 'error' }); save.disabled = false; }
  });
  if (existingName) {
    foot.firstChild.addEventListener('click', async () => {
      const ok = await confirm({ title: '删除主机', message: `删除主机配置「${existingName}」？此操作不可撤销。`, confirmText: '删除', danger: true });
      if (!ok) return;
      try { await REST.deleteConnection(existingName); m.close(); toast('已删除', { level: 'success' }); refreshConnections(); }
      catch (e) { toast(e.message, { level: 'error' }); }
    });
  }
  setTimeout(() => nameI.focus(), 50);
}
async function refreshConnections() {
  try { const r = await REST.connections(); store.patch({ connections: r.connections || [] }); } catch (e) {}
}

/* ---- add forward ------------------------------------------------------- */
function forward(sessionId) {
  if (!sessionId) { toast('请先选择一个会话', { level: 'warn' }); return; }
  const dirS = select([['local', '本地转发 (local)'], ['remote', '远程转发 (remote)'], ['dynamic', '动态 SOCKS5 (dynamic)']], 'local');
  const rhI = textInput('', '目标主机，如 127.0.0.1');
  const rpI = textInput('', '目标端口', 'number');
  const lhI = textInput('127.0.0.1', '监听地址');
  const lpI = textInput('0', '监听端口（0 = 自动）', 'number');
  const hint = el('div', 'hint'); hint.style.cssText = 'font-size:11.5px;color:var(--text-3);margin:-4px 0 12px;line-height:1.6';
  const targetFields = el('div');
  targetFields.append(field('目标主机', rhI), field('目标端口', rpI), field('监听地址', lhI));
  const lpField = field('监听端口', lpI);
  function sync() {
    const d = dirS.value;
    targetFields.style.display = d === 'dynamic' ? 'none' : '';
    hint.textContent = d === 'local' ? '把远程主机的端口映射到本地监听端口。'
      : d === 'remote' ? '把本地端口暴露到远程主机的监听端口。'
        : '在本地开启一个 SOCKS5 代理，流量经会话转发。';
    lpField.querySelector('label').textContent = d === 'dynamic' ? 'SOCKS5 端口' : '监听端口';
  }
  dirS.addEventListener('change', sync); sync();
  const body = el('div'); body.append(field('方向', dirS), hint, targetFields, lpField);
  const cancel = btn('取消', 'btn-secondary'); const create = btn('创建', 'btn-primary', 'plus');
  const m = openModal({ title: '添加端口转发', body, footer: footerWith(el('span', 'sp'), cancel, create) });
  cancel.addEventListener('click', () => m.close());
  create.addEventListener('click', async () => {
    const d = dirS.value;
    const bodyP = { direction: d, local_host: lhI.value.trim() || '127.0.0.1', local_port: parseInt(lpI.value) || 0, remote_host: rhI.value.trim(), remote_port: parseInt(rpI.value) || 0 };
    if (d !== 'dynamic' && (!bodyP.remote_host || !bodyP.remote_port)) { toast('请填写目标主机和端口', { level: 'warn' }); return; }
    create.disabled = true;
    try { await REST.addForward(sessionId, bodyP); m.close(); toast('已创建转发', { level: 'success' }); }
    catch (e) { toast(e.message, { level: 'error' }); create.disabled = false; }
  });
}

/* ---- new shell channel ------------------------------------------------- */
function command(sessionId) {
  if (!sessionId) { toast('请先选择一个会话', { level: 'warn' }); return; }
  const nameI = textInput('', '通道名称（可选）');
  const cmdI = textInput('', '留空 = 新登录 shell；或要运行的命令');
  const modeS = select([['pty', 'pty'], ['pipe', 'pipe']], 'pty');
  const body = el('div'); body.append(field('名称', nameI), field('命令', cmdI), field('模式', modeS));
  const cancel = btn('取消', 'btn-secondary'); const run = btn('运行', 'btn-primary', 'play');
  const m = openModal({ title: '新建 shell 通道', body, footer: footerWith(el('span', 'sp'), cancel, run) });
  cancel.addEventListener('click', () => m.close());
  async function submit() {
    run.disabled = true;
    try {
      const payload = { mode: modeS.value, rows: 24, cols: 80, name: nameI.value.trim() || undefined };
      if (cmdI.value.trim()) payload.command = cmdI.value.trim();
      const resp = await REST.createShell(sessionId, payload);
      TM.addChannel(sessionId, resp, nameI.value.trim());
      m.close();
    } catch (e) { toast(e.message, { level: 'error' }); run.disabled = false; }
  }
  run.addEventListener('click', submit);
  cmdI.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });
  setTimeout(() => cmdI.focus(), 50);
}

/* ---- approval queue ---------------------------------------------------- */
async function approvalQueue(sessionId) {
  const body = el('div');
  const m = openModal({ title: '审阅队列', sub: sessionId ? '当前会话' : '全部会话', wide: true, body, footer: footerWith(el('span', 'sp'), btn('关闭', 'btn-secondary')) });
  m.modal.querySelector('.modal-f .btn').addEventListener('click', () => m.close());
  async function load() {
    clear(body);
    let list = [];
    try { const r = await REST.approvals(sessionId); list = r.approvals || []; } catch (e) { body.appendChild(el('p', { text: e.message })); return; }
    const pending = list.filter((a) => a.request && a.request.state === 'pending');
    if (!pending.length) { body.appendChild(el('div', 'files-empty', { text: '暂无待审阅请求' })); return; }
    pending.forEach((a) => {
      const req = a.request;
      const card = el('div', 'appr-card');
      const h = el('div', 'appr-h');
      h.append(el('span', 'src ' + (req.source === 'mcp' ? 'ai' : 'human'), { text: req.source === 'mcp' ? 'AI' : 'REST' }),
        el('span', { text: req.summary || '输入请求' }), el('span', 'at', { text: fmtDate(req.created_at) }));
      const cmd = el('div', 'appr-cmd', { text: req.text || (req.keys || []).join(' ') });
      const f = el('div', 'appr-f');
      const rej = btn('拒绝', 'btn-danger', 'x'); const acc = btn('批准', 'btn-primary', 'check');
      rej.addEventListener('click', async () => { try { await REST.reject(req.id); toast('已拒绝', { level: 'success' }); load(); } catch (e) { toast(e.message, { level: 'error' }); } });
      acc.addEventListener('click', async () => { try { await REST.approve(req.id); toast('已批准', { level: 'success' }); load(); } catch (e) { toast(e.message, { level: 'error' }); } });
      f.append(rej, acc); card.append(h, cmd, f); body.appendChild(card);
    });
  }
  load();
}

/* ---- read-only transcript (archive) ----------------------------------- */
async function transcript(session) {
  const body = el('div'); body.style.cssText = 'height:420px;background:var(--bg-term);border-radius:9px;overflow:hidden';
  const m = openModal({ title: '转录', sub: session.name || session.id, wide: true, body, footer: footerWith(el('span', 'sp'), btn('关闭', 'btn-secondary')) });
  m.modal.querySelector('.modal-f .btn').addEventListener('click', () => m.close());
  let shells = [];
  try { shells = (await REST.shells(session.id)).shells || []; } catch (e) {}
  const primary = shells.sort((a, b) => (a.index || 0) - (b.index || 0))[0];
  const term = new Terminal({ fontSize: 12.5, fontFamily: '"Cascadia Code",Consolas,monospace', lineHeight: 1.2, scrollback: 100000, cursorBlink: false, theme: xtermTheme(), disableStdin: true });
  term.open(body);
  if (primary && primary.id) {
    let off = 0, total = -1;
    try {
      while (true) {
        let max = 512 * 1024; if (total >= 0) max = Math.min(max, total - off); if (max <= 0) break;
        const j = await REST.outputRange(primary.id, 'start=' + off + '&max=' + max);
        total = Number(j.total); if (j.d) term.write(new TextEncoder().encode(j.d));
        const prev = off; off = Number(j.end); if (off === prev || total === 0 || off >= total) break;
      }
    } catch (e) {}
  }
  setTimeout(() => { try { term.resize(Math.max(2, Math.floor(body.clientWidth / 8)), Math.max(2, Math.floor(body.clientHeight / 16))); term.scrollToBottom(); } catch (e) {} }, 60);
}

function renameSession(sid) {
  const s = store.session(sid);
  const inp = textInput(s ? (s.name || '') : '', '会话名称');
  const body = el('div'); body.append(field('会话名称', inp));
  const cancel = btn('取消', 'btn-secondary'); const ok = btn('保存', 'btn-primary', 'check');
  const m = openModal({ title: '重命名会话', body, footer: footerWith(el('span', 'sp'), cancel, ok) });
  cancel.addEventListener('click', () => m.close());
  async function submit() {
    const name = inp.value.trim(); if (!name) { m.close(); return; }
    ok.disabled = true;
    try { await REST.renameSession(sid, name); m.close(); toast('已重命名', { level: 'success' }); }
    catch (e) { toast(e.message, { level: 'error' }); ok.disabled = false; }
  }
  ok.addEventListener('click', submit);
  inp.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });
  setTimeout(() => { inp.focus(); inp.select(); }, 50);
}

export const Modals = { openModal, confirm, connect, hostEditor, forward, command, approvalQueue, transcript, renameSession };