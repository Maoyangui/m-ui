import { state, load, isReseller, takeHashParams } from '../app.js';
import { get, post, put, del, qrUrl, upload } from '../api.js';
import { t } from '../i18n.js';
import { deviceCount, gbValue, gbBytes, esc, fmtBytes, fmtDay, fmtDate, fmtDuration, fmtRelative, daysLeft, toast, confirm, openModal, openDrawer, closeDrawer, registerActions, badge, dot, progress, field, check, empty, fv, fchk, matches, debounce, copy, setHTML, setText, chipList, fieldBox } from '../ui.js';

// 生效中的规则限速(主面板才有):列表徽章与详情抽屉都从这里取
const limitsOf = id => (state.limitStates || []).filter(s => s.userId === id);
function ruleLimitsHTML(u) {
  const lim = limitsOf(u.id);
  if (!lim.length) return '';
  const now = Math.floor(Date.now() / 1000);
  return `<h3 style="margin-top:.6rem">${t('user.ruleLimits')}</h3><div class="chips">${lim.map(s => `<span class="chip" title="${esc(s.reason || '')}">${esc(s.ruleName)} · ${s.upMbps || '—'}/${s.downMbps || '—'} M · ${s.until ? t('rule.remaining', { n: Math.max(0, Math.ceil((s.until - now) / 60)) }) : t('rule.untilWindowEnd')}</span>`).join('')}</div>`;
}
import { barChart, bucketFor } from '../chart.js';
import { serverOfIP, relayIPs, deviceIPs } from '../relayip.js';
import { lineItems, keysFromRefs, linePicker, refLabels, refChips } from '../linepicker.js';

// 代理面板没有服务器页:服务器名与授权范围来自 status;主面板用 state.nodes
const pickerNodes = () => (Array.isArray(state.nodes) && state.nodes.length) ? state.nodes : (Array.isArray(state.status.nodes) ? state.status.nodes : []);
// relay: true —— 线路的中转入口也一个个列出来,和服务器并排着勾(只影响订阅里发不发)
const pickerItems = () => lineItems(state.lines, pickerNodes(), isReseller() ? (state.status.grants || []) : null, { relay: true });
const refsOf = u => u.lineRefs || (u.lineIds || []).map(id => ({ lineId: id }));
let picker = null;
import { tzOffsetMinutes, switchRow, toggleBatchBar } from '../ui.js';

export const title = () => t('user.title');
export const subtitle = () => t('user.subtitle');
let query = '', filter = 'all', drawerUser = null, sortKey = 'id', sortDir = 1;
// 快捷筛选:套餐(0 = 不限);only = 'expiring' | 'quota'(概况的"查看全部"带过来,工具栏里显示一个可清除的 chip,不进状态按钮)
let planFilter = 0, only = '';
// 全选只勾当前页;筛选结果多于一页时,批量条里另给"选中全部 N 个筛选结果"
const selected = new Set();
let selFrom = null; // 刚勾 / 取消的那个勾选框:批量栏露出 / 收起时让它那一行不跳(见 ui.js toggleBatchBar)
// 分页:全部用户 → 搜索 → 筛选 → 排序 → 只渲染当前页。上千用户时不再一次画出整张表。
const PAGE_SIZES = [25, 50, 100];
let page = 1, pageSize = 25;
try { const v = Number(localStorage.getItem('m-ui-users-ps')); if (PAGE_SIZES.includes(v)) pageSize = v; } catch {}
let lastPager = '';
let relayNow = new Set(); // 中转机的地址(每次画列表、刷新详情时重算):数在线设备时不算它们
const SORTS = {
  id: u => u.id, name: u => u.name.toLowerCase(), usage: u => (u.up || 0) + (u.down || 0),
  expiry: u => u.expiry || Number.MAX_SAFE_INTEGER, devices: u => deviceCount(deviceIPs(u.onlineIps, relayNow)), status: u => (u.enabled ? 0 : 1),
};
const th = (key, label, cls = '') => `<th class="sortable ${sortKey === key ? 'sorted' : ''} ${cls}" data-act="user.sort" data-id="${key}">${label}${sortKey === key ? (sortDir > 0 ? ' ▲' : ' ▼') : ''}</th>`;

const now = () => Math.floor(Date.now() / 1000);
const isExpired = u => u.expiry > 0 && u.expiry < now();
const isOver = u => u.volume > 0 && (u.up + u.down) > u.volume;
// 「快到期」「流量将尽」的口径:主面板跟 Telegram 预警同一组设置(默认 3 天 / 80%),代理面板固定 7 天 / 80%。概况共用
export function expiringDays() {
  if (isReseller()) return 7;
  const d = Number((state.settings || {}).tgExpiringDays);
  return d > 0 ? d : 3;
}
export function quotaPct() {
  if (isReseller()) return 80;
  const p = Number((state.settings || {}).tgQuotaPercent);
  return p > 0 && p <= 100 ? p : 80;
}
const isExpiring = u => u.enabled && u.expiry > now() && u.expiry - now() <= expiringDays() * 86400;
const isQuota = u => u.enabled && u.volume > 0 && (u.up + u.down) * 100 >= u.volume * quotaPct();
// 筛选里的「即将到期」一类:现在还能正常用(启用、没过期、没超量),但 N 天内到期,或者流量已经用到 P% 以上 —— 快用完也算在这一类里。
// 概况的「需要处理」不列这些人(用户不续费,管理员没什么可处理的),想看就来这里筛。
const isSoon = u => !isExpired(u) && !isOver(u) && (isExpiring(u) || isQuota(u));
const FILTERS = ['all', 'online', 'enabled', 'disabled', 'soon', 'expired', 'over'];
const onlineSet = () => new Set((state.onlines && state.onlines.users) || []);
const planSelect = (id, cur, allowNone = true) =>
  `<select id="${id}">${allowNone ? `<option value="0">${t('plan.none')}</option>` : ''}${state.plans.map(p => `<option value="${p.id}" ${cur === p.id ? 'selected' : ''}>${esc(p.name)} · ${p.volumeGb ? p.volumeGb + 'GB' : '∞'} / ${p.days ? p.days + t('common.day') : '∞'}</option>`).join('')}</select>`;

export async function render(el) {
  // 概况等页面的链接:#/users?f=online|soon|expiring|quota…、#/users?q=名字(恰好一个同名用户时直接打开详情)
  const hp = takeHashParams('users');
  let openId = 0;
  if (hp.has('f') || hp.has('q')) {
    const f = hp.get('f') || '';
    filter = FILTERS.includes(f) ? f : 'all';
    only = f === 'expiring' || f === 'quota' ? f : '';
    planFilter = 0;
    query = hp.get('q') || '';
    page = 1;
    selected.clear(); // 换成了另一份列表:以前勾的人不能悄悄带进下一次批量操作
    const hit = query ? state.users.filter(u => u.name === query) : [];
    if (hit.length === 1) openId = hit[0].id;
  }
  if (planFilter && !state.plans.some(p => p.id === planFilter)) planFilter = 0; // 筛着的套餐删掉了:下拉显示「全部套餐」,列表也别再按它筛
  el.innerHTML = `
    <div class="toolbar tb-users">
      <input type="search" id="user-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      <div class="tb-filters"><div class="seg" id="user-filter">${FILTERS.map(f => `<button data-act="user.filter" data-id="${f}" class="${f === filter ? 'active' : ''}"${f === 'soon' ? ` title="${esc(t('user.filter.soonHelp', { d: expiringDays(), p: quotaPct() }))}"` : ''}>${t('user.filter.' + f)}</button>`).join('')}</div>
      ${state.plans.length ? `<select class="sm" id="user-plan" style="width:auto;max-width:12rem" aria-label="${esc(t('user.plan'))}"><option value="0">${esc(t('user.allPlans'))}</option>${state.plans.map(pl => `<option value="${pl.id}" ${pl.id === planFilter ? 'selected' : ''}>${esc(pl.name)}</option>`).join('')}</select>` : ''}
      <span id="user-only"></span>
      <span class="muted small" id="user-count"></span></div>
      <span class="grow"></span>
      ${isReseller() ? '' : `<details class="menu"><summary class="btn">${t('common.more')}</summary><div class="menu-list">
        <button data-act="user.bulk"><b>${esc(t('user.bulk'))}</b><span class="muted small" style="display:block">${esc(t('user.menu.bulk'))}</span></button>
        <button data-act="user.import"><b>${esc(t('user.import'))}</b><span class="muted small" style="display:block">${esc(t('user.menu.import'))}</span></button>
        <button data-act="user.export"><b>${esc(t('user.batch.export'))}</b><span class="muted small" style="display:block">${esc(t('user.menu.export'))}</span></button>
      </div></details>`}
      <button class="btn primary" data-act="user.add">${t('user.add')}</button>
    </div>
    <div class="toolbar batch-bar" id="batch-bar" hidden>
      <span class="badge primary" id="batch-count"></span>
      <button class="btn sm ghost" data-act="user.selAllFiltered" id="sel-all-filtered" hidden></button>
      <button class="btn sm" data-act="user.batch" data-id="enable">${t('user.batch.enable')}</button>
      <button class="btn sm" data-act="user.batch" data-id="disable">${t('user.batch.disable')}</button>
      <button class="btn sm" data-act="user.batch" data-id="extend">${t('user.batch.extend')}</button>
      <button class="btn sm" data-act="user.batch" data-id="plan">${t('user.batch.plan')}</button>
      <button class="btn sm" data-act="user.batch" data-id="reset">${t('user.batch.reset')}</button>
      <button class="btn sm danger" data-act="user.batch" data-id="delete">${t('user.batch.delete')}</button>
      <span class="grow"></span>
      <button class="btn sm ghost" data-act="user.clearSel">${t('common.cancel')}</button>
    </div>
    <div class="table-wrap fill" style="--fill-reserve:10rem"><table class="grid" id="users-table" data-fit="p3:1060 p2:980 cards:840">
      <thead><tr><th style="width:1.5rem"><input type="checkbox" id="sel-all"></th>${th('name', t('common.name'))}${th('status', t('common.status'))}${th('usage', t('user.usage'))}${th('expiry', t('user.expiry'))}${th('devices', t('user.devices'), 'c-p2')}<th class="c-p3">${t('user.speed')}</th><th></th></tr></thead>
      <tbody id="users-body"></tbody>
    </table></div>
    <div class="pager" id="user-pager" hidden></div>`;
  document.getElementById('user-q').addEventListener('input', debounce(e => { query = e.target.value; page = 1; renderRows(); }));
  // 表头的全选只勾当前页显示的用户:免得以为选了 25 个,实际操作了几千个
  document.getElementById('sel-all').addEventListener('change', e => {
    pageRows().forEach(u => e.target.checked ? selected.add(u.id) : selected.delete(u.id));
    renderRows();
  });
  const ps = document.getElementById('user-plan');
  if (ps) ps.addEventListener('change', e => { planFilter = Number(e.target.value) || 0; page = 1; renderRows(); });
  lastPager = '';
  renderRows();
  if (openId) showDetail(openId);
  // 概览「开始使用」的「建用户」:#/users?new=1,直接打开新建框,线路默认全勾 ——
  // 指引先建线路后建用户,线路的「分配给全部用户」只管当时已有的用户;不勾的话照着三步走完订阅是空的(实机 11-23 查出)
  else if (hp.has('new')) editUser(null, { allLines: true });
}
export async function tick() { await load('users'); renderRows(); if (drawerUser) refreshDrawerLive(); }

function statusBadge(u) {
  if (!u.enabled) return badge(t('common.disabled') + (u.disabledReason ? ' · ' + t('user.reason.' + u.disabledReason) : ''), 'danger');
  if (isExpired(u)) return badge(t('user.expired'), 'warn');
  if (isOver(u)) return badge(t('user.over'), 'warn');
  const lim = limitsOf(u.id);
  if (lim.length) return badge(t('user.limited') + ' · ' + lim.map(s => s.ruleName).join(' / '), 'warn');
  return badge(t('common.enabled'), 'ok');
}
// 批量操作已经生效之后的刷新:在后台跑,失败只提示「已生效、列表没刷新」
function refreshAfterBatch() {
  renderRows();
  load('users', 'lines', 'status').then(() => renderRows(), e => toast(t('user.batchRefreshFail', { err: e.message }), 'warn'));
}
// 用户所属套餐的名字(套餐删了或没归入就是空)
const planNameOf = u => { const p = u.planId && state.plans.find(x => x.id === u.planId); return p ? p.name : ''; };
// 套餐页点人数跳过来:按套餐筛(和套餐页的人数一致;以前按名字搜,「月付」会搜出「月付Plus」的人),其它筛选和已选都复位
export function showUsersOfPlan(id) { query = ''; filter = 'all'; only = ''; planFilter = id; page = 1; selected.clear(); }

function expiryCell(u) {
  if (u.activateDays) return `<span class="muted">${t('user.activateIn', { n: u.activateDays })}</span>`;
  if (!u.expiry) return `<span class="muted">${t('user.never')}</span>`;
  const d = daysLeft(u.expiry);
  const b = d < 0 ? badge(t('user.expired'), 'danger') : d <= 7 ? badge(t('user.daysLeft', { n: d }), 'warn') : badge(t('user.daysLeft', { n: d }));
  return `<span class="nowrap">${fmtDay(u.expiry)} ${b}</span>`;
}
function visibleRows() {
  relayNow = relayIPs(); // 排序、画表都从这里过:按当时的线路和服务器重算一次
  let rows = state.users.filter(u => matches(query, u.name, u.remark, u.desc, planNameOf(u)));
  const on = filter === 'online' ? onlineSet() : null;
  rows = rows.filter(u => ({ all: true, online: on && on.has(u.name), enabled: u.enabled && !isExpired(u) && !isOver(u), disabled: !u.enabled, soon: isSoon(u), expired: isExpired(u), over: isOver(u) })[filter]);
  if (planFilter) rows = rows.filter(u => u.planId === planFilter);
  if (only === 'expiring') rows = rows.filter(isExpiring);
  if (only === 'quota') rows = rows.filter(isQuota);
  const key = SORTS[sortKey] || SORTS.id;
  return rows.slice().sort((a, b) => { const x = key(a), y = key(b); return (x < y ? -1 : x > y ? 1 : 0) * sortDir; });
}

// pageRows 当前页实际显示的那些用户(页码越界时先收敛,删掉本页最后一个也不会停在空页上)
function pageRows() {
  const rows = visibleRows();
  const pages = Math.max(1, Math.ceil(rows.length / pageSize));
  if (page > pages) page = pages;
  if (page < 1) page = 1;
  return rows.slice((page - 1) * pageSize, page * pageSize);
}

function renderPager(total) {
  const el = document.getElementById('user-pager');
  if (!el) return;
  const pages = Math.max(1, Math.ceil(total / pageSize));
  el.hidden = total <= PAGE_SIZES[0];
  if (el.hidden) { lastPager = ''; return; }
  const html = `<button class="btn sm" data-act="user.page" data-id="prev" ${page <= 1 ? 'disabled' : ''}>‹ ${t('common.prev')}</button>
    <span class="muted small">${page} / ${pages}</span>
    <button class="btn sm" data-act="user.page" data-id="next" ${page >= pages ? 'disabled' : ''}>${t('common.next')} ›</button>
    <span class="grow"></span>
    <label class="muted small">${t('common.perPage')} <select id="user-ps">${PAGE_SIZES.map(n => `<option value="${n}" ${n === pageSize ? 'selected' : ''}>${n}</option>`).join('')}</select></label>`;
  if (html === lastPager) return; // 定时刷新时内容没变就不动 DOM,免得把打开的下拉框关掉
  lastPager = html;
  el.innerHTML = html;
  document.getElementById('user-ps').addEventListener('change', e => {
    pageSize = Number(e.target.value); page = 1;
    try { localStorage.setItem('m-ui-users-ps', String(pageSize)); } catch {}
    renderRows();
  });
}

function renderRows() {
  const body = document.getElementById('users-body');
  if (!body) return;
  const online = new Set(state.onlines.users || []);
  const vis = visibleRows(), total = vis.length;
  const rows = pageRows();
  document.getElementById('user-count').textContent = `${total} / ${state.users.length}`;
  setHTML('user-only', only ? `<span class="chip">${esc(t('user.filter.only', { what: only === 'expiring' ? t('user.filter.expiring', { d: expiringDays() }) : t('user.filter.quota', { p: quotaPct() }) }))} <button type="button" class="btn xs ghost" data-act="user.clearOnly" title="${esc(t('user.filter.clear'))}" aria-label="${esc(t('user.filter.clear'))}"><i class="i i-x"></i></button></span>` : '');
  document.getElementById('batch-count').textContent = t('user.selected', { n: selected.size });
  // 当前页全选了、筛选结果又不止一页:给一个"选中全部 N 个筛选结果"(筛选结果里还有没勾上的才给)。
  // 手机上表格没有表头、也就没有全选框:勾了任意一个就给,不用先把一页卡片逐个勾满
  const allBtn = document.getElementById('sel-all-filtered');
  if (allBtn) {
    const pageAll = rows.length > 0 && rows.every(u => selected.has(u.id));
    // 排成卡片时没有表头(见 app.js 的 fitTables);第一次画时还没量过,先按手机宽度猜
    const tb = document.getElementById('users-table');
    const mobile = (tb && String(tb.className || '').includes('cards')) || (typeof matchMedia === 'function' && matchMedia('(max-width: 700px)').matches);
    allBtn.hidden = !((pageAll || mobile) && total > rows.length && vis.some(u => !selected.has(u.id)));
    allBtn.textContent = t('user.selAllFiltered', { n: total });
  }
  // 批量栏的内容先填好再露出 / 收起(量下面的列表挪了多少时,批量栏已经是最终的高度);刚勾的那一行不跳(见 ui.js toggleBatchBar)
  toggleBatchBar(document.getElementById('batch-bar'), selected.size > 0, selFrom);
  selFrom = null;
  const selAll = document.getElementById('sel-all');
  if (selAll) selAll.checked = rows.length > 0 && rows.every(u => selected.has(u.id));
  renderPager(total);
  if (!rows.length) { setHTML(body, `<tr><td colspan="8">${state.users.length ? empty() : firstUserGuide()}</td></tr>`); return; }
  setHTML(body, rows.map(u => {
    const used = (u.up || 0) + (u.down || 0);
    const mine = deviceIPs(u.onlineIps, relayNow), ips = deviceCount(mine);
    // 另有经中转入口进来的连接:不占设备名额,数字里不算,标一个 ⇄(悬停有说明)
    const via = (u.onlineIps || []).length > mine.length ? ` <span class="muted" title="${esc(t('user.viaRelay'))}">⇄</span>` : '';
    return `<tr class="${selected.has(u.id) ? 'selected' : ''}">
      <td><input type="checkbox" class="sel" data-change="user.sel" data-id="${u.id}" ${selected.has(u.id) ? 'checked' : ''}></td>
      <td class="primary-cell">${dot(online.has(u.name))}<a href="#" data-act="user.detail" data-id="${u.id}">${esc(u.name)}</a>${planNameOf(u) ? ` ${badge(planNameOf(u))}` : ''}${u.remark ? `<div class="sub-cell">${esc(u.remark)}</div>` : ''}</td>
      <td>${statusBadge(u)}</td>
      <td><span class="num">${fmtBytes(used, 1)}</span> <span class="muted small">/ ${u.volume ? fmtBytes(u.volume, 0) : t('common.unlimited')}</span>${progress(used, u.volume)}</td>
      <td>${expiryCell(u)}</td>
      <td class="num c-p2">${ips}${via} / ${u.deviceLimit || '∞'}</td>
      <td class="num c-p3">${(u.speedUp || u.speedDown) ? `${u.speedUp || '∞'}/${u.speedDown || '∞'} M` : '∞'}</td>
      <td class="actions">
        <button class="btn sm" data-act="user.detail" data-id="${u.id}">${t('common.details')}</button>
        <button class="btn sm" data-act="user.edit" data-id="${u.id}">${t('common.edit')}</button>
        <details class="menu"><summary class="btn sm">${t('common.more')}</summary><div class="menu-list">
          <button data-act="user.copy" data-id="${u.id}" data-fmt="clash">${t('user.subClash')}</button>
          <button data-act="user.copy" data-id="${u.id}" data-fmt="link">${t('user.subLink')}</button>
          <button data-act="user.copy" data-id="${u.id}" data-fmt="json">${t('user.subJson')}</button>
          <button data-act="user.renew" data-id="${u.id}">${t('user.renewMenu')}</button>
          ${state.plans.length ? `<button data-act="user.change" data-id="${u.id}">${t('user.changeMenu')}</button>` : ''}
          ${u.expiry || u.activateDays ? `<button data-act="user.extend" data-id="${u.id}">${t('user.extend')}</button>` : ''}
          <button data-act="user.reset" data-id="${u.id}">${t('user.reset')}</button>
          <button data-act="user.kick" data-id="${u.id}">${t('user.kick')}</button>
          <button data-act="user.rotate" data-id="${u.id}">${t('user.rotate')}</button>
          <hr><button data-act="user.toggle" data-id="${u.id}">${t(u.enabled ? 'user.batch.disable' : 'user.batch.enable')}</button>
          <button class="danger" data-act="user.del" data-id="${u.id}">${t('common.delete')}</button>
        </div></details>
      </td></tr>`;
  }).join(''));
}

// 一个用户都还没有:告诉新手下一步是什么。没有线路先去建线路(代理没有线路是主面板还没分配)。
function firstUserGuide() {
  if (!state.lines.length) {
    if (isReseller()) return empty();
    return `<div class="empty-guide"><p>${t('user.emptyNoLines')}</p><a class="btn primary" href="#/lines">${t('user.emptyNoLinesBtn')}</a></div>`;
  }
  return `<div class="empty-guide"><p>${t('user.emptyFirst')}</p><button class="btn primary" data-act="user.add">${t('user.emptyFirstBtn')}</button></div>`;
}

// ---- 详情抽屉 ----
async function showDetail(id) {
  const u = state.users.find(x => x.id === id);
  if (!u) return;
  drawerUser = id;
  const links = await get(`users/${id}/sub`);
  const used = (u.up || 0) + (u.down || 0);
  // 版式:最上面一排操作(常用的三个露着,其余收进「更多」)→ 四格概要(用量 / 到期 / 设备 / 限速)→ 订阅地址和二维码并排 →
  // 在线设备、流量图 → 其它资料、线路、套餐记录。以前是订阅、用量、一排七个按钮一路往下堆,最常看的几样要滚半屏
  const left = u.expiry ? daysLeft(u.expiry) : null;
  const ips = u.onlineIps || [];
  const subRow = (label, url, extra = '') => `<div class="ud-url"><span class="lb">${esc(label)}</span><code title="${esc(url)}">${esc(url)}</code><button class="btn sm" data-act="user.copyText" data-id="${esc(url)}">${t('common.copy')}</button>${extra}</div>`;
  const tile = (lb, v, sub = '', cls = '') => `<div class="ld-tile${cls ? ' ' + cls : ''}"><span class="lb">${esc(lb)}</span><b>${v}</b>${sub ? `<span class="sub">${sub}</span>` : ''}</div>`;
  openDrawer(`${esc(u.name)} ${statusBadge(u)}`, `
    <div class="ud-acts">
      <button class="btn sm primary" data-act="user.renew" data-id="${id}">${t('user.renewMenu')}</button>
      ${state.plans.length ? `<button class="btn sm" data-act="user.change" data-id="${id}">${t('user.changeMenu')}</button>` : ''}
      <button class="btn sm" data-act="user.edit" data-id="${id}">${t('common.edit')}</button>
      <span class="grow"></span>
      <details class="menu"><summary class="btn sm">${t('common.more')}</summary><div class="menu-list">
        ${u.expiry ? `<button data-act="user.extend" data-id="${id}">${t('user.extend')}</button>` : ''}
        <button data-act="user.reset" data-id="${id}">${t('user.reset')}</button>
        <button data-act="user.kick" data-id="${id}">${t('user.kick')}</button>
        <button data-act="user.rotate" data-id="${id}">${t('user.rotate')}</button>
        <hr><button data-act="user.toggle" data-id="${id}">${t(u.enabled ? 'user.batch.disable' : 'user.batch.enable')}</button>
      </div></details>
    </div>
    <div class="ld-sum">
      ${tile(t('user.usage'), fmtBytes(used, 1), `/ ${u.volume ? fmtBytes(u.volume, 0) : t('common.unlimited')}${progress(used, u.volume)}`)}
      ${tile(t('user.expiry'), u.activateDays ? esc(t('user.activateIn', { n: u.activateDays })) : u.expiry ? fmtDay(u.expiry) : esc(t('user.never')),
        left == null ? '' : left <= 0 ? esc(t('user.expired')) : esc(t('user.daysLeft', { n: left })), left != null && left <= 3 ? 'bad' : left != null && left <= 7 ? 'warn' : '')}
      ${tile(t('user.d.devices'), `${deviceCount(deviceIPs(ips))}<span class="of"> / ${u.deviceLimit || '∞'}</span>`, u.onlineAt ? esc(t('user.lastOnline') + ' ' + fmtRelative(u.onlineAt)) : '')}
      ${tile(t('user.speed'), esc(speedText(u.speedUp, u.speedDown)), (u.speedUp || u.speedDown) ? esc(t('user.d.speedSub')) : '')}
    </div>
    <section>
      <h3>${t('user.subscription')}</h3>
      <div class="ud-sub">
        <div class="ud-urls">
          ${subRow(t('user.d.link'), links.link)}
          ${subRow('Clash', links.clash)}
          ${links.share ? subRow(t('user.share'), links.share, `<button class="btn sm danger" data-act="user.unshare" data-id="${id}">${t('user.unshare')}</button>`) : ''}
        </div>
        <img class="qr" src="${qrUrl(id, 'clash')}" alt="QR" title="${t('user.subClash')}">
      </div>
    </section>
    <section><h3>${t('user.onlineIps')}</h3><div id="ud-ips"></div>${ruleLimitsHTML(u)}</section>
    <section>
      <div class="ld-head"><h3>${t('user.trafficChart')}</h3>
        <div class="seg sm">${[24, 168, 720].map(h => `<button data-act="user.chartRange" data-id="${h}" class="${h === drawerRange ? 'active' : ''}">${t('dash.range.' + (h === 24 ? '24h' : h === 168 ? '7d' : '30d'))}</button>`).join('')}</div></div>
      <div id="ud-chart"></div>
      ${state.status && state.status.role === 'node' ? `<p class="hint xp">${t('user.chartNode')}</p>` : ((state.nodes || []).some(n => n.ratio > 0 && n.ratio !== 1) ? `<p class="hint xp">${t('user.chartRatio')}</p>` : '')}
    </section>
    <section>
      <h3>${t('user.d.info')}</h3>
      <dl class="kv ud-kv">
        ${planNameOf(u) ? `<dt>${t('user.plan')}</dt><dd>${esc(planNameOf(u))}</dd>` : ''}
        ${u.remark ? `<dt>${t('user.f.remark')}</dt><dd>${esc(u.remark)}</dd>` : ''}
        <dt>${t('dash.up')} / ${t('dash.down')}</dt><dd class="num">${fmtBytes(u.up)} / ${fmtBytes(u.down)}</dd>
        <dt>${t('user.total')}</dt><dd class="num">${fmtBytes((u.totalUp || 0) + (u.totalDown || 0) + used)}</dd>
        <dt>${t('user.f.autoReset')}</dt><dd>${u.autoReset ? `${u.resetDays} ${t('common.day')} · ${fmtDay(u.nextReset)}` : t('common.no')}</dd>
      </dl>
    </section>
    <section><h3>${t('user.lines')}</h3>${refChips(refsOf(u), state.lines, pickerNodes(), { key: 'ud-lines-' + u.id, max: 20 }) || `<span class="muted small">${t('common.none')}</span>`}
      ${(u.extIds || []).length ? `<h3>${t('user.f.exts')}</h3><div class="chips">${u.extIds.map(eid => { const x = (state.exts || []).find(e => e.id === eid); const noGroup = x && x.type === 'sub' && (u.extNoGroupIds || []).includes(eid); return x ? `<span class="chip">${esc(x.name)}&nbsp;<span class="muted">${x.nodeCount || 0}</span>${noGroup ? `&nbsp;<span class="muted">· ${esc(t('user.f.extNoGroup'))}</span>` : ''}</span>` : ''; }).join('')}</div>` : ''}</section>
    <section><h3>${t('pc.history')}</h3><div id="ud-planops"><span class="muted small">${t('common.loading')}</span></div></section>`, { wide: true });
  refreshDrawerLive();
  loadPlanOps(id);
  await renderUserChart(u.name, drawerRange);
}

let drawerRange = 168;
async function renderUserChart(name, hours) {
  const ch = document.getElementById('ud-chart');
  if (!ch) return;
  const d = await get(`stats?resource=user&tag=${encodeURIComponent(name)}&hours=${hours}&bucket=${bucketFor(hours)}&tz=${tzOffsetMinutes()}`);
  barChart(ch, d.points, { span: d.span, height: 170, upLabel: t('dash.up'), downLabel: t('dash.down'), totalLabel: t('dash.total'), peakLabel: t('dash.peak'), emptyText: t('dash.noTraffic') });
}

function refreshDrawerLive() {
  const u = state.users.find(x => x.id === drawerUser);
  const el = document.getElementById('ud-ips');
  if (!u || !el) return;
  const ips = u.onlineIps || [];
  const conns = state.onlines.connCounts[u.name] || 0;
  // 每个 IP 右侧标出它正在使用的线路(线路名已带服务器后缀,如 "香港1-高带宽")
  const lines = u.onlineLines || {};
  el.innerHTML = ips.length
    ? `<div class="ip-list">${ips.map(ip => `<div class="ip"><span class="mono">${esc(ip)}${serverOfIP(ip) ? ` <span class="badge fw-badge" title="${esc(t('line.fw.viaIP', { name: serverOfIP(ip) }))}">⇄ ${esc(serverOfIP(ip))}</span>` : ''}</span><span class="ip-lines">${
        (lines[ip] || []).map(l => badge(l, 'primary')).join(' ') || `<span class="muted small">${t('user.lineUnknown')}</span>`
      }</span></div>`).join('')}</div><p class="hint">${conns} ${t('dash.conns')}</p>`
    : `<p class="muted small">${t('user.noOnline')}</p>`;
}

// 订阅地址是不是用用户名,取决于 设置 → 订阅 的开关(代理建的用户一律随机地址)
const nameIsSubPath = () => !isReseller() && String((state.settings || {}).subUseUserName).toLowerCase() !== 'false';
const nameLabel = () => t(nameIsSubPath() ? 'user.f.name' : 'user.f.nameOnly');
const nameHelp = () => t(nameIsSubPath() ? 'user.f.nameHelp' : 'user.f.nameOnlyHelp');

// ---- 编辑 / 新建(可按套餐填写)----
function fillFromPlan(planId) {
  const p = state.plans.find(x => x.id === Number(planId));
  if (!p) return;
  document.getElementById('f-volume').value = p.volumeGb || 0;
  const later = p.days && p.firstUse; // 首次连接才开始计时:到期日留空,填"首次连接后 N 天"
  document.getElementById('f-expiry').value = p.days && !later ? new Date(Date.now() + p.days * 86400000).toISOString().slice(0, 10) : '';
  document.getElementById('f-activate').value = later ? p.days : 0;
  document.getElementById('f-device').value = p.deviceLimit || 0;
  document.getElementById('f-up').value = p.speedUp || 0;
  document.getElementById('f-down').value = p.speedDown || 0;
  document.getElementById('f-autoreset').checked = !!p.autoReset;
  document.getElementById('f-resetdays').value = p.resetDays || 30;
  const ids = Array.isArray(p.lineIds) ? p.lineIds : (p.lineIds ? JSON.parse(p.lineIds) : []);
  if (ids.length && picker) {
    let nodesBy = {};
    try { nodesBy = typeof p.lineNodes === 'string' ? JSON.parse(p.lineNodes || '{}') : (p.lineNodes || {}); } catch { nodesBy = {}; }
    picker.set(ids.map(id => ({ lineId: id, nodeIds: nodesBy[String(id)] || [] })));
  }
}

// allLines:新建时线路默认全勾(只有概览指引的第三步这么开;平常的「新增用户」照旧一条不勾)
function editUser(id, { allLines = false } = {}) {
  const u = id ? state.users.find(x => x.id === id) : { enabled: true, lineIds: [], resetDays: 30 };
  const expiryVal = u.expiry ? new Date(u.expiry * 1000).toISOString().slice(0, 10) : '';
  // 一页两栏:左边是「基本」和「额度与限速」(各有小标题,两列排),右边整列是线路。一屏看全,不用切换。
  // 以前所有栏一屏四列铺开、线路又挤在最下面;试过分三个页签,这点内容分页反而要多点几次、看不到全貌
  const basic = `<div class="form-grid cols-2">
      ${field(nameLabel(), `<input id="f-name" value="${esc(u.name || '')}">`, nameHelp())}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(u.remark || '')}">`)}
      ${state.plans.length ? `<div class="full">${field(t('user.plan'), `<div class="row">${planSelect('f-plan', u.planId || 0)}<button type="button" class="btn" data-act="user.fillPlan">${t('user.fromPlan')}</button></div>`, t('user.planHelp'))}</div>` : ''}
      <div class="full">${switchRow('f-enabled', t('user.f.enabled'), u.enabled !== false, t('user.f.enabledHelp'))}</div>
    </div>`;
  const quota = `<div class="form-grid cols-2">
      ${field(t('user.f.volume'), `<input id="f-volume" type="number" min="0" step="any" value="${gbValue(u.volume)}">`)}
      ${field(t('user.f.expiry'), `<input id="f-expiry" type="date" value="${expiryVal}">`)}
      ${field(t('user.f.activate'), `<input id="f-activate" type="number" min="0" value="${u.activateDays || 0}">`, t('user.f.activateHelp'))}
      ${field(t('user.f.device'), `<input id="f-device" type="number" min="0" value="${u.deviceLimit || 0}">`, t('user.f.deviceHelp'))}
      ${field(t('user.f.up'), `<input id="f-up" type="number" min="0" value="${u.speedUp || 0}">`)}
      ${field(t('user.f.down'), `<input id="f-down" type="number" min="0" value="${u.speedDown || 0}">`, t('user.f.speedHelp'))}
      ${field(t('user.f.resetDays'), `<input id="f-resetdays" type="number" min="1" value="${u.resetDays || 30}">`)}
      <label class="field check" style="align-self:end;min-height:var(--ctl-h)"><input type="checkbox" id="f-autoreset" ${u.autoReset ? 'checked' : ''}><span>${esc(t('user.f.autoReset'))}</span></label>
    </div>`;
  // 外部节点:每行左边勾上 = 分给这个用户;外部订阅右边多一个「单独成组」小开关(Clash 订阅里自己成一个策略组;新分配的初始值按外部订阅自己的默认值)。
  // 两个控件各有各的 label,点哪个的字就是哪个(#91 原先把开关塞在分配的 label 里,点「生成策略组」几个字反而把整个订阅取消了);
  // 没分给他时开关变灰、点不动(开关记在分配上,不分配就无从谈起),再勾上时恢复原来的状态;分享链接只有一个节点,不成组,不给开关。
  const extRow = x => {
    // 没分给他的,开关显示勾上分配以后的样子:外部订阅自己的默认值(外部节点编辑框里的「分给用户时单独成组」)
    const on = (u.extIds || []).includes(x.id), grouped = on ? !(u.extNoGroupIds || []).includes(x.id) : !x.noGroup;
    const meta = `${x.type === 'sub' ? t('ext.typeSub') : t('ext.typeLink')} · ${x.nodeCount || 0}`;
    const grp = x.type === 'sub' ? `<label class="ext-grp" title="${esc(t('user.f.extGroupTip'))}"><span>${esc(t('user.f.extGenGroup'))}</span><span class="switch sm"><input type="checkbox" class="ext-group-cb" value="${x.id}" ${grouped ? 'checked' : ''} ${on ? '' : 'disabled'}><span></span></span></label>` : '';
    return `<div class="ext-pick"><label class="ext-main"><input type="checkbox" class="ext-cb" value="${x.id}" ${on ? 'checked' : ''}><span class="ext-nm" title="${esc(x.name)}">${esc(x.name)}</span><span class="ext-meta">${esc(meta)}</span></label>${grp}</div>`;
  };
  const linesTab = `<div id="f-lines"></div><p class="hint xp">${esc(t('lp.help'))}</p>
      ${(state.exts || []).length ? `<h3 class="form-sec">${esc(t('user.f.exts'))}</h3>
        <div class="check-list one-col ext-list">${(state.exts || []).map(extRow).join('')}</div><p class="hint xp">${esc(t('user.f.extsHelp'))}</p>` : ''}`;
  openModal(id ? t('user.edit') : t('user.add'), `<div class="edit-2pane">
      <div class="pane"><h3 class="form-sec">${esc(t('user.tab.basic'))}</h3>${basic}<h3 class="form-sec">${esc(t('user.tab.quota'))}</h3>${quota}</div>
      <div class="pane"><h3 class="form-sec">${esc(t('user.tab.lines'))}</h3>${linesTab}</div>
    </div>`, async () => {
    const expiry = fv('f-expiry');
    const activateDays = Math.max(0, Number(fv('f-activate')) || 0);
    const body = {
      name: fv('f-name').trim(), remark: fv('f-remark'), enabled: fchk('f-enabled'),
      planId: Number(fv('f-plan')) || 0, activateDays,
      volume: gbBytes(fv('f-volume')),
      expiry: !activateDays && expiry ? Math.floor(new Date(expiry + 'T23:59:59').getTime() / 1000) : 0,
      deviceLimit: Number(fv('f-device')), speedUp: Number(fv('f-up')), speedDown: Number(fv('f-down')),
      autoReset: fchk('f-autoreset'), resetDays: Number(fv('f-resetdays')),
      nextReset: u.nextReset || 0, desc: u.desc || '',
      lineRefs: picker.read(), lineIds: picker.read().map(r => r.lineId),
      extIds: [...document.querySelectorAll('.ext-cb:checked')].map(c => Number(c.value)),
      extNoGroupIds: [...document.querySelectorAll('.ext-cb:checked')].map(c => Number(c.value)).filter(id => { const g = document.querySelector(`.ext-group-cb[value="${id}"]`); return g && !g.checked; }),
    };
    const created = id ? null : await post('users', body);
    if (id) await put('users/' + id, body);
    await load('users', 'lines', 'status');
    renderRows();
    if (drawerUser === id) showDetail(id);
    toast(id ? t('user.updated') : t('user.created'), 'ok');
    // 系统里的第一个用户:直接打开详情抽屉,订阅地址 / 二维码 / 复制就在眼前,不用再找
    if (created && created.id && state.users.length === 1) showDetail(created.id);
  }, { size: 'xl' });
  document.querySelectorAll('#modal-body .ext-cb').forEach(cb => cb.addEventListener('change', () => {
    const g = document.querySelector(`#modal-body .ext-group-cb[value="${cb.value}"]`);
    if (g) g.disabled = !cb.checked;
  }));
  const items = pickerItems();
  picker = linePicker(document.getElementById('f-lines'), { items, selected: allLines && !id ? new Set(items.map(it => it.key)) : keysFromRefs(refsOf(u), items), keep: refsOf(u) });
  // 新建时选了套餐就直接填好额度;编辑老用户时只是改归属,要填额度再点按钮
  if (!id) { const sel = document.getElementById('f-plan'); if (sel) sel.addEventListener('change', () => fillFromPlan(sel.value)); }
}

// ---- 续费 / 延期(套餐):可以一次续 N 期;单个用户时实时预览到期与配额的变化 ----
// 每次打开对话框生成一个幂等键:双击、网络重试都只执行一次(重放时提示"已执行过")。
// crypto.randomUUID 只在 HTTPS 下有,面板常走 http://IP:端口,所以用 getRandomValues 拼
const newRequestId = () => 'ui-' + Array.from(crypto.getRandomValues(new Uint8Array(12)), b => b.toString(16).padStart(2, '0')).join('');
const planBrief = p => `${p.volumeGb ? p.volumeGb + 'GB' : '∞'} / ${p.days ? p.days + t('common.day') : '∞'}`;
const speedText = (up, down) => (up || down) ? `${up || '∞'}/${down || '∞'} M` : '∞';
const expiryText = s => s.activateDays ? t('user.activateIn', { n: s.activateDays }) : s.expiry ? fmtDay(s.expiry) : t('user.never');
const volText = s => s.volume ? fmtBytes(s.volume, 0) : t('common.unlimited');
const durText = sec => sec >= 86400 ? `${+(sec / 86400).toFixed(2)} ${t('common.day')}` : fmtDuration(sec);

// 套用套餐(续费 / 延期 / 换套餐)一律会启用手动停用的用户,外部 API 也一样(计费系统依赖这个行为,不改);
// 而「延长 30 天」、批量延期、重置流量都不会。执行前在对话框里说清楚,免得月底批量续费把封掉的人放回来。
const manualOff = u => !!u && !u.enabled && u.disabledReason === 'manual';
function manualNote(ids, key) {
  const users = ids.map(id => state.users.find(x => x.id === id)).filter(manualOff);
  if (!users.length) return '';
  if (ids.length === 1) return t(key);
  const names = users.slice(0, 5).map(u => u.name).join(t('reach.sep')) + (users.length > 5 ? '…' : '');
  return t('user.manualBatch', { n: users.length, names });
}

function renewDialog(ids) {
  if (!state.plans.length) { toast(t('plan.empty'), 'err'); return; }
  const single = ids.length === 1 ? state.users.find(x => x.id === ids[0]) : null;
  const def = single && state.plans.some(p => p.id === single.planId) ? single.planId : state.plans[0].id; // 默认续他现在的套餐
  const requestId = newRequestId();
  const periods = () => Math.max(1, Math.floor(Number(fv('f-periods'))) || 1);
  const manual = manualNote(ids, 'user.manualRenew');
  openModal(t('user.renew') + (single ? ' · ' + single.name : ` (${ids.length})`), `
    <div class="form-grid">
      <div class="full">${field(t('plan.pick'), planSelect('f-plan', def, false))}</div>
      <div class="full">${field('', `<select id="f-mode"><option value="renew">${t('plan.renew')}</option><option value="extend">${t('plan.extend')}</option><option value="bind">${t('plan.bind')}</option></select>`)}</div>
      ${field(t('pc.periods'), `<input id="f-periods" type="number" min="1" max="120" value="1">`, t('pc.periodsHelp'))}
    </div>
    ${manual ? `<p class="hint warn" id="rn-manual">${esc(manual)}</p>` : ''}
    ${single ? `<p class="hint" id="rn-preview"></p>` : ''}`, async () => {
    const planId = Number(fv('f-plan')), mode = fv('f-mode');
    let replayed = false;
    if (ids.length === 1) {
      let r;
      try {
        r = await post(`users/${ids[0]}/plan`, mode === 'bind' ? { planId, mode } : { planId, mode, periods: periods(), requestId });
      } catch (e) {
        if (e.data && e.data.code === 'idempotency_conflict') throw await conflictError(ids[0]);
        throw e;
      }
      replayed = !!(r && r.replayed);
    } else {
      // 批量续费没有幂等键:提交成功就关弹窗,刷新失败只提示(以前刷新一失败弹窗还开着,再点「保存」会再续一遍)
      await post('users/batch', { ids, action: 'plan', planId, mode, periods: periods() });
      selected.clear();
      toast(t('plan.applied'), 'ok');
      refreshAfterBatch();
      return;
    }
    await load('users'); renderRows();
    if (drawerUser && ids.includes(drawerUser)) showDetail(drawerUser);
    toast(replayed ? t('pc.replayed') : t('plan.applied'), 'ok');
  });
  let seq = 0;
  const preview = debounce(async () => {
    if (!single) return;
    const my = ++seq, mode = fv('f-mode');
    if (mode === 'bind') { setHTML('rn-preview', ''); return; }
    try {
      const r = await post(`users/${single.id}/plan`, { planId: Number(fv('f-plan')), mode, periods: periods(), dryRun: true });
      if (my !== seq) return;
      const warn = (r.warnings || []).filter(w => w.block).map(w => `<br><span class="pc-bad">${esc(warnText(w))}</span>`).join('');
      setHTML('rn-preview', esc(t('pc.renewPreview', { from: expiryText(r.from), to: expiryText(r.to), vfrom: volText(r.from), vto: volText(r.to) })) + warn);
    } catch (e) { if (my === seq) setHTML('rn-preview', `<span class="pc-bad">${esc(e.message)}</span>`); }
  }, 300);
  const sync = () => {
    const bind = fv('f-mode') === 'bind', p = document.getElementById('f-periods'), m = document.getElementById('rn-manual');
    if (p) p.disabled = bind;
    if (m) m.hidden = bind; // 只归入套餐不改启停
    preview();
  };
  ['f-plan', 'f-mode'].forEach(k => document.getElementById(k).addEventListener('change', sync));
  document.getElementById('f-periods').addEventListener('input', preview);
  sync();
}

// conflictError 409 idempotency_conflict:这个对话框之前的一次提交(同一个幂等键)其实已经执行成功了 ——
// 多半是当时没收到结果,用户改了选项又提交。这次的改动不会再执行;刷新列表,请用户看过套餐记录再决定。
async function conflictError(id) {
  try {
    await load('users'); renderRows();
    if (drawerUser === id) showDetail(id);
  } catch { /* 刷新失败不影响提示 */ }
  return new Error(t('pc.err.idemConflict'));
}

// ---- 换套餐:立即替换 / 叠加 / 按价值折算。改动后防抖 300 ms 预览(dryRun),有阻断影响时要勾"仍然执行" ----
// 折算的前提:用户有所属套餐、两个套餐都填了参考价、不是同一个套餐;不满足时置灰并说明
function prorateBlocker(u, target) {
  const cur = state.plans.find(p => p.id === u.planId);
  if (!cur) return t('pc.noProrate.noPlan');
  if (!target) return '';
  if (target.id === cur.id) return t('pc.noProrate.same');
  const unpriced = [cur, target].find(p => !(p.price > 0));
  return unpriced ? t('pc.noProrate.noPrice', { name: unpriced.name }) : '';
}

// warnText 告警按 code + args 翻译;新版后端多出来的 code 先用它自带的中文
function warnText(w) {
  const a = w.args || {};
  const speeds = v => Array.isArray(v) ? speedText(v[0], v[1]) : '∞'; // 限速是 [上行, 下行]
  // 按 code 各取各的参数(只算命中的那一个:args 的形状因 code 而异)
  const fmt = {
    over_quota: () => ({ used: fmtBytes(a.used), volume: fmtBytes(a.volume), date: a.nextReset ? fmtDay(a.nextReset) : '' }),
    expiry_earlier: () => ({ from: a.from ? fmtDay(a.from) : t('user.never'), to: fmtDay(a.to) }),
    volume_lower: () => ({ from: a.from ? fmtBytes(a.from) : t('common.unlimited'), to: fmtBytes(a.to) }),
    fewer_devices: () => ({ from: a.from || '∞', to: a.to }),
    slower: () => ({ from: speeds(a.from), to: speeds(a.to) }),
    lines_removed: () => ({ names: (a.lineIds || []).map(id => (state.lines.find(l => l.id === id) || {}).name || '#' + id).join(', ') }),
    per_period_not_stacked: () => ({ lost: fmtBytes(a.lost) }),
    carry_unlimited: () => ({ volume: fmtBytes(a.volume || 0) }),
    carry_expired: () => ({ lost: fmtBytes(a.lost) }),
    expiry_kept: () => ({ end: a.activateDays ? t('user.activateIn', { n: a.activateDays }) : fmtDay(a.expiry) }),
    volume_kept: () => ({ left: fmtBytes(a.left || 0) }),
    credit_rounded: () => ({ time: durText(a.seconds || 0) }),
    reseller_pool_low: () => ({ remaining: a.remaining == null ? t('common.unlimited') : fmtBytes(a.remaining), pool: fmtBytes(a.pool) }),
    reseller_expires_first: () => ({ date: fmtDay(a.resellerExpiry) }),
  }[w.code];
  // 立即超量:新套餐周期重置的到下次重置恢复,不重置的(一次性额度)要续费或清零用量,文案不同
  const key = 'pc.w.' + w.code + (w.code === 'over_quota' && !a.nextReset ? 'Once' : ''), s = t(key, fmt ? fmt() : undefined);
  return s === key ? (w.text || w.code) : s;
}

// 变更前 / 后两栏:套餐、流量、到期、周期重置、设备、限速、线路;变了的值加粗
const stateRows = s => [
  [t('user.plan'), esc(s.plan || t('plan.none'))],
  [t('user.usage'), `${fmtBytes(s.used)} / ${s.volume ? fmtBytes(s.volume) : t('common.unlimited')}`],
  [t('user.expiry'), esc(expiryText(s))],
  [t('user.f.autoReset'), s.autoReset ? `${s.resetDays} ${t('common.day')} · ${fmtDay(s.nextReset)}` : t('common.no')],
  [t('user.devices'), s.deviceLimit || '∞'],
  [t('user.speed'), speedText(s.speedUp, s.speedDown)],
  [t('user.lines'), esc(refLabels(s.lineRefs || [], state.lines, pickerNodes(), { servers: true }).join(', ') || t('common.none'))],
];
function stateCol(s, cmp) {
  const rows = stateRows(s), base = cmp ? stateRows(cmp) : null;
  return `<dl class="kv">${rows.map(([k, v], i) => `<dt>${k}</dt><dd class="${base && String(base[i][1]) !== String(v) ? 'chg' : ''}">${v}</dd>`).join('')}</dl>`;
}
const DIR_KIND = { upgrade: 'ok', downgrade: 'warn', mixed: 'primary', same: '' };
function previewHTML(r, hide = []) {
  const rem = r.remaining || {}, c = r.credit;
  const left = t('pc.remaining', { time: rem.seconds == null ? t('common.unlimited') : durText(rem.seconds), bytes: rem.bytes == null ? t('common.unlimited') : fmtBytes(rem.bytes) })
    + (rem.value != null ? ' · ' + t('pc.remainingValue', { v: +Number(rem.value).toFixed(2) }) : '');
  const credit = c ? `<p class="hint">${esc(t('pc.creditLine', { value: +Number(c.value).toFixed(2), from: c.fromPrice, to: c.toPrice, periods: +Number(c.periods).toFixed(3), time: durText(c.seconds || 0), bytes: fmtBytes(c.bytes || 0) }))}</p>` : '';
  const pool = r.pool && r.pool.volume ? `<p class="hint">${esc(t('pc.pool', { remaining: fmtBytes(r.pool.remaining), volume: fmtBytes(r.pool.volume) }))}</p>` : '';
  const ws = (r.warnings || []).filter(w => !hide.includes(w.code)); // 对话框上面已经单独说了的不再重复
  const warns = ws.length ? `<ul class="pc-warn">${ws.map(w => `<li class="${w.block ? 'bad' : 'warn'}">${esc(warnText(w))}</li>`).join('')}</ul>` : '';
  return `<div class="grid-2 pc-cols">
      <div><h3>${t('pc.before')}</h3>${stateCol(r.from)}</div>
      <div><h3>${t('pc.after')} ${badge(t('pc.dir.' + r.direction), DIR_KIND[r.direction] || '')}</h3>${stateCol(r.to, r.from)}</div>
    </div><p class="hint">${esc(left)}</p>${credit}${pool}${warns}`;
}

function changeDialog(id) {
  const u = state.users.find(x => x.id === id);
  if (!u) return;
  if (!state.plans.length) { toast(t('plan.empty'), 'err'); return; }
  const requestId = newRequestId();
  const pick = (state.plans.find(p => p.id !== u.planId) || state.plans[0]).id;
  let mode = 'replace', seq = 0;
  // shown:最近一次成功的预览用的参数。提交时原样带它(而不是表单此刻的值);参数一变就清掉、禁用提交、取消「仍然执行」,
  // 直到新的预览回来 —— force 只会跟着用户看过的那份预览一起提交。备注不影响预览,提交时现读。
  let shown = null;
  const params = () => ({ planId: Number(fv('pc-plan')), mode, periods: Math.max(0, Math.floor(Number(fv('pc-periods'))) || 0), usage: fv('pc-usage') });
  const el = k => document.getElementById(k);
  const setReady = ok => { const b = el('modal-save'); if (b) b.disabled = !ok; };
  const setForce = need => {
    const wrap = el('pc-force-wrap'), f = el('pc-force');
    if (wrap) wrap.hidden = !need;
    if (f) f.checked = false; // 每份新预览都要重新确认
  };
  const manual = manualOff(u);
  const show = (r, p) => { setHTML('pc-preview', previewHTML(r, manual ? ['enables_manual'] : [])); setForce(!!r.needsForce); shown = p; setReady(true); };
  openModal(t('pc.title') + ' · ' + u.name, `
    <div class="form-grid">
      <div class="full">${field(t('pc.target'), `<select id="pc-plan">${state.plans.map(p => `<option value="${p.id}" ${p.id === pick ? 'selected' : ''}>${esc(p.name)} · ${planBrief(p)}${p.price > 0 ? ' · ' + esc(t('plan.priceShort', { n: p.price })) : ''}${p.id === u.planId ? ' · ' + t('pc.current') : ''}</option>`).join('')}</select>`)}</div>
      <div class="full"><div class="field"><span class="label">${t('pc.mode')}</span><div class="seg" id="pc-mode">${['replace', 'stack', 'prorate'].map(m => `<button type="button" data-mode="${m}">${t('pc.mode.' + m)}</button>`).join('')}</div><span class="help xp" id="pc-mode-help"></span><span class="help warn-text" id="pc-mode-why" hidden></span></div></div>
      ${field(t('pc.periods'), `<input id="pc-periods" type="number" min="1" max="120" value="1">`, t('pc.periodsHelp'))}
      ${field(t('pc.usage'), `<select id="pc-usage">${['auto', 'keep', 'reset'].map(v => `<option value="${v}">${t('pc.usage.' + v)}</option>`).join('')}</select>`, t('pc.usageHelp'))}
      <div class="full">${field(t('pc.remark'), `<input id="pc-remark" maxlength="200" placeholder="${esc(t('pc.remarkPh'))}">`)}</div>
    </div>
    ${manual ? `<p class="hint warn">${esc(t('user.manualChange'))}</p>` : ''}
    <div class="pc-preview" id="pc-preview"><p class="muted small">${t('common.loading')}</p></div>
    <div id="pc-force-wrap" hidden>${check('pc-force', t('pc.force'), false)}</div>`, async () => {
    if (!shown) throw new Error(t('pc.previewPending')); // 等预览时按钮是禁用的;openModal 出错后会把它重新点亮,这里再拦一道
    const sent = shown, at = seq, wrap = el('pc-force-wrap'), needForce = !!(wrap && !wrap.hidden);
    // 预览已经说了要确认、用户还没勾:直接让他先勾,不发请求(发了服务端也是 409 needs_force,提示成「情况有变」会误导)
    if (needForce && !fchk('pc-force')) throw new Error(t('pc.err.tickForce'));
    let res;
    try {
      res = await post(`users/${id}/change`, { ...sent, remark: fv('pc-remark').trim(), force: needForce, requestId });
    } catch (e) {
      const code = e.data && e.data.code;
      if (code === 'needs_force' && e.data.preview) {
        // 预览之后情况变了(比如这期间又用了流量):换上服务端的最新预览,重新确认。提交途中又改了选项的,等新选项的预览
        if (at === seq) show(e.data.preview, sent);
        throw new Error(t('pc.err.needsForce'));
      }
      if (code === 'idempotency_conflict') throw await conflictError(id);
      throw e;
    }
    await load('users'); renderRows();
    if (drawerUser === id) showDetail(id);
    toast(res && res.replayed ? t('pc.replayed') : t('pc.done'), 'ok');
  }, { wide: true, saveText: t('pc.confirm') });

  const setMode = m => {
    const why = prorateBlocker(u, state.plans.find(p => p.id === Number(fv('pc-plan'))));
    if (m === 'prorate' && why) m = 'replace';
    mode = m;
    document.querySelectorAll('#pc-mode button').forEach(b => {
      b.classList.toggle('active', b.dataset.mode === mode);
      if (b.dataset.mode === 'prorate') { b.disabled = !!why; b.title = why; }
    });
    setText('pc-mode-help', t('pc.modeHelp.' + mode));
    // 不能折算的原因单独一行、一直显示(简洁模式下方式说明收进「?」,原因不能跟着藏)
    const whyEl = el('pc-mode-why');
    whyEl.hidden = !why;
    setText(whyEl, why ? t('pc.noProrate', { why }) : '');
    const per = el('pc-periods');
    per.min = mode === 'prorate' ? '0' : '1';
    if (mode !== 'prorate' && Number(per.value) < 1) per.value = '1';
  };
  const preview = debounce(async () => {
    const my = seq, p = params();
    try {
      const r = await post(`users/${id}/change`, { ...p, dryRun: true });
      if (my === seq) show(r, p);
    } catch (e) {
      if (my !== seq) return;
      setHTML('pc-preview', `<p class="hint bad">${esc(e.message)}</p>`);
      setForce(false); // 没有可确认的预览:提交保持禁用,改一下选项会再预览
    }
  }, 300);
  // 参数一变:作废在途的预览(seq + 1)、清掉已确认的那份、禁用提交,防抖后重新预览
  const changed = () => { seq++; shown = null; setForce(false); setReady(false); preview(); };
  el('pc-mode').addEventListener('click', e => {
    const b = e.target.closest('button[data-mode]');
    if (b && !b.disabled) { setMode(b.dataset.mode); changed(); }
  });
  el('pc-plan').addEventListener('change', () => { setMode(mode); changed(); });
  el('pc-usage').addEventListener('change', changed);
  el('pc-periods').addEventListener('input', changed);
  setMode('replace');
  changed();
}

// ---- 详情抽屉:最近 10 条套餐记录 ----
const opLabel = op => (op.op === 'renew' || op.op === 'extend')
  ? t('pc.op.' + op.op) + (op.periods > 1 ? ' ×' + op.periods : '') + (op.toPlan ? ' · ' + op.toPlan : '')
  : t('pc.op.change', { mode: t('pc.mode.' + op.op), from: op.fromPlan || '—', to: op.toPlan || '—' }) + (op.periods !== 1 ? ' ×' + op.periods : '');
async function loadPlanOps(id) {
  let list = [];
  try { list = await get(`users/${id}/plan-ops?limit=10`); } catch { list = []; }
  if (drawerUser !== id) return;
  if (!Array.isArray(list)) list = [];
  setHTML('ud-planops', list.length ? `<ul class="plan-ops">${list.map(op => {
    const a = op.before || {}, b = op.after || {};
    const actor = op.actor === '主面板' ? t('pc.byMaster') : op.actor; // 代理看到的主面板操作
    return `<li><span class="muted">${fmtDate(op.createdAt)} · ${esc(actor || '')}</span><br>${esc(opLabel(op))} · ${t('user.expiry')} ${esc(expiryText(a))} → ${esc(expiryText(b))} · ${t('plan.quota')} ${volText(a)} → ${volText(b)}${op.remark ? ` · <span class="muted">${esc(op.remark)}</span>` : ''}</li>`;
  }).join('')}</ul>` : `<span class="muted small">${t('pc.noHistory')}</span>`);
}

// ---- 批量生成 ----
function bulkDialog() {
  openModal(t('user.bulk'), `
    <div class="form-grid">
      ${field(t('user.bulkPrefix'), `<input id="f-prefix" value="user-">`)}
      ${field(t('user.bulkCount'), `<input id="f-count" type="number" min="1" max="500" value="10">`)}
      ${field(t('user.bulkMode'), `<select id="f-mode"><option value="random">${t('user.bulkRandom')}</option><option value="seq">${t('user.bulkSeq')}</option></select>`)}
      ${field(t('user.bulkStart'), `<input id="f-start" type="number" min="1" value="1">`)}
      ${field(t('user.f.remark'), `<input id="f-remark" value="">`)}
      ${field(t('plan.pick'), planSelect('f-plan', 0), t('user.bulkHelp'))}
      <div class="full">${fieldBox(t('user.f.lines'), `<div id="f-lines"></div>`, t('lp.help'))}</div>
    </div>`, async () => {
    const res = await post('users/bulk', {
      prefix: fv('f-prefix').trim(), count: Number(fv('f-count')), nameMode: fv('f-mode'), startIndex: Number(fv('f-start')),
      remark: fv('f-remark'), planId: Number(fv('f-plan')),
      lineIds: picker.read().map(r => r.lineId), lineRefs: picker.read(),
    });
    await load('users', 'lines', 'status'); renderRows();
    toast(t('user.bulkDone', { n: res.length }), 'ok');
    // 直接换成结果框:openModal 认得出保存回调里换了弹窗,不会再把它关掉;用户自己点「关闭」才关
    openModal(t('user.bulkResult'), `<textarea id="bulk-result" style="min-height:16rem" readonly>${esc(res.map(r => r.name + '\t' + r.link).join('\n'))}</textarea>
      <div class="toolbar"><button type="button" class="btn" data-act="user.bulkCopy">${t('common.copy')}</button></div>`, null, { cancelText: t('common.close') });
  }, { size: 'xl' });
  const items = pickerItems();
  picker = linePicker(document.getElementById('f-lines'), { items, selected: new Set(items.map(it => it.key)) });
}

async function fullUpdate(u, patch) {
  const body = Object.assign({}, u, patch, { lineIds: u.lineIds || [], extIds: u.extIds || [] });
  delete body.onlineIps; delete body.subUrl;
  await put('users/' + u.id, body);
}

registerActions({
  'user.filter': id => { filter = id; page = 1; document.querySelectorAll('#user-filter button').forEach(b => b.classList.toggle('active', b.dataset.id === id)); renderRows(); },
  'user.clearOnly': () => { only = ''; page = 1; renderRows(); },
  // 选中 = 当前筛选结果的全部(替换,不并进以前勾的):按钮上写的是 N 个,批量操作的也就是这 N 个
  'user.selAllFiltered': () => { selected.clear(); visibleRows().forEach(u => selected.add(u.id)); renderRows(); },
  'user.page': id => { page += id === 'next' ? 1 : -1; renderRows(); },
  'user.chartRange': async id => {
    drawerRange = Number(id);
    document.querySelectorAll('[data-act="user.chartRange"]').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    const u = state.users.find(x => x.id === drawerUser);
    if (u) await renderUserChart(u.name, drawerRange);
  },
  'user.sort': id => {
    if (sortKey === id) sortDir = -sortDir; else { sortKey = id; sortDir = 1; }
    page = 1;
    document.querySelectorAll('th.sortable').forEach(h => {
      h.classList.toggle('sorted', h.dataset.id === sortKey);
      h.innerHTML = h.innerHTML.replace(/ [▲▼]$/, '') + (h.dataset.id === sortKey ? (sortDir > 0 ? ' ▲' : ' ▼') : '');
    });
    renderRows();
  },
  'user.add': () => editUser(null),
  'user.edit': id => editUser(Number(id)),
  'user.detail': id => showDetail(Number(id)),
  'user.copyText': id => copy(id),
  // 「更多」里的三种订阅:data-fmt 是 clash / link / json,各复制各的地址(以前只认 link,sing-box 那项复制成了 Clash 地址)
  'user.copy': async (id, btn) => { const l = await get(`users/${id}/sub`); const f = btn.dataset.fmt; copy((f === 'link' || f === 'json') && l[f] ? l[f] : l.clash); },
  'user.fillPlan': () => fillFromPlan(fv('f-plan')),
  'user.renew': id => renewDialog([Number(id)]),
  'user.change': id => changeDialog(Number(id)),
  'user.bulk': () => bulkDialog(),
  'user.bulkCopy': () => { const ta = document.getElementById('bulk-result'); if (ta) copy(ta.value); },
  'user.export': () => { window.open('./api/users/export', '_blank'); },
  'user.import': () => {
    openModal(t('user.importTitle'), `
      <p class="hint xp">${t('user.importHelp')}</p>
      <input type="file" id="imp-file" accept=".db,.sqlite,.sqlite3" style="margin:.6rem 0">
      ${check('imp-assign', t('user.importAssign'), true)}`, async () => {
      const f = document.getElementById('imp-file').files[0];
      if (!f) throw new Error(t('user.importPick'));
      const r = await upload('users/import', f, { assign: String(fchk('imp-assign')) });
      await load('users', 'status');
      renderRows();
      const done = t('user.importDone', { c: r.created, u: r.updated, a: r.assigned });
      const skipped = r.skipped && r.skipped.length ? t('user.importSkipped', { n: r.skipped.length, names: r.skipped.slice(0, 10).join(', ') }) : '';
      toast(skipped ? done + ' · ' + skipped : done, skipped ? 'warn' : 'ok');
    }, { saveText: t('user.importGo') });
  },
  'user.sel': (id, cb) => { cb.checked ? selected.add(Number(id)) : selected.delete(Number(id)); selFrom = cb; renderRows(); },
  'user.clearSel': () => { selected.clear(); renderRows(); },
  'user.batch': async action => {
    const ids = [...selected];
    if (!ids.length) return;
    // 已选跨筛选保留(搜 alice 勾上、再搜 bob 勾上、一起停用):其中有当前列表里看不到的人时先说清楚
    const vis = new Set(visibleRows().map(u => u.id));
    const hidden = ids.filter(id => !vis.has(id)).length;
    if (action === 'delete') {
      if (!await confirm(t('user.batchDeleteConfirm', { n: ids.length }) + (hidden ? ' ' + t('user.batchHidden', { n: hidden }) : ''), { danger: true, okText: t('common.delete') })) return;
    } else if (hidden && !await confirm(t('user.batchHiddenConfirm', { n: ids.length, hidden }))) return;
    if (action === 'plan') { renewDialog(ids); return; }
    if (action === 'extend') { // 输入天数用面板自己的弹窗(浏览器原生 prompt 在一些环境里被拦)
      openModal(t('user.batch.extend') + ` (${ids.length})`, `<div class="form-grid">${field(t('user.batchDays'), '<input id="f-days" type="number" min="1" max="3650" step="1" value="30">')}</div>`, async () => {
        const days = Number(fv('f-days'));
        if (!(Number.isInteger(days) && days >= 1 && days <= 3650)) throw new Error(t('user.batchDaysBad'));
        const r = await post('users/batch', { ids, action, days });
        // 延期不是幂等的:提交成功就关弹窗,之后刷新失败只提示,不能让人在弹窗里再点一次「保存」又延一遍
        selected.clear();
        toast(t('user.batchDone', { n: r.affected }), 'ok');
        refreshAfterBatch();
      });
      return;
    }
    const days = 0;
    try {
      const r = await post('users/batch', { ids, action, days });
      selected.clear();
      await load('users', 'lines', 'status'); renderRows();
      toast(t('user.batchDone', { n: r.affected }), 'ok');
    } catch (e) { toast(e.message, 'err'); }
  },
  'user.extend': async id => {
    const u = state.users.find(x => x.id === Number(id));
    // 走后端的延期:不限期的不动,到期被停的自动恢复,手动停用的仍停着(以前带 enabled:true 把手动停用的也启用了)
    try { await post('users/batch', { ids: [u.id], action: 'extend', days: 30 }); await load('users'); renderRows(); if (drawerUser === u.id) showDetail(u.id); toast(t('user.extended'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  // 「更多」里的停用 / 启用:启用着的显示「停用」,停用着的显示「启用」,点一下就换(和勾上一个人再点上面的批量停用 / 启用是同一个操作)。
  // 到期、超量被停的人手动启用后,系统下一轮检查还会把他再停掉:启用成功的同时说明白,免得以为没生效
  'user.toggle': async id => {
    const u = state.users.find(x => x.id === Number(id));
    if (!u) return;
    const on = !u.enabled, name = u.name;
    try {
      await post('users/batch', { ids: [u.id], action: on ? 'enable' : 'disable' });
      await load('users', 'status'); renderRows();
      if (drawerUser === u.id) showDetail(u.id);
      if (on && isExpired(u)) toast(t('user.enableStillExpired', { name }), 'warn');
      else if (on && isOver(u)) toast(t('user.enableStillOver', { name }), 'warn');
      else toast(t(on ? 'user.toggledOn' : 'user.toggledOff', { name }), 'ok');
    } catch (e) { toast(e.message, 'err'); }
  },
  'user.reset': async id => {
    const u = state.users.find(x => x.id === Number(id));
    if (!await confirm(t('user.resetConfirm', { name: u.name }))) return;
    try { await post(`users/${id}/reset`); await load('users'); renderRows(); if (drawerUser === u.id) showDetail(u.id); toast(t('user.resetDone'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'user.kick': async id => {
    const u = state.users.find(x => x.id === Number(id));
    // 主面板(含代理面板)的踢线会下发到所有服务器;副机面板只管本机
    const local = state.status.role === 'node';
    if (!await confirm(t(local ? 'user.kickConfirmLocal' : 'user.kickConfirm', { name: u.name }))) return;
    try {
      const r = await post(`users/${id}/kick`);
      const servers = Array.isArray(r.servers) ? r.servers : [];
      // 多台机器时列出每台的结果:本机 2 · hk 1 · tw 未响应
      const detail = servers.length > 1 ? ' (' + servers.map(s => (s.local ? t('user.kickLocal') : (s.name || '')) + ' ' + // toast 走 textContent,不转义
        (s.error ? t(s.outdated ? 'user.kickOutdated' : s.unconfigured ? 'user.kickUnconfigured' : 'user.kickNoReply') : String(s.closed))).join(' · ') + ')' : '';
      if (r.failed > 0) toast(t('user.kickedPartial', { n: r.closed, f: r.failed }) + detail, 'warn');
      else toast(t('user.kicked', { n: r.closed }) + detail, 'ok');
    } catch (e) { toast(e.message, 'err'); }
  },
  'user.rotate': async id => {
    const u = state.users.find(x => x.id === Number(id));
    if (!await confirm(t('user.rotateConfirm', { name: u.name }), { danger: true, okText: t('user.rotate') })) return;
    try { await post(`users/${id}/rotate`); await load('users'); renderRows(); if (drawerUser === u.id) showDetail(u.id); toast(t('user.rotated'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'user.unshare': async id => {
    const u = state.users.find(x => x.id === Number(id));
    if (!await confirm(t('user.unshareConfirm', { name: u.name }), { danger: true, okText: t('user.unshare') })) return;
    try { await del(`users/${id}/share`); await load('users'); renderRows(); showDetail(Number(id)); toast(t('user.unshared'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'user.del': async id => {
    const u = state.users.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: u.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('users/' + id); selected.delete(u.id); await load('users', 'lines', 'status'); renderRows(); if (drawerUser === u.id) closeDrawer(); toast(t('user.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});
