import { state, load } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtBytes, fmtDay, toast, confirm, openModal, openDrawer, registerActions, badge, progress, field, check, empty, fv, fchk, copy } from '../ui.js';

export const title = () => t('rs.title');
export const subtitle = () => t('rs.subtitle');

let list = [];

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar"><span class="grow"></span><button class="btn primary" data-act="rs.add">${t('rs.add')}</button></div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('common.status')}</th><th>${t('rs.users')}</th><th>${t('rs.used')}</th>
        <th>${t('rs.devices')}</th><th>${t('rs.online')}</th><th>${t('rs.createdAt')}</th><th></th></tr></thead>
      <tbody id="rs-body"></tbody>
    </table></div>
    <p class="hint">${t('rs.panelHint', { url: panelURL() })}</p>`;
  await reload();
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
  if (!list.length) { body.innerHTML = `<tr><td colspan="8">${empty(t('rs.empty'))}</td></tr>`; return; }
  body.innerHTML = list.map(r => `<tr>
    <td class="primary-cell"><a href="#" data-act="rs.detail" data-id="${r.id}">${esc(r.name)}</a>
      ${r.remark ? `<div class="sub-cell">${esc(r.remark)}</div>` : ''}</td>
    <td>${r.enabled ? badge(t('common.enabled'), 'ok') : badge(t('common.disabled'), 'danger')}
      ${r.totpEnabled ? badge('2FA', 'primary') : ''}</td>
    <td class="num">${r.users}</td>
    <td class="num">${fmtBytes(r.used)}${r.volume ? ' / ' + fmtBytes(r.volume) : ''}${progress(r.used, r.volume)}</td>
    <td class="num">${r.devices}${r.deviceLimit ? ' / ' + r.deviceLimit : ''}</td>
    <td class="num">${r.online}</td>
    <td class="mono small">${r.createdAt ? fmtDay(r.createdAt) : '—'}</td>
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
  openDrawer(`${esc(r.name)} ${r.enabled ? badge(t('common.enabled'), 'ok') : badge(t('common.disabled'), 'danger')}`, `
    <section>
      <h3>${t('rs.quota')}</h3>
      <dl class="kv">
        <dt>${t('rs.used')}</dt><dd>${fmtBytes(r.used)} / ${r.volume ? fmtBytes(r.volume) : t('common.unlimited')}${progress(r.used, r.volume)}</dd>
        <dt>${t('rs.devices')}</dt><dd class="num">${r.devices} / ${r.deviceLimit || '∞'}</dd>
        <dt>${t('rs.online')}</dt><dd class="num">${r.online}</dd>
        <dt>${t('rs.lines')}</dt><dd><div class="chips">${(r.lineIds || []).map(lid => {
          const l = state.lines.find(x => x.id === lid); return l ? `<span class="chip">${esc(l.name)}</span>` : '';
        }).join('') || `<span class="muted small">${t('common.none')}</span>`}</div></dd>
        <dt>${t('rs.lastLogin')}</dt><dd class="mono small">${esc(r.lastLogins || '—')}</dd>
      </dl>
    </section>
    <section><h3>${t('rs.users')} <span class="badge">${users.length}</span></h3>
      ${users.length ? users.map(u => userCard(u)).join('') : `<p class="muted small">${t('rs.noUsers')}</p>`}
    </section>`);
}

function userCard(u) {
  const used = (u.up || 0) + (u.down || 0);
  const lines = u.onlineLines || {};
  return `<div class="rs-user">
    <div class="row" style="justify-content:space-between;align-items:baseline">
      <b>${esc(u.name)}</b>
      <span class="muted small">${fmtBytes(used)}${u.volume ? ' / ' + fmtBytes(u.volume) : ''} · ${u.expiry ? fmtDay(u.expiry) : t('user.never')}</span>
    </div>
    <div class="sub-box"><code>${esc(u.sub.link)}</code><button class="btn sm" data-act="rs.copy" data-id="${esc(u.sub.link)}">${t('common.copy')}</button></div>
    ${(u.onlineIps || []).length ? `<div class="ip-list">${u.onlineIps.map(ip => `<div class="ip"><span class="mono">${esc(ip)}</span><span class="ip-lines">${
      (lines[ip] || []).map(l => badge(l, 'primary')).join(' ') || `<span class="muted small">${t('user.lineUnknown')}</span>`}</span></div>`).join('')}</div>`
      : `<p class="muted small">${t('user.noOnline')}</p>`}
  </div>`;
}

// ---- 新建 / 编辑 ----
function editReseller(id) {
  const r = id ? list.find(x => x.id === id) : { enabled: true, volume: 0, deviceLimit: 0, lineIds: [] };
  const picked = new Set(r.lineIds || []);
  openModal(id ? t('rs.edit') : t('rs.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(r.name || '')}" placeholder="${t('rs.namePh')}">`, t('rs.nameHelp'))}
      ${field(t('rs.volumeGb'), `<input id="f-vol" type="number" min="0" value="${Math.round((r.volume || 0) / 1073741824)}">`, t('plan.zeroUnlimited'))}
      ${field(t('rs.deviceLimit'), `<input id="f-device" type="number" min="0" value="${r.deviceLimit || 0}">`, t('rs.deviceHelp'))}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(r.remark || '')}">`)}
      ${check('f-enabled', t('common.enabled'), r.enabled !== false)}
      <div class="full">${field(t('rs.lines'), `
        <div class="check-list">
          <label><input type="checkbox" id="f-all"> <b>${t('user.f.selectAll')}</b></label>
          ${state.lines.map(l => `<label><input type="checkbox" class="ln-cb" value="${l.id}" ${picked.has(l.id) ? 'checked' : ''}> ${esc(l.name)}</label>`).join('')}
        </div>`, t('rs.linesHelp'))}</div>
    </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), remark: fv('f-remark'), enabled: fchk('f-enabled'),
      volume: Number(fv('f-vol')) * 1073741824, deviceLimit: Number(fv('f-device')),
      lineIds: [...document.querySelectorAll('.ln-cb:checked')].map(c => Number(c.value)),
    };
    if (id) await put('resellers/' + id, body); else await post('resellers', body);
    await reload();
    toast(id ? t('rs.updated') : t('rs.created', { url: panelURL() }), 'ok');
  }, { wide: true });
  document.getElementById('f-all').addEventListener('change', e => document.querySelectorAll('.ln-cb').forEach(c => { c.checked = e.target.checked; }));
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
