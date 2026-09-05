import { state, load } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { lineItems, keysFromRefs, linePicker, refLabels } from '../linepicker.js';
import { esc, fmtBytes, fmtDay, toast, confirm, openModal, openDrawer, registerActions, badge, dot, progress, field, check, empty, fv, fchk, copy } from '../ui.js';

export const title = () => t('rs.title');
export const subtitle = () => t('rs.subtitle');

let list = [];

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar"><span class="grow"></span><button class="btn primary" data-act="rs.add">${t('rs.add')}</button></div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('common.status')}</th><th>${t('rs.users')}</th><th>${t('rs.used')}</th>
        <th>${t('rs.devices')}</th><th>${t('rs.speed')}</th><th>${t('user.expiry')}</th><th>${t('rs.online')}</th><th></th></tr></thead>
      <tbody id="rs-body"></tbody>
    </table></div>
    <p class="hint">${t('rs.panelHint', { url: panelURL() })}</p>`;
  await reload();
}

// 到期也是一种"停":名下用户会被一起停掉,列表要看得出来
function expired(r) { return r.expiry > 0 && r.expiry * 1000 < Date.now(); }
function statusBadge(r) {
  if (!r.enabled) return badge(t('common.disabled'), 'danger');
  if (expired(r)) return badge(t('user.expired'), 'warn');
  if (r.volume > 0 && r.used >= r.volume) return badge(t('user.over'), 'warn');
  return badge(t('common.enabled'), 'ok');
}

// 还没设过密码:窗口内可用名字直接登录,过期后要管理员再点一次"重置密码"
function claimBadge(r) {
  if (!r.needsClaim) return '';
  const open = r.claimBefore > 0 && r.claimBefore * 1000 > Date.now();
  return ' ' + badge(t(open ? 'rs.claimOpen' : 'rs.claimExpired'), open ? 'warn' : 'danger');
}

function panelURL() {
  const s = state.settings || {};
  const host = s.webDomain || (state.status && state.status.domain) || location.hostname;
  const path = s.resellerPath || '/dl/';
  const scheme = location.protocol === 'https:' ? 'https' : 'http';
  return `${scheme}://${host}:${s.resellerPort || 2054}${path}`;
}

export async function tick() { if (document.getElementById('rs-body')) await reload(); }

async function reload() {
  list = await get('resellers').catch(() => []);
  renderRows();
}

function renderRows() {
  const body = document.getElementById('rs-body');
  if (!body) return;
  if (!list.length) { body.innerHTML = `<tr><td colspan="9">${empty(t('rs.empty'))}</td></tr>`; return; }
  body.innerHTML = list.map(r => `<tr>
    <td class="primary-cell"><a href="#" data-act="rs.detail" data-id="${r.id}">${esc(r.name)}</a>
      ${r.remark ? `<div class="sub-cell">${esc(r.remark)}</div>` : ''}</td>
    <td>${statusBadge(r)}${r.totpEnabled ? ' ' + badge('2FA', 'primary') : ''}${claimBadge(r)}</td>
    <td class="num">${r.users} / ${r.userLimit || '∞'}</td>
    <td class="num">${fmtBytes(r.used)}${r.volume ? ' / ' + fmtBytes(r.volume) : ''}${progress(r.used, r.volume)}</td>
    <td class="num">${r.online} / ${r.deviceLimit || '∞'}${r.poolRejects ? ' <span class="badge warn" title="' + esc(t('rs.poolFull', { n: r.poolRejects })) + '">!</span>' : ''}</td>
    <td class="num">${(r.speedUp || r.speedDown) ? `${r.speedUp || '∞'}/${r.speedDown || '∞'} M` : '∞'}</td>
    <td class="${expired(r) ? 'danger' : ''}">${r.expiry ? fmtDay(r.expiry) : `<span class="muted small">${t('user.never')}</span>`}</td>
    <td class="num">${r.online}</td>
    <td class="actions">
      <button class="btn sm" data-act="rs.detail" data-id="${r.id}">${t('common.details')}</button>
      <button class="btn sm" data-act="rs.edit" data-id="${r.id}">${t('common.edit')}</button>
      <details class="menu"><summary class="btn sm">${t('common.more')} ▾</summary><div class="menu-list">
        <button data-act="rs.passwd" data-id="${r.id}">${t('rs.passwd')}</button>
        <button data-act="rs.reset" data-id="${r.id}">${t('rs.reset')}</button>
        <hr><button class="danger" data-act="rs.del" data-id="${r.id}">${t('common.delete')}</button>
      </div></details>
    </td></tr>`).join('');
}

// ---- 详情:名下用户 + 订阅地址 + 在线设备与线路 ----
async function showDetail(id) {
  const r = list.find(x => x.id === id);
  if (!r) return;
  const users = await get(`resellers/${id}/users`).catch(() => []);
  openDrawer(`${esc(r.name)} ${statusBadge(r)}`, `
    <section>
      <h3>${t('rs.quota')}</h3>
      <dl class="kv">
        <dt>${t('rs.used')}</dt><dd>${fmtBytes(r.used)} / ${r.volume ? fmtBytes(r.volume) : t('common.unlimited')}${progress(r.used, r.volume)}</dd>
        <dt>${t('rs.pool')}</dt><dd class="num">${r.online} / ${r.deviceLimit || '∞'} <span class="muted small">${t('rs.allocated')} ${r.devices}</span>${r.poolRejects ? `<div class="muted small" style="color:var(--warn)">${esc(t('rs.poolFull', { n: r.poolRejects }))}</div>` : ''}</dd>
        <dt>${t('rs.speed')}</dt><dd class="num">${(r.speedUp || r.speedDown) ? `${r.speedUp || '∞'} / ${r.speedDown || '∞'} Mbps` : t('common.unlimited')}</dd>
        <dt>${t('user.expiry')}</dt><dd>${r.expiry ? fmtDay(r.expiry) : t('user.never')}</dd>
        <dt>${t('rs.online')}</dt><dd class="num">${r.online}</dd>
        <dt>${t('rs.users')}</dt><dd class="num">${r.users} / ${r.userLimit || '∞'}</dd>
        <dt>${t('rs.lines')} <span class="muted small">${(r.lineIds || []).length}</span></dt>
        <dd><div class="chips rs-lines">${refLabels(r.lineRefs || (r.lineIds || []).map(id => ({ lineId: id })), state.lines, state.nodes || []).map(n => `<span class="chip">${esc(n)}</span>`).join('') || `<span class="muted small">${t('common.none')}</span>`}</div></dd>
        <dt>${t('rs.lastLogin')}</dt><dd class="mono small">${esc(r.lastLogins || '—')}</dd>
      </dl>
    </section>
    <section><h3>${t('rs.users')} <span class="badge">${users.length}</span></h3>
      ${users.length ? `<div class="rs-users">${users.map(u => userRow(u)).join('')}</div>`
        : `<p class="muted small">${t('rs.noUsers')}</p>`}
    </section>`);
}

// 一个用户两行:概要(名称/用量/到期/设备)+ 订阅地址;在线时再列出每个 IP 走的线路
function userRow(u) {
  const used = (u.up || 0) + (u.down || 0);
  const ips = u.onlineIps || [];
  const lines = u.onlineLines || {};
  return `<div class="rs-u">
    <div class="rs-u-head">
      <span class="rs-u-name" title="${esc(u.name)}">${dot(ips.length > 0)}${esc(u.name)}</span>
      <span class="muted">${fmtBytes(used, 1)}${u.volume ? ' / ' + fmtBytes(u.volume, 0) : ''}</span>
      <span class="muted">${u.expiry ? fmtDay(u.expiry) : t('user.never')}</span>
      <span class="muted">${ips.length} / ${u.deviceLimit || '∞'}</span>
    </div>
    <div class="rs-u-sub"><code title="${esc(u.sub.link)}">${esc(u.sub.link)}</code><button class="btn sm" data-act="rs.copy" data-id="${esc(u.sub.link)}">${t('common.copy')}</button></div>
    ${ips.length ? `<div class="ip-list">${ips.map(ip => `<div class="ip"><span class="mono">${esc(ip)}</span><span class="ip-lines">${
      (lines[ip] || []).map(l => badge(l, 'primary')).join(' ') || `<span class="muted small">${t('user.lineUnknown')}</span>`}</span></div>`).join('')}</div>` : ''}
  </div>`;
}

// ---- 新建 / 编辑 ----
function editReseller(id) {
  const r = id ? list.find(x => x.id === id) : { enabled: true, volume: 0, deviceLimit: 0, userLimit: 0, lineIds: [] };
  let picker = null;
  openModal(id ? t('rs.edit') : t('rs.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(r.name || '')}" placeholder="${t('rs.namePh')}">`, t('rs.nameHelp'))}
      ${field(t('rs.volumeGb'), `<input id="f-vol" type="number" min="0" value="${Math.round((r.volume || 0) / 1073741824)}">`, t('plan.zeroUnlimited'))}
      ${field(t('rs.userLimit'), `<input id="f-users" type="number" min="0" value="${r.userLimit || 0}">`, t('rs.userLimitHelp'))}
      ${field(t('rs.deviceLimit'), `<input id="f-device" type="number" min="0" value="${r.deviceLimit || 0}">`, t('rs.deviceHelp'))}
      ${field(t('rs.expiry'), `<input id="f-expiry" type="date" value="${r.expiry ? new Date(r.expiry * 1000).toISOString().slice(0, 10) : ''}">`, t('rs.expiryHelp'))}
      ${field(t('rs.speedUp'), `<input id="f-up" type="number" min="0" value="${r.speedUp || 0}">`, t('rs.speedHelp'))}
      ${field(t('rs.speedDown'), `<input id="f-down" type="number" min="0" value="${r.speedDown || 0}">`, t('rs.speedHelp'))}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(r.remark || '')}">`)}
      ${check('f-enabled', t('common.enabled'), r.enabled !== false)}
      <div class="full">${field(t('rs.lines'), `<div id="f-lines"></div>`, t('rs.linesHelp'))}</div>
    </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), remark: fv('f-remark'), enabled: fchk('f-enabled'),
      volume: Number(fv('f-vol')) * 1073741824, deviceLimit: Number(fv('f-device')), userLimit: Number(fv('f-users')),
      speedUp: Number(fv('f-up')), speedDown: Number(fv('f-down')),
      expiry: fv('f-expiry') ? Math.floor(new Date(fv('f-expiry') + 'T23:59:59').getTime() / 1000) : 0,
      lineRefs: picker.read(), lineIds: picker.read().map(x => x.lineId),
    };
    if (id) await put('resellers/' + id, body); else await post('resellers', body);
    await reload();
    toast(id ? t('rs.updated') : t('rs.created', { url: panelURL() }), 'ok');
  }, { wide: true });
  const items = lineItems(state.lines, state.nodes || [], null);
  picker = linePicker(document.getElementById('f-lines'), { items, selected: keysFromRefs(r.lineRefs || (r.lineIds || []).map(x => ({ lineId: x })), items) });
}

registerActions({
  'rs.add': () => editReseller(null),
  'rs.edit': id => editReseller(Number(id)),
  'rs.detail': id => showDetail(Number(id)),
  'rs.copy': id => copy(id),
  'rs.passwd': async id => {
    const r = list.find(x => x.id === Number(id));
    if (!await confirm(t('rs.passwdConfirm', { name: r.name }), { danger: true, okText: t('rs.passwd') })) return;
    try { await post(`resellers/${id}/passwd`); await reload(); toast(t('rs.passwdDone'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'rs.reset': async id => {
    const r = list.find(x => x.id === Number(id));
    if (!await confirm(t('rs.resetConfirm', { name: r.name }))) return;
    try { await post(`resellers/${id}/reset`); await reload(); toast(t('user.resetDone'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'rs.del': async id => {
    const r = list.find(x => x.id === Number(id));
    if (!await confirm(t('rs.delConfirm', { name: r.name, n: r.users }), { danger: true, okText: t('common.delete') })) return;
    try { await del('resellers/' + id); await reload(); await load('users', 'status'); toast(t('rs.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});
