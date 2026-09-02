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
export function fmtDate(ts) {
  if (!ts) return '—';
  return new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
}
export function fmtDay(ts) {
  if (!ts) return '—';
  return new Date(ts * 1000).toLocaleDateString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit' });
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
