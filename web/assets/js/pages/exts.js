import { state, load } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtRelative, toast, confirm, openModal, registerActions, badge, field, check, empty, fv, fchk } from '../ui.js';

export const title = () => t('ext.title');
export const subtitle = () => t('ext.subtitle');

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar"><span class="muted small">${t('ext.help')}</span><span class="grow"></span><button class="btn primary" data-act="ext.add">${t('ext.add')}</button></div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('common.type')}</th><th>${t('ext.value')}</th><th>${t('ext.nodes')}</th><th>${t('ext.lastFetch')}</th><th>${t('ext.users')}</th><th>${t('common.status')}</th><th></th></tr></thead>
      <tbody id="exts-body"></tbody>
    </table></div>`;
  renderRows();
}
export async function tick() { await load('exts'); renderRows(); }

function renderRows() {
  const body = document.getElementById('exts-body');
  if (!body) return;
  const rows = state.exts || [];
  if (!rows.length) { body.innerHTML = `<tr><td colspan="8">${empty(t('ext.empty'))}</td></tr>`; return; }
  body.innerHTML = rows.map(x => `<tr>
    <td class="primary-cell">${esc(x.name)}${x.prefix ? `<div class="sub-cell">${t('ext.prefix')}: ${esc(x.prefix)}</div>` : ''}${x.remark ? `<div class="sub-cell">${esc(x.remark)}</div>` : ''}</td>
    <td>${badge(x.type === 'sub' ? t('ext.typeSub') : t('ext.typeLink'), x.type === 'sub' ? 'primary' : '')}</td>
    <td class="mono small" title="${esc(x.value)}">${esc(x.value.length > 60 ? x.value.slice(0, 60) + '…' : x.value)}</td>
    <td class="num">${x.nodeCount || 0}</td>
    <td>${x.lastError ? badge(t('ext.failed'), 'danger') + `<div class="sub-cell" title="${esc(x.lastError)}">${esc(x.lastError.slice(0, 60))}</div>` : (x.lastFetch ? fmtRelative(x.lastFetch) : '—')}</td>
    <td class="num">${x.userCount || 0}</td>
    <td>${badge(x.enabled ? t('common.enabled') : t('common.disabled'), x.enabled ? 'ok' : 'danger')}</td>
    <td class="actions">
      ${x.type === 'sub' ? `<button class="btn sm" data-act="ext.refresh" data-id="${x.id}">${t('ext.refresh')}</button>` : ''}
      <button class="btn sm" data-act="ext.preview" data-id="${x.id}">${t('ext.preview')}</button>
      <button class="btn sm" data-act="ext.edit" data-id="${x.id}">${t('common.edit')}</button>
      <button class="btn sm danger" data-act="ext.del" data-id="${x.id}">${t('common.delete')}</button>
    </td></tr>`).join('');
}

function editExt(id) {
  const x = id ? state.exts.find(e => e.id === id) : { type: 'sub', enabled: true };
  openModal(id ? t('ext.edit') : t('ext.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(x.name || '')}" placeholder="${t('ext.namePh')}">`)}
      ${field(t('common.type'), `<select id="f-type"><option value="sub" ${x.type === 'sub' ? 'selected' : ''}>${t('ext.typeSub')}</option><option value="link" ${x.type === 'link' ? 'selected' : ''}>${t('ext.typeLink')}</option></select>`, t('ext.typeHelp'))}
      <div class="full">${field(t('ext.value'), `<textarea id="f-value" style="min-height:5rem">${esc(x.value || '')}</textarea>`, t('ext.valueHelp'))}</div>
      ${field(t('ext.prefix'), `<input id="f-prefix" value="${esc(x.prefix || '')}" placeholder="[中转] ">`, t('ext.prefixHelp'))}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(x.remark || '')}">`)}
      ${field(t('common.sort'), `<input id="f-sort" type="number" value="${x.sort || 0}">`)}
      ${check('f-enabled', t('common.enabled'), x.enabled !== false)}
    </div>`, async () => {
    const body = { name: fv('f-name').trim(), type: fv('f-type'), value: fv('f-value').trim(), prefix: fv('f-prefix'), remark: fv('f-remark'), sort: Number(fv('f-sort')) || 0, enabled: fchk('f-enabled') };
    if (id) await put('exts/' + id, body); else await post('exts', body);
    await load('exts'); renderRows();
    toast(id ? t('ext.updated') : t('ext.created'), 'ok');
  }, { wide: true });
}

registerActions({
  'ext.add': () => editExt(null),
  'ext.edit': id => editExt(Number(id)),
  'ext.refresh': async (id, btn) => {
    btn.disabled = true;
    try { const r = await post(`exts/${id}/refresh`, undefined, SLOW); toast(t('ext.refreshed', { n: r.clash }), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; await load('exts'); renderRows(); }
  },
  'ext.preview': async id => {
    try {
      const r = await get(`exts/${id}/preview`);
      openModal(t('ext.preview'), `<p class="hint">${t('ext.previewHelp', { links: r.links, clash: r.clash })}</p><div class="chips" style="margin-top:.6rem">${(r.names || []).map(n => `<span class="chip">${esc(n)}</span>`).join('') || empty()}</div>`, null);
    } catch (e) { toast(e.message, 'err'); }
  },
  'ext.del': async id => {
    const x = state.exts.find(e => e.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: x.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('exts/' + id); await load('exts', 'users'); renderRows(); toast(t('ext.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});
