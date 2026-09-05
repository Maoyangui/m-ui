import { state, load } from '../app.js';
import { post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { lineItems, keysFromRefs, linePicker } from '../linepicker.js';
import { esc, toast, confirm, openModal, registerActions, badge, field, check, empty, fv, fchk } from '../ui.js';

export const title = () => t('plan.title');
export const subtitle = () => t('plan.subtitle');

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar"><span class="grow"></span><button class="btn primary" data-act="plan.add">${t('plan.add')}</button></div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('plan.quota')}</th><th>${t('plan.days')}</th><th>${t('user.devices')}</th><th>${t('user.speed')}</th><th>${t('plan.reset')}</th><th>${t('plan.lines')}</th><th></th></tr></thead>
      <tbody id="plans-body"></tbody>
    </table></div>`;
  renderRows();
}

function renderRows() {
  const body = document.getElementById('plans-body');
  if (!body) return;
  if (!state.plans.length) { body.innerHTML = `<tr><td colspan="8">${empty(t('plan.empty'))}</td></tr>`; return; }
  body.innerHTML = state.plans.map(p => {
    const lines = Array.isArray(p.lineIds) ? p.lineIds.length : (p.lineIds ? JSON.parse(p.lineIds).length : 0);
    return `<tr>
      <td class="primary-cell">${esc(p.name)}${p.desc ? `<div class="sub-cell">${esc(p.desc)}</div>` : ''}</td>
      <td class="num">${p.volumeGb ? p.volumeGb + ' GB' : t('common.unlimited')}</td>
      <td class="num">${p.days ? p.days + ' ' + t('common.day') : t('common.unlimited')}</td>
      <td class="num">${p.deviceLimit || '∞'}</td>
      <td class="num">${(p.speedUp || p.speedDown) ? `${p.speedUp || '∞'}/${p.speedDown || '∞'} M` : '∞'}</td>
      <td>${p.autoReset ? badge(p.resetDays + ' ' + t('common.day'), 'primary') : badge(t('common.no'))}</td>
      <td class="num">${lines || t('plan.keepLines')}</td>
      <td class="actions">
        <button class="btn sm" data-act="plan.edit" data-id="${p.id}">${t('common.edit')}</button>
        <button class="btn sm danger" data-act="plan.del" data-id="${p.id}">${t('common.delete')}</button>
      </td></tr>`;
  }).join('');
}

export function editPlan(id) {
  const p = id ? state.plans.find(x => x.id === id) : { volumeGb: 100, days: 30, resetDays: 30 };
  const ids = Array.isArray(p.lineIds) ? p.lineIds : (p.lineIds ? JSON.parse(p.lineIds) : []);
  let nodesBy = {};
  try { nodesBy = typeof p.lineNodes === 'string' ? JSON.parse(p.lineNodes || '{}') : (p.lineNodes || {}); } catch { nodesBy = {}; }
  const refs = ids.map(id => ({ lineId: id, nodeIds: nodesBy[String(id)] || [] }));
  let picker = null;
  openModal(id ? t('plan.edit') : t('plan.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(p.name || '')}" placeholder="${t('plan.namePh')}">`)}
      ${field(t('plan.desc'), `<input id="f-desc" value="${esc(p.desc || '')}">`)}
      ${field(t('plan.quota') + ' GB', `<input id="f-vol" type="number" min="0" value="${p.volumeGb || 0}">`, t('plan.zeroUnlimited'))}
      ${field(t('plan.days'), `<input id="f-days" type="number" min="0" value="${p.days || 0}">`, t('plan.zeroUnlimited'))}
      ${field(t('user.f.device'), `<input id="f-device" type="number" min="0" value="${p.deviceLimit || 0}">`)}
      ${field(t('user.f.up'), `<input id="f-up" type="number" min="0" value="${p.speedUp || 0}">`)}
      ${field(t('user.f.down'), `<input id="f-down" type="number" min="0" value="${p.speedDown || 0}">`)}
      ${field(t('user.f.resetDays'), `<input id="f-resetdays" type="number" min="1" value="${p.resetDays || 30}">`)}
      ${check('f-autoreset', t('user.f.autoReset'), !!p.autoReset, t('plan.autoResetHelp'))}
      <div class="full">${field(t('plan.lines'), `<div id="f-lines"></div>`, t('plan.linesHelp'))}</div>
    </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), desc: fv('f-desc'), volumeGb: Number(fv('f-vol')), days: Number(fv('f-days')),
      deviceLimit: Number(fv('f-device')), speedUp: Number(fv('f-up')), speedDown: Number(fv('f-down')),
      autoReset: fchk('f-autoreset'), resetDays: Number(fv('f-resetdays')),
    };
    const picked = picker.read();
    body.lineIds = picked.map(r => r.lineId);
    const nodes = {};
    picked.forEach(r => { if (r.nodeIds && r.nodeIds.length) nodes[String(r.lineId)] = r.nodeIds; });
    body.lineNodes = Object.keys(nodes).length ? nodes : null;
    if (id) await put('plans/' + id, body); else await post('plans', body);
    await load('plans');
    renderRows();
    toast(id ? t('plan.updated') : t('plan.created'), 'ok');
  }, { wide: true });
  const nodes = (Array.isArray(state.nodes) && state.nodes.length) ? state.nodes : (Array.isArray(state.status.nodes) ? state.status.nodes : []);
  const items = lineItems(state.lines, nodes, state.status.scope === 'reseller' ? (state.status.grants || []) : null);
  picker = linePicker(document.getElementById('f-lines'), { items, selected: keysFromRefs(refs, items) });
}

registerActions({
  'plan.add': () => editPlan(null),
  'plan.edit': id => editPlan(Number(id)),
  'plan.del': async id => {
    const p = state.plans.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: p.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('plans/' + id); await load('plans'); renderRows(); toast(t('plan.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});
