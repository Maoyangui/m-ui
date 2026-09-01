import { state, load } from '../app.js';
import { get, post, put, del, qrUrl } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtBytes, fmtDay, fmtRelative, daysLeft, toast, confirm, openModal, openDrawer, closeDrawer, registerActions, badge, dot, progress, field, check, empty, fv, fchk, matches, debounce, copy } from '../ui.js';
import { areaChart } from '../chart.js';

export const title = () => t('user.title');
export const subtitle = () => t('user.subtitle');
let query = '', filter = 'all', drawerUser = null;

const now = () => Math.floor(Date.now() / 1000);
const isExpired = u => u.expiry > 0 && u.expiry < now();
const isOver = u => u.volume > 0 && (u.up + u.down) > u.volume;

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="user-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      <div class="seg" id="user-filter">${['all', 'enabled', 'disabled', 'expired', 'over'].map(f => `<button data-act="user.filter" data-id="${f}" class="${f === filter ? 'active' : ''}">${t('user.filter.' + f)}</button>`).join('')}</div>
      <span class="muted small" id="user-count"></span>
      <span class="grow"></span>
      <button class="btn primary" data-act="user.add">${t('user.add')}</button>
    </div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('common.status')}</th><th>${t('user.usage')}</th><th>${t('user.expiry')}</th><th>${t('user.devices')}</th><th>${t('user.speed')}</th><th></th></tr></thead>
      <tbody id="users-body"></tbody>
    </table></div>`;
  document.getElementById('user-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
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

function renderRows() {
  const body = document.getElementById('users-body');
  if (!body) return;
  const online = new Set(state.onlines.users || []);
  let rows = state.users.filter(u => matches(query, u.name, u.remark, u.desc));
  rows = rows.filter(u => ({ all: true, enabled: u.enabled && !isExpired(u) && !isOver(u), disabled: !u.enabled, expired: isExpired(u), over: isOver(u) })[filter]);
  document.getElementById('user-count').textContent = `${rows.length} / ${state.users.length}`;
  if (!rows.length) { body.innerHTML = `<tr><td colspan="7">${empty()}</td></tr>`; return; }
  body.innerHTML = rows.map(u => {
    const used = (u.up || 0) + (u.down || 0);
    const ips = (u.onlineIps || []).length;
    return `<tr>
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
          <button data-act="user.extend" data-id="${u.id}">${t('user.extend')}</button>
          <button data-act="user.reset" data-id="${u.id}">${t('user.reset')}</button>
          <button data-act="user.kick" data-id="${u.id}">${t('user.kick')}</button>
          <hr><button class="danger" data-act="user.del" data-id="${u.id}">${t('common.delete')}</button>
        </div></details>
      </td></tr>`;
  }).join('');
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
        <button class="btn sm" data-act="user.extend" data-id="${id}">${t('user.extend')}</button>
        <button class="btn sm" data-act="user.reset" data-id="${id}">${t('user.reset')}</button>
        <button class="btn sm" data-act="user.kick" data-id="${id}">${t('user.kick')}</button>
        <button class="btn sm" data-act="user.edit" data-id="${id}">${t('common.edit')}</button>
      </div>
    </section>
    <section><h3>${t('user.onlineIps')}</h3><div id="ud-ips"></div></section>
    <section><h3>${t('user.traffic7d')}</h3><div id="ud-chart"></div></section>
    <section><h3>${t('user.lines')}</h3><div class="chips">${(u.lineIds || []).map(lid => { const l = state.lines.find(x => x.id === lid); return l ? `<span class="chip">${esc(l.name)}</span>` : ''; }).join('') || `<span class="muted small">${t('common.none')}</span>`}</div></section>`);
  refreshDrawerLive();
  const d = await get(`stats?resource=user&tag=${encodeURIComponent(u.name)}&hours=168`);
  const ch = document.getElementById('ud-chart');
  if (ch) areaChart(ch, d.points, { height: 160, upLabel: t('dash.up'), downLabel: t('dash.down') });
}

function refreshDrawerLive() {
  const u = state.users.find(x => x.id === drawerUser);
  const el = document.getElementById('ud-ips');
  if (!u || !el) return;
  const ips = u.onlineIps || [];
  const conns = state.onlines.connCounts[u.name] || 0;
  el.innerHTML = ips.length
    ? `<div class="ip-list">${ips.map(ip => `<div class="ip"><span>${esc(ip)}</span></div>`).join('')}</div><p class="hint">${conns} ${t('dash.conns')}</p>`
    : `<p class="muted small">${t('user.noOnline')}</p>`;
}

// ---- 编辑 ----
function editUser(id) {
  const u = id ? state.users.find(x => x.id === id) : { enabled: true, lineIds: [], resetDays: 30 };
  const lineIds = new Set(u.lineIds || []);
  const expiryVal = u.expiry ? new Date(u.expiry * 1000).toISOString().slice(0, 10) : '';
  openModal(id ? t('user.edit') : t('user.add'), `
    <div class="form-grid">
      ${field(t('user.f.name'), `<input id="f-name" value="${esc(u.name || '')}" ${id ? '' : 'autofocus'}>`, t('user.f.nameHelp'))}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(u.remark || '')}">`)}
      ${field(t('user.f.volume'), `<input id="f-volume" type="number" min="0" value="${u.volume ? (u.volume / 1073741824).toFixed(0) : 0}">`)}
      ${field(t('user.f.expiry'), `<input id="f-expiry" type="date" value="${expiryVal}">`)}
      ${field(t('user.f.device'), `<input id="f-device" type="number" min="0" value="${u.deviceLimit || 0}">`, t('user.f.deviceHelp'))}
      ${field(t('user.f.up'), `<input id="f-up" type="number" min="0" value="${u.speedUp || 0}">`)}
      ${field(t('user.f.down'), `<input id="f-down" type="number" min="0" value="${u.speedDown || 0}">`, t('user.f.speedHelp'))}
      ${field(t('user.f.resetDays'), `<input id="f-resetdays" type="number" min="1" value="${u.resetDays || 30}">`)}
      ${check('f-enabled', t('user.f.enabled'), u.enabled !== false)}
      ${check('f-autoreset', t('user.f.autoReset'), !!u.autoReset)}
      <div class="full">${field(t('user.f.lines'), `
        <div class="check-list">
          <label><input type="checkbox" id="f-all"> <b>${t('user.f.selectAll')}</b></label>
          ${state.lines.map(l => `<label><input type="checkbox" class="ln-cb" value="${l.id}" ${lineIds.has(l.id) ? 'checked' : ''}> ${esc(l.name)}</label>`).join('')}
        </div>`)}</div>
    </div>`, async () => {
    const expiry = fv('f-expiry');
    const body = {
      name: fv('f-name').trim(), remark: fv('f-remark'), enabled: fchk('f-enabled'),
      volume: Math.round(Number(fv('f-volume')) * 1073741824),
      expiry: expiry ? Math.floor(new Date(expiry + 'T23:59:59').getTime() / 1000) : 0,
      deviceLimit: Number(fv('f-device')), speedUp: Number(fv('f-up')), speedDown: Number(fv('f-down')),
      autoReset: fchk('f-autoreset'), resetDays: Number(fv('f-resetdays')),
      nextReset: u.nextReset || 0, desc: u.desc || '',
      lineIds: [...document.querySelectorAll('.ln-cb:checked')].map(c => Number(c.value)),
    };
    if (id) await put('users/' + id, body); else await post('users', body);
    await load('users', 'lines', 'status');
    renderRows();
    if (drawerUser === id) showDetail(id);
    toast(id ? t('user.updated') : t('user.created'), 'ok');
  }, { wide: true });
  document.getElementById('f-all').addEventListener('change', e => document.querySelectorAll('.ln-cb').forEach(c => { c.checked = e.target.checked; }));
}

async function fullUpdate(u, patch) {
  const body = Object.assign({}, u, patch, { lineIds: u.lineIds || [] });
  delete body.onlineIps; delete body.subUrl;
  await put('users/' + u.id, body);
}

registerActions({
  'user.filter': id => { filter = id; document.querySelectorAll('#user-filter button').forEach(b => b.classList.toggle('active', b.dataset.id === id)); renderRows(); },
  'user.add': () => editUser(null),
  'user.edit': id => editUser(Number(id)),
  'user.detail': id => showDetail(Number(id)),
  'user.copyText': id => copy(id),
  'user.copy': async (id, btn) => { const l = await get(`users/${id}/sub`); copy(btn.dataset.fmt === 'link' ? l.link : l.clash); },
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
  'user.del': async id => {
    const u = state.users.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: u.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('users/' + id); await load('users', 'lines', 'status'); renderRows(); if (drawerUser === u.id) closeDrawer(); toast(t('user.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});
