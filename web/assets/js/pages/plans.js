import { state, load, isReseller } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { lineItems, keysFromRefs, linePicker } from '../linepicker.js';
import { showUsersOfPlan } from './users.js';
import { esc, toast, confirm, openModal, registerActions, badge, field, empty, fv, fchk, switchRow } from '../ui.js';

export const title = () => t('plan.title');
export const subtitle = () => t('plan.subtitle');

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar"><span class="grow"></span><button class="btn primary" data-act="plan.add">${t('plan.add')}</button></div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('plan.quota')}</th><th>${t('plan.days')}</th><th>${t('user.devices')}</th><th class="c-p2">${t('user.speed')}</th><th class="c-p3">${t('plan.reset')}</th><th>${t('plan.lines')}</th><th>${t('plan.users')}</th>${isReseller() ? '' : `<th class="c-p3">${t('plan.rules')}</th>`}<th></th></tr></thead>
      <tbody id="plans-body"></tbody>
    </table></div>`;
  if (!isReseller()) rules = await get('rules').catch(() => []) || [];
  renderRows();
}

let rules = []; // 主面板的限速规则(套餐可以选跟哪几条)

const ruleChips = p => {
  const list = (p.ruleIds || []).map(id => rules.find(r => r.id === id)).filter(Boolean);
  return list.length ? list.map(r => badge(r.name, r.enabled ? 'primary' : '')).join(' ') : '<span class="muted">—</span>';
};

function renderRows() {
  const body = document.getElementById('plans-body');
  if (!body) return;
  if (!state.plans.length) { body.innerHTML = `<tr><td colspan="9">${empty(t('plan.empty'))}</td></tr>`; return; }
  body.innerHTML = state.plans.map(p => {
    const lines = Array.isArray(p.lineIds) ? p.lineIds.length : (p.lineIds ? JSON.parse(p.lineIds).length : 0);
    return `<tr>
      <td class="primary-cell">${esc(p.name)}${p.desc ? `<div class="sub-cell">${esc(p.desc)}</div>` : ''}${p.price > 0 ? `<div class="sub-cell">${esc(t('plan.priceShort', { n: p.price }))}</div>` : ''}</td>
      <td class="num">${p.volumeGb ? p.volumeGb + ' GB' : t('common.unlimited')}</td>
      <td class="num">${p.days ? p.days + ' ' + t('common.day') : t('common.unlimited')}${p.days && p.firstUse ? `<div class="sub-cell">${t('plan.firstUseShort')}</div>` : ''}</td>
      <td class="num">${p.deviceLimit || '∞'}</td>
      <td class="num c-p2">${(p.speedUp || p.speedDown) ? `${p.speedUp || '∞'}/${p.speedDown || '∞'} M` : '∞'}</td>
      <td class="c-p3">${p.autoReset ? badge(p.resetDays + ' ' + t('common.day'), 'primary') : badge(t('common.no'))}</td>
      <td class="num">${lines || t('plan.keepLines')}</td>
      <td class="num">${p.userCount ? `<a href="#" data-act="plan.users" data-id="${p.id}">${p.userCount}</a>` : '<span class="muted">0</span>'}</td>
      ${isReseller() ? '' : `<td class="c-p3">${ruleChips(p)}</td>`}
      <td class="actions">
        <button class="btn sm" data-act="plan.edit" data-id="${p.id}">${t('common.edit')}</button>
        <button class="btn sm danger" data-act="plan.del" data-id="${p.id}">${t('common.delete')}</button>
      </td></tr>`;
  }).join('');
}

// 规则的条件写成一句话,勾选的时候看得出是什么规则
const ruleCond = r => r.kind === 'burst'
  ? t('rule.condBurst', { win: r.windowMin, gb: r.thresholdGb, pen: r.penaltyMin })
  : `${r.start} – ${r.end}`;

export async function editPlan(id) {
  if (!isReseller()) rules = await get('rules').catch(() => rules) || [];
  const p = id ? state.plans.find(x => x.id === id) : { volumeGb: 100, days: 30, resetDays: 30, ruleIds: [] };
  const ids = Array.isArray(p.lineIds) ? p.lineIds : (p.lineIds ? JSON.parse(p.lineIds) : []);
  let nodesBy = {};
  try { nodesBy = typeof p.lineNodes === 'string' ? JSON.parse(p.lineNodes || '{}') : (p.lineNodes || {}); } catch { nodesBy = {}; }
  const refs = ids.map(id => ({ lineId: id, nodeIds: nodesBy[String(id)] || [] }));
  let picker = null;
  // 版式:通栏从上到下 —— 名称与说明一行;「额度与限速」六个数一行(0 = 不限只说一遍,不在每栏下面各写一行);
  // 两个开关各是一行带说明的开关(周期重置的天数就在那一行里);然后是线路、限速规则。
  // 以前所有栏挤在一个四列格子里,每栏下面的小字长短不一,勾选框和输入框混排,上下对不齐
  const ruleRow = r => `<label class="${r.enabled ? '' : 'muted'}"><input type="checkbox" class="plan-rule" value="${r.id}" ${r.allUsers ? 'checked disabled' : ((p.ruleIds || []).includes(r.id) ? 'checked' : '')}> ${esc(r.name)} <span class="muted small">${esc(ruleCond(r))} · ${r.upMbps || '—'}/${r.downMbps || '—'} M${r.enabled ? '' : ' · ' + t('common.disabled')}${r.allUsers ? ' · ' + t('plan.ruleAll') : ''}</span></label>`;
  openModal(id ? t('plan.edit') : t('plan.add'), `<div class="pl-form">
    <div class="form-grid pl-head">
      ${field(t('common.name'), `<input id="f-name" value="${esc(p.name || '')}" placeholder="${t('plan.namePh')}">`)}
      ${field(t('plan.desc'), `<input id="f-desc" value="${esc(p.desc || '')}">`)}
    </div>
    <h3 class="form-sec">${t('user.tab.quota')}</h3>
    <div class="form-grid pl-nums">
      ${field(t('plan.quota') + ' GB', `<input id="f-vol" type="number" min="0" step="any" value="${p.volumeGb || 0}">`)}
      ${field(t('plan.days'), `<input id="f-days" type="number" min="0" value="${p.days || 0}">`)}
      ${field(t('plan.f.device'), `<input id="f-device" type="number" min="0" value="${p.deviceLimit || 0}">`)}
      ${field(t('plan.f.up'), `<input id="f-up" type="number" min="0" value="${p.speedUp || 0}">`)}
      ${field(t('plan.f.down'), `<input id="f-down" type="number" min="0" value="${p.speedDown || 0}">`)}
      ${field(t('plan.price'), `<input id="f-price" type="number" min="0" step="any" value="${p.price || 0}" title="${esc(t('plan.priceHelp'))}">`)}
    </div>
    <p class="hint">${esc(t('plan.numsHelp'))}</p>
    <div class="pl-opts">
      <div class="sw-row"><div><b>${esc(t('user.f.autoReset'))}</b><small>${esc(t('plan.autoResetHelp'))}</small></div>
        <span class="pl-every">${esc(t('plan.everyA'))}<input id="f-resetdays" type="number" min="1" value="${p.resetDays || 30}" aria-label="${esc(t('user.f.resetDays'))}">${esc(t('plan.everyB'))}</span>
        <label class="switch"><input type="checkbox" id="f-autoreset" ${p.autoReset ? 'checked' : ''}><span></span></label></div>
      ${switchRow('f-firstuse', t('plan.firstUse'), !!p.firstUse, t('plan.firstUseHelp'))}
    </div>
    <h3 class="form-sec">${t('plan.lines')}</h3>
    <div id="f-lines"></div>
    <p class="hint">${esc(t('plan.linesHelp'))}</p>
    ${isReseller() ? '' : `<h3 class="form-sec">${t('plan.rules')}</h3>
      ${rules.length ? `<div class="check-list one-col">${rules.map(ruleRow).join('')}</div>` : `<p class="muted small">${t('plan.rulesNone')}</p>`}
      <p class="hint">${esc(t('plan.rulesHelp'))}</p>`}
  </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), desc: fv('f-desc'), volumeGb: Number(fv('f-vol')), days: Number(fv('f-days')),
      deviceLimit: Number(fv('f-device')), speedUp: Number(fv('f-up')), speedDown: Number(fv('f-down')),
      autoReset: fchk('f-autoreset'), resetDays: Number(fv('f-resetdays')), firstUse: fchk('f-firstuse'),
      price: Math.max(0, Number(fv('f-price')) || 0),
    };
    if (!isReseller()) body.ruleIds = [...document.querySelectorAll('.plan-rule:checked:not(:disabled)')].map(c => Number(c.value));
    const picked = picker.read();
    body.lineIds = picked.map(r => r.lineId);
    const nodes = {};
    picked.forEach(r => { if (r.nodeIds && r.nodeIds.length) nodes[String(r.lineId)] = r.nodeIds; });
    body.lineNodes = Object.keys(nodes).length ? nodes : null;
    if (id) await put('plans/' + id, body); else await post('plans', body);
    await load('plans');
    if (!isReseller()) rules = await get('rules').catch(() => rules) || [];
    renderRows();
    toast(id ? t('plan.updated') : t('plan.created'), 'ok');
  }, { size: 'xl' });
  // 周期重置没开时,天数那一格是灰的
  const ar = document.getElementById('f-autoreset'), rd = document.getElementById('f-resetdays');
  const syncReset = () => { rd.disabled = !ar.checked; };
  ar.addEventListener('change', syncReset);
  syncReset();
  const nodes = (Array.isArray(state.nodes) && state.nodes.length) ? state.nodes : (Array.isArray(state.status.nodes) ? state.status.nodes : []);
  const items = lineItems(state.lines, nodes, state.status.scope === 'reseller' ? (state.status.grants || []) : null);
  picker = linePicker(document.getElementById('f-lines'), { items, selected: keysFromRefs(refs, items) });
}

registerActions({
  'plan.add': () => editPlan(null),
  'plan.edit': id => editPlan(Number(id)),
  'plan.users': (id, _, e) => {
    if (e) e.preventDefault();
    const p = state.plans.find(x => x.id === Number(id));
    if (p) { showUsersOfPlan(p.id); location.hash = '#/users'; }
  },
  'plan.del': async id => {
    const p = state.plans.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: p.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('plans/' + id); await load('plans'); renderRows(); toast(t('plan.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});
