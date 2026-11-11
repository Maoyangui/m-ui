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
// 流量输入框里显示的 GB 数:最多 6 位小数、去掉尾零。不能取整 —— 0.1 GB 取整成 0,再保存就成了不限量
export function gbValue(bytes) {
  if (!bytes) return 0;
  return +(bytes / 1073741824).toFixed(6) || 0.000001;
}
// 输入框里的 GB 数换成字节(后端要整数)
export const gbBytes = v => Math.round(Number(v) * 1073741824);

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
// 标题和按钮固定、正文自己滚动(长表单也看得见「保存」);opts.wide 加宽,opts.size === 'xl' 给带选择器的编辑框用。
export function openModal(title, html, onSave, opts = {}) {
  closeMenus(); // 菜单的层级比弹窗高,开着就会压在弹窗上
  const m = document.getElementById('modal');
  document.getElementById('modal-title').textContent = title;
  const body = document.getElementById('modal-body');
  body.innerHTML = html;
  body.scrollTop = 0;
  enhanceCheckLists(body);
  document.getElementById('modal-err').textContent = '';
  const save = document.getElementById('modal-save');
  save.hidden = !onSave;
  save.disabled = false; // 上一个对话框可能把它禁用着关掉(换套餐在等预览时禁用提交)
  save.textContent = opts.saveText || opts.okText || t('common.save');
  save.className = 'btn ' + (opts.danger ? 'danger' : 'primary');
  document.getElementById('modal-cancel').hidden = !!opts.noCancel; // 纯提示只留一个「知道了」
  m.hidden = false;
  m.querySelector('.modal-box').className = 'modal-box' + (opts.wide ? ' wide' : '') + (opts.size === 'xl' ? ' xl' : '');
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
      { saveText: opts.okText || t('common.confirm'), danger: opts.danger, noCancel: opts.notice });
    const m = document.getElementById('modal');
    const cancel = () => { resolve(false); m.removeEventListener('modal-cancel', cancel); };
    m.addEventListener('modal-cancel', cancel, { once: true });
  });
}

// ---- 抽屉(右侧详情面板)----
export function openDrawer(title, html, opts = {}) {
  closeMenus();
  const d = document.getElementById('drawer');
  d.classList.toggle('over', !!opts.over); // 从弹窗里打开的:盖在弹窗上面
  d.classList.toggle('wide', !!opts.wide); // 内容宽的(用户详情的流量图、各服务器延迟表)用宽抽屉
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

// setHTML / setText 安全写入:
//   1) 元素不在就跳过。异步渲染回来时页面可能已经切走,直接写 null 会抛异常,
//      而这个异常会被上一次路由的错误处理接住,把报错糊到刚画好的新页面上。
//   2) 内容和上次一字不差也跳过。刷新变快之后,整张表每几秒重画一次会闪、会丢滚动位置,
//      内容没变就不动 DOM。
export function setHTML(target, html) {
  const el = typeof target === 'string' ? document.getElementById(target) : target;
  if (!el) return false;
  if (el.__muiHTML === html) return false;
  el.__muiHTML = html;
  // 5 秒一轮的重画会把打开着的「更多」菜单换成一个没打开的(运行时长、在线人数一变就整表重画):
  // 按菜单里第一个按钮的 data-act + data-id 认出是哪一个,重画后再打开(toggle 会重新定位)
  const menus = typeof el.querySelectorAll === 'function' ? [...el.querySelectorAll('details.menu[open]')].map(menuKey).filter(Boolean) : [];
  el.innerHTML = html;
  for (const [act, id] of menus) {
    const b = [...el.querySelectorAll('details.menu .menu-list [data-act]')].find(x => x.dataset.act === act && (x.dataset.id || '') === id);
    const d = b && b.closest('details.menu');
    if (d) d.open = true;
  }
  return true;
}
// 收起所有打开着的「更多」等下拉菜单
export function closeMenus() {
  document.querySelectorAll('details.menu[open]').forEach(d => d.removeAttribute('open'));
}
function menuKey(d) {
  const b = d.querySelector('.menu-list [data-act]');
  return b ? [b.dataset.act, b.dataset.id || ''] : null;
}

export function setText(target, text) {
  const el = typeof target === 'string' ? document.getElementById(target) : target;
  if (!el || el.textContent === text) return false;
  el.textContent = text;
  return true;
}
document.addEventListener('click', e => {
  const el = e.target.closest('[data-act]');
  if (!el) return;
  const fn = actions[el.dataset.act];
  if (!fn) return;
  e.preventDefault();
  // 点了「更多」菜单里的项就收起菜单:不然 setHTML 重画时会把它当成还开着、重新打开,
  // 菜单(z-index 高于弹窗)就一直挂着、压在点出来的弹窗上。要连点几下的菜单(外部节点「服务器列」的全选 / 清空)标 data-keep-open
  const menu = el.closest('details.menu');
  if (menu && !menu.hasAttribute('data-keep-open')) menu.removeAttribute('open');
  fn(el.dataset.id, el, e);
  // 用户刚动过手,接下来十几秒刷快一点,好让连接数、在线状态这类立刻跟上(app.js 监听)
  document.dispatchEvent(new CustomEvent('mui:acted'));
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
['scroll', 'resize'].forEach(ev => window.addEventListener(ev, e => {
  if (Date.now() - menuOpenedAt < 300) return;
  // 菜单自己的列表在滚(外部节点的「服务器列」菜单服务器多时限高滚动):不收,不然后面几台勾不到
  if (e && e.target instanceof Element && e.target.closest('details.menu')) return;
  closeMenus();
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
// 拼 URL 的主机部分(和后端 model.URLHost 一致):协议的默认端口不写,http 跑在 443 上不能省
export const urlHost = (scheme, host, port) => ((scheme === 'https' && Number(port) === 443) || (scheme === 'http' && Number(port) === 80) ? host : host + ':' + port);

// 地址 + 端口的显示:IPv6 加方括号,不然分不清端口
export const hostPort = (host, port) => (String(host).includes(':') ? `[${host}]:${port}` : `${host}:${port}`);

// 设备数按它算(和后端 model.IPKey 一致):IPv6 取 /64,一台设备的几个临时地址算一台
export function ipKey(ip) {
  if (!String(ip).includes(':')) return ip;
  const [h, tail = ''] = String(ip).split('::');
  const a = h ? h.split(':') : [], b = tail ? tail.split(':') : [];
  return [...a, ...Array(Math.max(0, 8 - a.length - b.length)).fill('0'), ...b].slice(0, 4).map(x => parseInt(x, 16).toString(16)).join(':');
}
export const deviceCount = ips => new Set((ips || []).map(ipKey)).size;

export const field = (label, input, help = '') =>
  `<label class="field"><span class="label">${esc(label)}</span>${input}${help ? `<span class="help">${esc(help)}</span>` : ''}</label>`;
// fieldBox 同 field,但外层是 <div>:选择器、勾选列表、一排勾选框这类复合控件不能包在 <label> 里 ——
// 点标签字、说明或空白处,浏览器会把点击转给里面第一个控件(规则目标被切成「全部用户」、第一台服务器被勾掉)
export const fieldBox = (label, inner, help = '') =>
  `<div class="field"><span class="label">${esc(label)}</span>${inner}${help ? `<span class="help">${esc(help)}</span>` : ''}</div>`;
export const check = (id, label, checked, help = '') =>
  `<label class="field check"><input type="checkbox" id="${id}" ${checked ? 'checked' : ''}><span>${esc(label)}</span>${help ? `<span class="help">${esc(help)}</span>` : ''}</label>`;
// 落地页公告、联系方式的字数上限(和后端 web/page_logo.go 的 maxPageNotice / maxPageSupport 一致)
export const PAGE_TEXT_MAX = { notice: 4000, support: 300 };

// 有字数上限的文字框:框下面显示"当前字数 / 上限",和后端同一个算法(去掉首尾空白后按字符数),超过了标红。
// charCount 放在框后面,画好之后调一次 bindCharCounts 挂上输入事件。上限以后端为准。
export const charCount = (id, max) => `<span class="count" data-count-for="${esc(id)}" data-max="${Number(max)}"></span>`;
export function bindCharCounts(root = document) {
  root.querySelectorAll('[data-count-for]').forEach(el => {
    const box = document.getElementById(el.dataset.countFor);
    if (!box || el.dataset.bound) return;
    el.dataset.bound = '1';
    const max = Number(el.dataset.max);
    const show = () => {
      const n = [...box.value.trim()].length;
      el.textContent = t('common.charCount', { n, max });
      el.classList.toggle('over', n > max);
    };
    box.addEventListener('input', show);
    show();
  });
}
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

// ---- chip 折叠:服务器、在线用户这类可能很多的名单,先露前 max 个,其余收进「+N」----
// 展开状态按 key 记在模块里,5 秒一轮的 setHTML 重画后不会自己收起。
// lazy:折起时只渲染露出来的那几个(2000 人在线也只画几十个),展开后最多渲染 cap 个;
// 这种情况切换展开要让页面重画,页面用 onChipToggle(key, fn) 登记重画函数。
const openChips = new Set();
const chipRedraw = {};
export const chipOpen = key => openChips.has(key);
export function onChipToggle(key, fn) { chipRedraw[key] = fn; }
export function chipList(items, { key = '', max = 12, lazy = false, cap = 300, cls = '' } = {}) {
  const base = 'chips' + (cls ? ' ' + cls : '');
  if (items.length <= max) return `<span class="${base}">${items.join('')}</span>`;
  const open = !!key && openChips.has(key), n = items.length - max;
  const shown = lazy ? items.slice(0, open ? Math.max(max, cap) : max) : items;
  return `<span class="${base} fold${open ? ' open' : ''}">${shown.map((h, i) => (i < max ? h : `<span class="fold-x">${h}</span>`)).join('')}` +
    `<button type="button" class="chip more" data-act="ui.chips" data-id="${esc(key)}" data-n="${n}" aria-expanded="${open}">${open ? esc(t('common.less')) : '+' + n}</button></span>`;
}
registerActions({
  'ui.chips': (key, el) => {
    const box = el.closest('.fold');
    const o = key ? !openChips.has(key) : !(box && box.classList.contains('open'));
    if (key) { if (o) openChips.add(key); else openChips.delete(key); }
    if (key && chipRedraw[key]) { chipRedraw[key](); return; }
    if (box) box.classList.toggle('open', o);
    el.textContent = o ? t('common.less') : '+' + el.dataset.n;
    el.setAttribute('aria-expanded', String(o));
  },
});

// ---- 审计对象的显示(概况「最近操作」和日志页共用)----
// 字符串原样,对象写成 JSON,null / undefined 为空;数组超过 max 项写成「a, b, c 等 N 个」(max 为 0 不截)。
// 截断交给 CSS 的省略号,全文放 title:不在这里按字数切(切出半个 &amp; 还没有省略号)。
export function auditText(v, { max = 3 } = {}) {
  if (typeof v === 'string') { try { v = JSON.parse(v); } catch { return v; } }
  const one = x => (x === null || x === undefined ? '' : typeof x === 'object' ? JSON.stringify(x) : String(x));
  if (Array.isArray(v)) {
    const parts = v.map(one);
    if (max > 0 && parts.length > max) return t('common.andMore', { list: parts.slice(0, max).join(', '), n: parts.length, more: parts.length - max });
    return parts.join(', ');
  }
  return one(v);
}

// ---- 长勾选列表:超过 12 项的 .check-list 前面加 筛选 / 全选 / 全不选 / 计数 ----
// 只作用于看得见、没禁用的勾选框,并补发 change 事件(线路编辑里的 IP 版本联动照常);筛掉的只是藏起来,勾选不丢。
// 选择器自己有搜索的列表(.lp-list)不加。openModal 写入正文后自动调用,别处渲染的列表可以自己调。
export function enhanceCheckLists(root) {
  if (!root || !root.querySelectorAll) return;
  for (const list of root.querySelectorAll('.check-list:not(.lp-list)')) {
    if (list.dataset.cl || list.children.length <= 12) continue;
    list.dataset.cl = '1';
    const bar = document.createElement('div');
    bar.className = 'cl-tools';
    bar.innerHTML = `<input type="search" class="sm cl-q" placeholder="${esc(t('cl.filter'))}" aria-label="${esc(t('cl.filter'))}">
      <button type="button" class="btn sm ghost" data-cl="all">${esc(t('cl.all'))}</button>
      <button type="button" class="btn sm ghost" data-cl="none">${esc(t('cl.none'))}</button>
      <span class="muted small cl-count"></span>`;
    list.before(bar);
    const boxes = () => [...list.querySelectorAll('input[type=checkbox]')];
    const count = () => {
      const all = boxes();
      bar.querySelector('.cl-count').textContent = t('lp.count', { n: all.filter(c => c.checked).length, total: all.length });
    };
    bar.querySelector('.cl-q').addEventListener('input', e => {
      const q = e.target.value.trim().toLowerCase();
      for (const item of list.children) item.hidden = !!q && !(item.textContent || '').toLowerCase().includes(q);
    });
    bar.addEventListener('click', e => {
      const b = e.target.closest('[data-cl]');
      if (!b) return;
      for (const item of list.children) {
        if (item.hidden) continue;
        const cb = item.matches('input[type=checkbox]') ? item : item.querySelector('input[type=checkbox]');
        const want = b.dataset.cl === 'all';
        if (!cb || cb.disabled || cb.checked === want) continue;
        cb.checked = want;
        cb.dispatchEvent(new Event('change', { bubbles: true }));
      }
      count();
    });
    list.addEventListener('change', count);
    count();
  }
}
