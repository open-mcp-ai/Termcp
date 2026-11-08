/* Small DOM + formatting helpers shared across views. */
const SVGNS = 'http://www.w3.org/2000/svg';

export function el(tag, cls, attrs) {
  if (cls && typeof cls === 'object') { attrs = cls; cls = null; }
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (attrs) for (const k in attrs) {
    if (k === 'text') e.textContent = attrs[k];
    else if (k === 'html') e.innerHTML = attrs[k];
    else if (k.startsWith('on') && typeof attrs[k] === 'function') e.addEventListener(k.slice(2), attrs[k]);
    else if (attrs[k] != null) e.setAttribute(k, attrs[k]);
  }
  return e;
}
export function icon(name, cls) {
  const s = document.createElementNS(SVGNS, 'svg');
  s.setAttribute('class', 'ic' + (cls ? ' ' + cls : ''));
  const u = document.createElementNS(SVGNS, 'use');
  u.setAttribute('href', '#i-' + name);
  s.appendChild(u);
  return s;
}
export function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}
export function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); return node; }

export function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const u = ['KB', 'MB', 'GB', 'TB']; let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n >= 100 ? 0 : 1) + ' ' + u[i];
}
export function fmtUptime(createdAt) {
  if (!createdAt) return '00:00:00';
  let s = Math.max(0, Math.floor((Date.now() - createdAt) / 1000));
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  const p = (x) => String(x).padStart(2, '0');
  return `${p(h)}:${p(m)}:${p(s)}`;
}
export function fmtTime(ms) {
  if (!ms) return '';
  try { return new Date(ms).toLocaleTimeString('zh-CN', { hour12: false }); } catch (e) { return ''; }
}
export function fmtDate(ms) {
  if (!ms) return '';
  try { return new Date(ms).toLocaleString('zh-CN', { hour12: false }); } catch (e) { return ''; }
}
export function ago(ms) {
  if (!ms) return '';
  const d = Date.now() - ms;
  if (d < 60000) return '刚刚';
  if (d < 3600000) return Math.floor(d / 60000) + ' 分钟前';
  if (d < 86400000) return Math.floor(d / 3600000) + ' 小时前';
  return Math.floor(d / 86400000) + ' 天前';
}
export function debounce(fn, ms) {
  let t; return function (...a) { clearTimeout(t); t = setTimeout(() => fn.apply(this, a), ms); };
}

export function toast(message, opts = {}) {
  const root = document.getElementById('toastRoot');
  if (!root) return;
  const level = opts.level || 'info';
  const t = el('div', 'toast ' + level);
  const ic = el('div', 'tic');
  ic.appendChild(icon(level === 'error' ? 'alert' : level === 'success' ? 'check' : level === 'warn' ? 'alert' : 'bell'));
  const tx = el('div', 'tt');
  if (opts.title) tx.appendChild(el('div', 'ttl', { text: opts.title }));
  tx.appendChild(el('div', 'msg', { text: message }));
  const x = el('div', 'tx'); x.appendChild(icon('x', 'sm'));
  t.append(ic, tx, x);
  let gone = false;
  const remove = () => { if (gone) return; gone = true; t.style.opacity = '0'; t.style.transition = '.2s'; setTimeout(() => t.remove(), 200); };
  x.addEventListener('click', (e) => { e.stopPropagation(); remove(); });
  t.addEventListener('click', () => { if (opts.onClick) opts.onClick(); remove(); });
  root.appendChild(t);
  const dur = opts.duration == null ? 5000 : opts.duration;
  if (dur > 0) setTimeout(remove, dur);
  return t;
}