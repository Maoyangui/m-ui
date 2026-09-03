// 通用 UI 组件与格式化工具:toast / 确认框 / 弹窗 / 抽屉 / 事件委托 / 格式化。
import { t } from './i18n.js';

// ---- 转义与格式化 ----
export function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
export function fmtBytes(n, digits = 2) {
  n = Number(n) || 0;
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n.toFixed(0) : n.toFixed(digits)) + ' ' + u[i];
}
// ---- 时区 ----
// 服务器多在 UTC、浏览器又各不相同,面板里所有时间统一按设置中的时区显示(默认 Asia/Shanghai),
// 这样页面上的时间、数据面日志里的时间、流量图的分桶才是同一套。
let TZ = 'Asia/Shanghai';
const dtfCache = new Map();
export function setTimezone(z) {
  z = (z || '').trim() || 'Asia/Shanghai';
  try { new Intl.DateTimeFormat('zh-CN', { timeZone: z }); } catch { z = 'Asia/Shanghai'; } // 名字无效就回落
  if (z !== TZ) { TZ = z; dtfCache.clear(); }
}
export function getTimezone() { return TZ; }
function dtf(opts) {
  const key = JSON.stringify(opts);
  let f = dtfCache.get(key);
  if (!f) { f = new Intl.DateTimeFormat('zh-CN', { timeZone: TZ, hour12: false, ...opts }); dtfCache.set(key, f); }
  return f;
}
// tzOffsetMinutes 面板时区在该时刻相对 UTC 的偏移(分钟),用于把流量图按当地零点/整点分桶
export function tzOffsetMinutes(at = new Date()) {
  const p = dtf({ year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }).formatToParts(at);
  const g = k => Number((p.find(x => x.type === k) || {}).value);
  return Math.round((Date.UTC(g('year'), g('month') - 1, g('day'), g('hour') % 24, g('minute'), g('second')) - at.getTime()) / 60000);
}
// tzDate 把时间戳平移到面板时区:之后用 getUTC* 取到的就是该时区的墙上时间
export function tzDate(ts) { return new Date((ts + tzOffsetMinutes(new Date(ts * 1000)) * 60) * 1000); }

export function fmtDate(ts) {
  if (!ts) return '—';
  return dtf({ year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(new Date(ts * 1000));
}
// fmtTime 到秒,列表里更紧凑(不带年份)
export function fmtTime(ts) {
  if (!ts) return '—';
  return dtf({ month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date(ts * 1000));
}
export function fmtDay(ts) {
  if (!ts) return '—';
  return dtf({ year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date(ts * 1000));
}
export function fmtRelative(ts) {
  if (!ts) return '—';
  const d = Math.floor(Date.now() / 1000 - ts);
  if (d < 60) return t('time.justNow');
  if (d < 3600) return t('time.minAgo', { n: Math.floor(d / 60) });
  if (d < 86400) return t('time.hourAgo', { n: Math.floor(d / 3600) });
  return t('time.dayAgo', { n: Math.floor(d / 86400) });
}
export function fmtDuration(sec) {
  sec = Number(sec) || 0;
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  return (d ? d + t('time.d') + ' ' : '') + (h ? h + t('time.h') + ' ' : '') + m + t('time.m');
}
export function daysLeft(expiry) { return Math.ceil((expiry - Date.now() / 1000) / 86400); }

// ---- toast ----
let toastTimer;
export function toast(msg, kind = '') {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast ' + kind;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 2800);
}

// ---- 弹窗 ----
export function openModal(title, html, onSave, opts = {}) {
  const m = document.getElementById('modal');
  document.getElementById('modal-title').textContent = title;
  document.getElementById('modal-body').innerHTML = html;
  document.getElementById('modal-err').textContent = '';
  const save = document.getElementById('modal-save');
  save.hidden = !onSave;
  save.textContent = opts.saveText || t('common.save');
  save.className = 'btn ' + (opts.danger ? 'danger' : 'primary');
  m.hidden = false;
  m.querySelector('.modal-box').classList.toggle('wide', !!opts.wide);
  save.onclick = async () => {
    save.disabled = true;
    try { await onSave(); closeModal(); }
    catch (e) { document.getElementById('modal-err').textContent = e.message; }
    finally { save.disabled = false; }
  };
  const first = m.querySelector('input,select,textarea');
  if (first) setTimeout(() => first.focus(), 50);
}
export function closeModal() { document.getElementById('modal').hidden = true; }

// ---- 确认框(替代 window.confirm,风格一致且可 await)----
export function confirm(message, opts = {}) {
  return new Promise(resolve => {
    openModal(opts.title || t('common.confirm'), `<p class="confirm-text">${esc(message)}</p>`, async () => resolve(true),
      { saveText: opts.okText || t('common.confirm'), danger: opts.danger });
    const m = document.getElementById('modal');
    const cancel = () => { resolve(false); m.removeEventListener('modal-cancel', cancel); };
    m.addEventListener('modal-cancel', cancel, { once: true });
  });
}

// ---- 抽屉(右侧详情面板)----
export function openDrawer(title, html) {
  const d = document.getElementById('drawer');
  document.getElementById('drawer-title').innerHTML = title;
  document.getElementById('drawer-body').innerHTML = html;
  d.hidden = false;
  void d.offsetWidth; // 强制回流后再加 class,过渡动画在后台标签页也能正确触发
  d.classList.add('open');
}
export function closeDrawer() {
  const d = document.getElementById('drawer');
  d.classList.remove('open');
  setTimeout(() => { d.hidden = true; }, 200);
}
export function drawerOpen() { return !document.getElementById('drawer').hidden; }

// ---- 事件委托:data-act="name" data-id="..." ----
const actions = {};
export function registerActions(map) { Object.assign(actions, map); }
document.addEventListener('click', e => {
  const el = e.target.closest('[data-act]');
  if (!el) return;
  const fn = actions[el.dataset.act];
  if (!fn) return;
  e.preventDefault();
  fn(el.dataset.id, el, e);
});
document.addEventListener('change', e => {
  const el = e.target.closest('[data-change]');
  if (!el) return;
  const fn = actions[el.dataset.change];
  if (fn) fn(el.dataset.id, el, e);
});

// ---- 下拉菜单:表格容器有 overflow 会裁掉菜单,展开时改为 fixed 定位到 summary 下方 ----
let menuOpenedAt = 0;
document.addEventListener('toggle', e => {
  const d = e.target;
  if (!(d instanceof HTMLDetailsElement) || !d.classList.contains('menu')) return;
  const list = d.querySelector('.menu-list');
  if (!list) return;
  if (!d.open) { list.classList.remove('fixed'); list.style.top = list.style.left = list.style.right = ''; return; }
  menuOpenedAt = Date.now();
  const r = d.querySelector('summary').getBoundingClientRect();
  list.classList.add('fixed');
  list.style.top = (r.bottom + 4) + 'px';
  list.style.left = 'auto';
  list.style.right = Math.max(8, window.innerWidth - r.right) + 'px';
  // 下方放不下就往上翻
  const h = list.offsetHeight;
  if (r.bottom + 4 + h > window.innerHeight) list.style.top = Math.max(8, r.top - 4 - h) + 'px';
}, true);
// 页面滚动/缩放时收起(fixed 定位会脱离原位置);刚打开 300ms 内的滚动事件(如 scrollIntoView 尾巴)忽略
['scroll', 'resize'].forEach(ev => window.addEventListener(ev, () => {
  if (Date.now() - menuOpenedAt < 300) return;
  document.querySelectorAll('details.menu[open]').forEach(d => d.removeAttribute('open'));
}, true));

// ---- 剪贴板 ----
export async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast(t('common.copied'), 'ok'); }
  catch {
    const ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    document.execCommand('copy'); ta.remove();
    toast(t('common.copied'), 'ok');
  }
}

// ---- 小部件 ----
export const badge = (text, kind = '') => `<span class="badge ${kind}">${esc(text)}</span>`;
export const dot = on => `<span class="dot ${on ? 'on' : ''}" title="${on ? t('common.online') : t('common.offline')}"></span>`;
export function progress(used, total) {
  if (!total) return `<div class="progress"><div class="bar" style="width:0"></div></div>`;
  const pct = Math.min(100, Math.round(used / total * 100));
  const kind = pct >= 100 ? 'danger' : pct >= 80 ? 'warn' : '';
  return `<div class="progress ${kind}" title="${pct}%"><div class="bar" style="width:${pct}%"></div></div>`;
}
export const field = (label, input, help = '') =>
  `<label class="field"><span class="label">${esc(label)}</span>${input}${help ? `<span class="help">${esc(help)}</span>` : ''}</label>`;
export const check = (id, label, checked, help = '') =>
  `<label class="field check"><input type="checkbox" id="${id}" ${checked ? 'checked' : ''}><span>${esc(label)}</span>${help ? `<span class="help">${esc(help)}</span>` : ''}</label>`;
export const empty = (text) => `<div class="empty">${esc(text || t('common.empty'))}</div>`;
export const fv = id => ((document.getElementById(id) || {}).value || '');
export const fchk = id => !!(document.getElementById(id) || {}).checked;

// 简易搜索:在多字段里做不区分大小写的包含匹配
export function matches(q, ...fields) {
  q = (q || '').trim().toLowerCase();
  if (!q) return true;
  return fields.some(f => String(f ?? '').toLowerCase().includes(q));
}
export function debounce(fn, ms = 200) {
  let tm;
  return (...a) => { clearTimeout(tm); tm = setTimeout(() => fn(...a), ms); };
}
