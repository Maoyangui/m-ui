import { state, load, isReseller } from '../app.js';
import { get, post, put, del, qrUrl, upload } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtBytes, fmtDay, fmtRelative, daysLeft, toast, confirm, openModal, closeModal, openDrawer, closeDrawer, registerActions, badge, dot, progress, field, check, empty, fv, fchk, matches, debounce, copy } from '../ui.js';
import { barChart, bucketFor } from '../chart.js';
import { lineItems, keysFromRefs, linePicker, refLabels } from '../linepicker.js';

// 代理面板没有服务器页:服务器名与授权范围来自 status;主面板用 state.nodes
const pickerNodes = () => (Array.isArray(state.nodes) && state.nodes.length) ? state.nodes : (Array.isArray(state.status.nodes) ? state.status.nodes : []);
const pickerItems = () => lineItems(state.lines, pickerNodes(), isReseller() ? (state.status.grants || []) : null);
const refsOf = u => u.lineRefs || (u.lineIds || []).map(id => ({ lineId: id }));
let picker = null;
import { tzOffsetMinutes } from '../ui.js';

export const title = () => t('user.title');
export const subtitle = () => t('user.subtitle');
let query = '', filter = 'all', drawerUser = null, sortKey = 'id', sortDir = 1;
const selected = new Set();
// 分页:全部用户 → 搜索 → 筛选 → 排序 → 只渲染当前页。上千用户时不再一次画出整张表。
const PAGE_SIZES = [25, 50, 100];
let page = 1, pageSize = 25;
try { const v = Number(localStorage.getItem('m-ui-users-ps')); if (PAGE_SIZES.includes(v)) pageSize = v; } catch {}
let lastPager = '';
const SORTS = {
  id: u => u.id, name: u => u.name.toLowerCase(), usage: u => (u.up || 0) + (u.down || 0),
  expiry: u => u.expiry || Number.MAX_SAFE_INTEGER, devices: u => (u.onlineIps || []).length, status: u => (u.enabled ? 0 : 1),
};
const th = (key, label) => `<th class="sortable ${sortKey === key ? 'sorted' : ''}" data-act="user.sort" data-id="${key}">${label}${sortKey === key ? (sortDir > 0 ? ' ▲' : ' ▼') : ''}</th>`;

const now = () => Math.floor(Date.now() / 1000);
const isExpired = u => u.expiry > 0 && u.expiry < now();
const isOver = u => u.volume > 0 && (u.up + u.down) > u.volume;
const planSelect = (id, cur, allowNone = true) =>
  `<select id="${id}">${allowNone ? `<option value="0">${t('plan.none')}</option>` : ''}${state.plans.map(p => `<option value="${p.id}" ${cur === p.id ? 'selected' : ''}>${esc(p.name)} · ${p.volumeGb ? p.volumeGb + 'GB' : '∞'} / ${p.days ? p.days + t('common.day') : '∞'}</option>`).join('')}</select>`;

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="user-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      <div class="seg" id="user-filter">${['all', 'enabled', 'disabled', 'expired', 'over'].map(f => `<button data-act="user.filter" data-id="${f}" class="${f === filter ? 'active' : ''}">${t('user.filter.' + f)}</button>`).join('')}</div>
      <span class="muted small" id="user-count"></span>
      <span class="grow"></span>
      ${isReseller() ? '' : `<button class="btn" data-act="user.import">${t('user.import')}</button>
      <button class="btn" data-act="user.export">${t('user.batch.export')}</button>
      <button class="btn" data-act="user.bulk">${t('user.bulk')}</button>`}
      <button class="btn primary" data-act="user.add">${t('user.add')}</button>
    </div>
    <div class="toolbar batch-bar" id="batch-bar" hidden>
      <span class="badge primary" id="batch-count"></span>
      <button class="btn sm" data-act="user.batch" data-id="enable">${t('user.batch.enable')}</button>
      <button class="btn sm" data-act="user.batch" data-id="disable">${t('user.batch.disable')}</button>
      <button class="btn sm" data-act="user.batch" data-id="extend">${t('user.batch.extend')}</button>
      <button class="btn sm" data-act="user.batch" data-id="plan">${t('user.batch.plan')}</button>
      <button class="btn sm" data-act="user.batch" data-id="reset">${t('user.batch.reset')}</button>
      <button class="btn sm danger" data-act="user.batch" data-id="delete">${t('user.batch.delete')}</button>
      <span class="grow"></span>
      <button class="btn sm ghost" data-act="user.clearSel">${t('common.cancel')}</button>
    </div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th style="width:1.5rem"><input type="checkbox" id="sel-all"></th>${th('name', t('common.name'))}${th('status', t('common.status'))}${th('usage', t('user.usage'))}${th('expiry', t('user.expiry'))}${th('devices', t('user.devices'))}<th>${t('user.speed')}</th><th></th></tr></thead>
      <tbody id="users-body"></tbody>
    </table></div>
    <div class="pager" id="user-pager" hidden></div>`;
  document.getElementById('user-q').addEventListener('input', debounce(e => { query = e.target.value; page = 1; renderRows(); }));
  // 表头的全选只勾当前页显示的用户:免得以为选了 25 个,实际操作了几千个
  document.getElementById('sel-all').addEventListener('change', e => {
    pageRows().forEach(u => e.target.checked ? selected.add(u.id) : selected.delete(u.id));
    renderRows();
  });
  lastPager = '';
  renderRows();
}
export async function tick() { await load('users'); renderRows(); if (drawerUser) refreshDrawerLive(); }

function statusBadge(u) {
  if (!u.enabled) return badge(t('common.disabled'), 'danger');
  if (isExpired(u)) return badge(t('user.expired'), 'warn');
  if (isOver(u)) return badge(t('user.over'), 'warn');
  return badge(t('common.enabled'), 'ok');
}
function expiryCell(u) {
  if (!u.expiry) return `<span class="muted">${t('user.never')}</span>`;
  const d = daysLeft(u.expiry);
  const b = d < 0 ? badge(t('user.expired'), 'danger') : d <= 7 ? badge(t('user.daysLeft', { n: d }), 'warn') : badge(t('user.daysLeft', { n: d }));
  return `${fmtDay(u.expiry)}<div class="sub-cell">${b}</div>`;
}
function visibleRows() {
  let rows = state.users.filter(u => matches(query, u.name, u.remark, u.desc));
  rows = rows.filter(u => ({ all: true, enabled: u.enabled && !isExpired(u) && !isOver(u), disabled: !u.enabled, expired: isExpired(u), over: isOver(u) })[filter]);
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
  const total = visibleRows().length;
  const rows = pageRows();
  document.getElementById('user-count').textContent = `${total} / ${state.users.length}`;
  const bar = document.getElementById('batch-bar');
  bar.hidden = selected.size === 0;
  document.getElementById('batch-count').textContent = t('user.selected', { n: selected.size });
  const selAll = document.getElementById('sel-all');
  if (selAll) selAll.checked = rows.length > 0 && rows.every(u => selected.has(u.id));
  renderPager(total);
  if (!rows.length) { body.innerHTML = `<tr><td colspan="8">${state.users.length ? empty() : firstUserGuide()}</td></tr>`; return; }
  body.innerHTML = rows.map(u => {
    const used = (u.up || 0) + (u.down || 0);
    const ips = (u.onlineIps || []).length;
    return `<tr class="${selected.has(u.id) ? 'selected' : ''}">
      <td><input type="checkbox" class="sel" data-change="user.sel" data-id="${u.id}" ${selected.has(u.id) ? 'checked' : ''}></td>
      <td class="primary-cell">${dot(online.has(u.name))}<a href="#" data-act="user.detail" data-id="${u.id}">${esc(u.name)}</a>${u.remark ? `<div class="sub-cell">${esc(u.remark)}</div>` : ''}</td>
      <td>${statusBadge(u)}</td>
      <td><span class="num">${fmtBytes(used, 1)}</span> <span class="muted small">/ ${u.volume ? fmtBytes(u.volume, 0) : t('common.unlimited')}</span>${progress(used, u.volume)}</td>
      <td>${expiryCell(u)}</td>
      <td class="num">${ips} / ${u.deviceLimit || '∞'}</td>
      <td class="num">${(u.speedUp || u.speedDown) ? `${u.speedUp || '∞'}/${u.speedDown || '∞'} M` : '∞'}</td>
      <td class="actions">
        <button class="btn sm" data-act="user.detail" data-id="${u.id}">${t('common.details')}</button>
        <button class="btn sm" data-act="user.edit" data-id="${u.id}">${t('common.edit')}</button>
        <details class="menu"><summary class="btn sm">${t('common.more')} ▾</summary><div class="menu-list">
          <button data-act="user.copy" data-id="${u.id}" data-fmt="clash">${t('user.subClash')}</button>
          <button data-act="user.copy" data-id="${u.id}" data-fmt="link">${t('user.subLink')}</button>
          <button data-act="user.copy" data-id="${u.id}" data-fmt="json">${t('user.subJson')}</button>
          <button data-act="user.renew" data-id="${u.id}">${t('user.renew')}</button>
          <button data-act="user.extend" data-id="${u.id}">${t('user.extend')}</button>
          <button data-act="user.reset" data-id="${u.id}">${t('user.reset')}</button>
          <button data-act="user.kick" data-id="${u.id}">${t('user.kick')}</button>
          <hr><button class="danger" data-act="user.del" data-id="${u.id}">${t('common.delete')}</button>
        </div></details>
      </td></tr>`;
  }).join('');
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
  openDrawer(`${esc(u.name)} ${statusBadge(u)}`, `
    <section>
      <h3>${t('user.subscription')}</h3>
      <div class="sub-box"><code>${esc(links.clash)}</code><button class="btn sm" data-act="user.copyText" data-id="${esc(links.clash)}">${t('common.copy')}</button></div>
      <div class="sub-box"><code>${esc(links.link)}</code><button class="btn sm" data-act="user.copyText" data-id="${esc(links.link)}">${t('common.copy')}</button></div>
      ${links.share ? `<div class="sub-box">${badge(t('user.share'), 'warn')}<code>${esc(links.share)}</code><button class="btn sm" data-act="user.copyText" data-id="${esc(links.share)}">${t('common.copy')}</button><button class="btn sm danger" data-act="user.unshare" data-id="${id}">${t('user.unshare')}</button></div>` : ''}
      <img class="qr" src="${qrUrl(id, 'clash')}" alt="QR" title="${t('user.subClash')}">
    </section>
    <section>
      <h3>${t('user.usage')}</h3>
      <dl class="kv">
        <dt>${t('user.usage')}</dt><dd>${fmtBytes(used)} / ${u.volume ? fmtBytes(u.volume) : t('common.unlimited')}${progress(used, u.volume)}</dd>
        <dt>${t('dash.up')} / ${t('dash.down')}</dt><dd class="num">${fmtBytes(u.up)} / ${fmtBytes(u.down)}</dd>
        <dt>${t('user.total')}</dt><dd class="num">${fmtBytes((u.totalUp || 0) + (u.totalDown || 0) + used)}</dd>
        <dt>${t('user.expiry')}</dt><dd>${u.expiry ? fmtDay(u.expiry) : t('user.never')}</dd>
        <dt>${t('user.lastOnline')}</dt><dd>${u.onlineAt ? fmtRelative(u.onlineAt) : '—'}</dd>
        <dt>${t('user.f.autoReset')}</dt><dd>${u.autoReset ? `${u.resetDays} ${t('common.day')} · ${fmtDay(u.nextReset)}` : t('common.no')}</dd>
      </dl>
      <div class="row" style="margin-top:.6rem;flex-wrap:wrap">
        <button class="btn sm primary" data-act="user.renew" data-id="${id}">${t('user.renew')}</button>
        <button class="btn sm" data-act="user.extend" data-id="${id}">${t('user.extend')}</button>
        <button class="btn sm" data-act="user.reset" data-id="${id}">${t('user.reset')}</button>
        <button class="btn sm" data-act="user.kick" data-id="${id}">${t('user.kick')}</button>
        <button class="btn sm" data-act="user.edit" data-id="${id}">${t('common.edit')}</button>
      </div>
    </section>
    <section><h3>${t('user.onlineIps')}</h3><div id="ud-ips"></div></section>
    <section>
      <div class="row" style="justify-content:space-between;align-items:center;margin-bottom:.4rem"><h3 style="margin:0">${t('user.trafficChart')}</h3>
        <div class="seg">${[24, 168, 720].map(h => `<button data-act="user.chartRange" data-id="${h}" class="${h === drawerRange ? 'active' : ''}">${t('dash.range.' + (h === 24 ? '24h' : h === 168 ? '7d' : '30d'))}</button>`).join('')}</div></div>
      <div id="ud-chart"></div>
    </section>
    <section><h3>${t('user.lines')}</h3><div class="chips">${refLabels(refsOf(u), state.lines, pickerNodes()).map(n => `<span class="chip">${esc(n)}</span>`).join('') || `<span class="muted small">${t('common.none')}</span>`}</div>
      ${(u.extIds || []).length ? `<h3 style="margin-top:.6rem">${t('user.f.exts')}</h3><div class="chips">${u.extIds.map(eid => { const x = (state.exts || []).find(e => e.id === eid); return x ? `<span class="chip">${esc(x.name)} <span class="muted">${x.nodeCount || 0}</span></span>` : ''; }).join('')}</div>` : ''}</section>`);
  refreshDrawerLive();
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
    ? `<div class="ip-list">${ips.map(ip => `<div class="ip"><span class="mono">${esc(ip)}</span><span class="ip-lines">${
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
  document.getElementById('f-expiry').value = p.days ? new Date(Date.now() + p.days * 86400000).toISOString().slice(0, 10) : '';
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

function editUser(id) {
  const u = id ? state.users.find(x => x.id === id) : { enabled: true, lineIds: [], resetDays: 30 };
  const expiryVal = u.expiry ? new Date(u.expiry * 1000).toISOString().slice(0, 10) : '';
  openModal(id ? t('user.edit') : t('user.add'), `
    <div class="form-grid">
      ${field(nameLabel(), `<input id="f-name" value="${esc(u.name || '')}">`, nameHelp())}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(u.remark || '')}">`)}
      ${state.plans.length ? `<div class="full">${field(t('user.fromPlan'), `<div class="row">${planSelect('f-plan', 0)}<button type="button" class="btn" data-act="user.fillPlan">${t('user.fromPlan')}</button></div>`)}</div>` : ''}
      ${field(t('user.f.volume'), `<input id="f-volume" type="number" min="0" value="${u.volume ? (u.volume / 1073741824).toFixed(0) : 0}">`)}
      ${field(t('user.f.expiry'), `<input id="f-expiry" type="date" value="${expiryVal}">`)}
      ${field(t('user.f.device'), `<input id="f-device" type="number" min="0" value="${u.deviceLimit || 0}">`, t('user.f.deviceHelp'))}
      ${field(t('user.f.up'), `<input id="f-up" type="number" min="0" value="${u.speedUp || 0}">`)}
      ${field(t('user.f.down'), `<input id="f-down" type="number" min="0" value="${u.speedDown || 0}">`, t('user.f.speedHelp'))}
      ${field(t('user.f.resetDays'), `<input id="f-resetdays" type="number" min="1" value="${u.resetDays || 30}">`)}
      ${check('f-enabled', t('user.f.enabled'), u.enabled !== false)}
      ${check('f-autoreset', t('user.f.autoReset'), !!u.autoReset)}
      <div class="full">${field(t('user.f.lines'), `<div id="f-lines"></div>`, t('lp.help'))}</div>
      ${(state.exts || []).length ? `<div class="full">${field(t('user.f.exts'), `
        <div class="check-list">
          ${(state.exts || []).map(x => `<label><input type="checkbox" class="ext-cb" value="${x.id}" ${(u.extIds || []).includes(x.id) ? 'checked' : ''}> ${esc(x.name)} <span class="muted small">${x.type === 'sub' ? t('ext.typeSub') : t('ext.typeLink')} · ${x.nodeCount || 0}</span></label>`).join('')}
        </div>`, t('user.f.extsHelp'))}</div>` : ''}
    </div>`, async () => {
    const expiry = fv('f-expiry');
    const body = {
      name: fv('f-name').trim(), remark: fv('f-remark'), enabled: fchk('f-enabled'),
      volume: Math.round(Number(fv('f-volume')) * 1073741824),
      expiry: expiry ? Math.floor(new Date(expiry + 'T23:59:59').getTime() / 1000) : 0,
      deviceLimit: Number(fv('f-device')), speedUp: Number(fv('f-up')), speedDown: Number(fv('f-down')),
      autoReset: fchk('f-autoreset'), resetDays: Number(fv('f-resetdays')),
      nextReset: u.nextReset || 0, desc: u.desc || '',
      lineRefs: picker.read(), lineIds: picker.read().map(r => r.lineId),
      extIds: [...document.querySelectorAll('.ext-cb:checked')].map(c => Number(c.value)),
    };
    const created = id ? null : await post('users', body);
    if (id) await put('users/' + id, body);
    await load('users', 'lines', 'status');
    renderRows();
    if (drawerUser === id) showDetail(id);
    toast(id ? t('user.updated') : t('user.created'), 'ok');
    // 系统里的第一个用户:直接打开详情抽屉,订阅地址 / 二维码 / 复制就在眼前,不用再找
    if (created && created.id && state.users.length === 1) showDetail(created.id);
  }, { wide: true });
  const items = pickerItems();
  picker = linePicker(document.getElementById('f-lines'), { items, selected: keysFromRefs(refsOf(u), items) });
}

// ---- 续费 / 延期(套餐)----
function renewDialog(ids) {
  if (!state.plans.length) { toast(t('plan.empty'), 'err'); return; }
  const single = ids.length === 1 ? state.users.find(x => x.id === ids[0]) : null;
  openModal(t('user.renew') + (single ? ' · ' + single.name : ` (${ids.length})`), `
    <div class="form-grid">
      <div class="full">${field(t('plan.pick'), planSelect('f-plan', state.plans[0].id, false))}</div>
      <div class="full">${field('', `<select id="f-mode"><option value="renew">${t('plan.renew')}</option><option value="extend">${t('plan.extend')}</option></select>`)}</div>
    </div>`, async () => {
    const planId = Number(fv('f-plan')), mode = fv('f-mode');
    if (ids.length === 1) await post(`users/${ids[0]}/plan`, { planId, mode });
    else await post('users/batch', { ids, action: 'plan', planId, mode });
    await load('users'); renderRows();
    if (drawerUser && ids.includes(drawerUser)) showDetail(drawerUser);
    toast(t('plan.applied'), 'ok');
  });
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
      <div class="full">${field(t('user.f.lines'), `<div id="f-lines"></div>`, t('lp.help'))}</div>
    </div>`, async () => {
    const res = await post('users/bulk', {
      prefix: fv('f-prefix').trim(), count: Number(fv('f-count')), nameMode: fv('f-mode'), startIndex: Number(fv('f-start')),
      remark: fv('f-remark'), planId: Number(fv('f-plan')),
      lineIds: picker.read().map(r => r.lineId), lineRefs: picker.read(),
    });
    await load('users', 'lines', 'status'); renderRows();
    toast(t('user.bulkDone', { n: res.length }), 'ok');
    closeModal();
    openModal(t('user.bulkResult'), `<textarea style="min-height:16rem" readonly>${esc(res.map(r => r.name + '\t' + r.link).join('\n'))}</textarea>`, null);
  }, { wide: true });
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
  'user.copy': async (id, btn) => { const l = await get(`users/${id}/sub`); copy(btn.dataset.fmt === 'link' ? l.link : l.clash); },
  'user.fillPlan': () => fillFromPlan(fv('f-plan')),
  'user.renew': id => renewDialog([Number(id)]),
  'user.bulk': () => bulkDialog(),
  'user.export': () => { window.open('./api/users/export', '_blank'); },
  'user.import': () => {
    openModal(t('user.importTitle'), `
      <p class="hint">${t('user.importHelp')}</p>
      <input type="file" id="imp-file" accept=".db,.sqlite,.sqlite3" style="margin:.6rem 0">
      ${check('imp-assign', t('user.importAssign'), true)}`, async () => {
      const f = document.getElementById('imp-file').files[0];
      if (!f) throw new Error(t('user.importPick'));
      const r = await upload('users/import', f, { assign: String(fchk('imp-assign')) });
      await load('users', 'status');
      renderRows();
      toast(t('user.importDone', { c: r.created, u: r.updated, a: r.assigned }), 'ok');
    }, { saveText: t('user.importGo') });
  },
  'user.sel': (id, cb) => { cb.checked ? selected.add(Number(id)) : selected.delete(Number(id)); renderRows(); },
  'user.clearSel': () => { selected.clear(); renderRows(); },
  'user.batch': async action => {
    const ids = [...selected];
    if (!ids.length) return;
    if (action === 'plan') { renewDialog(ids); return; }
    let days = 0;
    if (action === 'extend') {
      days = Number(prompt(t('user.batchDays'), '30'));
      if (!days) return;
    }
    if (action === 'delete' && !await confirm(t('user.batchDeleteConfirm', { n: ids.length }), { danger: true, okText: t('common.delete') })) return;
    try {
      const r = await post('users/batch', { ids, action, days });
      selected.clear();
      await load('users', 'lines', 'status'); renderRows();
      toast(t('user.batchDone', { n: r.affected }), 'ok');
    } catch (e) { toast(e.message, 'err'); }
  },
  'user.extend': async id => {
    const u = state.users.find(x => x.id === Number(id));
    const base = Math.max(u.expiry || 0, now());
    try { await fullUpdate(u, { expiry: base + 30 * 86400, enabled: true }); await load('users'); renderRows(); if (drawerUser === u.id) showDetail(u.id); toast(t('user.extended'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'user.reset': async id => {
    const u = state.users.find(x => x.id === Number(id));
    if (!await confirm(t('user.resetConfirm', { name: u.name }))) return;
    try { await post(`users/${id}/reset`); await load('users'); renderRows(); if (drawerUser === u.id) showDetail(u.id); toast(t('user.resetDone'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'user.kick': async id => {
    const u = state.users.find(x => x.id === Number(id));
    if (!await confirm(t('user.kickConfirm', { name: u.name }))) return;
    try { const r = await post(`users/${id}/kick`); toast(t('user.kicked', { n: r.closed }), 'ok'); }
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
